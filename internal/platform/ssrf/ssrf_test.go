package ssrf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestValidateURLBlocksInternalDestinations(t *testing.T) {
	p := Policy{}
	bad := []string{
		"http://127.0.0.1/", "http://localhost/admin", "http://10.0.0.5/", "http://192.168.1.1/", "http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data/", "http://[::1]/", "http://[fd00::1]/", "http://[::ffff:127.0.0.1]/",
		"http://0.0.0.0/", "http://2130706433/", "http://0x7f.0.0.1/", "http://017700000001/", "file:///etc/passwd",
		"gopher://example.com/", "http://user:pass@example.com/", "http://example.com:6379/", "http://metadata.google.internal/",
		"http://foo.internal/", "http://100.64.1.1/", "ftp://example.com/",
	}
	for _, u := range bad {
		if _, err := p.ValidateURL(u); !errors.Is(err, ErrBlocked) {
			t.Errorf("expected %q to be blocked, got %v", u, err)
		}
	}
	good := []string{"https://example.com/feed.xml", "http://news.example.md/rss", "https://example.org:443/x"}
	for _, u := range good {
		if _, err := p.ValidateURL(u); err != nil {
			t.Errorf("expected %q to be allowed: %v", u, err)
		}
	}
}

func TestIsBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "fe80::1", "::ffff:10.0.0.1", "100.64.0.1"} {
		if !IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700::1111"} {
		if IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

// The dial-time check must block a server on loopback even if URL validation is bypassed
// (e.g. hostname resolving to 127.0.0.1 — DNS rebinding).
func TestDialTimeBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	p := Policy{AllowedPorts: map[int]bool{}}
	c := p.Client(5 * time.Second)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := c.Do(req)
	if err == nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected dial to be blocked, got %v", err)
	}
}

// A public-looking first hop that redirects to an internal address must be blocked.
func TestRedirectToInternalBlocked(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("internal")) }))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirector.Close()
	// Allow private for the first hop only via a policy that permits loopback dialing but
	// still validates redirect targets: simulate by checking CheckRedirect directly.
	p := Policy{}
	c := p.Client(5 * time.Second)
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err := c.CheckRedirect(req, []*http.Request{{}}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to metadata endpoint must be blocked, got %v", err)
	}
	// End-to-end with AllowPrivate=false: the very first loopback hop is blocked too.
	if _, err := p.Get(context.Background(), c, redirector.URL, nil); err == nil {
		t.Fatal("expected loopback redirector to be blocked")
	}
}

func TestRedirectLimit(t *testing.T) {
	p := Policy{AllowPrivate: true, MaxRedirects: 2}
	c := p.Client(5 * time.Second)
	via := []*http.Request{{}, {}}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err := c.CheckRedirect(req, via); err == nil {
		t.Fatal("expected redirect cap error")
	}
}
