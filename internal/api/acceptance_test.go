package api_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

func articleID(t *testing.T, e *testutil.Env, title string) string {
	t.Helper()
	var id string
	if err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT id::text FROM articles WHERE title=$1`, title).Scan(&id); err != nil {
		t.Fatalf("article %q: %v", title, err)
	}
	return id
}

func articleCats(t *testing.T, e *testutil.Env, admin, id string) map[string]string {
	t.Helper()
	a := e.MustStatus(e.Do("GET", "/v1/admin/articles/"+id, admin, nil), 200)
	out := map[string]string{}
	for _, c := range a.Get("categories").([]any) {
		m := c.(map[string]any)
		out[m["slug"].(string)] = m["source"].(string)
	}
	return out
}

// Scenario 1: an admin onboards a source end to end through the API, without code changes.
func TestSourceAdminLifecycle(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("src-admin@example.com", "admin")
	pub := e.MustStatus(e.Do("POST", "/v1/admin/publishers", admin, map[string]any{"name": "Știri Diverse Exemplu", "canonical_domain": "diverse-exemplu.md",
		"publisher_type": "newsroom", "primary_languages": []string{"ro"}}), 201)
	pid := pub.Str("id")
	ep := e.MustStatus(e.Do("POST", "/v1/admin/sources", admin, map[string]any{"publisher_id": pid, "name": "Diverse RSS", "endpoint_type": "fixture",
		"entry_url": "fixture://feeds/diverse-ro.xml", "languages": []string{"ro"}, "poll_interval_seconds": 900}), 201)
	eid := ep.Str("id")
	// Cannot enable before the publisher is active and permissions are granted.
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+eid+"/enable", admin, nil), 422)
	e.MustStatus(e.Do("PATCH", "/v1/admin/publishers/"+pid, admin, map[string]any{"status": "active"}), 200)
	e.MustStatus(e.Do("PUT", "/v1/admin/publishers/"+pid+"/permissions", admin, map[string]any{"legal_basis": "pending review",
		"permission_status": "pending", "allow_crawl": false}), 200)
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+eid+"/enable", admin, nil), 422)
	e.MustStatus(e.Do("PUT", "/v1/admin/publishers/"+pid+"/permissions", admin, map[string]any{"legal_basis": "written agreement (synthetic)",
		"permission_status": "granted", "allow_crawl": true, "allow_store_full_text": true, "allow_summarize": true, "allow_embed": true,
		"allow_snippets": true, "robots_review_status": "compliant", "terms_review_status": "compliant", "attribution_text": "Sursa: Diverse"}), 200)

	// Adjust polling interval (bounds enforced) and test extraction before enabling.
	e.MustStatus(e.Do("PATCH", "/v1/admin/sources/"+eid, admin, map[string]any{"poll_interval_seconds": 5}), 422)
	upd := e.MustStatus(e.Do("PATCH", "/v1/admin/sources/"+eid, admin, map[string]any{"poll_interval_seconds": 1800}), 200)
	if upd.Get("poll_interval_seconds").(float64) != 1800 {
		t.Fatalf("poll interval: %s", upd.Raw)
	}
	test := e.MustStatus(e.Do("POST", "/v1/admin/sources/"+eid+"/test", admin, nil), 200)
	if test.Get("ok") != true || test.Get("item_count").(float64) != 3 || test.Str("detected_format") == "" {
		t.Fatalf("source test: %s", test.Raw)
	}
	var nArticles int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM articles`).Scan(&nArticles)
	if nArticles != 0 {
		t.Fatal("testing a source must not persist articles")
	}
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+eid+"/enable", admin, nil), 200)

	// First crawl: the endpoint is configured to fetch article pages, which this source does not
	// serve. The feed parses, but item failures must surface as a partial run and degraded health.
	e.Crawl(admin, eid)
	h := e.MustStatus(e.Do("GET", "/v1/admin/sources/"+eid+"/health", admin, nil), 200)
	if h.Str("health_state") != "degraded" || h.Get("articles_total").(float64) != 0 || h.Str("last_error") == "" {
		t.Fatalf("item failures must degrade health: %s", h.Raw)
	}
	runs := e.MustStatus(e.Do("GET", "/v1/admin/sources/"+eid+"/runs", admin, nil), 200)
	if r0 := runs.Items()[0].(map[string]any); r0["status"] != "partial" || r0["error_count"].(float64) != 3 {
		t.Fatalf("run should be partial with errors: %s", runs.Raw)
	}

	// The admin fixes the configuration (feed-only extraction) without code changes.
	e.MustStatus(e.Do("PATCH", "/v1/admin/sources/"+eid, admin, map[string]any{"fetch_article_pages": false}), 200)
	e.Crawl(admin, eid)
	h = e.MustStatus(e.Do("GET", "/v1/admin/sources/"+eid+"/health", admin, nil), 200)
	if h.Str("health_state") != "healthy" || h.Get("articles_total").(float64) != 3 || h.Get("last_success_at") == nil {
		t.Fatalf("health: %s", h.Raw)
	}
	runs = e.MustStatus(e.Do("GET", "/v1/admin/sources/"+eid+"/runs", admin, nil), 200)
	if len(runs.Items()) != 2 || runs.Items()[0].(map[string]any)["status"] != "succeeded" || runs.Items()[0].(map[string]any)["accepted_count"].(float64) != 3 {
		t.Fatalf("runs: %s", runs.Raw)
	}
	// Every change is audited with the acting admin.
	audit := e.MustStatus(e.Do("GET", "/v1/admin/audit-logs?resource_type=source_endpoint", admin, nil), 200)
	if len(audit.Items()) < 3 {
		t.Fatalf("audit trail: %s", audit.Raw)
	}
	// Disabling stops scheduling.
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+eid+"/disable", admin, nil), 200)
	if g := e.MustStatus(e.Do("GET", "/v1/admin/sources/"+eid, admin, nil), 200); g.Str("status") != "disabled" {
		t.Fatalf("disable: %s", g.Raw)
	}
}

