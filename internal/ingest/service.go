package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/storage"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

// Message payloads (versioned via message type names).
type PollRequested struct {
	CrawlRunID       uuid.UUID  `json:"crawl_run_id"`
	SourceEndpointID uuid.UUID  `json:"source_endpoint_id"`
	Trigger          string     `json:"trigger"`
	RequestedBy      *uuid.UUID `json:"requested_by,omitempty"`
	MaxItems         int        `json:"max_items,omitempty"`
}

type ArticleDiscovered struct {
	CrawlRunID       uuid.UUID `json:"crawl_run_id"`
	SourceEndpointID uuid.UUID `json:"source_endpoint_id"`
	PublisherID      uuid.UUID `json:"publisher_id"`
	Item             FeedItem  `json:"item"`
}

type ArticleExtracted struct {
	ArticleID   uuid.UUID `json:"article_id"`
	PublisherID uuid.UUID `json:"publisher_id"`
	Revision    int       `json:"revision"`
	Outcome     string    `json:"outcome"` // new | updated | duplicate
	Language    string    `json:"language"`
}

type EnrichmentRequested struct {
	ArticleID uuid.UUID `json:"article_id"`
	Revision  int       `json:"revision"`
	Reason    string    `json:"reason"` // new | revision | reprocess | reclassify
}

type Service struct {
	db      *db.DB
	sources *sources.Service
	fetcher *Fetcher
	store   storage.Store
	log     *slog.Logger
	now     func() time.Time
}

func NewService(d *db.DB, src *sources.Service, f *Fetcher, st storage.Store, log *slog.Logger) *Service {
	return &Service{db: d, sources: src, fetcher: f, store: st, log: log, now: time.Now}
}

func (s *Service) Fetcher() *Fetcher { return s.fetcher }

// ---------- Scheduling ----------

