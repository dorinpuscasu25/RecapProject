package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ssrf"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

const (
	maxFeedBytes = 5 << 20
	maxPageBytes = 3 << 20
)

// FetchResult is a bounded HTTP (or fixture) response.
type FetchResult struct {
	URL          string
	FinalURL     string
	Status       int
	ContentType  string
	Body         []byte
	ETag         string
	LastModified string
	NotModified  bool
	FromFixture  bool
}

// Fetcher performs polite, SSRF-safe fetches with robots.txt compliance and per-source rate limits.
type Fetcher struct {
	Policy      ssrf.Policy
	UserAgent   string
	FixtureDir  string
	Limiter     ratelimit.Limiter
	LiveEnabled bool
	// FixtureAnchor, when set, is the reference "now" the fixture corpus was written for: dates of
	// fixture content are shifted by (now - anchor) so recency windows behave the same on any day.
	// Development/test only; it never applies to live endpoints.
	FixtureAnchor *time.Time
	client        *http.Client
	robots        robotsCache
}

func NewFetcher(policy ssrf.Policy, ua, fixtureDir string, limiter ratelimit.Limiter, live bool) *Fetcher {
	return &Fetcher{Policy: policy, UserAgent: ua, FixtureDir: fixtureDir, Limiter: limiter, LiveEnabled: live, client: policy.Client(30 * time.Second)}
}

// FixtureShift returns the offset to apply to fixture content dates.
func (f *Fetcher) FixtureShift(now time.Time) time.Duration {
	if f.FixtureAnchor == nil {
		return 0
	}
	return now.Sub(*f.FixtureAnchor).Truncate(time.Minute)
}

var ErrRobotsDisallowed = errors.New("disallowed by robots.txt")
var ErrLiveDisabled = errors.New("live crawling is disabled (CRAWL_LIVE_ENABLED=false)")

type Conditional struct {
	ETag         string
	LastModified string
}

