package notifications

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func apnsFor(t *testing.T, h http.HandlerFunc) (*APNs, *ecdsa.PrivateKey) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	a, err := NewAPNs("KEY123", "TEAM456", "md.example.news", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	a.BaseURL = srv.URL
	return a, k
}

func TestAPNsContract(t *testing.T) {
	var got struct {
		path, auth, topic, prio, collapse string
		body                              map[string]any
	}
	var key *ecdsa.PrivateKey
	a, key := apnsFor(t, func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.topic, got.prio, got.collapse = r.URL.Path, r.Header.Get("authorization"), r.Header.Get("apns-topic"),
			r.Header.Get("apns-priority"), r.Header.Get("apns-collapse-id")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(410)
		case strings.HasSuffix(r.URL.Path, "/bad"):
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"reason":"BadDeviceToken"}`)
		case strings.HasSuffix(r.URL.Path, "/busy"):
			w.WriteHeader(503)
		default:
			w.Header().Set("apns-id", "abc-123")
		}
	})
	res, err := a.Send(context.Background(), PushMessage{Token: "tok", Title: "T", Body: "B", Data: map[string]string{"event_id": "e1"}, CollapseKey: "ev-e1"})
	if err != nil || res.ProviderMessageID != "abc-123" {
		t.Fatalf("send: %+v %v", res, err)
	}
	if got.path != "/3/device/tok" || got.topic != "md.example.news" || got.prio != "5" || got.collapse != "ev-e1" {
		t.Fatalf("request: %+v", got)
	}
	alert := got.body["aps"].(map[string]any)["alert"].(map[string]any)
	if alert["title"] != "T" || got.body["event_id"] != "e1" {
		t.Fatalf("payload: %+v", got.body)
	}
	// Provider token: ES256 with kid and team issuer, verifiable with the public key.
	tok, err := jwt.Parse(strings.TrimPrefix(got.auth, "bearer "), func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"ES256"}))
	if err != nil || tok.Header["kid"] != "KEY123" || tok.Claims.(jwt.MapClaims)["iss"] != "TEAM456" {
		t.Fatalf("provider token: %v %+v", err, tok)
	}
	for _, tc := range []string{"gone", "bad"} {
		if r, err := a.Send(context.Background(), PushMessage{Token: tc}); err != nil || !r.InvalidToken {
			t.Fatalf("%s: expected invalid token, got %+v %v", tc, r, err)
		}
	}
	var te *TransientError
	if _, err := a.Send(context.Background(), PushMessage{Token: "busy"}); !errors.As(err, &te) || te.Status != 503 {
		t.Fatalf("expected transient: %v", err)
	}
}

func TestFCMContract(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	var oauthCalls int
	var sent map[string]any
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			oauthCalls++
			_ = r.ParseForm()
			assertion, err := jwt.Parse(r.Form.Get("assertion"), func(*jwt.Token) (any, error) { return &k.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
			c := assertion.Claims.(jwt.MapClaims)
			if err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || c["aud"] != srvURL+"/token" ||
				c["scope"] != "https://www.googleapis.com/auth/firebase.messaging" {
				w.WriteHeader(400)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"ya29.test","expires_in":3600}`)
		case "/v1/projects/proj-1/messages:send":
			if r.Header.Get("Authorization") != "Bearer ya29.test" {
				w.WriteHeader(401)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			msg := body["message"].(map[string]any)
			if msg["token"] == "dead" {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`)
				return
			}
			sent = msg
			_, _ = io.WriteString(w, `{"name":"projects/proj-1/messages/42"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	sa, _ := json.Marshal(map[string]string{"client_email": "push@proj-1.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": srv.URL + "/token"})
	f, err := NewFCM("proj-1", string(sa))
	if err != nil {
		t.Fatal(err)
	}
	f.BaseURL = srv.URL
	res, err := f.Send(context.Background(), PushMessage{Token: "tok", Title: "T", Body: "B", Data: map[string]string{"type": "digest"}, HighPriority: true})
	if err != nil || res.ProviderMessageID != "projects/proj-1/messages/42" {
		t.Fatalf("send: %+v %v", res, err)
	}
	if sent["notification"].(map[string]any)["title"] != "T" || sent["android"].(map[string]any)["priority"] != "high" {
		t.Fatalf("message: %+v", sent)
	}
	if r, err := f.Send(context.Background(), PushMessage{Token: "dead"}); err != nil || !r.InvalidToken {
		t.Fatalf("unregistered: %+v %v", r, err)
	}
	if oauthCalls != 1 {
		t.Fatalf("access token should be cached, got %d oauth calls", oauthCalls)
	}
}

func TestUnconfiguredNeverClaimsDelivery(t *testing.T) {
	if _, err := NewAPNs("", "", "", "", false); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	if _, err := NewFCM("", ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	if _, err := (Unconfigured{Provider: "apns"}).Send(context.Background(), PushMessage{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
}
