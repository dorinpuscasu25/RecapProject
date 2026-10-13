package ingest_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

const listHTML = `<!doctype html><html><head><title>Știri locale</title></head><body>
<nav><a href="/despre">Despre noi și echipa redacției noastre</a></nav>
<main>
 <article><h2><a href="/stiri/primaria-troleibuze">Primăria Chișinău introduce zece troleibuze noi pe rutele din Botanica</a></h2></article>
 <article><h2><a href="/stiri/biblioteca-renovata">Biblioteca municipală a fost redeschisă după renovare</a></h2>
   <a href="/stiri/biblioteca-renovata">Citește mai mult</a></article>
 <article><h2><a href="https://alt-site.example/stiri/x">Link extern care nu trebuie urmat de crawler</a></h2></article>
 <a href="/tag/transport">Toate știrile despre transport public urban</a>
</main>
<footer><a href="/stiri/arhiva">Arhiva completă a știrilor publicate</a></footer>
</body></html>`

func articleHTML(title, body string) string {
	return fmt.Sprintf(`<!doctype html><html lang="ro"><head><title>%s</title><meta property="og:title" content="%s">
<meta property="article:published_time" content="2026-10-13T09:00:00+03:00"></head><body><article><h1>%s</h1><p>%s</p></article></body></html>`, title, title, title, body)
}

// html_list endpoints: links are discovered on a section page over live HTTP (robots-checked,
// same-site only, navigation/footer/tag links ignored), then each article page is fetched.
func TestHTMLListEndpointOverHTTP(t *testing.T) {
	var fetched []string
	robotsSeen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = append(fetched, r.URL.Path)
		switch r.URL.Path {
		case "/robots.txt":
			robotsSeen = true
			fmt.Fprint(w, "User-agent: *\nDisallow: /privat/\n")
		case "/stiri":
			fmt.Fprint(w, listHTML)
		case "/stiri/primaria-troleibuze":
			fmt.Fprint(w, articleHTML("Primăria Chișinău introduce zece troleibuze noi pe rutele din Botanica",
				"Primăria municipiului Chișinău a anunțat că zece troleibuze noi vor circula pe rutele din sectorul Botanica începând de luni. Investiția a fost aprobată de consiliul municipal."))
		case "/stiri/biblioteca-renovata":
			fmt.Fprint(w, articleHTML("Biblioteca municipală a fost redeschisă după renovare",
				"Biblioteca municipală din Chișinău a fost redeschisă după lucrări de renovare care au durat opt luni. Sala de lectură are acum 120 de locuri."))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := testutil.NewEnv(t, testutil.WithConfig(func(c *config.Config) { c.CrawlLiveEnabled = true; c.CrawlAllowPrivate = true }))
	admin := e.Admin("list-admin@example.com", "admin")

	pub := e.MustStatus(e.Do("POST", "/v1/admin/publishers", admin, map[string]any{"name": "Știri Locale Exemplu", "canonical_domain": "stiri-locale.md",
		"publisher_type": "newsroom", "primary_languages": []string{"ro"}}), 201)
	e.MustStatus(e.Do("PUT", "/v1/admin/publishers/"+pub.Str("id")+"/permissions", admin, map[string]any{"legal_basis": "synthetic test server",
		"permission_status": "granted", "allow_crawl": true, "allow_store_full_text": true, "allow_summarize": true, "allow_embed": true, "allow_snippets": true,
		"robots_review_status": "compliant", "terms_review_status": "compliant", "attribution_text": "Sursa: Știri Locale"}), 200)
	e.MustStatus(e.Do("PATCH", "/v1/admin/publishers/"+pub.Str("id"), admin, map[string]any{"status": "active"}), 200)

	// Unsupported modes are rejected up front instead of failing later.
	for _, bad := range []map[string]any{{"endpoint_type": "official_api"}, {"endpoint_type": "rss", "fetch_mode": "headless"},
		{"endpoint_type": "html_list", "fetch_article_pages": false}} {
		body := map[string]any{"publisher_id": pub.Str("id"), "name": "x", "entry_url": srv.URL + "/stiri", "languages": []string{"ro"}}
		for k, v := range bad {
			body[k] = v
		}
		if r := e.Do("POST", "/v1/admin/sources", admin, body); r.Status != 422 {
			t.Fatalf("%v should be rejected: %d %s", bad, r.Status, r.Raw)
		}
	}
	test := e.MustStatus(e.Do("POST", "/v1/admin/sources/test", admin, map[string]any{"endpoint_type": "html_list", "entry_url": srv.URL + "/stiri"}), 200)
	if test.Get("ok") != true || test.Get("item_count").(float64) != 2 || test.Str("detected_format") != "html_list" {
		t.Fatalf("list page test: %s", test.Raw)
	}
	ep := e.MustStatus(e.Do("POST", "/v1/admin/sources", admin, map[string]any{"publisher_id": pub.Str("id"), "name": "Știri locale (listă)",
		"endpoint_type": "html_list", "entry_url": srv.URL + "/stiri", "fetch_article_pages": true, "languages": []string{"ro"}, "poll_interval_seconds": 900}), 201)
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+ep.Str("id")+"/enable", admin, nil), 200)

	// The test server's host is not the publisher's domain: items are rejected and health says so.
	e.Crawl(admin, ep.Str("id"))
	if h := e.MustStatus(e.Do("GET", "/v1/admin/sources/"+ep.Str("id")+"/health", admin, nil), 200); h.Str("health_state") != "degraded" {
		t.Fatalf("all-rejected run must degrade health: %s", h.Raw)
	}
	e.MustStatus(e.Do("PATCH", "/v1/admin/sources/"+ep.Str("id"), admin, map[string]any{"allowed_hosts": []string{"127.0.0.1"}}), 200)
	fetched = nil
	e.Crawl(admin, ep.Str("id"))

	runs := e.MustStatus(e.Do("GET", "/v1/admin/sources/"+ep.Str("id")+"/runs", admin, nil), 200)
	if r0 := runs.Items()[0].(map[string]any); r0["status"] != "succeeded" || r0["accepted_count"].(float64) != 2 {
		t.Fatalf("run: %s", runs.Raw)
	}
	var n int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM articles WHERE source_endpoint_id=$1 AND status='enriched' AND length(body_text) > 50`, ep.Str("id")).Scan(&n)
	if n != 2 {
		t.Fatalf("expected 2 enriched articles with bodies, got %d", n)
	}
	for _, p := range fetched {
		if p == "/despre" || p == "/stiri/arhiva" || strings.HasPrefix(p, "/tag/") {
			t.Fatalf("navigation/footer/tag link was fetched: %v", fetched)
		}
	}
	if !robotsSeen {
		t.Fatal("robots.txt must be consulted")
	}
}
