// Package ingest implements feed discovery, permission-gated fetching, HTML extraction,
// canonicalization, fingerprinting, deduplication and article persistence.
package ingest

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
)

// FeedItem is a discovered entry, independent of feed format.
type FeedItem struct {
	URL         string     `json:"url"`
	Title       string     `json:"title"`
	Summary     string     `json:"summary,omitempty"`
	ContentHTML string     `json:"content_html,omitempty"`
	Author      string     `json:"author,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	GUID        string     `json:"guid,omitempty"`
	Categories  []string   `json:"categories,omitempty"`
	Language    string     `json:"language,omitempty"`
}

type Feed struct {
	Format   string     `json:"format"`
	Title    string     `json:"title"`
	Language string     `json:"language,omitempty"`
	Items    []FeedItem `json:"items"`
}

var ErrUnknownFeed = errors.New("unrecognized feed format")

// ParseFeed detects RSS 2.0, Atom, JSON Feed 1.x and XML sitemaps. Untrusted input: entity
// expansion is disabled by encoding/xml (no DTD processing), and size is bounded by the caller.
func ParseFeed(body []byte, base string) (*Feed, error) {
	trim := bytes.TrimSpace(body)
	if len(trim) == 0 {
		return nil, ErrUnknownFeed
	}
	if trim[0] == '{' {
		return parseJSONFeed(trim, base)
	}
	dec := xml.NewDecoder(bytes.NewReader(trim))
	dec.CharsetReader = charset.NewReaderLabel
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return nil, ErrUnknownFeed
			}
			return nil, fmt.Errorf("xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(se.Name.Local) {
		case "rss", "rdf":
			return parseRSS(trim, base)
		case "feed":
			return parseAtom(trim, base)
		case "urlset":
			return parseSitemap(trim, base)
		default:
			return nil, ErrUnknownFeed
		}
	}
}

func newDecoder(b []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.CharsetReader = charset.NewReaderLabel
	d.Strict = false
	d.Entity = xml.HTMLEntity
	return d
}

type rssDoc struct {
	Channel struct {
		Title    string    `xml:"title"`
		Language string    `xml:"language"`
		Items    []rssItem `xml:"item"`
	} `xml:"channel"`
	Items []rssItem `xml:"item"` // RSS 1.0 (RDF)
}

type rssItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	GUID        string   `xml:"guid"`
	Description string   `xml:"description"`
	Content     string   `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	PubDate     string   `xml:"pubDate"`
	DCDate      string   `xml:"http://purl.org/dc/elements/1.1/ date"`
	Creator     string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Author      string   `xml:"author"`
	Categories  []string `xml:"category"`
	Updated     string   `xml:"updated"`
}

func parseRSS(b []byte, base string) (*Feed, error) {
	var d rssDoc
	if err := newDecoder(b).Decode(&d); err != nil {
		return nil, fmt.Errorf("rss: %w", err)
	}
	items := d.Channel.Items
	if len(items) == 0 {
		items = d.Items
	}
	f := &Feed{Format: "rss", Title: clean(d.Channel.Title), Language: normLang(d.Channel.Language)}
	for _, it := range items {
		link := strings.TrimSpace(it.Link)
		if link == "" && strings.HasPrefix(it.GUID, "http") {
			link = strings.TrimSpace(it.GUID)
		}
		fi := FeedItem{URL: resolve(base, link), Title: clean(it.Title), Summary: clean(stripTags(it.Description)), ContentHTML: it.Content,
			GUID: strings.TrimSpace(it.GUID), Categories: it.Categories, Language: f.Language}
		fi.Author = clean(firstNonEmpty(it.Creator, it.Author))
		fi.PublishedAt = parseTime(firstNonEmpty(it.PubDate, it.DCDate))
		fi.UpdatedAt = parseTime(it.Updated)
		if fi.URL != "" {
			f.Items = append(f.Items, fi)
		}
	}
	return f, nil
}

type atomDoc struct {
	Title   string      `xml:"title"`
	Lang    string      `xml:"http://www.w3.org/XML/1998/namespace lang,attr"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title     string `xml:"title"`
	ID        string `xml:"id"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
	Summary   string `xml:"summary"`
	Content   string `xml:"content"`
	Links     []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
		Type string `xml:"type,attr"`
	} `xml:"link"`
	Author struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
}

