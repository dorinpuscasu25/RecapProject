package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/identity"
	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

// ---------- Taxonomy (consumer) ----------

func (a *API) consumerCategories(r chi.Router) {
	r.Get("/categories", h(func(r *http.Request) (int, any, error) {
		tree, err := a.Taxonomy.PublicTree(r.Context())
		if err != nil {
			return 0, nil, err
		}
		var flat []*taxonomy.PublicCategory
		var walk func([]*taxonomy.PublicCategory)
		walk = func(cs []*taxonomy.PublicCategory) {
			for _, c := range cs {
				cp := *c
				cp.Children = nil
				flat = append(flat, &cp)
				walk(c.Children)
			}
		}
		walk(tree)
		return 200, list(flat), nil
	}))
	r.Get("/categories/tree", h(func(r *http.Request) (int, any, error) {
		tree, err := a.Taxonomy.PublicTree(r.Context())
		if err != nil {
			return 0, nil, err
		}
		ver, _ := a.Taxonomy.CurrentVersion(r.Context())
		return 200, map[string]any{"taxonomy_version": ver, "items": tree}, nil
	}))
}

// ---------- Taxonomy (admin) ----------

func (a *API) adminCategories(r chi.Router) {
	r.Get("/admin/categories", h(func(r *http.Request) (int, any, error) {
		all, ver, err := a.Taxonomy.All(r.Context())
		if err != nil {
			return 0, nil, err
		}
		return 200, map[string]any{"taxonomy_version": ver, "items": taxonomy.Tree(all)}, nil
	}))
	r.Post("/admin/categories", h(func(r *http.Request) (int, any, error) {
		in, err := decode[taxonomy.Input](r)
		if err != nil {
			return 0, nil, err
		}
		c, err := a.Taxonomy.Create(r.Context(), in)
		return 201, c, err
	}))
	r.Get("/admin/categories/{id}", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "category")
		if err != nil {
			return 0, nil, err
		}
		c, err := a.Taxonomy.Get(r.Context(), id)
		return 200, c, err
	}))
	r.Patch("/admin/categories/{id}", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "category")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[taxonomy.Input](r)
		if err != nil {
			return 0, nil, err
		}
		c, err := a.Taxonomy.Update(r.Context(), id, in)
		return 200, c, err
	}))
	for _, st := range []string{"activate", "hide", "archive"} {
		status := map[string]string{"activate": "active", "hide": "hidden", "archive": "archived"}[st]
		r.Post("/admin/categories/{id}/"+st, h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "category")
			if err != nil {
				return 0, nil, err
			}
			c, err := a.Taxonomy.Update(r.Context(), id, taxonomy.Input{Status: &status})
			return 200, c, err
		}))
	}
	r.Post("/admin/categories/{id}/reparent", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "category")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[struct {
			ParentID *uuid.UUID `json:"parent_id"`
			Version  *int       `json:"version,omitempty"`
		}](r)
		if err != nil {
			return 0, nil, err
		}
		upd := taxonomy.Input{ParentID: in.ParentID, ClearParent: in.ParentID == nil, Version: in.Version}
		c, err := a.Taxonomy.Update(r.Context(), id, upd)
		return 200, c, err
	}))
	r.Post("/admin/categories/reorder", h(func(r *http.Request) (int, any, error) {
		in, err := decode[struct {
			ParentID *uuid.UUID  `json:"parent_id"`
			Ordered  []uuid.UUID `json:"ordered_ids"`
		}](r)
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, a.Taxonomy.Reorder(r.Context(), in.ParentID, in.Ordered)
	}))
	r.Post("/admin/categories/{id}/merge", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "category")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[struct {
			Into   uuid.UUID `json:"into_category_id"`
			Reason string    `json:"reason"`
		}](r)
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, a.Taxonomy.Merge(r.Context(), id, in.Into, in.Reason)
	}))
	r.Post("/admin/categories/reclassify", h(func(r *http.Request) (int, any, error) {
		in, err := decode[taxonomy.ReclassifyRequest](r)
		if err != nil {
			return 0, nil, err
		}
		id, err := a.Taxonomy.RequestReclassify(r.Context(), in)
		if err != nil {
			return 0, nil, err
		}
		return 202, map[string]any{"job_id": id, "status": "queued", "status_url": "/v1/admin/jobs/" + id.String(), "dry_run": in.DryRun}, nil
	}))
}

