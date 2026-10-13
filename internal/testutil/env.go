// Package testutil provides an integration-test environment: an isolated PostgreSQL database
// cloned from a migrated template, a fully wired App and an httptest server.
package testutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/app"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/storage"
	"github.com/dorinpuscasu25/recapproject/internal/seed"
	"github.com/dorinpuscasu25/recapproject/migrations"
)

// AdminURL is the maintenance connection used to create per-test databases.
func AdminURL() string {
	if v := os.Getenv("TEST_DATABASE_ADMIN_URL"); v != "" {
		return v
	}
	return "postgres://newsintel:newsintel@localhost:5432/postgres?sslmode=disable"
}

func withDB(url, name string) string {
	re := regexp.MustCompile(`/[^/?]*(\?|$)`)
	idx := strings.Index(url, "://")
	head, tail := url[:idx+3], url[idx+3:]
	return head + re.ReplaceAllString(tail, "/"+name+"$1")
}

func migrationsHash() string {
	h := sha256.New()
	_ = fs.WalkDir(migrations.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(migrations.FS, p)
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}

// NewDatabase returns a fresh migrated database URL or skips the test if Postgres is unavailable.
func NewDatabase(t testing.TB) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, AdminURL())
	if err != nil {
		t.Skipf("integration database unavailable (%v); set TEST_DATABASE_ADMIN_URL", err)
	}
	defer admin.Close(ctx)
	tmpl := "newsintel_tmpl_" + migrationsHash()
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(424242)`); err != nil {
		t.Fatal(err)
	}
	var exists bool
	_ = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, tmpl).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+tmpl); err != nil {
			_, _ = admin.Exec(ctx, `SELECT pg_advisory_unlock(424242)`)
			t.Fatal(err)
		}
		if err := db.Migrate(ctx, withDB(AdminURL(), tmpl)); err != nil {
			_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+tmpl)
			_, _ = admin.Exec(ctx, `SELECT pg_advisory_unlock(424242)`)
			t.Fatalf("migrate template: %v", err)
		}
	}
	name := "newsintel_t_" + strings.ReplaceAll(ids.New().String(), "-", "")[:20]
	_, err = admin.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE `+tmpl)
	_, _ = admin.Exec(ctx, `SELECT pg_advisory_unlock(424242)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a, err := pgx.Connect(c, AdminURL())
		if err == nil {
			_, _ = a.Exec(c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
			a.Close(c)
		}
	})
	return withDB(AdminURL(), name)
}

// RepoRoot locates the repository root (for fixtures).
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Env is a running test environment.
type Env struct {
	T      testing.TB
	App    *app.App
	Server *httptest.Server
	Ctx    context.Context
}

type EnvOption func(*config.Config, *app.Options)

// envSettings are test-harness settings (not app config).
var skipCategorySeed = map[*config.Config]bool{}

// WithoutCategorySeed starts with an empty taxonomy (admin creates categories in the test).
func WithoutCategorySeed() EnvOption {
	return func(c *config.Config, _ *app.Options) { skipCategorySeed[c] = true }
}

// WithRateLimiter overrides the limiter (default: no-op to avoid flakiness; rate-limit tests use memory).
func WithRateLimiter(l ratelimit.Limiter) EnvOption {
	return func(_ *config.Config, o *app.Options) { o.Limiter = l }
}

func WithConfig(fn func(*config.Config)) EnvOption {
	return func(c *config.Config, _ *app.Options) { fn(c) }
}

func NewEnv(t testing.TB, opts ...EnvOption) *Env {
	t.Helper()
	url := NewDatabase(t)
	t.Setenv("DATABASE_URL", url)
	t.Setenv("APP_ENV", "test")
	t.Setenv("MAIL_DRIVER", "devsink")
	t.Setenv("BUS_DRIVER", "postgres")
	t.Setenv("REDIS_URL", "")
	t.Setenv("AUTH_ARGON2_MEMORY_KIB", "1024")
	t.Setenv("AUTH_ARGON2_ITERATIONS", "1")
	t.Setenv("AUTH_ARGON2_PARALLELISM", "1")
	t.Setenv("FIXTURE_DIR", filepath.Join(RepoRoot(), "fixtures"))
	t.Setenv("STORAGE_DIR", t.TempDir())
	t.Setenv("CRAWL_LIVE_ENABLED", "false")
	t.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	t.Setenv("AUTH_CODE_HMAC_SECRET", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	o := app.Options{Limiter: ratelimit.Noop{}, Store: &storage.Filesystem{Root: t.TempDir()}}
	for _, fn := range opts {
		fn(cfg, &o)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	a, err := app.New(ctx, cfg, log, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Run(ctx, a.DB, seed.Options{Categories: !skipCategorySeed[cfg], Entities: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() {
		srv.Close()
		a.Close()
	})
	return &Env{T: t, App: a, Server: srv, Ctx: ctx}
}

// Resp is a decoded HTTP response.
type Resp struct {
	Status int
	Body   map[string]any
	Raw    []byte
	Header http.Header
}

func (r *Resp) Str(path ...string) string {
	v := r.Get(path...)
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// Get walks nested maps/arrays: Get("items","0","id").
func (r *Resp) Get(path ...string) any {
	var cur any = r.Body
	for _, p := range path {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			var i int
			fmt.Sscanf(p, "%d", &i)
			if i < 0 || i >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
	}
	return cur
}

func (r *Resp) Items() []any {
	if v, ok := r.Body["items"].([]any); ok {
		return v
	}
	return nil
}

// Do sends a JSON request. token may be "" (anonymous).
func (e *Env) Do(method, path, token string, body any) *Resp {
	e.T.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.Server.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := &Resp{Status: resp.StatusCode, Raw: raw, Header: resp.Header}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

// MustStatus fails the test if status differs.
func (e *Env) MustStatus(r *Resp, want int) *Resp {
	e.T.Helper()
	if r.Status != want {
		e.T.Fatalf("want status %d, got %d: %s", want, r.Status, string(r.Raw))
	}
	return r
}

// Drain processes all pending messages, jobs and mail.
func (e *Env) Drain() {
	e.T.Helper()
	if err := e.App.Drain(e.Ctx); err != nil {
		e.T.Fatal(err)
	}
}

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

// LatestCode reads the newest 6-digit code mailed to addr from the dev sink.
func (e *Env) LatestCode(addr string) string {
	e.T.Helper()
	e.Drain()
	r := e.Do("GET", "/v1/dev/mailbox?to="+addr, "", nil)
	for _, it := range r.Items() {
		m := it.(map[string]any)
		if c := codeRe.FindStringSubmatch(fmt.Sprint(m["subject"])); c != nil {
			return c[1]
		}
	}
	e.T.Fatalf("no code mailed to %s: %s", addr, string(r.Raw))
	return ""
}

// RegisterVerified registers, verifies and returns (accessToken, refreshToken, userID).
func (e *Env) RegisterVerified(email string) (string, string, string) {
	e.T.Helper()
	e.MustStatus(e.Do("POST", "/v1/auth/register", "", map[string]any{
		"first_name": "Test", "last_name": "User", "email": email, "phone": "+37369123456", "password": "Str0ng-Passw0rd!",
		"accept_terms": true, "accept_privacy": true,
	}), 202)
	code := e.LatestCode(email)
	r := e.MustStatus(e.Do("POST", "/v1/auth/verify-email", "", map[string]any{"email": email, "code": code}), 200)
	return r.Str("access_token"), r.Str("refresh_token"), r.Str("user", "id")
}

// Admin creates a verified user with the given staff roles (via bootstrap + role API) and returns a token.
func (e *Env) Admin(email string, roles ...string) string {
	e.T.Helper()
	tok, _, uid := e.RegisterVerified(email)
	if len(roles) == 0 {
		roles = []string{"super_admin"}
	}
	ctx := e.Ctx
	if _, err := e.App.DB.Pool.Exec(ctx, `INSERT INTO user_roles (user_id, role_code) SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`, uid, roles); err != nil {
		e.T.Fatal(err)
	}
	identityInvalidate()
	return tok
}

// FixtureSource describes a synthetic source to configure through the admin API.
type FixtureSource struct {
	Name, Domain, Type, Feed string
	FetchPages               bool
	Languages                []string
}

// SetupSource creates a publisher, grants permission, creates and enables a fixture endpoint.
// Returns (publisherID, endpointID).
func (e *Env) SetupSource(admin string, s FixtureSource) (string, string) {
	e.T.Helper()
	if s.Type == "" {
		s.Type = "newsroom"
	}
	pub := e.MustStatus(e.Do("POST", "/v1/admin/publishers", admin, map[string]any{"name": s.Name, "canonical_domain": s.Domain,
		"publisher_type": s.Type, "primary_languages": s.Languages}), 201)
	pid := pub.Str("id")
	e.MustStatus(e.Do("PUT", "/v1/admin/publishers/"+pid+"/permissions", admin, map[string]any{"legal_basis": "synthetic test fixture",
		"permission_status": "granted", "allow_crawl": true, "allow_store_full_text": true, "allow_summarize": true, "allow_embed": true,
		"allow_snippets": true, "allow_audio": true, "robots_review_status": "compliant", "terms_review_status": "compliant", "attribution_text": "Sursa: " + s.Name}), 200)
	e.MustStatus(e.Do("PATCH", "/v1/admin/publishers/"+pid, admin, map[string]any{"status": "active"}), 200)
	ep := e.MustStatus(e.Do("POST", "/v1/admin/sources", admin, map[string]any{"publisher_id": pid, "name": s.Name + " feed", "endpoint_type": "fixture",
		"entry_url": "fixture://feeds/" + s.Feed, "fetch_article_pages": s.FetchPages, "languages": s.Languages, "poll_interval_seconds": 900}), 201)
	eid := ep.Str("id")
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+eid+"/enable", admin, nil), 200)
	return pid, eid
}

// Crawl forces a crawl of an endpoint and processes the whole pipeline.
func (e *Env) Crawl(admin, endpointID string) {
	e.T.Helper()
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+endpointID+"/crawl", admin, map[string]any{"max_items": 20}), 202)
	e.Drain()
}

// SwitchFeed points an endpoint at another fixture feed file.
func (e *Env) SwitchFeed(admin, endpointID, feed string) {
	e.T.Helper()
	e.MustStatus(e.Do("PATCH", "/v1/admin/sources/"+endpointID, admin, map[string]any{"entry_url": "fixture://feeds/" + feed}), 200)
}
