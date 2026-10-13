package ingest

import (
	"strings"
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

func TestCanonicalizeURL(t *testing.T) {
	cases := map[string]string{
		"http://WWW.Example.md/stiri/articol-1/?utm_source=fb&b=2&a=1#comments": "https://example.md/stiri/articol-1?a=1&b=2",
		"https://m.example.md:443/x/":                                          "https://example.md/x",
		"https://example.md/a/amp":                                             "https://example.md/a",
		"https://example.md/?fbclid=abc":                                       "https://example.md/",
	}
	for in, want := range cases {
		got, err := CanonicalizeURL(in)
		if err != nil || got != want {
			t.Errorf("CanonicalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestParseRSSAndAtomAndJSON(t *testing.T) {
	rss := `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel><title>Știri</title><language>ro-MD</language>
<item><title>Guvernul a aprobat &amp; publicat</title><link>/stiri/1</link><pubDate>Mon, 12 Oct 2026 09:00:00 +0300</pubDate>
<dc:creator>Ion Popescu</dc:creator><content:encoded><![CDATA[<p>Text complet.</p>]]></content:encoded></item>
</channel></rss>`
	f, err := ParseFeed([]byte(rss), "https://example.md/rss")
	if err != nil || f.Format != "rss" || len(f.Items) != 1 {
		t.Fatalf("rss: %v %+v", err, f)
	}
	it := f.Items[0]
	if it.URL != "https://example.md/stiri/1" || it.Title != "Guvernul a aprobat & publicat" || it.Author != "Ion Popescu" || it.PublishedAt == nil || it.Language != "ro" {
		t.Errorf("rss item: %+v", it)
	}
	atom := `<feed xmlns="http://www.w3.org/2005/Atom" xml:lang="ru"><title>Новости</title>
<entry><title>Парламент утвердил закон</title><id>urn:1</id><updated>2026-10-12T08:00:00Z</updated>
<link rel="alternate" href="https://example.md/ru/1"/><author><name>Анна</name></author></entry></feed>`
	f, err = ParseFeed([]byte(atom), "https://example.md/atom")
	if err != nil || f.Format != "atom" || len(f.Items) != 1 || f.Items[0].URL != "https://example.md/ru/1" || f.Items[0].Language != "ru" {
		t.Fatalf("atom: %v %+v", err, f)
	}
	js := `{"version":"https://jsonfeed.org/version/1.1","title":"X","items":[{"id":"1","url":"https://example.md/j/1","title":"T","content_text":"Body","date_published":"2026-10-12T08:00:00Z"}]}`
	f, err = ParseFeed([]byte(js), "https://example.md/feed.json")
	if err != nil || f.Format != "json_feed" || f.Items[0].ContentHTML == "" {
		t.Fatalf("json feed: %v %+v", err, f)
	}
	if _, err := ParseFeed([]byte("<html><body>no</body></html>"), ""); err == nil {
		t.Error("html must not parse as a feed")
	}
}

func TestParseFeedRejectsEntityExpansion(t *testing.T) {
	bomb := `<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;">]><rss><channel><item><title>&lol2;</title><link>https://example.md/1</link></item></channel></rss>`
	f, err := ParseFeed([]byte(bomb), "")
	if err == nil && len(f.Items) == 1 && strings.Contains(f.Items[0].Title, "lollol") {
		t.Fatal("custom DTD entities must not be expanded")
	}
}

func TestRobots(t *testing.T) {
	r := ParseRobots([]byte("User-agent: *\nDisallow: /private\nAllow: /private/ok\nCrawl-delay: 2\n\nUser-agent: NewsIntelBot\nDisallow: /nobots$\n"))
	if !r.Allowed("Mozilla", "/public") || r.Allowed("Mozilla", "/private/x") || !r.Allowed("Mozilla", "/private/ok/1") {
		t.Error("wildcard group rules wrong")
	}
	if r.Allowed("NewsIntelBot/0.1", "/nobots") || !r.Allowed("NewsIntelBot/0.1", "/private/x") {
		t.Error("specific agent group should take precedence")
	}
	if r.CrawlDelay("Mozilla").Seconds() != 2 {
		t.Error("crawl delay")
	}
}

func TestExtractHTMLWithProfileAndSanitization(t *testing.T) {
	html := `<html lang="ro"><head><title>Site | Titlu</title><link rel="canonical" href="https://example.md/stiri/a-1">
<meta property="article:published_time" content="2026-10-12T09:00:00+03:00"></head><body>
<h1 class="headline">Parlamentul a aprobat noul tarif la energie</h1><span class="author-name">Maria Rusu</span>
<div class="story"><p>Parlamentul Republicii Moldova a aprobat luni noul tarif la energia electrică pentru consumatorii casnici, care va intra în vigoare de la 1 noiembrie.</p>
<script>alert('x')</script><div class="ad">Reclamă</div>
<p>Potrivit Agenției Naționale pentru Reglementare în Energetică, tariful crește cu 15% față de nivelul actual, până la 3,5 lei pentru un kilowatt-oră.</p>
<p>Opoziția a criticat decizia și a cerut compensații pentru familiile vulnerabile din toată țara, în special pentru pensionari.</p></div></body></html>`
	prof := &sources.ExtractionProfile{Name: "p", TitleSelectors: []string{"h1.headline"}, ContentSelectors: []string{".story"},
		AuthorSelectors: []string{".author-name"}, CleanupSelectors: []string{".ad"}}
	ex, err := ExtractHTML([]byte(html), "https://example.md/stiri/a-1?utm_source=x", prof)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Title != "Parlamentul a aprobat noul tarif la energie" || ex.Byline != "Maria Rusu" || ex.CanonicalURL != "https://example.md/stiri/a-1" {
		t.Errorf("fields: %+v", ex)
	}
	if strings.Contains(ex.Text, "alert") || strings.Contains(ex.Text, "Reclamă") || !strings.Contains(ex.Text, "15%") {
		t.Errorf("text not sanitized/extracted: %q", ex.Text)
	}
	if ex.Language != "ro" || ex.PublishedAt == nil {
		t.Errorf("lang/date: %s %v", ex.Language, ex.PublishedAt)
	}
	for _, sel := range []string{"h1.headline", "div > p:first-child", "[itemprop=articleBody]"} {
		if err := ValidateSelector(sel); err != nil {
			t.Errorf("valid selector rejected: %s", sel)
		}
	}
	if err := ValidateSelector("div[[["); err == nil {
		t.Error("invalid selector accepted")
	}
}

func TestProfileFixtures(t *testing.T) {
	p := &sources.ExtractionProfile{Name: "p", TitleSelectors: []string{"h1"}, ContentSelectors: []string{".body"}}
	p.Fixtures = append(p.Fixtures, sources.ProfileFixture{Name: "basic", HTML: `<h1>Titlu</h1><div class="body"><p>` + strings.Repeat("Conținut relevant. ", 20) + `</p></div>`})
	p.Fixtures[0].Expected.Title = "Titlu"
	p.Fixtures[0].Expected.Contains = []string{"Conținut relevant"}
	res := RunProfileFixtures(p)
	if len(res) != 1 || !res[0].Passed {
		t.Fatalf("fixture failed: %+v", res)
	}
}