func parseAtom(b []byte, base string) (*Feed, error) {
	var d atomDoc
	if err := newDecoder(b).Decode(&d); err != nil {
		return nil, fmt.Errorf("atom: %w", err)
	}
	f := &Feed{Format: "atom", Title: clean(d.Title), Language: normLang(d.Lang)}
	for _, e := range d.Entries {
		link := ""
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = l.Href
				break
			}
		}
		fi := FeedItem{URL: resolve(base, link), Title: clean(e.Title), Summary: clean(stripTags(e.Summary)), ContentHTML: e.Content,
			GUID: e.ID, Author: clean(e.Author.Name), Language: f.Language}
		fi.PublishedAt = parseTime(firstNonEmpty(e.Published, e.Updated))
		fi.UpdatedAt = parseTime(e.Updated)
		for _, c := range e.Categories {
			fi.Categories = append(fi.Categories, c.Term)
		}
		if fi.URL != "" {
			f.Items = append(f.Items, fi)
		}
	}
	return f, nil
}

type jsonFeed struct {
	Version  string `json:"version"`
	Title    string `json:"title"`
	Language string `json:"language"`
	Items    []struct {
		ID            string   `json:"id"`
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		ContentHTML   string   `json:"content_html"`
		ContentText   string   `json:"content_text"`
		Summary       string   `json:"summary"`
		DatePublished string   `json:"date_published"`
		DateModified  string   `json:"date_modified"`
		Language      string   `json:"language"`
		Tags          []string `json:"tags"`
		Authors       []struct {
			Name string `json:"name"`
		} `json:"authors"`
	} `json:"items"`
}

func parseJSONFeed(b []byte, base string) (*Feed, error) {
	var d jsonFeed
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("json feed: %w", err)
	}
	if !strings.Contains(d.Version, "jsonfeed.org") {
		return nil, ErrUnknownFeed
	}
	f := &Feed{Format: "json_feed", Title: d.Title, Language: normLang(d.Language)}
	for _, it := range d.Items {
		content := it.ContentHTML
		if content == "" && it.ContentText != "" {
			content = "<p>" + html.EscapeString(it.ContentText) + "</p>"
		}
		fi := FeedItem{URL: resolve(base, it.URL), Title: clean(it.Title), Summary: clean(it.Summary), ContentHTML: content, GUID: it.ID,
			Categories: it.Tags, Language: normLang(firstNonEmpty(it.Language, d.Language))}
		if len(it.Authors) > 0 {
			fi.Author = it.Authors[0].Name
		}
		fi.PublishedAt = parseTime(it.DatePublished)
		fi.UpdatedAt = parseTime(it.DateModified)
		if fi.URL != "" {
			f.Items = append(f.Items, fi)
		}
	}
	return f, nil
}

type sitemapDoc struct {
	URLs []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
		News    struct {
			Title           string `xml:"title"`
			PublicationDate string `xml:"publication_date"`
			Publication     struct {
				Language string `xml:"language"`
			} `xml:"publication"`
		} `xml:"news"`
	} `xml:"url"`
}

func parseSitemap(b []byte, base string) (*Feed, error) {
	var d sitemapDoc
	if err := newDecoder(b).Decode(&d); err != nil {
		return nil, fmt.Errorf("sitemap: %w", err)
	}
	f := &Feed{Format: "sitemap"}
	for _, u := range d.URLs {
		fi := FeedItem{URL: resolve(base, strings.TrimSpace(u.Loc)), Title: clean(u.News.Title), Language: normLang(u.News.Publication.Language)}
		fi.PublishedAt = parseTime(firstNonEmpty(u.News.PublicationDate, u.LastMod))
		fi.UpdatedAt = parseTime(u.LastMod)
		if fi.URL != "" {
			f.Items = append(f.Items, fi)
		}
	}
	return f, nil
}

var timeLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC3339Nano, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700", "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
	"Mon, 02 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04 -0700",
}

func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			t = t.UTC()
			// Reject absurd dates (feeds sometimes emit year 0001 or far future).
			if t.Year() < 1990 || t.After(time.Now().Add(48*time.Hour)) {
				return nil
			}
			return &t
		}
	}
	return nil
}

func resolve(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if r.IsAbs() {
		return r.String()
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return b.ResolveReference(r).String()
}

func clean(s string) string { return strings.TrimSpace(strings.Join(strings.Fields(html.UnescapeString(s)), " ")) }

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func normLang(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) >= 2 {
		s = s[:2]
	}
	switch s {
	case "ro", "ru", "en", "mo":
		if s == "mo" {
			return "ro"
		}
		return s
	}
	return ""
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			b.WriteByte(' ')
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}
