package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
)

// CrawlingStatus is the operator's overview of live crawling.
type CrawlingStatus struct {
	// LiveFetchAllowed mirrors CRAWL_LIVE_ENABLED: without it no real site is ever fetched.
	LiveFetchAllowed bool `json:"live_fetch_allowed"`
	// FlagEnabled is the administrator's start/stop switch (feature flag live_crawling).
	FlagEnabled bool `json:"flag_enabled"`
	// Scheduling is true when enabled real sources are polled on their interval.
	Scheduling bool `json:"scheduling"`
	Sources    struct {
		Total    int `json:"total"`
		Enabled  int `json:"enabled"`
		Paused   int `json:"paused"`
		Disabled int `json:"disabled"`
		Pending  int `json:"pending_permission"`
		Failing  int `json:"failing"`
		Degraded int `json:"degraded"`
	} `json:"sources"`
	DueNow          int         `json:"due_now"`
	RunsLastHour    int         `json:"runs_last_hour"`
	AcceptedLast24h int         `json:"articles_accepted_last_24h"`
	ArticlesLast24h int         `json:"articles_last_24h"`
	PipelinePending int         `json:"pipeline_pending"`
	LastRunAt       *time.Time  `json:"last_run_at,omitempty"`
	RecentRuns      []RecentRun `json:"recent_runs"`
	Note            string      `json:"note,omitempty"`
}

// RecentRun is one crawl run with its source name.
type RecentRun struct {
	ID         string     `json:"id"`
	SourceID   string     `json:"source_endpoint_id"`
	Source     string     `json:"source"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"completed_at,omitempty"`
	Discovered int        `json:"discovered_count"`
	Accepted   int        `json:"accepted_count"`
	Duplicates int        `json:"duplicate_count"`
	Rejected   int        `json:"rejected_count"`
	Errors     int        `json:"error_count"`
}

type CrawlingSwitch struct {
	Enabled bool `json:"enabled"`
}

type PresetImportRequest struct {
	LegalBasis      string   `json:"legal_basis"`
	Enable          bool     `json:"enable"`
	Only            []string `json:"only,omitempty"`
	MaxItemsPerPoll int      `json:"max_items_per_poll,omitempty"`
}

type RunAllAccepted struct {
	Scheduled int    `json:"scheduled"`
	Note      string `json:"note,omitempty"`
}

