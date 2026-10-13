package sources

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
)

type RetryPolicy struct {
	MaxAttempts        int `json:"max_attempts"`
	BaseBackoffSeconds int `json:"base_backoff_seconds"`
}

type CrawlWindow struct {
	Timezone string `json:"timezone,omitempty"`
	Start    string `json:"start,omitempty"` // HH:MM local
	End      string `json:"end,omitempty"`
}

// Allows reports whether t falls inside the crawl window (empty window = always).
func (w CrawlWindow) Allows(t time.Time) bool {
	if w.Start == "" || w.End == "" {
		return true
	}
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil || w.Timezone == "" {
		loc = time.UTC
	}
	lt := t.In(loc)
	cur := lt.Format("15:04")
	if w.Start <= w.End {
		return cur >= w.Start && cur < w.End
	}
	return cur >= w.Start || cur < w.End // overnight window
}

type Endpoint struct {
	ID                   uuid.UUID   `json:"id"`
	PublisherID          uuid.UUID   `json:"publisher_id"`
	Name                 string      `json:"name"`
	EndpointType         string      `json:"endpoint_type"`
	BaseURL              string      `json:"base_url"`
	EntryURL             string      `json:"entry_url"`
	Status               string      `json:"status"`
	PollIntervalSeconds  int         `json:"poll_interval_seconds"`
	Priority             int         `json:"priority"`
	Languages            []string    `json:"languages"`
	Tags                 []string    `json:"tags"`
	CategoryHints        []uuid.UUID `json:"category_hints"`
	AllowedCategoryIDs   []uuid.UUID `json:"allowed_category_ids"`
	AllowedHosts         []string    `json:"allowed_hosts"`
	URLBlocklist         []string    `json:"url_blocklist"`
	FetchMode            string      `json:"fetch_mode"`
	JSRequired           bool        `json:"js_required"`
	FetchArticlePages    bool        `json:"fetch_article_pages"`
	ExtractionProfileID  *uuid.UUID  `json:"extraction_profile_id"`
	MaxRequestsPerMinute int         `json:"max_requests_per_minute"`
	ConcurrencyLimit     int         `json:"concurrency_limit"`
	TimeoutSeconds       int         `json:"timeout_seconds"`
	MaxItemsPerPoll      int         `json:"max_items_per_poll"`
	RetryPolicy          RetryPolicy `json:"retry_policy"`
	CrawlWindow          CrawlWindow `json:"crawl_window"`
	ETag                 *string     `json:"etag,omitempty"`
	LastModified         *string     `json:"last_modified,omitempty"`
	HealthState          string      `json:"health_state"`
	ConsecutiveFailures  int         `json:"consecutive_failures"`
	LastSuccessAt        *time.Time  `json:"last_success_at,omitempty"`
	LastError            *string     `json:"last_error,omitempty"`
	NextPollAt           *time.Time  `json:"next_poll_at,omitempty"`
	CreatedAt            time.Time   `json:"created_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
	Version              int         `json:"version"`
}

const epCols = `id, publisher_id, name, endpoint_type, base_url, entry_url, status, poll_interval_seconds, priority, languages, tags,
	category_hints, allowed_category_ids, allowed_hosts, url_blocklist, fetch_mode, js_required, fetch_article_pages, extraction_profile_id,
	max_requests_per_minute, concurrency_limit, timeout_seconds, max_items_per_poll, retry_policy, crawl_window, etag, last_modified,
	health_state, consecutive_failures, last_success_at, last_error, next_poll_at, created_at, updated_at, version`

func scanEndpoint(row pgx.Row) (*Endpoint, error) {
	var e Endpoint
	var rp, cw []byte
	err := row.Scan(&e.ID, &e.PublisherID, &e.Name, &e.EndpointType, &e.BaseURL, &e.EntryURL, &e.Status, &e.PollIntervalSeconds, &e.Priority,
		&e.Languages, &e.Tags, &e.CategoryHints, &e.AllowedCategoryIDs, &e.AllowedHosts, &e.URLBlocklist, &e.FetchMode, &e.JSRequired,
		&e.FetchArticlePages, &e.ExtractionProfileID, &e.MaxRequestsPerMinute, &e.ConcurrencyLimit, &e.TimeoutSeconds, &e.MaxItemsPerPoll,
		&rp, &cw, &e.ETag, &e.LastModified, &e.HealthState, &e.ConsecutiveFailures, &e.LastSuccessAt, &e.LastError, &e.NextPollAt,
		&e.CreatedAt, &e.UpdatedAt, &e.Version)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(rp, &e.RetryPolicy)
	_ = json.Unmarshal(cw, &e.CrawlWindow)
	return &e, nil
}

type EndpointInput struct {
	PublisherID          *uuid.UUID   `json:"publisher_id,omitempty"`
	Name                 *string      `json:"name,omitempty"`
	EndpointType         *string      `json:"endpoint_type,omitempty"`
	BaseURL              *string      `json:"base_url,omitempty"`
	EntryURL             *string      `json:"entry_url,omitempty"`
	PollIntervalSeconds  *int         `json:"poll_interval_seconds,omitempty"`
	Priority             *int         `json:"priority,omitempty"`
	Languages            []string     `json:"languages,omitempty"`
	Tags                 []string     `json:"tags,omitempty"`
	CategoryHints        []uuid.UUID  `json:"category_hints,omitempty"`
	AllowedCategoryIDs   []uuid.UUID  `json:"allowed_category_ids,omitempty"`
	AllowedHosts         []string     `json:"allowed_hosts,omitempty"`
	URLBlocklist         []string     `json:"url_blocklist,omitempty"`
	FetchMode            *string      `json:"fetch_mode,omitempty"`
	JSRequired           *bool        `json:"js_required,omitempty"`
	FetchArticlePages    *bool        `json:"fetch_article_pages,omitempty"`
	ExtractionProfileID  *uuid.UUID   `json:"extraction_profile_id,omitempty"`
	MaxRequestsPerMinute *int         `json:"max_requests_per_minute,omitempty"`
	ConcurrencyLimit     *int         `json:"concurrency_limit,omitempty"`
	TimeoutSeconds       *int         `json:"timeout_seconds,omitempty"`
	MaxItemsPerPoll      *int         `json:"max_items_per_poll,omitempty"`
	RetryPolicy          *RetryPolicy `json:"retry_policy,omitempty"`
	CrawlWindow          *CrawlWindow `json:"crawl_window,omitempty"`
	Version              *int         `json:"version,omitempty"`
}

var endpointTypes = map[string]bool{"rss": true, "atom": true, "json_feed": true, "sitemap": true, "html_list": true, "official_api": true, "fixture": true}

func (e *Endpoint) apply(in EndpointInput) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	setI := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	if in.PublisherID != nil {
		e.PublisherID = *in.PublisherID
	}
	set(&e.Name, in.Name)
	set(&e.EndpointType, in.EndpointType)
	set(&e.BaseURL, in.BaseURL)
	set(&e.EntryURL, in.EntryURL)
	setI(&e.PollIntervalSeconds, in.PollIntervalSeconds)
	setI(&e.Priority, in.Priority)
	if in.Languages != nil {
		e.Languages = in.Languages
	}
	if in.Tags != nil {
		e.Tags = in.Tags
	}
	if in.CategoryHints != nil {
		e.CategoryHints = in.CategoryHints
	}
	if in.AllowedCategoryIDs != nil {
		e.AllowedCategoryIDs = in.AllowedCategoryIDs
	}
	if in.AllowedHosts != nil {
		e.AllowedHosts = lowerAll(in.AllowedHosts)
	}
	if in.URLBlocklist != nil {
		e.URLBlocklist = in.URLBlocklist
	}
	set(&e.FetchMode, in.FetchMode)
	if in.JSRequired != nil {
		e.JSRequired = *in.JSRequired
	}
	if in.FetchArticlePages != nil {
		e.FetchArticlePages = *in.FetchArticlePages
	}
	if in.ExtractionProfileID != nil {
		e.ExtractionProfileID = in.ExtractionProfileID
	}
	setI(&e.MaxRequestsPerMinute, in.MaxRequestsPerMinute)
	setI(&e.ConcurrencyLimit, in.ConcurrencyLimit)
	setI(&e.TimeoutSeconds, in.TimeoutSeconds)
	setI(&e.MaxItemsPerPoll, in.MaxItemsPerPoll)
	if in.RetryPolicy != nil {
		e.RetryPolicy = *in.RetryPolicy
	}
	if in.CrawlWindow != nil {
		e.CrawlWindow = *in.CrawlWindow
	}
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (s *Service) validateEndpoint(e *Endpoint) error {
	var v apperr.Validator
	v.Check(endpointTypes[e.EndpointType], "endpoint_type", "invalid", "Invalid endpoint type.")
	v.Check(e.EndpointType != "official_api", "endpoint_type", "unsupported",
		"official_api endpoints need a source-specific adapter that is not available in this build; use json_feed, rss, atom, sitemap or html_list.")
	if e.EndpointType == "html_list" {
		v.Check(e.FetchArticlePages, "fetch_article_pages", "required", "html_list endpoints discover links on a list page and must fetch article pages.")
	}
	if e.EndpointType == "fixture" {
		// Fixture endpoints read deterministic local files (fixture://name.xml); never network.
		v.Check(strings.HasPrefix(e.EntryURL, "fixture://") && !strings.Contains(e.EntryURL, ".."), "entry_url", "invalid", "Fixture URLs look like fixture://feeds/name.xml")
	} else {
		if _, err := s.policy.ValidateURL(e.EntryURL); err != nil {
			v.Add("entry_url", "blocked", "URL is not allowed: "+err.Error())
		}
		if e.BaseURL != "" {
			if _, err := s.policy.ValidateURL(e.BaseURL); err != nil {
				v.Add("base_url", "blocked", "URL is not allowed: "+err.Error())
			}
		}
	}
	v.Check(e.PollIntervalSeconds >= 60 && e.PollIntervalSeconds <= 604800, "poll_interval_seconds", "out_of_range", "60..604800 seconds.")
	v.Check(e.MaxRequestsPerMinute >= 1 && e.MaxRequestsPerMinute <= 600, "max_requests_per_minute", "out_of_range", "1..600")
	v.Check(e.ConcurrencyLimit >= 1 && e.ConcurrencyLimit <= 16, "concurrency_limit", "out_of_range", "1..16")
	v.Check(e.TimeoutSeconds >= 1 && e.TimeoutSeconds <= 120, "timeout_seconds", "out_of_range", "1..120")
	v.Check(e.MaxItemsPerPoll >= 1 && e.MaxItemsPerPoll <= 1000, "max_items_per_poll", "out_of_range", "1..1000")
	v.Check(e.FetchMode == "http", "fetch_mode", "unsupported", "Only fetch_mode http is available; headless rendering is not part of this build.")
	v.Check(!e.JSRequired, "js_required", "unsupported", "Sources that require JavaScript rendering are not supported in this build.")
	v.Check(e.RetryPolicy.MaxAttempts >= 1 && e.RetryPolicy.MaxAttempts <= 10, "retry_policy.max_attempts", "out_of_range", "1..10")
	for _, l := range e.Languages {
		v.Check(langRe.MatchString(l), "languages", "invalid", "Languages must be ISO 639-1 codes.")
	}
	if e.CrawlWindow.Start != "" || e.CrawlWindow.End != "" {
		_, err1 := time.Parse("15:04", e.CrawlWindow.Start)
		_, err2 := time.Parse("15:04", e.CrawlWindow.End)
		v.Check(err1 == nil && err2 == nil, "crawl_window", "invalid", "Crawl window times must be HH:MM.")
		if e.CrawlWindow.Timezone != "" {
			_, err := time.LoadLocation(e.CrawlWindow.Timezone)
			v.Check(err == nil, "crawl_window.timezone", "invalid", "Invalid IANA timezone.")
		}
	}
	return v.Err()
}

// HostAllowed checks that a discovered article URL stays within the publisher's domain or the
// endpoint's explicit allowed hosts (prevents feeds from steering the crawler elsewhere).
func HostAllowed(e *Endpoint, publisherDomain, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if h == publisherDomain || strings.HasSuffix(h, "."+publisherDomain) {
		return true
	}
	for _, a := range e.AllowedHosts {
		a = strings.TrimPrefix(a, "www.")
		if h == a || strings.HasSuffix(h, "."+a) {
			return true
		}
	}
	return false
}

func (s *Service) CreateEndpoint(ctx context.Context, in EndpointInput) (*Endpoint, error) {
	if in.PublisherID == nil {
		return nil, apperr.Validation(apperr.FieldError{Field: "publisher_id", Code: "required", Message: "publisher_id is required."})
	}
	e := &Endpoint{ID: ids.New(), Status: "paused", PollIntervalSeconds: 900, Priority: 100, Languages: []string{}, Tags: []string{},
		CategoryHints: []uuid.UUID{}, AllowedCategoryIDs: []uuid.UUID{}, AllowedHosts: []string{}, URLBlocklist: []string{}, FetchMode: "http",
		FetchArticlePages: true, MaxRequestsPerMinute: 10, ConcurrencyLimit: 2, TimeoutSeconds: 20, MaxItemsPerPoll: 50,
		RetryPolicy: RetryPolicy{MaxAttempts: 3, BaseBackoffSeconds: 30}, HealthState: "unknown"}
	e.apply(in)
	if e.BaseURL == "" {
		e.BaseURL = e.EntryURL
	}
	if err := s.validateEndpoint(e); err != nil {
		return nil, err
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.getPublisherTx(ctx, tx, e.PublisherID); err != nil {
			return err
		}
		if err := s.insertEndpoint(ctx, tx, e); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "source.create", ResourceType: "source_endpoint", ResourceID: e.ID.String(), New: e})
	})
	if err != nil {
		return nil, err
	}
	return s.GetEndpoint(ctx, e.ID)
}

func (s *Service) insertEndpoint(ctx context.Context, tx pgx.Tx, e *Endpoint) error {
	rp, _ := json.Marshal(e.RetryPolicy)
	cw, _ := json.Marshal(e.CrawlWindow)
	_, err := tx.Exec(ctx, `INSERT INTO source_endpoints (id, publisher_id, name, endpoint_type, base_url, entry_url, status, poll_interval_seconds,
		priority, languages, tags, category_hints, allowed_category_ids, allowed_hosts, url_blocklist, fetch_mode, js_required, fetch_article_pages,
		extraction_profile_id, max_requests_per_minute, concurrency_limit, timeout_seconds, max_items_per_poll, retry_policy, crawl_window,
		created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$26)`,
		e.ID, e.PublisherID, e.Name, e.EndpointType, e.BaseURL, e.EntryURL, e.Status, e.PollIntervalSeconds, e.Priority, e.Languages, e.Tags,
		e.CategoryHints, e.AllowedCategoryIDs, e.AllowedHosts, e.URLBlocklist, e.FetchMode, e.JSRequired, e.FetchArticlePages,
		e.ExtractionProfileID, e.MaxRequestsPerMinute, e.ConcurrencyLimit, e.TimeoutSeconds, e.MaxItemsPerPoll, rp, cw, actor(ctx))
	if db.IsUniqueViolation(err) {
		return apperr.Conflict("source_entry_url_taken", "A non-archived source with this entry URL already exists.")
	}
	if db.IsForeignKeyViolation(err) {
		return apperr.ValidationMsg("source_reference_invalid", "Referenced publisher or extraction profile does not exist.")
	}
	return err
}

func (s *Service) GetEndpoint(ctx context.Context, id uuid.UUID) (*Endpoint, error) {
	return s.GetEndpointQ(ctx, s.db.Pool, id)
}

func (s *Service) GetEndpointQ(ctx context.Context, q db.Querier, id uuid.UUID) (*Endpoint, error) {
	e, err := scanEndpoint(q.QueryRow(ctx, `SELECT `+epCols+` FROM source_endpoints WHERE id=$1`, id))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("source_not_found", "Source endpoint not found.")
	}
	return e, err
}

type EndpointFilter struct {
	PublisherID *uuid.UUID
	Status      string
	Type        string
	Tag         string
	Health      string
	Limit       int
}

func (s *Service) ListEndpoints(ctx context.Context, f EndpointFilter) ([]Endpoint, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT `+epCols+` FROM source_endpoints
		WHERE ($1::uuid IS NULL OR publisher_id=$1) AND ($2='' OR status=$2) AND ($3='' OR endpoint_type=$3)
		  AND ($4='' OR $4 = ANY(tags)) AND ($5='' OR health_state=$5)
		ORDER BY priority, created_at LIMIT $6`, f.PublisherID, f.Status, f.Type, f.Tag, f.Health, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (s *Service) UpdateEndpoint(ctx context.Context, id uuid.UUID, in EndpointInput) (*Endpoint, error) {
	if in.PublisherID != nil {
		return nil, apperr.ValidationMsg("source_publisher_immutable", "A source cannot be moved to another publisher; create a new one.")
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		cur, err := scanEndpoint(tx.QueryRow(ctx, `SELECT `+epCols+` FROM source_endpoints WHERE id=$1 FOR UPDATE`, id))
		if db.IsNoRows(err) {
			return apperr.NotFound("source_not_found", "Source endpoint not found.")
		}
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != cur.Version {
			return apperr.VersionConflict("source endpoint")
		}
		if cur.Status == "archived" {
			return apperr.Conflict("source_archived", "Archived sources cannot be edited.")
		}
		old := *cur
		cur.apply(in)
		if err := s.validateEndpoint(cur); err != nil {
			return err
		}
		rp, _ := json.Marshal(cur.RetryPolicy)
		cw, _ := json.Marshal(cur.CrawlWindow)
		// Changing the poll interval reschedules the next poll.
		_, err = tx.Exec(ctx, `UPDATE source_endpoints SET name=$2, endpoint_type=$3, base_url=$4, entry_url=$5, poll_interval_seconds=$6,
			priority=$7, languages=$8, tags=$9, category_hints=$10, allowed_category_ids=$11, allowed_hosts=$12, url_blocklist=$13, fetch_mode=$14,
			js_required=$15, fetch_article_pages=$16, extraction_profile_id=$17, max_requests_per_minute=$18, concurrency_limit=$19,
			timeout_seconds=$20, max_items_per_poll=$21, retry_policy=$22, crawl_window=$23, updated_by=$24, updated_at=now(), version=version+1,
			next_poll_at = CASE WHEN $6 <> poll_interval_seconds AND status='enabled' THEN now() + make_interval(secs => $6) ELSE next_poll_at END,
			etag = CASE WHEN $5 <> entry_url THEN NULL ELSE etag END,
			last_modified = CASE WHEN $5 <> entry_url THEN NULL ELSE last_modified END
			WHERE id=$1`,
			id, cur.Name, cur.EndpointType, cur.BaseURL, cur.EntryURL, cur.PollIntervalSeconds, cur.Priority, cur.Languages, cur.Tags,
			cur.CategoryHints, cur.AllowedCategoryIDs, cur.AllowedHosts, cur.URLBlocklist, cur.FetchMode, cur.JSRequired, cur.FetchArticlePages,
			cur.ExtractionProfileID, cur.MaxRequestsPerMinute, cur.ConcurrencyLimit, cur.TimeoutSeconds, cur.MaxItemsPerPoll, rp, cw, actor(ctx))
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("source_entry_url_taken", "A non-archived source with this entry URL already exists.")
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "source.update", ResourceType: "source_endpoint", ResourceID: id.String(), Old: old, New: cur})
	})
	if err != nil {
		return nil, err
	}
	return s.GetEndpoint(ctx, id)
}