// ScheduleDue enqueues polls for due endpoints and advances next_poll_at atomically, so a poll
// is scheduled at most once per interval across replicas.
func (s *Service) ScheduleDue(ctx context.Context) (int, error) {
	n := 0
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+epColsAlias+` FROM source_endpoints e
			JOIN publishers p ON p.id = e.publisher_id AND p.status = 'active'
			WHERE e.status='enabled' AND (e.next_poll_at IS NULL OR e.next_poll_at <= now())
			ORDER BY e.priority, e.next_poll_at NULLS FIRST LIMIT 50 FOR UPDATE OF e SKIP LOCKED`)
		if err != nil {
			return err
		}
		var due []sources.Endpoint
		for rows.Next() {
			e, err := sources.ScanEndpoint(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, *e)
		}
		rows.Close()
		now := s.now()
		for _, e := range due {
			next := now.Add(time.Duration(e.PollIntervalSeconds) * time.Second)
			if e.EndpointType != "fixture" && !s.fetcher.LiveEnabled {
				if _, err := tx.Exec(ctx, `UPDATE source_endpoints SET next_poll_at=$2 WHERE id=$1`, e.ID, next); err != nil {
					return err
				}
				continue
			}
			if !e.CrawlWindow.Allows(now) {
				if _, err := tx.Exec(ctx, `UPDATE source_endpoints SET next_poll_at=$2 WHERE id=$1`, e.ID, now.Add(15*time.Minute)); err != nil {
					return err
				}
				continue
			}
			if _, err := s.requestPollTx(ctx, tx, &e, "schedule", nil, 0); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE source_endpoints SET next_poll_at=$2 WHERE id=$1`, e.ID, next); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

var epColsAlias = func() string {
	cols := strings.Split(sources.EndpointColumns, ",")
	for i, c := range cols {
		cols[i] = "e." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}()

func (s *Service) requestPollTx(ctx context.Context, tx pgx.Tx, e *sources.Endpoint, trigger string, by *uuid.UUID, maxItems int) (uuid.UUID, error) {
	runID := ids.New()
	if _, err := tx.Exec(ctx, `INSERT INTO source_crawl_runs (id, source_endpoint_id, trigger, status, requested_by) VALUES ($1,$2,$3,'queued',$4)`,
		runID, e.ID, trigger, by); err != nil {
		return uuid.Nil, err
	}
	_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.SourcePollRequested, AggregateType: "source_endpoint", AggregateID: e.ID.String(),
		Payload: PollRequested{CrawlRunID: runID, SourceEndpointID: e.ID, Trigger: trigger, RequestedBy: by, MaxItems: maxItems}})
	return runID, err
}

// RequestCrawl forces a bounded one-time fetch (admin action). Allowed for enabled or paused
// endpoints whose permission permits crawling.
func (s *Service) RequestCrawl(ctx context.Context, endpointID uuid.UUID, by *uuid.UUID, maxItems int) (uuid.UUID, error) {
	if maxItems <= 0 || maxItems > 200 {
		maxItems = 20
	}
	var runID uuid.UUID
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		e, err := s.sources.GetEndpointQ(ctx, tx, endpointID)
		if err != nil {
			return err
		}
		if e.Status == "archived" || e.Status == "disabled" {
			return apperr.Conflict("source_not_crawlable", "Archived or disabled sources cannot be crawled.")
		}
		perm, err := s.sources.EffectivePermission(ctx, tx, e.PublisherID, &e.ID)
		if err != nil {
			return err
		}
		if !perm.CrawlAllowed() {
			return apperr.ValidationMsg("source_permission_required", "Crawling is not permitted for this source.")
		}
		if e.EndpointType != "fixture" && !s.fetcher.LiveEnabled {
			return apperr.Unavailable("live_crawl_disabled", "Live crawling is disabled in this environment (CRAWL_LIVE_ENABLED=false).")
		}
		runID, err = s.requestPollTx(ctx, tx, e, "manual", by, maxItems)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "source.crawl_requested", ResourceType: "source_endpoint", ResourceID: e.ID.String(),
			New: map[string]any{"crawl_run_id": runID, "max_items": maxItems}})
	})
	return runID, err
}

// ---------- Polling ----------

// HandlePoll fetches and parses a feed, then emits article.discovered for new/changed items.
func (s *Service) HandlePoll(ctx context.Context, d *bus.Delivery) error {
	var msg PollRequested
	if err := d.Message.Decode(&msg); err != nil {
		return bus.PermanentErr(err)
	}
	if done, err := d.AlreadyProcessed(ctx); err != nil || done {
		return err
	}
	start := s.now()
	e, err := s.sources.GetEndpoint(ctx, msg.SourceEndpointID)
	if err != nil {
		if apperr.Is(err, "source_not_found") {
			return bus.PermanentErr(err)
		}
		return err
	}
	pub, err := s.sources.GetPublisher(ctx, e.PublisherID)
	if err != nil {
		return err
	}
	perm, err := s.sources.EffectivePermission(ctx, s.db.Pool, e.PublisherID, &e.ID)
	if err != nil {
		return err
	}
	if !perm.CrawlAllowed() || (msg.Trigger == "schedule" && e.Status != "enabled") {
		return d.Once(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE source_crawl_runs SET status='skipped', completed_at=now(), errors=$2 WHERE id=$1`,
				msg.CrawlRunID, `[{"code":"not_permitted_or_not_enabled"}]`)
			return err
		})
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE source_crawl_runs SET status='processing', started_at=now() WHERE id=$1`, msg.CrawlRunID); err != nil {
		return err
	}
	cond := &Conditional{}
	if e.ETag != nil {
		cond.ETag = *e.ETag
	}
	if e.LastModified != nil {
		cond.LastModified = *e.LastModified
	}
	if msg.Trigger != "schedule" {
		cond = nil // manual crawls bypass conditional caching
	}
	res, ferr := s.fetcher.Fetch(ctx, e, e.EntryURL, cond, maxFeedBytes, false)
	if ferr != nil {
		return s.failRun(ctx, d, e, msg, res, ferr, start)
	}
	if res.NotModified {
		return d.Once(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE source_crawl_runs SET status='succeeded', not_modified=true, http_status=$2, completed_at=now(),
				duration_ms=$3 WHERE id=$1`, msg.CrawlRunID, res.Status, int(s.now().Sub(start).Milliseconds()))
			if err != nil {
				return err
			}
			return s.markEndpointSuccess(ctx, tx, e.ID, res)
		})
	}
	feed, perr := ParseFeed(res.Body, res.FinalURL)
	if perr != nil {
		return s.failRun(ctx, d, e, msg, res, bus.PermanentErr(fmt.Errorf("parse feed: %w", perr)), start)
	}
	limit := e.MaxItemsPerPoll
	if msg.MaxItems > 0 && msg.MaxItems < limit {
		limit = msg.MaxItems
	}
	var discovered, rejected, unchanged int
	var errs []map[string]string
	return d.Once(ctx, func(tx pgx.Tx) error {
		for i, it := range feed.Items {
			if i >= limit {
				break
			}
			if it.Language == "" && len(e.Languages) == 1 {
				it.Language = e.Languages[0]
			}
			if !strings.HasPrefix(it.URL, "http://") && !strings.HasPrefix(it.URL, "https://") {
				rejected++
				errs = append(errs, map[string]string{"url": it.URL, "code": "invalid_url"})
				continue
			}
			if !sources.HostAllowed(e, pub.CanonicalDomain, it.URL) {
				rejected++
				errs = append(errs, map[string]string{"url": it.URL, "code": "host_not_allowed"})
				continue
			}
			if blocked, why, err := s.sources.IsBlocked(ctx, it.URL, e.URLBlocklist); err != nil {
				return err
			} else if blocked {
				rejected++
				errs = append(errs, map[string]string{"url": it.URL, "code": why})
				continue
			}
			canon, err := CanonicalizeURL(it.URL)
			if err != nil {
				rejected++
				continue
			}
			// Skip unchanged known items cheaply (no page fetch) unless the feed reports an update.
			var existingUpdated *time.Time
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT true, COALESCE(modified_at, published_at) FROM articles WHERE canonical_url_hash=$1`,
				crypto.SHA256([]byte(canon))).Scan(&exists, &existingUpdated); err != nil && !db.IsNoRows(err) {
				return err
			}
			if exists && (it.UpdatedAt == nil || (existingUpdated != nil && !it.UpdatedAt.After(*existingUpdated))) && msg.Trigger != "reprocess" {
				unchanged++
				continue
			}
			if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleDiscovered, AggregateType: "article_url", AggregateID: canon,
				Payload: ArticleDiscovered{CrawlRunID: msg.CrawlRunID, SourceEndpointID: e.ID, PublisherID: e.PublisherID, Item: it}}); err != nil {
				return err
			}
			discovered++
		}
		eb, _ := json.Marshal(capErrors(errs))
		if _, err := tx.Exec(ctx, `UPDATE source_crawl_runs SET status='succeeded', http_status=$2, discovered_count=$3, rejected_count=$4,
			duplicate_count=$5, errors=$6, completed_at=now(), duration_ms=$7 WHERE id=$1`,
			msg.CrawlRunID, res.Status, discovered, rejected, unchanged, eb, int(s.now().Sub(start).Milliseconds())); err != nil {
			return err
		}
		if err := s.markEndpointSuccess(ctx, tx, e.ID, res); err != nil {
			return err
		}
		return refreshHealth(ctx, tx, e.ID, res.Status)
	})
}

func capErrors(in []map[string]string) []map[string]string {
	if len(in) > 50 {
		return in[:50]
	}
	if in == nil {
		return []map[string]string{}
	}
	return in
}

func (s *Service) markEndpointSuccess(ctx context.Context, tx pgx.Tx, id uuid.UUID, res *FetchResult) error {
	_, err := tx.Exec(ctx, `UPDATE source_endpoints SET consecutive_failures=0, health_state='healthy', last_success_at=now(), last_error=NULL,
		etag=COALESCE(NULLIF($2,''), etag), last_modified=COALESCE(NULLIF($3,''), last_modified) WHERE id=$1`, id, res.ETag, res.LastModified)
	return err
}

func (s *Service) failRun(ctx context.Context, d *bus.Delivery, e *sources.Endpoint, msg PollRequested, res *FetchResult, ferr error, start time.Time) error {
	status := 0
	if res != nil {
		status = res.Status
	}
	transient := true
	var perm *PermanentHTTPError
	var permBus *bus.Permanent
	if errors.As(ferr, &perm) || errors.As(ferr, &permBus) || errors.Is(ferr, ErrRobotsDisallowed) || errors.Is(ferr, ErrLiveDisabled) {
		transient = false
	}
	code := "fetch_failed"
	switch {
	case errors.Is(ferr, ErrRobotsDisallowed):
		code = "robots_disallowed"
	case errors.Is(ferr, ErrLiveDisabled):
		code = "live_crawl_disabled"
	case strings.Contains(ferr.Error(), "SSRF"):
		code = "ssrf_blocked"
	}
	eb, _ := json.Marshal([]map[string]string{{"code": code, "message": truncate(ferr.Error(), 300)}})
	err := d.Once(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE source_crawl_runs SET status='failed', http_status=NULLIF($2,0), error_count=1, errors=$3,
			completed_at=now(), duration_ms=$4 WHERE id=$1`, msg.CrawlRunID, status, eb, int(s.now().Sub(start).Milliseconds())); err != nil {
			return err
		}
		// Exponential backoff on repeated failures: interval * 2^failures, capped at 24h.
		fails := e.ConsecutiveFailures + 1
		backoff := time.Duration(float64(e.PollIntervalSeconds)*math.Pow(2, float64(min(fails, 6)))) * time.Second
		if backoff > 24*time.Hour {
			backoff = 24 * time.Hour
		}
		state := "degraded"
		if fails >= 3 {
			state = "failing"
		}
		if _, err := tx.Exec(ctx, `UPDATE source_endpoints SET consecutive_failures=$2, health_state=$3, last_error=$4,
			next_poll_at = now() + $5::interval WHERE id=$1`, e.ID, fails, state, truncate(code+": "+ferr.Error(), 500),
			fmt.Sprintf("%d seconds", int(backoff.Seconds()))); err != nil {
			return err
		}
		return refreshHealth(ctx, tx, e.ID, status)
	})
	if err != nil {
		return err
	}
	if transient {
		s.log.Warn("source poll failed", "source_endpoint_id", e.ID, "error", ferr)
	}
	return nil // failure recorded; scheduler retries with backoff
}