func (a *API) crawlingStatus(ctx context.Context) (*CrawlingStatus, error) {
	st := &CrawlingStatus{LiveFetchAllowed: a.Cfg.CrawlLiveEnabled, RecentRuns: []RecentRun{}}
	_ = a.DB.Pool.QueryRow(ctx, `SELECT coalesce((SELECT enabled AND rollout_percent > 0 FROM feature_flags WHERE key=$1), false)`,
		ingest.LiveSchedulingFlag).Scan(&st.FlagEnabled)
	st.Scheduling = st.LiveFetchAllowed && st.FlagEnabled
	err := a.DB.Pool.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE status='enabled'), count(*) FILTER (WHERE status='paused'),
			count(*) FILTER (WHERE status='disabled'), count(*) FILTER (WHERE status='pending_permission'),
			count(*) FILTER (WHERE status='enabled' AND health_state='failing'), count(*) FILTER (WHERE status='enabled' AND health_state='degraded'),
			count(*) FILTER (WHERE status='enabled' AND (next_poll_at IS NULL OR next_poll_at <= now()))
		FROM source_endpoints WHERE endpoint_type <> 'fixture' AND status <> 'archived'`).Scan(
		&st.Sources.Total, &st.Sources.Enabled, &st.Sources.Paused, &st.Sources.Disabled, &st.Sources.Pending,
		&st.Sources.Failing, &st.Sources.Degraded, &st.DueNow)
	if err != nil {
		return nil, err
	}
	err = a.DB.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM source_crawl_runs WHERE started_at > now() - interval '1 hour'),
			(SELECT coalesce(sum(accepted_count),0) FROM source_crawl_runs WHERE started_at > now() - interval '24 hours'),
			(SELECT count(*) FROM articles WHERE first_seen_at > now() - interval '24 hours'),
			(SELECT count(*) FROM articles WHERE status NOT IN ('enriched','duplicate','quarantined','failed','withdrawn')),
			(SELECT max(started_at) FROM source_crawl_runs)`).Scan(&st.RunsLastHour, &st.AcceptedLast24h, &st.ArticlesLast24h, &st.PipelinePending, &st.LastRunAt)
	if err != nil {
		return nil, err
	}
	rows, err := a.DB.Pool.Query(ctx, `SELECT r.id::text, r.source_endpoint_id::text, e.name, r.trigger, r.status, r.started_at, r.completed_at,
			r.discovered_count, r.accepted_count, r.duplicate_count, r.rejected_count, r.error_count
		FROM source_crawl_runs r JOIN source_endpoints e ON e.id = r.source_endpoint_id
		ORDER BY r.started_at DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	st.RecentRuns, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecentRun, error) {
		var x RecentRun
		err := r.Scan(&x.ID, &x.SourceID, &x.Source, &x.Trigger, &x.Status, &x.StartedAt, &x.FinishedAt, &x.Discovered, &x.Accepted, &x.Duplicates, &x.Rejected, &x.Errors)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	switch {
	case !st.LiveFetchAllowed:
		st.Note = "CRAWL_LIVE_ENABLED is false: real sites are never fetched. Set it to true in .env and restart api and worker."
	case !st.FlagEnabled:
		st.Note = "Scheduled crawling is stopped. Manual \"crawl now\" still works."
	case st.Sources.Enabled == 0:
		st.Note = "No real source is enabled. Import a preset or add a source, then enable it."
	}
	return st, nil
}

// adminCrawling mounts the crawling overview, the global start/stop switch and source presets.
func (a *API) adminCrawling(r chi.Router, write func(http.HandlerFunc) http.HandlerFunc) {
	r.Get("/admin/crawling", h(func(r *http.Request) (int, any, error) {
		st, err := a.crawlingStatus(r.Context())
		return 200, st, err
	}))
	r.Put("/admin/crawling", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[CrawlingSwitch](r)
		if err != nil {
			return 0, nil, err
		}
		pct := 0
		if in.Enabled {
			pct = 100
		}
		if err := a.Policy.SetFlag(r.Context(), ingest.LiveSchedulingFlag, in.Enabled, pct); err != nil {
			return 0, nil, err
		}
		st, err := a.crawlingStatus(r.Context())
		return 200, st, err
	})))
	r.Post("/admin/crawling/run-now", write(h(func(r *http.Request) (int, any, error) {
		if !a.Cfg.CrawlLiveEnabled {
			return 0, nil, apperr.Conflict("live_crawling_disabled", "CRAWL_LIVE_ENABLED is false: real sites are never fetched.")
		}
		var out RunAllAccepted
		err := a.DB.Tx(r.Context(), func(tx pgx.Tx) error {
			tag, err := tx.Exec(r.Context(), `UPDATE source_endpoints SET next_poll_at = now() WHERE status='enabled' AND endpoint_type <> 'fixture'`)
			if err != nil {
				return err
			}
			out.Scheduled = int(tag.RowsAffected())
			return audit.Record(r.Context(), tx, audit.Entry{Action: "crawling.run_now", ResourceType: "source_endpoint", ResourceID: "*"})
		})
		if err == nil && !a.Ingest.LiveScheduling(r.Context()) {
			out.Note = "Scheduled crawling is stopped; start it so the worker picks these sources up."
		}
		return 202, out, err
	})))
	r.Get("/admin/source-presets", h(func(r *http.Request) (int, any, error) {
		l, err := a.Sources.Presets(r.Context(), a.Cfg.SourcePresetsDir)
		return 200, list(l), err
	}))
	r.Post("/admin/source-presets/{name}/import", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[PresetImportRequest](r)
		if err != nil {
			return 0, nil, err
		}
		if strings.TrimSpace(in.LegalBasis) == "" {
			return 0, nil, apperr.Validation(apperr.FieldError{Field: "legal_basis", Code: "required",
				Message: "State the permission or legal basis for crawling these publishers; it is recorded on each permission."})
		}
		m, err := sources.LoadPreset(a.Cfg.SourcePresetsDir, chi.URLParam(r, "name"))
		if err != nil {
			return 0, nil, err
		}
		res := a.Sources.Import(r.Context(), m.Sources, sources.ImportOptions{LegalBasis: strings.TrimSpace(in.LegalBasis), Enable: in.Enable,
			Only: in.Only, MaxItemsPerPoll: in.MaxItemsPerPoll})
		return 200, list(res), nil
	})))
}
