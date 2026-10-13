package ingest

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/dorinpuscasu25/recapproject/internal/nlp"
)

// DefaultLinkSelectors locate article links on typical news section pages when the endpoint's
// extraction profile does not define link_selectors.
var DefaultLinkSelectors = []string{"article a[href]", "h1 a[href]", "h2 a[href]", "h3 a[href]", ".news-item a[href]", ".article a[href]"}

var nonArticlePath = []string{"/tag/", "/tags/", "/category/", "/categorie/", "/author/", "/autor/", "/page/", "/search", "/cautare", "/login", "/contact"}

// ParseListPage extracts article links from an HTML section/list page (endpoint type html_list).
// Only same-site links (or links to allowedHosts) are kept; navigation, header, footer and aside
// links are ignored; titles shorter than 15 characters ("Read more") fall back to the nearest heading.
func ParseListPage(body []byte, pageURL string, selectors, allowedHosts []string) (*Feed, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("list page: %w", err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	if len(selectors) == 0 {
		selectors = DefaultLinkSelectors
	}
	allowed := map[string]bool{}
	for _, h := range allowedHosts {
		allowed[strings.ToLower(h)] = true
	}
	f := &Feed{Format: "html_list", Title: nlp.NormalizeText(doc.Find("title").First().Text())}
	seen := map[string]bool{}
	for _, sel := range selectors {
		doc.Find(sel).Each(func(_ int, a *goquery.Selection) {
			if a.Closest("nav, header, footer, aside").Length() > 0 {
				return
			}
			href, _ := a.Attr("href")
			href = strings.TrimSpace(href)
			if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") || strings.HasPrefix(strings.ToLower(href), "mailto:") {
				return
			}
			u, err := base.Parse(href)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return
			}
			u.Fragment = ""
			host := strings.ToLower(u.Hostname())
			if !SameSite(u.String(), pageURL) && !allowed[host] {
				return
			}
			lp := strings.ToLower(u.Path)
			if lp == "" || lp == "/" || u.String() == pageURL {
				return
			}
			for _, bad := range nonArticlePath {
				if strings.Contains(lp, bad) {
					return
				}
			}
			canon, err := CanonicalizeURL(u.String())
			if err != nil || seen[canon] {
				return
			}
			title := nlp.NormalizeText(a.Text())
			if len([]rune(title)) < 15 {
				if t, ok := a.Attr("title"); ok && len([]rune(nlp.NormalizeText(t))) >= 15 {
					title = nlp.NormalizeText(t)
				} else if h := a.Closest("article, li, div").Find("h1, h2, h3, h4").First(); h.Length() > 0 {
					title = nlp.NormalizeText(h.Text())
				}
			}
			if len([]rune(title)) < 15 {
				return
			}
			seen[canon] = true
			f.Items = append(f.Items, FeedItem{URL: u.String(), Title: title})
		})
	}
	if len(f.Items) == 0 {
		return nil, fmt.Errorf("no article links found (check link_selectors)")
	}
	return f, nil
}
