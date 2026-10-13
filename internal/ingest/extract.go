package ingest

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"

	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

// ExtractorVersion is recorded on every article for reprocessing decisions.
const ExtractorVersion = "html-extract/1.2.0"

// Extracted is the normalized result of HTML extraction.
type Extracted struct {
	Title        string     `json:"title"`
	Byline       string     `json:"byline"`
	Description  string     `json:"description"`
	Text         string     `json:"text"`
	CanonicalURL string     `json:"canonical_url"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	ModifiedAt   *time.Time `json:"modified_at,omitempty"`
	Language     string     `json:"language"`
	LanguageConf float64    `json:"language_confidence"`
	Confidence   float64    `json:"confidence"`
	SelectorsUsed map[string]string `json:"selectors_used"`
	Warnings     []string   `json:"warnings,omitempty"`
}

// ValidateSelector compiles a CSS selector (no code execution; selectors only).
func ValidateSelector(sel string) error {
	if _, err := cascadia.ParseGroup(sel); err != nil {
		return fmt.Errorf("invalid CSS selector %q", sel)
	}
	return nil
}

var defaultContentSelectors = []string{"article [itemprop=articleBody]", "[itemprop=articleBody]", "article .entry-content", ".article-content",
	".article__content", ".post-content", ".news-content", ".content-text", "article", "main"}
var defaultTitleSelectors = []string{"meta[property='og:title']", "h1", "title"}
var defaultDateSelectors = []string{"meta[property='article:published_time']", "time[datetime]", "meta[name='pubdate']", "meta[itemprop=datePublished]"}
var defaultAuthorSelectors = []string{"meta[name=author]", "[rel=author]", ".author", ".byline"}
var defaultCleanup = []string{"script", "style", "noscript", "iframe", "form", "nav", "aside", "footer", "header .menu", ".share", ".social",
	".related", ".comments", ".advert", ".ads", "[aria-hidden=true]", "figure figcaption"}

// ExtractHTML extracts article fields from untrusted HTML using an optional profile.
// Scripts, styles and embedded markup are dropped; only text is retained.
func ExtractHTML(body []byte, pageURL string, prof *sources.ExtractionProfile) (*Extracted, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	ex := &Extracted{SelectorsUsed: map[string]string{}}
	cleanup := defaultCleanup
	titleSels, contentSels, dateSels, authorSels, canonSels := defaultTitleSelectors, defaultContentSelectors, defaultDateSelectors, defaultAuthorSelectors, []string{"link[rel=canonical]", "meta[property='og:url']"}
	if prof != nil {
		cleanup = append(append([]string{}, defaultCleanup...), prof.CleanupSelectors...)
		titleSels = prefer(prof.TitleSelectors, titleSels)
		contentSels = prefer(prof.ContentSelectors, contentSels)
		dateSels = prefer(prof.DateSelectors, dateSels)
		authorSels = prefer(prof.AuthorSelectors, authorSels)
		canonSels = prefer(prof.CanonicalSelectors, canonSels)
	}
	// Canonical before cleanup (head elements).
	for _, sel := range canonSels {
		if v := attrOrText(doc.Find(sel).First()); v != "" {
			ex.CanonicalURL = resolve(pageURL, v)
			ex.SelectorsUsed["canonical"] = sel
			break
		}
	}
	if d := doc.Find("meta[name=description], meta[property='og:description']").First(); d.Length() > 0 {
		ex.Description = clean(d.AttrOr("content", ""))
	}
	for _, sel := range dateSels {
		s := doc.Find(sel).First()
		if s.Length() == 0 {
			continue
		}
		v := firstNonEmpty(s.AttrOr("content", ""), s.AttrOr("datetime", ""), s.Text())
		if t := parseTime(v); t != nil {
			ex.PublishedAt = t
			ex.SelectorsUsed["date"] = sel
			break
		}
	}
	if m := doc.Find("meta[property='article:modified_time']").First(); m.Length() > 0 {
		ex.ModifiedAt = parseTime(m.AttrOr("content", ""))
	}
	for _, sel := range titleSels {
		if v := clean(attrOrText(doc.Find(sel).First())); v != "" {
			ex.Title = v
			ex.SelectorsUsed["title"] = sel
			break
		}
	}
	for _, sel := range authorSels {
		if v := clean(attrOrText(doc.Find(sel).First())); v != "" && len(v) < 200 {
			ex.Byline = v
			ex.SelectorsUsed["author"] = sel
			break
		}
	}
	for _, sel := range cleanup {
		doc.Find(sel).Remove()
	}
	best := ""
	for _, sel := range contentSels {
		s := doc.Find(sel)
		if s.Length() == 0 {
			continue
		}
		txt := blockText(s.First())
		if len([]rune(txt)) > 200 {
			best = txt
			ex.SelectorsUsed["content"] = sel
			break
		}
		if len(txt) > len(best) {
			best = txt
			ex.SelectorsUsed["content"] = sel
		}
	}
	if len([]rune(best)) < 200 {
		// Fallback: densest cluster of <p> siblings.
		if alt := densestParagraphs(doc); len([]rune(alt)) > len([]rune(best)) {
			best = alt
			ex.SelectorsUsed["content"] = "heuristic:paragraph-density"
		}
	}
	ex.Text = nlp.NormalizeParagraphs(best)
	ex.Language, ex.LanguageConf = nlp.DetectLanguage(ex.Title + ". " + ex.Text)
	if lang := normLang(doc.Find("html").AttrOr("lang", "")); lang != "" && ex.LanguageConf < 0.7 {
		ex.Language, ex.LanguageConf = lang, 0.7
	}
	ex.Confidence = confidence(ex)
	if ex.Title == "" {
		ex.Warnings = append(ex.Warnings, "title_not_found")
	}
	if len([]rune(ex.Text)) < 300 {
		ex.Warnings = append(ex.Warnings, "short_body")
	}
	if ex.PublishedAt == nil {
		ex.Warnings = append(ex.Warnings, "published_at_not_found")
	}
	return ex, nil
}

func prefer(custom, def []string) []string {
	if len(custom) > 0 {
		return append(append([]string{}, custom...), def...)
	}
	return def
}

func attrOrText(s *goquery.Selection) string {
	if s.Length() == 0 {
		return ""
	}
	for _, a := range []string{"content", "href", "datetime"} {
		if v, ok := s.Attr(a); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return s.Text()
}

// blockText renders block-level children as paragraphs.
func blockText(s *goquery.Selection) string {
	var parts []string
	s.Find("p, h2, h3, h4, li, blockquote").Each(func(_ int, b *goquery.Selection) {
		if t := clean(b.Text()); t != "" {
			parts = append(parts, t)
		}
	})
	if len(parts) == 0 {
		return clean(s.Text())
	}
	return strings.Join(dedupeAdjacent(parts), "\n\n")
}

func dedupeAdjacent(in []string) []string {
	var out []string
	for i, p := range in {
		if i > 0 && strings.Contains(in[i-1], p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func densestParagraphs(doc *goquery.Document) string {
	type cand struct {
		node  *goquery.Selection
		score int
	}
	scores := map[*goquery.Selection]int{}
	var list []cand
	doc.Find("p").Each(func(_ int, p *goquery.Selection) {
		parent := p.Parent()
		n := len([]rune(clean(p.Text())))
		if n < 40 {
			return
		}
		found := false
		for i := range list {
			if list[i].node.IsSelection(parent) {
				list[i].score += n
				found = true
				break
			}
		}
		if !found {
			list = append(list, cand{parent, n})
		}
		_ = scores
	})
	if len(list) == 0 {
		return ""
	}
	sort.Slice(list, func(i, j int) bool { return list[i].score > list[j].score })
	return blockText(list[0].node)
}

func confidence(ex *Extracted) float64 {
	c := 0.0
	if ex.Title != "" {
		c += 0.3
	}
	n := len([]rune(ex.Text))
	switch {
	case n > 1500:
		c += 0.4
	case n > 500:
		c += 0.3
	case n > 200:
		c += 0.15
	}
	if ex.PublishedAt != nil {
		c += 0.15
	}
	if ex.LanguageConf > 0.6 {
		c += 0.15
	}
	return c
}

// ---------- Canonicalization ----------

var trackingParams = regexp.MustCompile(`^(utm_.*|fbclid|gclid|yclid|mc_cid|mc_eid|_ga|igshid|ref|ref_src|cmpid|from|share|amp)$`)

// CanonicalizeURL normalizes a URL for identity: lower-case scheme/host, drop default ports,
// fragments, tracking params, "www.", trailing slash; sort remaining params.
func CanonicalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid url %q", raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme == "http" {
		u.Scheme = "https" // treat http/https variants as the same resource
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.") // mobile mirrors
	port := u.Port()
	if port == "80" || port == "443" {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	u.Fragment, u.RawFragment = "", ""
	u.User = nil
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		if trackingParams.MatchString(strings.ToLower(k)) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	u.RawQuery = strings.Join(parts, "&")
	p := u.EscapedPath()
	p = strings.TrimSuffix(p, "/amp")
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		p = "/"
	}
	u.RawPath = ""
	u.Path, _ = url.PathUnescape(p)
	return u.String(), nil
}

// SameSite reports whether two URLs share a registrable-ish host (exact or subdomain).
func SameSite(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	ha := strings.TrimPrefix(strings.ToLower(ua.Hostname()), "www.")
	hb := strings.TrimPrefix(strings.ToLower(ub.Hostname()), "www.")
	return ha == hb || strings.HasSuffix(ha, "."+hb) || strings.HasSuffix(hb, "."+ha)
}