func refreshHealth(ctx context.Context, tx pgx.Tx, id uuid.UUID, lastStatus int) error {
	_, err := tx.Exec(ctx, `INSERT INTO source_health (source_endpoint_id, last_http_status, runs_24h, failed_runs_24h, parse_coverage, duplicate_rate, freshness_lag_seconds, anomalies, updated_at)
		SELECT $1, NULLIF($2,0),
		  count(*) FILTER (WHERE r.started_at > now() - interval '24 hours'),
		  count(*) FILTER (WHERE r.started_at > now() - interval '24 hours' AND r.status='failed'),
		  CASE WHEN sum(r.fetched_count) > 0 THEN sum(r.accepted_count + r.duplicate_count)::real / sum(r.fetched_count) END,
		  CASE WHEN sum(r.discovered_count + r.duplicate_count) > 0 THEN sum(r.duplicate_count)::real / sum(r.discovered_count + r.duplicate_count) END,
		  (SELECT extract(epoch FROM now() - max(published_at))::bigint FROM articles WHERE source_endpoint_id=$1),
		  CASE WHEN count(*) FILTER (WHERE r.started_at > now() - interval '24 hours' AND r.status='failed') >= 3
		       THEN '["repeated_failures_24h"]'::jsonb ELSE '[]'::jsonb END,
		  now()
		FROM source_crawl_runs r WHERE r.source_endpoint_id=$1 AND r.started_at > now() - interval '7 days'
		ON CONFLICT (source_endpoint_id) DO UPDATE SET last_http_status=EXCLUDED.last_http_status, runs_24h=EXCLUDED.runs_24h,
		  failed_runs_24h=EXCLUDED.failed_runs_24h, parse_coverage=EXCLUDED.parse_coverage, duplicate_rate=EXCLUDED.duplicate_rate,
		  freshness_lag_seconds=EXCLUDED.freshness_lag_seconds, anomalies=EXCLUDED.anomalies, updated_at=now()`, id, lastStatus)
	return err
}