// Scenario 2: categories are data. An admin adds Energy → Electricity prices (ro/ru labels), new
// articles are classified into it, an editor corrects one, and a later category triggers reclassification.
func TestDynamicCategoriesClassifyCorrectReclassify(t *testing.T) {
	e := testutil.NewEnv(t, testutil.WithoutCategorySeed())
	admin := e.Admin("tax-admin@example.com", "super_admin")
	energy := e.MustStatus(e.Do("POST", "/v1/admin/categories", admin, map[string]any{"slug": "energy",
		"names": map[string]string{"ro": "Energie", "ru": "Энергетика", "en": "Energy"}, "status": "active"}), 201)
	e.MustStatus(e.Do("POST", "/v1/admin/categories", admin, map[string]any{"slug": "electricity-prices", "parent_id": energy.Str("id"),
		"names":                map[string]string{"ro": "Prețuri la energia electrică", "ru": "Цены на электроэнергию", "en": "Electricity prices"},
		"classification_hints": []string{"tarif energie electrică", "preț kilowatt-oră", "тариф на электроэнергию"},
		"aliases":              []string{"energie electrică", "электроэнергия"}, "status": "active"}), 201)
	// Users see it in the public tree, localized.
	user, _, _ := e.RegisterVerified("tax-reader@example.com")
	tree := e.MustStatus(e.Do("GET", "/v1/categories/tree?lang=ru", user, nil), 200)
	if !strings.Contains(string(tree.Raw), "Цены на электроэнергию") {
		t.Fatalf("tree: %s", tree.Raw)
	}
	// Ordinary users cannot change the taxonomy.
	e.MustStatus(e.Do("POST", "/v1/admin/categories", user, map[string]any{"slug": "x", "names": map[string]string{"ro": "X"}}), 403)

	_, ro := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	_, ru := e.SetupSource(admin, testutil.FixtureSource{Name: "Новости Пример", Domain: "novosti-primer.md", Feed: "novosti-primer.xml", Languages: []string{"ru"}})
	e.Crawl(admin, ro)
	e.Crawl(admin, ru)
	roArt := articleID(t, e, "ANRE a aprobat majorarea tarifului la energia electrică cu 15%")
	ruArt := articleID(t, e, "НАРЭ утвердило повышение тарифа на электроэнергию на 15%")
	gasArt := articleID(t, e, "ANRE a aprobat reducerea tarifului la gazele naturale cu 8%")
	for _, id := range []string{roArt, ruArt} {
		if c := articleCats(t, e, admin, id); c["electricity-prices"] != "ai" {
			t.Fatalf("article %s should be classified as electricity-prices: %v", id, c)
		}
	}
	if c := articleCats(t, e, admin, gasArt); c["electricity-prices"] != "" {
		t.Fatalf("gas article must not be electricity-prices: %v", c)
	}

	// Editorial correction: the gas article is filed under Energy explicitly; AI rows are preserved.
	e.MustStatus(e.Do("PUT", "/v1/admin/articles/"+gasArt+"/categories", admin, map[string]any{"category_ids": []string{energy.Str("id")},
		"primary_category_id": energy.Str("id")}), 204)
	if c := articleCats(t, e, admin, gasArt); c["energy"] != "editorial" {
		t.Fatalf("editorial override: %v", c)
	}

	// A new subcategory appears; reclassification (dry run first) applies it to existing articles.
	gas := e.MustStatus(e.Do("POST", "/v1/admin/categories", admin, map[string]any{"slug": "gas-prices", "parent_id": energy.Str("id"),
		"names":                map[string]string{"ro": "Prețuri la gaze", "ru": "Цены на газ", "en": "Gas prices"},
		"classification_hints": []string{"tarif gaze naturale", "тариф на газ"}, "aliases": []string{"gazele naturale", "природный газ"}, "status": "active"}), 201)
	dry := e.MustStatus(e.Do("POST", "/v1/admin/categories/reclassify", admin, map[string]any{"dry_run": true}), 202)
	e.Drain()
	job := e.MustStatus(e.Do("GET", "/v1/admin/jobs/"+dry.Str("job_id"), admin, nil), 200)
	if job.Str("status") != "succeeded" || !strings.Contains(string(job.Raw), gas.Str("id")) && !strings.Contains(string(job.Raw), "gas-prices") {
		t.Fatalf("dry run should report proposed gas-prices assignments: %s", job.Raw)
	}
	if c := articleCats(t, e, admin, gasArt); c["gas-prices"] != "" {
		t.Fatal("dry run must not change assignments")
	}
	e.MustStatus(e.Do("POST", "/v1/admin/categories/reclassify", admin, map[string]any{"dry_run": false}), 202)
	e.Drain()
	c := articleCats(t, e, admin, gasArt)
	if c["gas-prices"] != "ai" || c["energy"] != "editorial" {
		t.Fatalf("after reclassify: %v", c)
	}
	if c := articleCats(t, e, admin, roArt); c["electricity-prices"] != "ai" || c["gas-prices"] != "" {
		t.Fatalf("electricity article after reclassify: %v", c)
	}
}

