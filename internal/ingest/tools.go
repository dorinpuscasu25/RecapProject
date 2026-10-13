package ingest

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ssrf"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

// TestSourceInput describes an unsaved source to probe.
type TestSourceInput struct {
	EndpointType        string     `json:"endpoint_type"`
	EntryURL            string     `json:"entry_url"`
	ExtractionProfileID *uuid.UUID `json:"extraction_profile_id,omitempty"`
	FetchArticlePages   *bool      `json:"fetch_article_pages,omitempty"`
	PreviewItems        int        `json:"preview_items,omitempty"`
}

type ItemPreview struct {
	URL           string     `json:"url"`
	CanonicalURL  string     `json:"canonical_url"`
	Title         string     `json:"title"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	Language      string     `json:"language"`
	LanguageConf  float64    `json:"language_confidence"`
	Byline        string     `json:"byline,omitempty"`
	TextPreview   string     `json:"text_preview,omitempty"`
	TextChars     int        `json:"text_chars"`
	Confidence    float64    `json:"extraction_confidence"`
	SelectorsUsed map[string]string `json:"selectors_used,omitempty"`
	Warnings      []string   `json:"warnings,omitempty"`
	Error         string     `json:"error,omitempty"`
}

type TestSourceResult struct {
	OK                 bool          `json:"ok"`
	EntryURL           string        `json:"entry_url"`
	FinalURL           string        `json:"final_url,omitempty"`
	HTTPStatus         int           `json:"http_status,omitempty"`
	ContentType        string        `json:"content_type,omitempty"`
	DetectedFormat     string        `json:"detected_format,omitempty"`
	FeedTitle          string        `json:"feed_title,omitempty"`
	ItemCount          int           `json:"item_count"`
	Items              []ItemPreview `json:"items"`
	PublisherMatched   *uuid.UUID    `json:"publisher_id,omitempty"`
	PermissionWarnings []string      `json:"permission_warnings"`
	Errors             []string      `json:"errors"`
	DurationMS         int64         `json:"duration_ms"`
	Note               string        `json:"note"`
}

// TestSource fetches and parses an unsaved source and previews extraction of the first items.
// Nothing is persisted. Network access is SSRF-guarded and robots-aware.
func (s *Service) TestSource(ctx context.Context, in TestSourceInput) (*TestSourceResult, error) {
	start := time.Now()
	out := &TestSourceResult{EntryURL: in.EntryURL, Items: []ItemPreview{}, PermissionWarnings: []string{}, Errors: []string{},
		Note: "Diagnostics only; nothing was saved."}
	if in.PreviewItems <= 0 || in.PreviewItems > 5 {
		in.PreviewItems = 3
	}
	ep := &sources.Endpoint{ID: uuid.Nil, EndpointType: in.EndpointType, EntryURL: in.EntryURL, MaxRequestsPerMinute: 10, TimeoutSeconds: 20, FetchArticlePages: true}
	if in.FetchArticlePages != nil {
		ep.FetchArticlePages = *in.FetchArticlePages
	}
	if in.EndpointType != "fixture" {
		if _, err := s.sources.SSRFPolicy().ValidateURL(in.EntryURL); err != nil {
			return nil, apperr.Validation(apperr.FieldError{Field: "entry_url", Code: "blocked", Message: err.Error()})
		}
	}
	var prof *sources.ExtractionProfile
	if in.ExtractionProfileID != nil {
		p, err := s.sources.GetProfile(ctx, *in.ExtractionProfileID)
		if err != nil {
			return nil, err
		}
		prof = p
	}
	// Permission warnings based on domain match.
	if u, err := url.Parse(in.EntryURL); err == nil && u.Hostname() != "" {
		host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		var pubID uuid.UUID
		err := s.db.Pool.QueryRow(ctx, `SELECT id FROM publishers WHERE $1 = canonical_domain OR $1 LIKE '%.' || canonical_domain LIMIT 1`, host).Scan(&pubID)
		if err == nil {
			out.PublisherMatched = &pubID
			perm, _ := s.sources.EffectivePermission(ctx, s.db.Pool, pubID, nil)
			switch {
			case perm == nil:
				out.PermissionWarnings = append(out.PermissionWarnings, "no_permission_record")
			case !perm.CrawlAllowed():
				out.PermissionWarnings = append(out.PermissionWarnings, "crawl_not_permitted:"+perm.PermissionStatus)
			default:
				if !perm.AllowStoreFullText {
					out.PermissionWarnings = append(out.PermissionWarnings, "full_text_storage_not_permitted")
				}
				if !perm.AllowEmbed {
					out.PermissionWarnings = append(out.PermissionWarnings, "embedding_not_permitted")
				}
			}
		} else {
			out.PermissionWarnings = append(out.PermissionWarnings, "unknown_publisher_domain")
		}
	}
	res, err := s.fetcher.Fetch(ctx, ep, in.EntryURL, nil, maxFeedBytes, true)
	if res != nil {
		out.HTTPStatus, out.FinalURL, out.ContentType = res.Status, res.FinalURL, res.ContentType
	}
	if err != nil {
		if errors.Is(err, ssrf.ErrBlocked) {
			out.Errors = append(out.Errors, "ssrf_blocked: "+err.Error())
		} else if errors.Is(err, ErrRobotsDisallowed) {
			out.Errors = append(out.Errors, "robots_disallowed")
		} else {
			out.Errors = append(out.Errors, "fetch_failed: "+err.Error())
		}
		out.DurationMS = time.Since(start).Milliseconds()
		return out, nil
	}
	feed, perr := ParseFeed(res.Body, res.FinalURL)
	if perr != nil {
		// Not a feed: treat as a single article page preview.
		ex, eerr := ExtractHTML(res.Body, res.FinalURL, prof)
		if eerr != nil {
			out.Errors = append(out.Errors, "not_a_feed_and_not_html")
		} else {
			out.DetectedFormat = "html_page"
			out.ItemCount = 1
			out.Items = append(out.Items, preview(res.FinalURL, ex))
			out.OK = ex.Title != ""
		}
		out.DurationMS = time.Since(start).Milliseconds()
		return out, nil
	}
	out.DetectedFormat, out.FeedTitle, out.ItemCount = feed.Format, feed.Title, len(feed.Items)
	for i, it := range feed.Items {
		if i >= in.PreviewItems {
			break
		}
		var ex *Extracted
		var ierr error
		if ep.FetchArticlePages {
			var pr *FetchResult
			pr, ierr = s.fetcher.Fetch(ctx, ep, it.URL, nil, maxPageBytes, true)
			if ierr == nil {
				ex, ierr = ExtractHTML(pr.Body, pr.FinalURL, prof)
			}
		} else if it.ContentHTML != "" {
			ex, ierr = ExtractHTML([]byte("<html><body><article>"+it.ContentHTML+"</article></body></html>"), it.URL, prof)
		}
		p := ItemPreview{URL: it.URL, Title: it.Title, PublishedAt: it.PublishedAt}
		if ex != nil {
			p = preview(it.URL, ex)
			if p.Title == "" {
				p.Title = it.Title
			}
			if p.PublishedAt == nil {
				p.PublishedAt = it.PublishedAt
			}
		}
		if p.CanonicalURL == "" {
			p.CanonicalURL, _ = CanonicalizeURL(it.URL)
		}
		if ierr != nil {
			p.Error = ierr.Error()
		}
		out.Items = append(out.Items, p)
	}
	out.OK = out.ItemCount > 0
	out.DurationMS = time.Since(start).Milliseconds()
	return out, nil
}

func preview(u string, ex *Extracted) ItemPreview {
	canon := ex.CanonicalURL
	if canon == "" {
		canon = u
	}
	c, _ := CanonicalizeURL(canon)
	return ItemPreview{URL: u, CanonicalURL: c, Title: ex.Title, PublishedAt: ex.PublishedAt, Language: ex.Language, LanguageConf: ex.LanguageConf,
		Byline: ex.Byline, TextPreview: nlp.Truncate(ex.Text, 600), TextChars: len([]rune(ex.Text)), Confidence: ex.Confidence,
		SelectorsUsed: ex.SelectorsUsed, Warnings: ex.Warnings}
}

// FixtureResult reports regression results for an extraction profile.
type FixtureResult struct {
	Name     string   `json:"name"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures"`
	Title    string   `json:"extracted_title"`
	Author   string   `json:"extracted_author"`
}