// ---------- Article processing ----------

// HandleDiscovered fetches (if configured), extracts, deduplicates and persists one article.
func (s *Service) HandleDiscovered(ctx context.Context, d *bus.Delivery) error {
	var msg ArticleDiscovered
	if err := d.Message.Decode(&msg); err != nil {
		return bus.PermanentErr(err)
	}
	if done, err := d.AlreadyProcessed(ctx); err != nil || done {
		return err
	}
	e, err := s.sources.GetEndpoint(ctx, msg.SourceEndpointID)
	if err != nil {
		return bus.PermanentErr(err)
	}
	perm, err := s.sources.EffectivePermission(ctx, s.db.Pool, e.PublisherID, &e.ID)
	if err != nil {
		return err
	}
	if !perm.CrawlAllowed() {
		return d.Once(ctx, func(tx pgx.Tx) error { return bumpRun(ctx, tx, msg.CrawlRunID, "rejected_count") })
	}
	var prof *sources.ExtractionProfile
	if e.ExtractionProfileID != nil {
		prof, _ = s.sources.GetProfile(ctx, *e.ExtractionProfileID)
	}
	cand, raw, err := s.buildCandidate(ctx, e, prof, msg.Item)
	if err != nil {
		var ph *PermanentHTTPError
		if errors.As(err, &ph) || errors.Is(err, ErrRobotsDisallowed) || errors.Is(err, ErrLiveDisabled) {
			return d.Once(ctx, func(tx pgx.Tx) error {
				if err := bumpRun(ctx, tx, msg.CrawlRunID, "rejected_count"); err != nil {
					return err
				}
				return appendRunError(ctx, tx, msg.CrawlRunID, msg.Item.URL, err.Error())
			})
		}
		if d.Attempt >= 3 {
			return bus.PermanentErr(err)
		}
		return err
	}
	cand.PublisherID, cand.EndpointID = e.PublisherID, &e.ID
	cand.Permission = perm
	if raw != nil && perm.AllowStoreRaw && s.store != nil {
		key := fmt.Sprintf("raw/%s/%s/%x.html", e.PublisherID, s.now().UTC().Format("2006/01/02"), crypto.SHA256([]byte(cand.CanonicalURL))[:12])
		if err := s.store.Put(ctx, key, raw, "text/html"); err != nil {
			return err
		}
		cand.RawObjectKey = &key
	}
	return d.Once(ctx, func(tx pgx.Tx) error {
		res, err := s.PersistArticle(ctx, tx, cand)
		if err != nil {
			return err
		}
		col := map[string]string{"new": "accepted_count", "updated": "accepted_count", "unchanged": "duplicate_count", "duplicate": "duplicate_count",
			"quarantined": "rejected_count"}[res.Outcome]
		if err := bumpRun(ctx, tx, msg.CrawlRunID, "fetched_count"); err != nil {
			return err
		}
		return bumpRun(ctx, tx, msg.CrawlRunID, col)
	})
}

func bumpRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID, col string) error {
	switch col {
	case "fetched_count", "accepted_count", "duplicate_count", "rejected_count", "error_count":
	default:
		return fmt.Errorf("invalid counter %q", col)
	}
	_, err := tx.Exec(ctx, `UPDATE source_crawl_runs SET `+col+` = `+col+` + 1 WHERE id=$1`, runID)
	return err
}

func appendRunError(ctx context.Context, tx pgx.Tx, runID uuid.UUID, url, msg string) error {
	b, _ := json.Marshal([]map[string]string{{"url": url, "message": truncate(msg, 300)}})
	_, err := tx.Exec(ctx, `UPDATE source_crawl_runs SET errors = CASE WHEN jsonb_array_length(errors) < 50 THEN errors || $2::jsonb ELSE errors END,
		error_count = error_count + 1 WHERE id=$1`, runID, b)
	return err
}

// Candidate is a normalized article ready for persistence.
type Candidate struct {
	PublisherID  uuid.UUID
	EndpointID   *uuid.UUID
	OriginalURL  string
	CanonicalURL string
	Title        string
	Byline       string
	Description  string
	Text         string
	Language     string
	LanguageConf float64
	PublishedAt  *time.Time
	ModifiedAt   *time.Time
	Confidence   float64
	ContentType  string
	Tags         []string
	RawObjectKey *string
	Permission   *sources.Permission
	Diagnostics  *Extracted
}

const maxBodyChars = 100_000

func (s *Service) buildCandidate(ctx context.Context, e *sources.Endpoint, prof *sources.ExtractionProfile, it FeedItem) (*Candidate, []byte, error) {
	c := &Candidate{OriginalURL: it.URL, Title: it.Title, Byline: it.Author, Description: it.Summary, PublishedAt: it.PublishedAt,
		ModifiedAt: it.UpdatedAt, Tags: it.Categories, Language: it.Language}
	var raw []byte
	var ex *Extracted
	if e.FetchArticlePages {
		res, err := s.fetcher.Fetch(ctx, e, it.URL, nil, maxPageBytes, false)
		if err != nil {
			return nil, nil, err
		}
		raw = res.Body
		ex, err = ExtractHTML(res.Body, res.FinalURL, prof)
		if err != nil {
			return nil, nil, &PermanentHTTPError{Status: res.Status, Reason: err.Error()}
		}
	} else if it.ContentHTML != "" {
		var err error
		ex, err = ExtractHTML([]byte("<html><body><article>"+it.ContentHTML+"</article></body></html>"), it.URL, prof)
		if err != nil {
			return nil, nil, &PermanentHTTPError{Status: 200, Reason: err.Error()}
		}
	}
	if ex != nil {
		c.Diagnostics = ex
		if ex.Title != "" && (c.Title == "" || e.FetchArticlePages) {
			c.Title = ex.Title
		}
		if ex.Byline != "" {
			c.Byline = ex.Byline
		}
		if ex.Description != "" && c.Description == "" {
			c.Description = ex.Description
		}
		c.Text = ex.Text
		if ex.PublishedAt != nil && c.PublishedAt == nil {
			c.PublishedAt = ex.PublishedAt
		}
		if ex.ModifiedAt != nil {
			c.ModifiedAt = ex.ModifiedAt
		}
		c.Confidence = ex.Confidence
		if ex.CanonicalURL != "" && SameSite(ex.CanonicalURL, it.URL) {
			c.CanonicalURL = ex.CanonicalURL
		}
	}
	if c.CanonicalURL == "" {
		c.CanonicalURL = it.URL
	}
	canon, err := CanonicalizeURL(c.CanonicalURL)
	if err != nil {
		return nil, nil, &PermanentHTTPError{Status: 0, Reason: err.Error()}
	}
	c.CanonicalURL = canon
	c.Title = nlp.NormalizeText(c.Title)
	if c.Title == "" {
		return nil, nil, &PermanentHTTPError{Status: 0, Reason: "no title extracted"}
	}
	if len([]rune(c.Text)) > maxBodyChars {
		c.Text = string([]rune(c.Text)[:maxBodyChars])
	}
	lang, conf := nlp.DetectLanguage(c.Title + ". " + c.Description + " " + c.Text)
	if lang != "und" && (c.Language == "" || conf >= 0.75) {
		c.Language, c.LanguageConf = lang, conf
	} else if c.Language == "" {
		c.Language = "und"
	}
	c.ContentType = classifyContentType(c.Title, it.URL, it.Categories)
	return c, raw, nil
}

