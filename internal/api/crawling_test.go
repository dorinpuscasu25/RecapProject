package api_test

import (
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

// The crawling switch is a database flag the scheduler reads; it never fetches real sites while
// CRAWL_LIVE_ENABLED is off (the test default), and only source managers may change it.
func TestCrawlingSwitchAndStatus(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("crawl-admin@example.com", "super_admin")
	user, _, _ := e.RegisterVerified("crawl-user@example.com")

	st := e.MustStatus(e.Do("GET", "/v1/admin/crawling", admin, nil), 200)
	if st.Get("scheduling") != false || st.Get("live_fetch_allowed") != false {
		t.Fatalf("expected scheduling off without CRAWL_LIVE_ENABLED: %s", st.Raw)
	}
	on := e.MustStatus(e.Do("PUT", "/v1/admin/crawling", admin, map[string]any{"enabled": true}), 200)
	if on.Get("flag_enabled") != true || on.Get("scheduling") != false {
		t.Fatalf("flag should be on but scheduling still gated by the environment: %s", on.Raw)
	}
	off := e.MustStatus(e.Do("PUT", "/v1/admin/crawling", admin, map[string]any{"enabled": false}), 200)
	if off.Get("flag_enabled") != false {
		t.Fatalf("flag should be off: %s", off.Raw)
	}
	e.MustStatus(e.Do("POST", "/v1/admin/crawling/run-now", admin, nil), 409)
	e.MustStatus(e.Do("PUT", "/v1/admin/crawling", user, map[string]any{"enabled": true}), 403)

	// Importing a preset requires a stated legal basis.
	r := e.Do("POST", "/v1/admin/source-presets/moldova/import", admin, map[string]any{"enable": true})
	if r.Status != 422 {
		t.Fatalf("import without legal basis: want 422, got %d %s", r.Status, r.Raw)
	}
	e.MustStatus(e.Do("POST", "/v1/admin/source-presets/..%2Fsecrets/import", admin, map[string]any{"legal_basis": "x"}), 404)
}