// switchable wraps the deterministic heuristic as a "remote" provider that can be made to fail.
type switchable struct {
	mu    sync.Mutex
	fail  bool
	calls int
}

func (s *switchable) Generate(ctx context.Context, r ai.ProviderRequest) (*ai.ProviderResponse, error) {
	s.mu.Lock()
	s.calls++
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return nil, &ai.ProviderError{Category: ai.ErrTransient, Status: 503, Msg: "upstream overloaded"}
	}
	return ai.Heuristic{}.Generate(ctx, r)
}

// hashEmbed is an alternative embedding model (different dims) used to exercise migration.
type hashEmbed struct{ dims int }

func (h hashEmbed) Embed(_ context.Context, _ *ai.Model, texts []string) ([][]float32, ai.Usage, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, h.dims)
		for _, w := range strings.Fields(strings.ToLower(t)) {
			f := fnv.New32a()
			_, _ = f.Write([]byte(w))
			v[int(f.Sum32())%h.dims]++
		}
		var n float64
		for _, x := range v {
			n += float64(x * x)
		}
		if n > 0 {
			for j := range v {
				v[j] /= float32(math.Sqrt(n))
			}
		}
		out[i] = v
	}
	return out, ai.Usage{InputTokens: len(texts)}, nil
}

func routeID(t *testing.T, e *testutil.Env, admin, task string) map[string]any {
	t.Helper()
	for _, it := range e.MustStatus(e.Do("GET", "/v1/admin/ai/routes", admin, nil), 200).Items() {
		if m := it.(map[string]any); m["task"] == task && m["mode"] == "production" {
			return m
		}
	}
	t.Fatalf("no route for %s", task)
	return nil
}

func executions(t *testing.T, e *testutil.Env, admin, task string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, it := range e.MustStatus(e.Do("GET", "/v1/admin/ai/executions?task="+task+"&limit=200", admin, nil), 200).Items() {
		out = append(out, it.(map[string]any))
	}
	return out
}