func classifyContentType(title, u string, cats []string) string {
	l := strings.ToLower(title + " " + u + " " + strings.Join(cats, " "))
	switch {
	case strings.Contains(l, "satir") || strings.Contains(l, "сатир") || strings.Contains(l, "/umor") || strings.Contains(l, "pamflet"):
		return "satire"
	case strings.Contains(l, "opinie") || strings.Contains(l, "/opinii") || strings.Contains(l, "editorial") || strings.Contains(l, "мнение") || strings.Contains(l, "колонка"):
		return "opinion"
	case strings.Contains(l, "comunicat") || strings.Contains(l, "press release") || strings.Contains(l, "пресс-релиз"):
		return "press_release"
	case strings.Contains(l, "analiz") || strings.Contains(l, "аналитик"):
		return "analysis"
	}
	return "news"
}

var correctionMarkers = []string{"corecție", "rectificare", "erată", "actualizare:", "update:", "исправление", "уточнение", "опровержение"}

// PersistResult reports the deduplication outcome.
type PersistResult struct {
	ArticleID   uuid.UUID  `json:"article_id"`
	Outcome     string     `json:"outcome"` // new | updated | unchanged | duplicate | quarantined
	DuplicateOf *uuid.UUID `json:"duplicate_of,omitempty"`
	Kind        string     `json:"duplicate_kind,omitempty"`
	Revision    int        `json:"revision"`
}