// SetEndpointStatus transitions enabled/paused/disabled/archived/pending_permission. Enabling
// requires an active publisher and an effective permission that allows crawling.
func (s *Service) SetEndpointStatus(ctx context.Context, id uuid.UUID, status string) (*Endpoint, error) {
	switch status {
	case "enabled", "paused", "disabled", "archived", "pending_permission":
	default:
		return nil, apperr.ValidationMsg("source_status_invalid", "Invalid status.")
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		cur, err := scanEndpoint(tx.QueryRow(ctx, `SELECT `+epCols+` FROM source_endpoints WHERE id=$1 FOR UPDATE`, id))
		if db.IsNoRows(err) {
			return apperr.NotFound("source_not_found", "Source endpoint not found.")
		}
		if err != nil {
			return err
		}
		if cur.Status == "archived" && status != "archived" {
			return apperr.Conflict("source_archived", "Archived sources cannot be re-enabled; create a new source.")
		}
		if status == "enabled" {
			pub, err := s.getPublisherTx(ctx, tx, cur.PublisherID)
			if err != nil {
				return err
			}
			if pub.Status != "active" {
				return apperr.ValidationMsg("publisher_not_active", "The publisher must be active before enabling its sources.")
			}
			perm, err := s.EffectivePermission(ctx, tx, cur.PublisherID, &cur.ID)
			if err != nil {
				return err
			}
			if !perm.CrawlAllowed() {
				return apperr.ValidationMsg("source_permission_required",
					"A granted permission with crawling allowed (and robots/terms not blocked) is required before enabling.")
			}
		}
		_, err = tx.Exec(ctx, `UPDATE source_endpoints SET status=$2, updated_at=now(), version=version+1, updated_by=$3,
			next_poll_at = CASE WHEN $2='enabled' THEN COALESCE(next_poll_at, now()) ELSE next_poll_at END WHERE id=$1`, id, status, actor(ctx))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "source.status", ResourceType: "source_endpoint", ResourceID: id.String(),
			Old: map[string]any{"status": cur.Status}, New: map[string]any{"status": status}})
	})
	if err != nil {
		return nil, err
	}
	return s.GetEndpoint(ctx, id)
}