// ---------- Sources (admin) ----------

func (a *API) adminSources(r chi.Router, write func(http.HandlerFunc) http.HandlerFunc) {
	// Publishers
	r.Get("/admin/publishers", h(func(r *http.Request) (int, any, error) {
		ps, err := a.Sources.ListPublishers(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("q"), httpx.Limit(r, 100, 500))
		return 200, list(ps), err
	}))
	r.Post("/admin/publishers", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[sources.PublisherInput](r)
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.CreatePublisher(r.Context(), in)
		return 201, p, err
	})))
	r.Get("/admin/publishers/{id}", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "publisher")
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.GetPublisher(r.Context(), id)
		return 200, p, err
	}))
	r.Patch("/admin/publishers/{id}", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "publisher")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[sources.PublisherInput](r)
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.UpdatePublisher(r.Context(), id, in)
		return 200, p, err
	})))
	r.Get("/admin/publishers/{id}/permissions", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "publisher")
		if err != nil {
			return 0, nil, err
		}
		ps, err := a.Sources.ListPermissions(r.Context(), id)
		return 200, list(ps), err
	}))
	r.Put("/admin/publishers/{id}/permissions", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "publisher")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[sources.PermissionInput](r)
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.UpsertPermission(r.Context(), id, in)
		return 200, p, err
	})))

	// Endpoints
	r.Get("/admin/sources", h(func(r *http.Request) (int, any, error) {
		pub, err := queryUUID(r, "publisher_id")
		if err != nil {
			return 0, nil, err
		}
		es, err := a.Sources.ListEndpoints(r.Context(), sources.EndpointFilter{PublisherID: pub, Status: r.URL.Query().Get("status"),
			Type: r.URL.Query().Get("type"), Tag: r.URL.Query().Get("tag"), Health: r.URL.Query().Get("health"), Limit: httpx.Limit(r, 100, 500)})
		return 200, list(es), err
	}))
	r.Post("/admin/sources", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[sources.EndpointInput](r)
		if err != nil {
			return 0, nil, err
		}
		e, err := a.Sources.CreateEndpoint(r.Context(), in)
		return 201, e, err
	})))
	// Test an unsaved source (no persistence).
	r.Post("/admin/sources/test", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[ingest.TestSourceInput](r)
		if err != nil {
			return 0, nil, err
		}
		res, err := a.Ingest.TestSource(r.Context(), in)
		return 200, res, err
	})))
	r.Post("/admin/sources/preview-url", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[struct {
			URL       string     `json:"url"`
			ProfileID *uuid.UUID `json:"extraction_profile_id,omitempty"`
		}](r)
		if err != nil {
			return 0, nil, err
		}
		res, err := a.Ingest.PreviewURL(r.Context(), in.URL, in.ProfileID)
		return 200, res, err
	})))
	r.Get("/admin/sources/{id}", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "source")
		if err != nil {
			return 0, nil, err
		}
		e, err := a.Sources.GetEndpoint(r.Context(), id)
		return 200, e, err
	}))
	r.Patch("/admin/sources/{id}", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "source")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[sources.EndpointInput](r)
		if err != nil {
			return 0, nil, err
		}
		e, err := a.Sources.UpdateEndpoint(r.Context(), id, in)
		return 200, e, err
	})))
	for _, op := range []string{"enable", "pause", "disable", "archive", "pending-permission"} {
		status := map[string]string{"enable": "enabled", "pause": "paused", "disable": "disabled", "archive": "archived", "pending-permission": "pending_permission"}[op]
		r.Post("/admin/sources/{id}/"+op, write(h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "source")
			if err != nil {
				return 0, nil, err
			}
			e, err := a.Sources.SetEndpointStatus(r.Context(), id, status)
			return 200, e, err
		})))
	}
	r.Post("/admin/sources/{id}/test", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "source")
		if err != nil {
			return 0, nil, err
		}
		e, err := a.Sources.GetEndpoint(r.Context(), id)
		if err != nil {
			return 0, nil, err
		}
		fap := e.FetchArticlePages
		res, err := a.Ingest.TestSource(r.Context(), ingest.TestSourceInput{EndpointType: e.EndpointType, EntryURL: e.EntryURL,
			ExtractionProfileID: e.ExtractionProfileID, FetchArticlePages: &fap})
		return 200, res, err
	})))
	r.Post("/admin/sources/{id}/crawl", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "source")
		if err != nil {
			return 0, nil, err
		}
		var in struct {
			MaxItems int `json:"max_items"`
		}
		if r.ContentLength > 0 {
			if err := httpx.DecodeJSON(r, &in); err != nil {
				return 0, nil, err
			}
		}
		runID, err := a.Ingest.RequestCrawl(r.Context(), id, actorPtr(r), in.MaxItems)
		if err != nil {
			return 0, nil, err
		}
		return 202, map[string]any{"crawl_run_id": runID, "status": "queued", "status_url": "/v1/admin/sources/" + id.String() + "/runs"}, nil
	})))
	r.Get("/admin/sources/{id}/runs", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "source")
		if err != nil {
			return 0, nil, err
		}
		runs, err := a.Sources.ListCrawlRuns(r.Context(), id, httpx.Limit(r, 50, 200))
		return 200, list(runs), err
	}))
	r.Get("/admin/sources/{id}/health", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "source")
		if err != nil {
			return 0, nil, err
		}
		hl, err := a.Sources.GetHealth(r.Context(), id)
		return 200, hl, err
	}))
	r.Post("/admin/sources/{id}/reprocess", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "source")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[ReprocessRequest](r)
		if err != nil {
			return 0, nil, err
		}
		in.SourceEndpointID = &id
		jobID, err := a.enqueueReprocess(r, in)
		if err != nil {
			return 0, nil, err
		}
		return 202, map[string]any{"job_id": jobID, "status": "queued", "status_url": "/v1/admin/jobs/" + jobID.String()}, nil
	})))
	r.Get("/admin/health/source-metrics", h(func(r *http.Request) (int, any, error) {
		m, err := a.Sources.SourceMetrics(r.Context())
		return 200, list(m), err
	}))

	// Blocklist
	r.Get("/admin/blocked-urls", h(func(r *http.Request) (int, any, error) {
		b, err := a.Sources.ListBlocked(r.Context())
		return 200, list(b), err
	}))
	r.Post("/admin/blocked-urls", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[struct {
			MatchType string `json:"match_type"`
			Value     string `json:"value"`
			Reason    string `json:"reason"`
		}](r)
		if err != nil {
			return 0, nil, err
		}
		b, err := a.Sources.AddBlocked(r.Context(), in.MatchType, in.Value, in.Reason)
		return 201, b, err
	})))
	r.Delete("/admin/blocked-urls/{id}", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "blocked_url")
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, a.Sources.DeleteBlocked(r.Context(), id)
	})))

	// Extraction profiles
	r.Get("/admin/extraction-profiles", h(func(r *http.Request) (int, any, error) {
		ps, err := a.Sources.ListProfiles(r.Context())
		return 200, list(ps), err
	}))
	r.Post("/admin/extraction-profiles", write(h(func(r *http.Request) (int, any, error) {
		in, err := decode[sources.ProfileInput](r)
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.CreateProfile(r.Context(), in, ingest.ValidateSelector)
		return 201, p, err
	})))
	r.Get("/admin/extraction-profiles/{id}", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "profile")
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.GetProfile(r.Context(), id)
		return 200, p, err
	}))
	r.Patch("/admin/extraction-profiles/{id}", write(h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "profile")
		if err != nil {
			return 0, nil, err
		}
		in, err := decode[sources.ProfileInput](r)
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.UpdateProfile(r.Context(), id, in, ingest.ValidateSelector)
		return 200, p, err
	})))
	r.Post("/admin/extraction-profiles/{id}/validate", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "profile")
		if err != nil {
			return 0, nil, err
		}
		p, err := a.Sources.GetProfile(r.Context(), id)
		if err != nil {
			return 0, nil, err
		}
		res := ingest.RunProfileFixtures(p)
		passed := 0
		for _, x := range res {
			if x.Passed {
				passed++
			}
		}
		return 200, map[string]any{"total": len(res), "passed": passed, "results": res}, nil
	}))
}