// PersistArticle applies dedupe rules and writes the article + revision + outbox events in tx.
func (s *Service) PersistArticle(ctx context.Context, tx pgx.Tx, c *Candidate) (*PersistResult, error) {
	urlHash := crypto.SHA256([]byte(c.CanonicalURL))
	textHash := nlp.ContentHash(c.Title, c.Text)
	sim := int64(nlp.SimHash(c.Title + " " + c.Text))
	perm := c.Permission
	storeText := perm != nil && perm.AllowStoreFullText
	body := c.Text
	if !storeText {
		// Without full-text permission keep only a permitted snippet for enrichment/attribution.
		max := 0
		if perm != nil && perm.AllowSnippets {
			max = perm.MaxSnippetChars
		}
		body = nlp.Truncate(c.Text, max)
		if max == 0 {
			body = ""
		}
	}
	var bodyPtr *string
	if body != "" {
		bodyPtr = &body
	}
	quarantine := looksSuspicious(c)

	// 1) Same canonical URL → revision or unchanged.
	var existingID uuid.UUID
	var existingHash []byte
	var revision int
	var status string
	err := tx.QueryRow(ctx, `SELECT id, text_hash, revision, status FROM articles WHERE canonical_url_hash=$1 FOR UPDATE`, urlHash).
		Scan(&existingID, &existingHash, &revision, &status)
	if err == nil {
		if crypto.Equal(existingHash, textHash) || status == "withdrawn" {
			return &PersistResult{ArticleID: existingID, Outcome: "unchanged", Revision: revision}, nil
		}
		revision++
		kind := "update"
		isCorrection := false
		lt := strings.ToLower(c.Title + " " + nlp.Truncate(c.Text, 400))
		for _, m := range correctionMarkers {
			if strings.Contains(lt, m) {
				kind, isCorrection = "correction", true
				break
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE articles SET title=$2, byline=$3, description=$4, body_text=$5, body_chars=$6, text_hash=$7, simhash=$8,
			modified_at=COALESCE($9, now()), fetched_at=now(), revision=$10, is_correction = is_correction OR $11, status='extracted',
			extraction_confidence=$12, language=$13, language_confidence=$14, updated_at=now(), version=version+1, raw_object_key=COALESCE($15, raw_object_key)
			WHERE id=$1`, existingID, c.Title, c.Byline, c.Description, bodyPtr, len([]rune(c.Text)), textHash, sim, c.ModifiedAt, revision,
			isCorrection, c.Confidence, c.Language, c.LanguageConf, c.RawObjectKey); err != nil {
			return nil, err
		}
		if err := insertVersion(ctx, tx, existingID, revision, c.Title, bodyPtr, textHash, kind); err != nil {
			return nil, err
		}
		if err := s.emitExtracted(ctx, tx, existingID, c, revision, "updated", "revision"); err != nil {
			return nil, err
		}
		return &PersistResult{ArticleID: existingID, Outcome: "updated", Revision: revision}, nil
	}
	if !db.IsNoRows(err) {
		return nil, err
	}

	id := ids.New()
	res := &PersistResult{ArticleID: id, Outcome: "new", Revision: 1}
	// 2) Exact content elsewhere, 3) near-duplicate by SimHash within 7 days.
	var dupOf uuid.UUID
	var dupPub uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id, publisher_id FROM articles WHERE text_hash=$1 AND status <> 'withdrawn' ORDER BY first_seen_at LIMIT 1`, textHash).Scan(&dupOf, &dupPub)
	if err == nil {
		res.Outcome, res.DuplicateOf = "duplicate", &dupOf
		res.Kind = "exact"
		if dupPub != c.PublisherID {
			res.Kind = "syndicated"
		}
	} else if db.IsNoRows(err) && len([]rune(c.Text)) >= 300 {
		err = tx.QueryRow(ctx, `SELECT id, publisher_id FROM articles
			WHERE first_seen_at > now() - interval '7 days' AND status <> 'withdrawn' AND body_chars >= 300
			  AND bit_count((simhash # $1)::bit(64)) <= 3
			ORDER BY first_seen_at LIMIT 1`, sim).Scan(&dupOf, &dupPub)
		if err == nil {
			res.Outcome, res.DuplicateOf, res.Kind = "duplicate", &dupOf, "near"
			if dupPub != c.PublisherID {
				res.Kind = "syndicated"
			}
		} else if !db.IsNoRows(err) {
			return nil, err
		}
	} else if err != nil && !db.IsNoRows(err) {
		return nil, err
	}
	status = "extracted"
	if res.Outcome == "duplicate" {
		status = "duplicate"
	} else if quarantine != "" {
		status, res.Outcome = "quarantined", "quarantined"
	}
	enrichment, _ := json.Marshal(map[string]any{"extraction": map[string]any{"selectors": diagSelectors(c), "warnings": diagWarnings(c)},
		"quarantine_reason": quarantine})
	_, err = tx.Exec(ctx, `INSERT INTO articles (id, publisher_id, source_endpoint_id, canonical_url, canonical_url_hash, original_url, language,
		language_confidence, title, byline, description, body_text, body_chars, raw_object_key, text_hash, simhash, content_type, published_at,
		modified_at, extractor_version, extraction_confidence, status, duplicate_of, duplicate_kind, tags, enrichment)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		id, c.PublisherID, c.EndpointID, c.CanonicalURL, urlHash, c.OriginalURL, c.Language, c.LanguageConf, c.Title, c.Byline, c.Description,
		bodyPtr, len([]rune(c.Text)), c.RawObjectKey, textHash, sim, c.ContentType, c.PublishedAt, c.ModifiedAt, ExtractorVersion, c.Confidence,
		status, res.DuplicateOf, nullable(res.Kind), nonNil(c.Tags), enrichment)
	if db.IsUniqueViolation(err) {
		return nil, fmt.Errorf("concurrent insert of %s: %w", c.CanonicalURL, err) // retried; next attempt sees the row
	}
	if err != nil {
		return nil, err
	}
	if err := insertVersion(ctx, tx, id, 1, c.Title, bodyPtr, textHash, "initial"); err != nil {
		return nil, err
	}
	reason := "new"
	if res.Outcome == "duplicate" || res.Outcome == "quarantined" {
		reason = "" // duplicates are provenance only; quarantined await review
	}
	if err := s.emitExtracted(ctx, tx, id, c, 1, res.Outcome, reason); err != nil {
		return nil, err
	}
	return res, nil
}

// looksSuspicious flags content that should not flow into AI processing automatically.
func looksSuspicious(c *Candidate) string {
	l := strings.ToLower(c.Text)
	for _, m := range []string{"ignore previous instructions", "ignore all previous", "ignoră instrucțiunile", "игнорируй предыдущие инструкции",
		"system prompt", "you are chatgpt", "### instruction"} {
		if strings.Contains(l, m) {
			return "possible_prompt_injection"
		}
	}
	if len([]rune(c.Text)) > 0 && len([]rune(c.Text)) < 40 && c.Description == "" {
		return "insufficient_content"
	}
	return ""
}

func diagSelectors(c *Candidate) map[string]string {
	if c.Diagnostics == nil {
		return map[string]string{}
	}
	return c.Diagnostics.SelectorsUsed
}

func diagWarnings(c *Candidate) []string {
	if c.Diagnostics == nil {
		return []string{}
	}
	return c.Diagnostics.Warnings
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func insertVersion(ctx context.Context, tx pgx.Tx, articleID uuid.UUID, rev int, title string, body *string, hash []byte, kind string) error {
	_, err := tx.Exec(ctx, `INSERT INTO article_versions (id, article_id, revision, title, body_text, text_hash, change_kind) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		ids.New(), articleID, rev, title, body, hash, kind)
	return err
}

func (s *Service) emitExtracted(ctx context.Context, tx pgx.Tx, id uuid.UUID, c *Candidate, rev int, outcome, enrichReason string) error {
	if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleExtracted, AggregateType: "article", AggregateID: id.String(),
		Payload: ArticleExtracted{ArticleID: id, PublisherID: c.PublisherID, Revision: rev, Outcome: outcome, Language: c.Language}}); err != nil {
		return err
	}
	if enrichReason == "" {
		return nil
	}
	_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleEnrichmentRequested, AggregateType: "article", AggregateID: id.String(),
		Payload: EnrichmentRequested{ArticleID: id, Revision: rev, Reason: enrichReason}})
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------- Reprocessing ----------