// ============ Crawl runs & health ============

type CrawlRun struct {
	ID             uuid.UUID       `json:"id"`
	SourceEndpoint uuid.UUID       `json:"source_endpoint_id"`
	Trigger        string          `json:"trigger"`
	Status         string          `json:"status"`
	StartedAt      time.Time       `json:"started_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	HTTPStatus     *int            `json:"http_status,omitempty"`
	NotModified    bool            `json:"not_modified"`
	Discovered     int             `json:"discovered_count"`
	Fetched        int             `json:"fetched_count"`
	Accepted       int             `json:"accepted_count"`
	Duplicates     int             `json:"duplicate_count"`
	Rejected       int             `json:"rejected_count"`
	Errors         int             `json:"error_count"`
	ErrorDetails   json.RawMessage `json:"errors"`
	DurationMS     *int            `json:"duration_ms,omitempty"`
}

func (s *Service) ListCrawlRuns(ctx context.Context, endpointID uuid.UUID, limit int) ([]CrawlRun, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT id, source_endpoint_id, trigger, status, started_at, completed_at, http_status, not_modified,
		discovered_count, fetched_count, accepted_count, duplicate_count, rejected_count, error_count, errors, duration_ms
		FROM source_crawl_runs WHERE source_endpoint_id=$1 ORDER BY started_at DESC LIMIT $2`, endpointID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CrawlRun, error) {
		var c CrawlRun
		err := r.Scan(&c.ID, &c.SourceEndpoint, &c.Trigger, &c.Status, &c.StartedAt, &c.CompletedAt, &c.HTTPStatus, &c.NotModified,
			&c.Discovered, &c.Fetched, &c.Accepted, &c.Duplicates, &c.Rejected, &c.Errors, &c.ErrorDetails, &c.DurationMS)
		return c, err
	})
}

