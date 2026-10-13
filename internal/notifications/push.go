package notifications

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type PushMessage struct {
	Token        string
	Title        string
	Body         string
	Data         map[string]string
	CollapseKey  string
	HighPriority bool
}

type PushResult struct {
	ProviderMessageID string
	InvalidToken      bool
}

// PushProvider sends to one device. Implementations never log the device token.
type PushProvider interface {
	Name() string
	Send(ctx context.Context, m PushMessage) (*PushResult, error)
}

// ErrNotConfigured means the provider has no credentials: deliveries are recorded as
// provider_unavailable (explicit), never as sent.
var ErrNotConfigured = errors.New("push provider not configured")

type TransientError struct{ Status int }

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient push error (status %d)", e.Status)
}

// ---------- APNs (token-based auth, HTTP/2) ----------

type APNs struct {
	KeyID, TeamID, Topic string
	Key                  *ecdsa.PrivateKey
	BaseURL              string // https://api.push.apple.com or https://api.sandbox.push.apple.com
	Client               *http.Client
	mu                   sync.Mutex
	jwt                  string
	jwtAt                time.Time
}

func NewAPNs(keyID, teamID, topic, keyPEM string, production bool) (*APNs, error) {
	if keyID == "" || teamID == "" || topic == "" || keyPEM == "" {
		return nil, ErrNotConfigured
	}
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, errors.New("APNS_KEY_PEM is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse APNs key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APNs key must be an EC P-256 key")
	}
	base := "https://api.sandbox.push.apple.com"
	if production {
		base = "https://api.push.apple.com"
	}
	return &APNs{KeyID: keyID, TeamID: teamID, Topic: topic, Key: ec, BaseURL: base, Client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (a *APNs) Name() string { return "apns" }

func (a *APNs) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Since(a.jwtAt) < 50*time.Minute {
		return a.jwt, nil
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": a.TeamID, "iat": time.Now().Unix()})
	t.Header["kid"] = a.KeyID
	s, err := t.SignedString(a.Key)
	if err != nil {
		return "", err
	}
	a.jwt, a.jwtAt = s, time.Now()
	return s, nil
}

func (a *APNs) Send(ctx context.Context, m PushMessage) (*PushResult, error) {
	tok, err := a.token()
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"aps": map[string]any{"alert": map[string]string{"title": m.Title, "body": m.Body}, "sound": "default"}}
	for k, v := range m.Data {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(a.BaseURL, "/")+"/3/device/"+url.PathEscape(m.Token), bytes.NewReader(b))
	req.Header.Set("authorization", "bearer "+tok)
	req.Header.Set("apns-topic", a.Topic)
	req.Header.Set("apns-push-type", "alert")
	if m.HighPriority {
		req.Header.Set("apns-priority", "10")
	} else {
		req.Header.Set("apns-priority", "5")
	}
	if m.CollapseKey != "" {
		req.Header.Set("apns-collapse-id", m.CollapseKey)
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, &TransientError{}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == 200:
		return &PushResult{ProviderMessageID: resp.Header.Get("apns-id")}, nil
	case resp.StatusCode == 410 || (resp.StatusCode == 400 && (strings.Contains(string(raw), "BadDeviceToken") || strings.Contains(string(raw), "DeviceTokenNotForTopic"))):
		return &PushResult{InvalidToken: true}, nil
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return nil, &TransientError{Status: resp.StatusCode}
	default:
		return nil, fmt.Errorf("apns status %d: %s", resp.StatusCode, string(raw))
	}
}

// ---------- FCM HTTP v1 (service account OAuth) ----------

type FCM struct {
	ProjectID   string
	ClientEmail string
	Key         *rsa.PrivateKey
	TokenURI    string
	BaseURL     string // https://fcm.googleapis.com
	Client      *http.Client
	mu          sync.Mutex
	access      string
	accessExp   time.Time
}

func NewFCM(projectID, serviceAccountJSON string) (*FCM, error) {
	if projectID == "" || serviceAccountJSON == "" {
		return nil, ErrNotConfigured
	}
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(serviceAccountJSON), &sa); err != nil {
		return nil, fmt.Errorf("FCM service account JSON: %w", err)
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, errors.New("FCM private key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("FCM key must be RSA")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &FCM{ProjectID: projectID, ClientEmail: sa.ClientEmail, Key: rk, TokenURI: sa.TokenURI, BaseURL: "https://fcm.googleapis.com",
		Client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (f *FCM) Name() string { return "fcm" }

func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access != "" && time.Now().Before(f.accessExp.Add(-time.Minute)) {
		return f.access, nil
	}
	now := time.Now()
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": f.ClientEmail, "scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud": f.TokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}).SignedString(f.Key)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.TokenURI, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", &TransientError{}
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&tr) != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("fcm oauth failed (status %d)", resp.StatusCode)
	}
	f.access, f.accessExp = tr.AccessToken, now.Add(time.Duration(tr.ExpiresIn)*time.Second)
	return f.access, nil
}

func (f *FCM) Send(ctx context.Context, m PushMessage) (*PushResult, error) {
	tok, err := f.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	msg := map[string]any{"token": m.Token, "notification": map[string]string{"title": m.Title, "body": m.Body}, "data": m.Data}
	android := map[string]any{"priority": "normal"}
	if m.HighPriority {
		android["priority"] = "high"
	}
	if m.CollapseKey != "" {
		android["collapse_key"] = m.CollapseKey
	}
	msg["android"] = android
	b, _ := json.Marshal(map[string]any{"message": msg})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(f.BaseURL, "/")+"/v1/projects/"+url.PathEscape(f.ProjectID)+"/messages:send", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, &TransientError{}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == 200:
		var r struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &r)
		return &PushResult{ProviderMessageID: r.Name}, nil
	case resp.StatusCode == 404 || strings.Contains(string(raw), "UNREGISTERED") || (resp.StatusCode == 400 && strings.Contains(string(raw), "registration token")):
		return &PushResult{InvalidToken: true}, nil
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return nil, &TransientError{Status: resp.StatusCode}
	default:
		return nil, fmt.Errorf("fcm status %d: %s", resp.StatusCode, string(raw))
	}
}

// Unconfigured is the explicit no-credentials provider: it never claims delivery.
type Unconfigured struct{ Provider string }

func (u Unconfigured) Name() string { return u.Provider }
func (u Unconfigured) Send(context.Context, PushMessage) (*PushResult, error) {
	return nil, ErrNotConfigured
}