// RunProfileFixtures runs a profile's regression fixtures (no network).
func RunProfileFixtures(p *sources.ExtractionProfile) []FixtureResult {
	var out []FixtureResult
	for _, f := range p.Fixtures {
		r := FixtureResult{Name: f.Name, Failures: []string{}}
		u := f.URL
		if u == "" {
			u = "https://fixture.invalid/article"
		}
		ex, err := ExtractHTML([]byte(f.HTML), u, p)
		if err != nil {
			r.Failures = append(r.Failures, "extract: "+err.Error())
			out = append(out, r)
			continue
		}
		r.Title, r.Author = ex.Title, ex.Byline
		if f.Expected.Title != "" && ex.Title != f.Expected.Title {
			r.Failures = append(r.Failures, "title mismatch: got "+ex.Title)
		}
		if f.Expected.Author != "" && ex.Byline != f.Expected.Author {
			r.Failures = append(r.Failures, "author mismatch: got "+ex.Byline)
		}
		for _, c := range f.Expected.Contains {
			if !strings.Contains(ex.Text, c) {
				r.Failures = append(r.Failures, "missing text: "+c)
			}
		}
		for _, c := range f.Expected.Excludes {
			if strings.Contains(ex.Text, c) {
				r.Failures = append(r.Failures, "unexpected text: "+c)
			}
		}
		r.Passed = len(r.Failures) == 0
		out = append(out, r)
	}
	return out
}

// PreviewURL extracts a single page with an optional profile (admin preview; nothing saved).
func (s *Service) PreviewURL(ctx context.Context, rawURL string, profileID *uuid.UUID) (*ItemPreview, error) {
	if _, err := s.sources.SSRFPolicy().ValidateURL(rawURL); err != nil {
		return nil, apperr.Validation(apperr.FieldError{Field: "url", Code: "blocked", Message: err.Error()})
	}
	var prof *sources.ExtractionProfile
	if profileID != nil {
		p, err := s.sources.GetProfile(ctx, *profileID)
		if err != nil {
			return nil, err
		}
		prof = p
	}
	res, err := s.fetcher.Fetch(ctx, nil, rawURL, nil, maxPageBytes, true)
	if err != nil {
		return nil, apperr.Unavailable("fetch_failed", err.Error())
	}
	ex, err := ExtractHTML(res.Body, res.FinalURL, prof)
	if err != nil {
		return nil, apperr.ValidationMsg("extract_failed", err.Error())
	}
	p := preview(res.FinalURL, ex)
	return &p, nil
}