type Health struct {
	SourceEndpointID    uuid.UUID       `json:"source_endpoint_id"`
	HealthState         string          `json:"health_state"`
	Status              string          `json:"status"`
	LastSuccessAt       *time.Time      `json:"last_success_at,omitempty"`
	LastError           *string         `json:"last_error,omitempty"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	NextPollAt          *time.Time      `json:"next_poll_at,omitempty"`
	LastHTTPStatus      *int            `json:"last_http_status,omitempty"`
	Runs24h             int             `json:"runs_24h"`
	FailedRuns24h       int             `json:"failed_runs_24h"`
	ParseCoverage       *float32        `json:"parse_coverage,omitempty"`
	DuplicateRate       *float32        `json:"duplicate_rate,omitempty"`
	FreshnessLagSeconds *int64          `json:"freshness_lag_seconds,omitempty"`
	Anomalies           json.RawMessage `json:"anomalies"`
	ArticlesTotal       int64           `json:"articles_total"`
	ArticlesLast24h     int64           `json:"articles_last_24h"`
	UpdatedAt           *time.Time      `json:"updated_at,omitempty"`
}

func (s *Service) GetHealth(ctx context.Context, id uuid.UUID) (*Health, error) {
	e, err := s.GetEndpoint(ctx, id)
	if err != nil {
		return nil, err
	}
	h := &Health{SourceEndpointID: id, HealthState: e.HealthState, Status: e.Status, LastSuccessAt: e.LastSuccessAt, LastError: e.LastError,
		ConsecutiveFailures: e.ConsecutiveFailures, NextPollAt: e.NextPollAt, Anomalies: json.RawMessage("[]")}
	err = s.db.Pool.QueryRow(ctx, `SELECT last_http_status, runs_24h, failed_runs_24h, parse_coverage, duplicate_rate, freshness_lag_seconds, anomalies, updated_at
		FROM source_health WHERE source_endpoint_id=$1`, id).Scan(&h.LastHTTPStatus, &h.Runs24h, &h.FailedRuns24h, &h.ParseCoverage,
		&h.DuplicateRate, &h.FreshnessLagSeconds, &h.Anomalies, &h.UpdatedAt)
	if err != nil && !db.IsNoRows(err) {
		return nil, err
	}
	_ = s.db.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE first_seen_at > now() - interval '24 hours') FROM articles WHERE source_endpoint_id=$1`, id).
		Scan(&h.ArticlesTotal, &h.ArticlesLast24h)
	return h, nil
}