// Fetch retrieves rawURL on behalf of an endpoint. Fixture endpoints never touch the network.
func (f *Fetcher) Fetch(ctx context.Context, ep *sources.Endpoint, rawURL string, cond *Conditional, maxBytes int64, interactive bool) (*FetchResult, error) {
	if ep != nil && ep.EndpointType == "fixture" {
		return f.fetchFixture(rawURL, maxBytes)
	}
	if !f.LiveEnabled && !interactive {
		return nil, ErrLiveDisabled
	}
	u, err := f.Policy.ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	if ep != nil {
		if err := f.rateWait(ctx, ep); err != nil {
			return nil, err
		}
	}
	if ok, err := f.robotsAllowed(ctx, u); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrRobotsDisallowed
	}
	timeout := 20 * time.Second
	if ep != nil && ep.TimeoutSeconds > 0 {
		timeout = time.Duration(ep.TimeoutSeconds) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("User-Agent", f.UserAgent)
	hdr.Set("Accept", "application/rss+xml, application/atom+xml, application/feed+json, application/xml;q=0.9, text/html;q=0.8, */*;q=0.5")
	hdr.Set("Accept-Language", "ro, ru;q=0.9, en;q=0.5")
	if cond != nil {
		if cond.ETag != "" {
			hdr.Set("If-None-Match", cond.ETag)
		}
		if cond.LastModified != "" {
			hdr.Set("If-Modified-Since", cond.LastModified)
		}
	}
	resp, err := f.Policy.Get(cctx, f.client, u.String(), hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	res := &FetchResult{URL: rawURL, FinalURL: resp.Request.URL.String(), Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
		ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified {
		res.NotModified = true
		return res, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return res, &TransientHTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	if resp.StatusCode >= 400 {
		return res, &PermanentHTTPError{Status: resp.StatusCode}
	}
	if mt, _, _ := mime.ParseMediaType(res.ContentType); mt != "" && !allowedType(mt) {
		return res, &PermanentHTTPError{Status: resp.StatusCode, Reason: "unsupported content type " + mt}
	}
	res.Body, err = ssrf.ReadLimited(resp.Body, maxBytes)
	return res, err
}

func allowedType(mt string) bool {
	switch {
	case strings.HasPrefix(mt, "text/"), strings.Contains(mt, "xml"), strings.Contains(mt, "json"), mt == "application/octet-stream":
		return true
	}
	return false
}

type TransientHTTPError struct {
	Status     int
	RetryAfter string
}

func (e *TransientHTTPError) Error() string { return fmt.Sprintf("transient http status %d", e.Status) }

type PermanentHTTPError struct {
	Status int
	Reason string
}

func (e *PermanentHTTPError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("http %d: %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("http status %d", e.Status)
}

func (f *Fetcher) rateWait(ctx context.Context, ep *sources.Endpoint) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, retry, err := f.Limiter.Allow(ctx, "crawl:"+ep.ID.String(), ep.MaxRequestsPerMinute, time.Minute)
		if err != nil || ok {
			return nil
		}
		if time.Now().Add(retry).After(deadline) {
			return apperr.RateLimited(retry).WithDetail("reason", "source_rate_limit")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
}

func (f *Fetcher) robotsAllowed(ctx context.Context, u *url.URL) (bool, error) {
	key := u.Scheme + "://" + u.Host
	r, ok := f.robots.get(key)
	if !ok {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		hdr := http.Header{"User-Agent": {f.UserAgent}}
		resp, err := f.Policy.Get(cctx, f.client, key+"/robots.txt", hdr)
		switch {
		case err != nil:
			return false, fmt.Errorf("robots.txt unavailable: %w", err)
		case resp.StatusCode >= 500:
			resp.Body.Close()
			return false, &TransientHTTPError{Status: resp.StatusCode} // RFC 9309: assume disallow on server errors
		case resp.StatusCode >= 400:
			resp.Body.Close()
			r = &Robots{} // no robots.txt: allowed
		default:
			b, _ := ssrf.ReadLimited(resp.Body, 512*1024)
			resp.Body.Close()
			r = ParseRobots(b)
		}
		f.robots.put(key, r)
	}
	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if path == "" {
		path = "/"
	}
	return r.Allowed(f.UserAgent, path), nil
}

// fetchFixture resolves fixture://relative/path or an http(s) URL mapped to
// FIXTURE_DIR/pages/<host>/<path>.html. Paths are confined to FIXTURE_DIR.
func (f *Fetcher) fetchFixture(raw string, maxBytes int64) (*FetchResult, error) {
	var rel string
	if strings.HasPrefix(raw, "fixture://") {
		rel = strings.TrimPrefix(raw, "fixture://")
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		p := strings.Trim(u.EscapedPath(), "/")
		if p == "" {
			p = "index"
		}
		rel = filepath.Join("pages", u.Hostname(), p)
		if filepath.Ext(rel) == "" {
			rel += ".html"
		}
	}
	root, _ := filepath.Abs(f.FixtureDir)
	full, _ := filepath.Abs(filepath.Join(root, filepath.FromSlash(rel)))
	if !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return nil, fmt.Errorf("fixture path escapes fixture dir")
	}
	b, err := os.ReadFile(full)
	if errors.Is(err, os.ErrNotExist) {
		return &FetchResult{URL: raw, FinalURL: raw, Status: 404, FromFixture: true}, &PermanentHTTPError{Status: 404, Reason: "fixture not found"}
	}
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("fixture exceeds %d bytes", maxBytes)
	}
	ct := "text/html"
	if strings.HasSuffix(full, ".xml") {
		ct = "application/xml"
	} else if strings.HasSuffix(full, ".json") {
		ct = "application/feed+json"
	}
	return &FetchResult{URL: raw, FinalURL: raw, Status: 200, Body: b, ContentType: ct, FromFixture: true}, nil
}