// ---------- Jobs, dead letters, audit ----------

func (a *API) adminOps(r chi.Router) {
	r.Get("/admin/jobs", h(func(r *http.Request) (int, any, error) {
		js, err := jobs.List(r.Context(), a.DB.Pool, r.URL.Query().Get("kind"), r.URL.Query().Get("status"), httpx.Limit(r, 50, 500))
		return 200, list(js), err
	}))
	r.Get("/admin/jobs/{id}", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "job")
		if err != nil {
			return 0, nil, err
		}
		j, err := jobs.Get(r.Context(), a.DB.Pool, id)
		return 200, j, err
	}))
	r.Post("/admin/jobs/{id}/retry", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "job")
		if err != nil {
			return 0, nil, err
		}
		if err := jobs.Retry(r.Context(), a.DB.Pool, id); err != nil {
			return 0, nil, err
		}
		_ = audit.Record(r.Context(), a.DB.Pool, audit.Entry{Action: "job.retry", ResourceType: "job", ResourceID: id.String()})
		return 202, map[string]string{"status": "queued"}, nil
	}))
	r.Post("/admin/jobs/{id}/cancel", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "job")
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, jobs.Cancel(r.Context(), a.DB.Pool, id)
	}))
	r.Get("/admin/dead-letters", h(func(r *http.Request) (int, any, error) {
		rows, err := a.DB.Pool.Query(r.Context(), `SELECT id, consumer, message_id, topic, event_type, error, attempts, created_at, replayed_at
			FROM dead_letters WHERE ($1='' OR consumer=$1) ORDER BY created_at DESC LIMIT $2`, r.URL.Query().Get("consumer"), httpx.Limit(r, 50, 500))
		if err != nil {
			return 0, nil, err
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return 0, nil, err
			}
			names := []string{"id", "consumer", "message_id", "topic", "event_type", "error", "attempts", "created_at", "replayed_at"}
			m := map[string]any{}
			for i, n := range names {
				m[n] = vals[i]
			}
			if b, ok := m["id"].([16]byte); ok {
				m["id"] = uuid.UUID(b)
			}
			if b, ok := m["message_id"].([16]byte); ok {
				m["message_id"] = uuid.UUID(b)
			}
			out = append(out, m)
		}
		return 200, list(out), rows.Err()
	}))
	r.Post("/admin/dead-letters/{id}/replay", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "dead_letter")
		if err != nil {
			return 0, nil, err
		}
		if err := bus.ReplayDeadLetter(r.Context(), a.DB, id); err != nil {
			return 0, nil, apperr.NotFound("dead_letter_not_found", "Dead letter not found or already replayed.").WithErr(err)
		}
		_ = audit.Record(r.Context(), a.DB.Pool, audit.Entry{Action: "dead_letter.replay", ResourceType: "dead_letter", ResourceID: id.String()})
		return 202, map[string]string{"status": "requeued"}, nil
	}))
}

func (a *API) adminAudit(r chi.Router) {
	r.Get("/admin/audit-logs", h(func(r *http.Request) (int, any, error) {
		actor, err := queryUUID(r, "actor_id")
		if err != nil {
			return 0, nil, err
		}
		before, err := httpx.QueryTime(r, "before")
		if err != nil {
			return 0, nil, err
		}
		logs, err := audit.List(r.Context(), a.DB.Pool, audit.Filter{ResourceType: r.URL.Query().Get("resource_type"),
			ResourceID: r.URL.Query().Get("resource_id"), ActorID: actor, Before: before, Limit: httpx.Limit(r, 50, 500)})
		return 200, list(logs), err
	}))
}

// requirePerm wraps a handler with a permission check (used for write routes on read-allowed groups).
func requirePerm(perm string) func(http.HandlerFunc) http.HandlerFunc {
	mw := identity.Require(perm)
	return func(next http.HandlerFunc) http.HandlerFunc {
		return mw(next).ServeHTTP
	}
}