// SourceMetrics returns a compact health overview of all non-archived sources.
func (s *Service) SourceMetrics(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT e.id, e.name, p.name, e.status, e.health_state, e.consecutive_failures, e.last_success_at,
		h.runs_24h, h.failed_runs_24h, h.parse_coverage, h.duplicate_rate, h.freshness_lag_seconds
		FROM source_endpoints e JOIN publishers p ON p.id=e.publisher_id LEFT JOIN source_health h ON h.source_endpoint_id=e.id
		WHERE e.status <> 'archived' ORDER BY e.health_state DESC, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id uuid.UUID
		var name, pub, status, health string
		var fails int
		var lastOK *time.Time
		var runs, failed *int
		var cov, dup *float32
		var lag *int64
		if err := rows.Scan(&id, &name, &pub, &status, &health, &fails, &lastOK, &runs, &failed, &cov, &dup, &lag); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"source_endpoint_id": id, "name": name, "publisher": pub, "status": status, "health_state": health,
			"consecutive_failures": fails, "last_success_at": lastOK, "runs_24h": runs, "failed_runs_24h": failed, "parse_coverage": cov,
			"duplicate_rate": dup, "freshness_lag_seconds": lag})
	}
	return out, rows.Err()
}

// ============ Extraction profiles ============

type ProfileFixture struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	HTML     string `json:"html"`
	Expected struct {
		Title    string   `json:"title,omitempty"`
		Author   string   `json:"author,omitempty"`
		Contains []string `json:"contains,omitempty"`
		Excludes []string `json:"excludes,omitempty"`
	} `json:"expected"`
}