// Scenario 5: model routing is configuration. Switching a route changes the provider without code,
// failures fall back or defer explicitly, and an embedding-model change is a versioned backfill.
func TestModelRoutingFallbackDeferAndEmbeddingMigration(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("ai-admin@example.com", "admin")
	alt := &switchable{}
	e.App.AIGateway().OverrideProvider("alt-llm", alt)
	e.App.AIGateway().OverrideProvider("alt-embed", hashEmbed{dims: 64})

	p := e.MustStatus(e.Do("POST", "/v1/admin/ai/providers", admin, map[string]any{"slug": "alt-llm", "kind": "openai_compatible",
		"display_name": "Alternative LLM", "base_url": "http://127.0.0.1:9/v1", "api_key_env": "ALT_LLM_API_KEY", "enabled": true}), 201)
	if strings.Contains(string(p.Raw), "api_key\"") {
		t.Fatalf("provider view must not expose secrets: %s", p.Raw)
	}
	m := e.MustStatus(e.Do("POST", "/v1/admin/ai/models", admin, map[string]any{"provider_id": p.Str("id"), "slug": "alt-chat", "model_name": "alt-chat-1",
		"kind": "chat", "supports_structured": true, "input_price_per_mtok": 1.0, "output_price_per_mtok": 2.0, "enabled": true}), 201)
	cls := routeID(t, e, admin, "category_classification")
	rules := cls["primary_model_id"].(string)

	// 1) Switch classification to the alternative provider.
	e.MustStatus(e.Do("PATCH", "/v1/admin/ai/routes/"+cls["id"].(string), admin, map[string]any{"primary_model_id": m.Str("id"),
		"fallback_model_ids": []string{rules}, "version": cls["version"]}), 200)
	_, ep := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Diverse Exemplu", Domain: "diverse-exemplu.md", Feed: "diverse-ro.xml", Languages: []string{"ro"}})
	e.Crawl(admin, ep)
	ex := executions(t, e, admin, "category_classification")
	if len(ex) == 0 || ex[0]["provider_slug"] != "alt-llm" || ex[0]["status"] != "succeeded" || alt.calls == 0 {
		t.Fatalf("classification should run on alt-llm: %+v", ex)
	}

	// 2) Provider outage with a fallback: explicit fallback, recorded.
	alt.mu.Lock()
	alt.fail = true
	alt.mu.Unlock()
	e.SwitchFeed(admin, ep, "diverse-ro-day2.xml")
	e.Crawl(admin, ep)
	var failed, fellBack bool
	for _, x := range executions(t, e, admin, "category_classification") {
		if x["provider_slug"] == "alt-llm" && x["status"] == "failed" && x["error_category"] == "transient" {
			failed = true
		}
		if x["model_slug"] == "local-rules" && x["is_fallback"] == true && x["status"] == "succeeded" {
			fellBack = true
		}
	}
	if !failed || !fellBack {
		t.Fatalf("expected recorded failure and fallback: failed=%v fallback=%v", failed, fellBack)
	}

	// 3) No fallback + defer: the article waits (no fabricated categories).
	cls = routeID(t, e, admin, "category_classification")
	e.MustStatus(e.Do("PATCH", "/v1/admin/ai/routes/"+cls["id"].(string), admin, map[string]any{"fallback_model_ids": []string{},
		"on_unavailable": "defer", "version": cls["version"]}), 200)
	e.MustStatus(e.Do("POST", "/v1/admin/articles/reprocess", admin, map[string]any{"article_ids": []string{articleID(t, e, "Fermierii din sudul țării au început recoltarea strugurilor")},
		"reason": "reclassify"}), 202)
	e.Drain()
	var status string

	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT status FROM articles WHERE title=$1`, "Fermierii din sudul țării au început recoltarea strugurilor").Scan(&status)
	if status == "enriched" {
		t.Fatalf("article should be waiting on AI, got %s", status)
	}
	usage := e.MustStatus(e.Do("GET", "/v1/admin/ai/usage", admin, nil), 200)
	if !strings.Contains(string(usage.Raw), `"model":"alt-chat"`) {
		t.Fatalf("usage should account for alt-chat: %s", usage.Raw)
	}

	// 4) Embedding migration: new model → building collections + backfill; search keeps working; cutover.
	user, _, _ := e.RegisterVerified("searcher@example.com")
	before := e.MustStatus(e.Do("GET", "/v1/search?q=struguri", user, nil), 200)
	if len(before.Items()) == 0 {
		t.Fatalf("search before migration: %s", before.Raw)
	}
	em := e.MustStatus(e.Do("POST", "/v1/admin/ai/models", admin, map[string]any{"provider_id": e.MustStatus(e.Do("POST", "/v1/admin/ai/providers", admin,
		map[string]any{"slug": "alt-embed", "kind": "openai_compatible", "display_name": "Alt embeddings", "base_url": "http://127.0.0.1:9/v1", "enabled": true}), 201).Str("id"),
		"slug": "alt-embed-64", "model_name": "alt-embed-64", "kind": "embedding", "embedding_dims": 64, "enabled": true}), 201)
	emb := routeID(t, e, admin, "embeddings")
	e.MustStatus(e.Do("PATCH", "/v1/admin/ai/routes/"+emb["id"].(string), admin, map[string]any{"primary_model_id": em.Str("id"), "version": emb["version"]}), 200)
	plan := e.MustStatus(e.Do("POST", "/v1/admin/search/embedding-migration", admin, nil), 202)
	cols := plan.Get("collections").([]any)
	if len(cols) == 0 || !strings.Contains(plan.Str("target_model"), "alt-embed-64") {
		t.Fatalf("plan: %s", plan.Raw)
	}
	// Active collections still serve queries while the new ones build.
	if r := e.MustStatus(e.Do("GET", "/v1/search?q=struguri", user, nil), 200); len(r.Items()) == 0 {
		t.Fatalf("search during migration: %s", r.Raw)
	}
	e.Drain()
	status2 := e.MustStatus(e.Do("GET", "/v1/admin/search/collections", admin, nil), 200)
	if !strings.Contains(string(status2.Raw), `"building"`) {
		t.Fatalf("collections should be building until cutover: %s", status2.Raw)
	}
	for _, c := range cols {
		dt := c.(map[string]any)["doc_type"].(string)
		e.MustStatus(e.Do("POST", "/v1/admin/search/collections/"+dt+"/cutover", admin, nil), 204)
	}
	after := e.MustStatus(e.Do("GET", "/v1/search?q=struguri", user, nil), 200)
	if len(after.Items()) == 0 {
		t.Fatalf("search after cutover: %s", after.Raw)
	}
	// Rollback restores the previous collection.
	e.MustStatus(e.Do("POST", "/v1/admin/search/collections/"+cols[0].(map[string]any)["doc_type"].(string)+"/rollback", admin, nil), 204)
}

// Scenario 13: SSRF via source URLs and redirects is blocked; ordinary users cannot reach admin controls.
func TestSSRFAndAdminRBAC(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<rss><channel><item><title>secret</title></item></channel></rss>"))
	}))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirector.Close()
	e := testutil.NewEnv(t, testutil.WithConfig(func(c *config.Config) { c.CrawlAllowPrivate = false }))
	admin := e.Admin("sec-admin@example.com", "admin")
	pub := e.MustStatus(e.Do("POST", "/v1/admin/publishers", admin, map[string]any{"name": "Evil", "canonical_domain": "evil.example",
		"publisher_type": "newsroom", "primary_languages": []string{"ro"}}), 201)

	for _, u := range []string{"http://127.0.0.1:8080/admin", "http://169.254.169.254/latest/meta-data/", "http://[::1]/", "file:///etc/passwd",
		"gopher://localhost:25/", "http://localhost/feed", "http://10.0.0.5/rss", "http://user:pass@example.com/"} {
		r := e.Do("POST", "/v1/admin/sources", admin, map[string]any{"publisher_id": pub.Str("id"), "name": "x", "endpoint_type": "rss", "entry_url": u})
		if r.Status != 422 {
			t.Fatalf("%s should be rejected, got %d %s", u, r.Status, r.Raw)
		}
		pr := e.Do("POST", "/v1/admin/sources/preview-url", admin, map[string]any{"url": u})
		if pr.Status != 422 {
			t.Fatalf("preview %s should be rejected, got %d %s", u, pr.Status, pr.Raw)
		}
	}
	// A public-looking test request that resolves to loopback is refused at dial time.
	tr := e.Do("POST", "/v1/admin/sources/test", admin, map[string]any{"endpoint_type": "rss", "entry_url": internal.URL + "/feed"})
	if tr.Status == 200 && tr.Get("ok") == true {
		t.Fatalf("loopback fetch must not succeed: %s", tr.Raw)
	}
	rr := e.Do("POST", "/v1/admin/sources/test", admin, map[string]any{"endpoint_type": "rss", "entry_url": redirector.URL + "/feed"})
	if rr.Status == 200 && rr.Get("ok") == true {
		t.Fatalf("redirect to metadata service must not succeed: %s", rr.Raw)
	}

	user, _, _ := e.RegisterVerified("plain@example.com")
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/admin/sources"}, {"POST", "/v1/admin/sources"}, {"POST", "/v1/admin/categories"}, {"GET", "/v1/admin/publishers"},
		{"GET", "/v1/admin/ai/routes"}, {"PUT", "/v1/admin/policies/feed_ranking"}, {"GET", "/v1/admin/audit-logs"}, {"GET", "/v1/admin/jobs"},
	} {
		if r := e.Do(c.method, c.path, user, map[string]any{}); r.Status != 403 {
			t.Fatalf("%s %s as user: %d", c.method, c.path, r.Status)
		}
		if r := e.Do(c.method, c.path, "", map[string]any{}); r.Status != 401 {
			t.Fatalf("%s %s anonymous: %d", c.method, c.path, r.Status)
		}
	}
	// A fact-checker can review but not configure sources.
	fc := e.Admin("checker@example.com", "fact_checker")
	if r := e.Do("POST", "/v1/admin/sources", fc, map[string]any{}); r.Status != 403 {
		t.Fatalf("fact_checker creating sources: %d", r.Status)
	}
	var problem map[string]any
	r := e.Do("GET", "/v1/admin/sources", user, nil)
	_ = json.Unmarshal(r.Raw, &problem)
	if problem["code"] == nil || r.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("errors must be problem+json: %v %s", r.Header, r.Raw)
	}
}

// repairing returns an invalid category first and corrects itself when shown the validation error.
type repairing struct {
	mu   sync.Mutex
	seen []string
}

func (p *repairing) Generate(ctx context.Context, r ai.ProviderRequest) (*ai.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	last := r.Messages[len(r.Messages)-1].Content
	p.seen = append(p.seen, last)
	if !strings.Contains(last, "rejected by validation") {
		return &ai.ProviderResponse{Text: `{"assignments":[{"category_slug":"invented-category","confidence":0.9,"is_primary":true,"rationale":"x"}]}`}, nil
	}
	return ai.Heuristic{}.Generate(ctx, r)
}

// Invalid structured output is never accepted: the model is shown the validation error and retried.
func TestInvalidOutputRepairRetry(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("repair-admin@example.com", "admin")
	fake := &repairing{}
	e.App.AIGateway().OverrideProvider("repairing", fake)
	p := e.MustStatus(e.Do("POST", "/v1/admin/ai/providers", admin, map[string]any{"slug": "repairing", "kind": "openai_compatible",
		"display_name": "Repairing", "base_url": "http://127.0.0.1:9/v1", "enabled": true}), 201)
	m := e.MustStatus(e.Do("POST", "/v1/admin/ai/models", admin, map[string]any{"provider_id": p.Str("id"), "slug": "repairing-chat",
		"model_name": "r1", "kind": "chat", "supports_structured": true, "enabled": true}), 201)
	cls := routeID(t, e, admin, "category_classification")
	e.MustStatus(e.Do("PATCH", "/v1/admin/ai/routes/"+cls["id"].(string), admin, map[string]any{"primary_model_id": m.Str("id"),
		"fallback_model_ids": []string{}, "max_attempts": 2, "version": cls["version"]}), 200)
	_, ep := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Diverse Exemplu", Domain: "diverse-exemplu.md", Feed: "diverse-ro.xml", Languages: []string{"ro"}})
	e.Crawl(admin, ep)
	var invalid, repaired bool
	for _, x := range executions(t, e, admin, "category_classification") {
		if x["status"] == "invalid_output" && x["attempt"].(float64) == 1 {
			invalid = true
		}
		if x["status"] == "succeeded" && x["attempt"].(float64) == 2 && x["model_slug"] == "repairing-chat" {
			repaired = true
		}
	}
	if !invalid || !repaired {
		t.Fatalf("expected invalid first attempt and repaired second: invalid=%v repaired=%v", invalid, repaired)
	}
	var n int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM article_categories ac JOIN categories c ON c.id=ac.category_id WHERE c.slug='invented-category'`).Scan(&n)
	if n != 0 {
		t.Fatal("an invalid category must never be stored")
	}
}