const JobReprocess = "articles.reprocess"

type ReprocessRequest struct {
	SourceEndpointID *uuid.UUID  `json:"source_endpoint_id,omitempty"`
	PublisherID      *uuid.UUID  `json:"publisher_id,omitempty"`
	ArticleIDs       []uuid.UUID `json:"article_ids,omitempty"`
	Since            *time.Time  `json:"since,omitempty"`
	Until            *time.Time  `json:"until,omitempty"`
	Reason           string      `json:"reason"`
	Limit            int         `json:"limit,omitempty"`
}

// RequestReprocess enqueues a bounded reprocessing job (selective source/date window).
func (s *Service) RequestReprocess(ctx context.Context, in ReprocessRequest, by *uuid.UUID) (uuid.UUID, error) {
	if in.Limit <= 0 || in.Limit > 20000 {
		in.Limit = 2000
	}
	if in.Reason == "" {
		return uuid.Nil, apperr.Validation(apperr.FieldError{Field: "reason", Code: "required", Message: "A reason is required for audit."})
	}
	if in.Since != nil && in.Until != nil && in.Until.Before(*in.Since) {
		return uuid.Nil, apperr.Validation(apperr.FieldError{Field: "until", Code: "invalid", Message: "until must be after since."})
	}
	var id uuid.UUID
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		id, err = jobs.Enqueue(ctx, tx, JobReprocess, in, jobs.EnqueueOpts{CreatedBy: by, Priority: 200})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "articles.reprocess_requested", ResourceType: "job", ResourceID: id.String(), New: in})
	})
	return id, err
}

// RunReprocess is the job handler: re-emits enrichment requests for matching articles.
func (s *Service) RunReprocess(ctx context.Context, j *jobs.Job) (any, error) {
	var in ReprocessRequest
	if err := json.Unmarshal(j.Payload, &in); err != nil {
		return nil, jobs.Permanent(err)
	}
	n := 0
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, revision FROM articles
			WHERE status IN ('extracted','enriched','failed','enriching')
			  AND ($1::uuid IS NULL OR source_endpoint_id=$1) AND ($2::uuid IS NULL OR publisher_id=$2)
			  AND (cardinality($3::uuid[]) = 0 OR id = ANY($3))
			  AND ($4::timestamptz IS NULL OR first_seen_at >= $4) AND ($5::timestamptz IS NULL OR first_seen_at < $5)
			ORDER BY first_seen_at LIMIT $6`, in.SourceEndpointID, in.PublisherID, nonNilUUIDs(in.ArticleIDs), in.Since, in.Until, in.Limit)
		if err != nil {
			return err
		}
		type ar struct {
			id  uuid.UUID
			rev int
		}
		var list []ar
		for rows.Next() {
			var a ar
			if err := rows.Scan(&a.id, &a.rev); err != nil {
				rows.Close()
				return err
			}
			list = append(list, a)
		}
		rows.Close()
		for _, a := range list {
			if _, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.ArticleEnrichmentRequested, AggregateType: "article", AggregateID: a.id.String(),
				Payload: EnrichmentRequested{ArticleID: a.id, Revision: a.rev, Reason: "reprocess"}}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return map[string]int{"requeued": n}, err
}

func nonNilUUIDs(in []uuid.UUID) []uuid.UUID {
	if in == nil {
		return []uuid.UUID{}
	}
	return in
}