type ExtractionProfile struct {
	ID                 uuid.UUID        `json:"id"`
	Name               string           `json:"name"`
	TitleSelectors     []string         `json:"title_selectors"`
	ContentSelectors   []string         `json:"content_selectors"`
	DateSelectors      []string         `json:"date_selectors"`
	CanonicalSelectors []string         `json:"canonical_selectors"`
	AuthorSelectors    []string         `json:"author_selectors"`
	CleanupSelectors   []string         `json:"cleanup_selectors"`
	LinkSelectors      []string         `json:"link_selectors"` // html_list endpoints: anchors to article pages
	Pagination         map[string]any   `json:"pagination"`
	Fixtures           []ProfileFixture `json:"fixtures"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
	Version            int              `json:"version"`
}

const profCols = `id, name, title_selectors, content_selectors, date_selectors, canonical_selectors, author_selectors, cleanup_selectors,
	link_selectors, pagination, fixtures, created_at, updated_at, version`

func scanProfile(row pgx.Row) (*ExtractionProfile, error) {
	var p ExtractionProfile
	var pag, fix []byte
	if err := row.Scan(&p.ID, &p.Name, &p.TitleSelectors, &p.ContentSelectors, &p.DateSelectors, &p.CanonicalSelectors, &p.AuthorSelectors,
		&p.CleanupSelectors, &p.LinkSelectors, &pag, &fix, &p.CreatedAt, &p.UpdatedAt, &p.Version); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(pag, &p.Pagination)
	_ = json.Unmarshal(fix, &p.Fixtures)
	if p.Fixtures == nil {
		p.Fixtures = []ProfileFixture{}
	}
	return &p, nil
}

type ProfileInput struct {
	Name               *string          `json:"name,omitempty"`
	TitleSelectors     []string         `json:"title_selectors,omitempty"`
	ContentSelectors   []string         `json:"content_selectors,omitempty"`
	DateSelectors      []string         `json:"date_selectors,omitempty"`
	CanonicalSelectors []string         `json:"canonical_selectors,omitempty"`
	AuthorSelectors    []string         `json:"author_selectors,omitempty"`
	CleanupSelectors   []string         `json:"cleanup_selectors,omitempty"`
	LinkSelectors      []string         `json:"link_selectors,omitempty"`
	Pagination         map[string]any   `json:"pagination,omitempty"`
	Fixtures           []ProfileFixture `json:"fixtures,omitempty"`
	Version            *int             `json:"version,omitempty"`
}

// SelectorValidator is implemented by the extractor (validates selector syntax without executing code).
type SelectorValidator func(selector string) error

func (p *ExtractionProfile) apply(in ProfileInput) {
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.TitleSelectors != nil {
		p.TitleSelectors = in.TitleSelectors
	}
	if in.ContentSelectors != nil {
		p.ContentSelectors = in.ContentSelectors
	}
	if in.DateSelectors != nil {
		p.DateSelectors = in.DateSelectors
	}
	if in.CanonicalSelectors != nil {
		p.CanonicalSelectors = in.CanonicalSelectors
	}
	if in.AuthorSelectors != nil {
		p.AuthorSelectors = in.AuthorSelectors
	}
	if in.CleanupSelectors != nil {
		p.CleanupSelectors = in.CleanupSelectors
	}
	if in.LinkSelectors != nil {
		p.LinkSelectors = in.LinkSelectors
	}
	if in.Pagination != nil {
		p.Pagination = in.Pagination
	}
	if in.Fixtures != nil {
		p.Fixtures = in.Fixtures
	}
}

func (p *ExtractionProfile) validate(check SelectorValidator) error {
	var v apperr.Validator
	v.Check(p.Name != "" && len(p.Name) <= 100, "name", "invalid", "Name is required.")
	groups := map[string][]string{"title_selectors": p.TitleSelectors, "content_selectors": p.ContentSelectors, "date_selectors": p.DateSelectors,
		"canonical_selectors": p.CanonicalSelectors, "author_selectors": p.AuthorSelectors, "cleanup_selectors": p.CleanupSelectors, "link_selectors": p.LinkSelectors}
	for field, sels := range groups {
		v.Check(len(sels) <= 20, field, "too_many", "At most 20 selectors.")
		for _, sel := range sels {
			if len(sel) > 300 {
				v.Add(field, "too_long", "Selector too long.")
				continue
			}
			if check != nil {
				if err := check(sel); err != nil {
					v.Add(field, "invalid_selector", err.Error())
				}
			}
		}
	}
	v.Check(len(p.Fixtures) <= 20, "fixtures", "too_many", "At most 20 fixtures.")
	for _, f := range p.Fixtures {
		v.Check(len(f.HTML) <= 512*1024, "fixtures", "too_large", "Fixture HTML must be <= 512 KiB.")
	}
	return v.Err()
}

func (s *Service) CreateProfile(ctx context.Context, in ProfileInput, check SelectorValidator) (*ExtractionProfile, error) {
	p := &ExtractionProfile{ID: ids.New(), TitleSelectors: []string{}, ContentSelectors: []string{}, DateSelectors: []string{},
		CanonicalSelectors: []string{}, AuthorSelectors: []string{}, CleanupSelectors: []string{}, LinkSelectors: []string{}, Pagination: map[string]any{},
		Fixtures: []ProfileFixture{}}
	p.apply(in)
	if err := p.validate(check); err != nil {
		return nil, err
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		pag, _ := json.Marshal(p.Pagination)
		fix, _ := json.Marshal(p.Fixtures)
		_, err := tx.Exec(ctx, `INSERT INTO extraction_profiles (id, name, title_selectors, content_selectors, date_selectors, canonical_selectors,
			author_selectors, cleanup_selectors, link_selectors, pagination, fixtures, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`,
			p.ID, p.Name, p.TitleSelectors, p.ContentSelectors, p.DateSelectors, p.CanonicalSelectors, p.AuthorSelectors, p.CleanupSelectors, p.LinkSelectors,
			pag, fix, actor(ctx))
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("profile_name_taken", "A profile with this name already exists.")
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "extraction_profile.create", ResourceType: "extraction_profile", ResourceID: p.ID.String(), New: map[string]any{"name": p.Name}})
	})
	if err != nil {
		return nil, err
	}
	return s.GetProfile(ctx, p.ID)
}

func (s *Service) GetProfile(ctx context.Context, id uuid.UUID) (*ExtractionProfile, error) {
	p, err := scanProfile(s.db.Pool.QueryRow(ctx, `SELECT `+profCols+` FROM extraction_profiles WHERE id=$1`, id))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("profile_not_found", "Extraction profile not found.")
	}
	return p, err
}

func (s *Service) ListProfiles(ctx context.Context) ([]ExtractionProfile, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT `+profCols+` FROM extraction_profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtractionProfile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Service) UpdateProfile(ctx context.Context, id uuid.UUID, in ProfileInput, check SelectorValidator) (*ExtractionProfile, error) {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		cur, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profCols+` FROM extraction_profiles WHERE id=$1 FOR UPDATE`, id))
		if db.IsNoRows(err) {
			return apperr.NotFound("profile_not_found", "Extraction profile not found.")
		}
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != cur.Version {
			return apperr.VersionConflict("extraction profile")
		}
		old := *cur
		cur.apply(in)
		if err := cur.validate(check); err != nil {
			return err
		}
		pag, _ := json.Marshal(cur.Pagination)
		fix, _ := json.Marshal(cur.Fixtures)
		_, err = tx.Exec(ctx, `UPDATE extraction_profiles SET name=$2, title_selectors=$3, content_selectors=$4, date_selectors=$5,
			canonical_selectors=$6, author_selectors=$7, cleanup_selectors=$8, pagination=$9, fixtures=$10, updated_by=$11, updated_at=now(),
			link_selectors=$12, version=version+1 WHERE id=$1`, id, cur.Name, cur.TitleSelectors, cur.ContentSelectors, cur.DateSelectors, cur.CanonicalSelectors,
			cur.AuthorSelectors, cur.CleanupSelectors, pag, fix, actor(ctx), cur.LinkSelectors)
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("profile_name_taken", "A profile with this name already exists.")
		}
		if err != nil {
			return err
		}
		old.Fixtures, cur.Fixtures = nil, nil // keep audit compact
		return audit.Record(ctx, tx, audit.Entry{Action: "extraction_profile.update", ResourceType: "extraction_profile", ResourceID: id.String(), Old: old, New: cur})
	})
	if err != nil {
		return nil, err
	}
	return s.GetProfile(ctx, id)
}

// EndpointColumns and ScanEndpoint are exported for modules that join on source_endpoints.
const EndpointColumns = epCols

func ScanEndpoint(row pgx.Row) (*Endpoint, error) { return scanEndpoint(row) }
