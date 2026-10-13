package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/eval"
	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/identity"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/search"
)

func (a *API) aiAdmin() *ai.Admin {
	return &ai.Admin{DB: a.DB, GW: a.AI, Policy: a.Sources.SSRFPolicy()}
}

func (a *API) adminContent(r chi.Router) {
	// ---- Articles (articles:moderate) ----
	r.Group(func(r chi.Router) {
		r.Use(identity.Require("articles:moderate"))
		r.Get("/admin/articles", h(func(r *http.Request) (int, any, error) {
			pub, err := queryUUID(r, "publisher_id")
			if err != nil {
				return 0, nil, err
			}
			l, err := a.Ingest.ListArticles(r.Context(), r.URL.Query().Get("status"), pub, r.URL.Query().Get("q"), httpx.Limit(r, 50, 500))
			return 200, list(l), err
		}))
		r.Get("/admin/articles/{id}", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "article")
			if err != nil {
				return 0, nil, err
			}
			d, err := a.Ingest.ArticleDetail(r.Context(), id)
			return 200, d, err
		}))
		for _, action := range []string{"quarantine", "release", "retry"} {
			r.Post("/admin/articles/{id}/"+action, h(func(r *http.Request) (int, any, error) {
				id, err := pathID(r, "id", "article")
				if err != nil {
					return 0, nil, err
				}
				var in struct {
					Reason string `json:"reason"`
				}
				if r.ContentLength > 0 {
					if err := httpx.DecodeJSON(r, &in); err != nil {
						return 0, nil, err
					}
				}
				if err := a.Ingest.SetArticleStatus(r.Context(), id, action, in.Reason); err != nil {
					return 0, nil, err
				}
				if action == "quarantine" {
					a.refreshEventsOf(r, id)
				}
				return 202, map[string]string{"status": "accepted"}, nil
			}))
		}
		r.Post("/admin/articles/{id}/withdraw", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "article")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[struct {
				Reason     string `json:"reason"`
				LegalBasis string `json:"legal_basis"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			evs, err := a.Ingest.Withdraw(r.Context(), id, in.Reason, in.LegalBasis)
			if err != nil {
				return 0, nil, err
			}
			for _, e := range evs {
				if err := a.Events.Refresh(r.Context(), e, "A source article was withdrawn"); err != nil {
					return 0, nil, err
				}
			}
			return 200, map[string]any{"status": "withdrawn", "affected_events": evs}, nil
		}))
		r.Put("/admin/articles/{id}/categories", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "article")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[struct {
				CategoryIDs []uuid.UUID `json:"category_ids"`
				PrimaryID   *uuid.UUID  `json:"primary_category_id"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			return 204, nil, a.Ingest.SetEditorialCategories(r.Context(), id, in.CategoryIDs, in.PrimaryID)
		}))
		r.Post("/admin/articles/reprocess", h(func(r *http.Request) (int, any, error) {
			in, err := decode[ReprocessRequest](r)
			if err != nil {
				return 0, nil, err
			}
			id, err := a.Ingest.RequestReprocess(r.Context(), in, actorPtr(r))
			if err != nil {
				return 0, nil, err
			}
			return 202, map[string]any{"job_id": id, "status": "queued", "status_url": "/v1/admin/jobs/" + id.String()}, nil
		}))
	})

	// ---- Events (events:edit) ----
	r.Group(func(r chi.Router) {
		r.Use(identity.Require("events:edit"))
		r.Get("/admin/events/{id}", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "event")
			if err != nil {
				return 0, nil, err
			}
			e, err := a.Events.Get(r.Context(), id, a.Lang(r))
			if err != nil {
				return 0, nil, err
			}
			versions, err := a.Events.Versions(r.Context(), id)
			if err != nil {
				return 0, nil, err
			}
			tl, err := a.Events.Timeline(r.Context(), id, true)
			if err != nil {
				return 0, nil, err
			}
			links, err := a.eventLinks(r, id)
			if err != nil {
				return 0, nil, err
			}
			return 200, map[string]any{"event": e, "versions": versions, "timeline": tl, "links": links}, nil
		}))
		r.Post("/admin/events/{id}/merge", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "event")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[struct {
				SourceEventIDs []uuid.UUID `json:"source_event_ids"`
				Reason         string      `json:"reason"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			hist, err := a.Events.Merge(r.Context(), id, in.SourceEventIDs, in.Reason)
			return 200, map[string]any{"merge_history_id": hist, "event_id": id}, err
		}))
		r.Post("/admin/events/unmerge/{historyId}", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "historyId", "merge_history")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[struct {
				Reason string `json:"reason"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			return 204, nil, a.Events.Unmerge(r.Context(), id, in.Reason)
		}))
		r.Post("/admin/events/{id}/split", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "event")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[struct {
				ArticleIDs []uuid.UUID `json:"article_ids"`
				Reason     string      `json:"reason"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			nid, err := a.Events.Split(r.Context(), id, in.ArticleIDs, in.Reason)
			return 201, map[string]any{"new_event_id": nid}, err
		}))
		r.Post("/admin/events/{id}/correct", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "event")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[events.CorrectInput](r)
			if err != nil {
				return 0, nil, err
			}
			e, err := a.Events.Correct(r.Context(), id, in)
			return 200, e, err
		}))
		r.Post("/admin/events/{id}/updates/{updateId}/approve", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "event")
			if err != nil {
				return 0, nil, err
			}
			uid, err := pathID(r, "updateId", "update")
			if err != nil {
				return 0, nil, err
			}
			return 204, nil, a.Events.ApproveUpdate(r.Context(), id, uid)
		}))
	})

	// ---- Evaluation results (ai:evaluate); runs are started with `newsctl eval` ----
	r.Group(func(r chi.Router) {
		r.Use(identity.Require("ai:evaluate"))
		r.Get("/admin/ai/evals", h(func(r *http.Request) (int, any, error) {
			l, err := eval.Runs(r.Context(), a.DB, r.URL.Query().Get("suite"), httpx.Limit(r, 50, 200))
			return 200, list(l), err
		}))
		r.Get("/admin/ai/evals/{id}", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "eval_run")
			if err != nil {
				return 0, nil, err
			}
			res, err := eval.Results(r.Context(), a.DB, id)
			if err == nil && len(res) == 0 {
				return 0, nil, apperr.NotFound("eval_run_not_found", "Evaluation run not found.")
			}
			return 200, map[string]any{"run_id": id, "results": res}, err
		}))
	})

	// ---- AI configuration (ai:configure) ----
	r.Group(func(r chi.Router) {
		r.Use(identity.Require("ai:configure"))
		r.Get("/admin/ai/providers", h(func(r *http.Request) (int, any, error) {
			l, err := a.aiAdmin().ListProviders(r.Context())
			return 200, list(l), err
		}))
		r.Post("/admin/ai/providers", h(func(r *http.Request) (int, any, error) {
			in, err := decode[ai.ProviderInput](r)
			if err != nil {
				return 0, nil, err
			}
			p, err := a.aiAdmin().UpsertProvider(r.Context(), nil, in)
			return 201, p, err
		}))
		r.Patch("/admin/ai/providers/{id}", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "provider")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[ai.ProviderInput](r)
			if err != nil {
				return 0, nil, err
			}
			p, err := a.aiAdmin().UpsertProvider(r.Context(), &id, in)
			return 200, p, err
		}))
		r.Get("/admin/ai/models", h(func(r *http.Request) (int, any, error) {
			l, err := a.aiAdmin().ListModels(r.Context())
			return 200, list(l), err
		}))
		r.Post("/admin/ai/models", h(func(r *http.Request) (int, any, error) {
			in, err := decode[ai.ModelInput](r)
			if err != nil {
				return 0, nil, err
			}
			m, err := a.aiAdmin().UpsertModel(r.Context(), nil, in)
			return 201, m, err
		}))
		r.Patch("/admin/ai/models/{id}", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "model")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[ai.ModelInput](r)
			if err != nil {
				return 0, nil, err
			}
			m, err := a.aiAdmin().UpsertModel(r.Context(), &id, in)
			return 200, m, err
		}))
		r.Get("/admin/ai/routes", h(func(r *http.Request) (int, any, error) {
			l, err := a.aiAdmin().ListRoutes(r.Context())
			return 200, list(l), err
		}))
		r.Post("/admin/ai/routes", h(func(r *http.Request) (int, any, error) {
			in, err := decode[ai.RouteInput](r)
			if err != nil {
				return 0, nil, err
			}
			rt, err := a.aiAdmin().UpsertRoute(r.Context(), nil, in)
			return 201, rt, err
		}))
		r.Patch("/admin/ai/routes/{id}", h(func(r *http.Request) (int, any, error) {
			id, err := pathID(r, "id", "route")
			if err != nil {
				return 0, nil, err
			}
			in, err := decode[ai.RouteInput](r)
			if err != nil {
				return 0, nil, err
			}
			rt, err := a.aiAdmin().UpsertRoute(r.Context(), &id, in)
			return 200, rt, err
		}))
		r.Get("/admin/ai/prompts", h(func(r *http.Request) (int, any, error) {
			l, err := a.aiAdmin().ListPrompts(r.Context(), r.URL.Query().Get("task"))
			return 200, list(l), err
		}))
		r.Post("/admin/ai/prompts", h(func(r *http.Request) (int, any, error) {
			in, err := decode[ai.PromptInput](r)
			if err != nil {
				return 0, nil, err
			}
			p, err := a.aiAdmin().CreatePromptVersion(r.Context(), in)
			return 201, p, err
		}))
		r.Get("/admin/ai/executions", h(func(r *http.Request) (int, any, error) {
			l, err := a.aiAdmin().ListExecutions(r.Context(), r.URL.Query().Get("task"), r.URL.Query().Get("status"), httpx.Limit(r, 50, 500))
			return 200, list(l), err
		}))
		r.Get("/admin/ai/usage", h(func(r *http.Request) (int, any, error) {
			l, err := a.aiAdmin().UsageSummary(r.Context(), queryInt(r, "hours", 24))
			return 200, list(l), err
		}))
		// Embedding/vector collection lifecycle.
		r.Get("/admin/search/collections", h(func(r *http.Request) (int, any, error) {
			l, err := a.Search.ListCollections(r.Context())
			return 200, list(l), err
		}))
		r.Post("/admin/search/embedding-migration", h(func(r *http.Request) (int, any, error) {
			plan, err := a.Search.PlanEmbeddingMigration(r.Context())
			return 202, plan, err
		}))
		r.Post("/admin/search/collections/{docType}/cutover", h(func(r *http.Request) (int, any, error) {
			return 204, nil, a.Search.Cutover(r.Context(), chi.URLParam(r, "docType"))
		}))
		r.Post("/admin/search/collections/{docType}/rollback", h(func(r *http.Request) (int, any, error) {
			return 204, nil, a.Search.Rollback(r.Context(), chi.URLParam(r, "docType"))
		}))
		r.Post("/admin/search/reindex", h(func(r *http.Request) (int, any, error) {
			in, err := decode[struct {
				DocTypes []string `json:"doc_types"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			id, err := jobs.Enqueue(r.Context(), a.DB.Pool, search.JobReindex, in, jobs.EnqueueOpts{CreatedBy: actorPtr(r), Priority: 300})
			if err != nil {
				return 0, nil, err
			}
			return 202, map[string]any{"job_id": id, "status": "queued", "status_url": "/v1/admin/jobs/" + id.String()}, nil
		}))
	})

	// ---- Policies & feature flags (policies:write) ----
	r.Group(func(r chi.Router) {
		r.Use(identity.Require("policies:write"))
		r.Get("/admin/policies", h(func(r *http.Request) (int, any, error) {
			l, err := a.Policy.List(r.Context())
			return 200, list(l), err
		}))
		r.Put("/admin/policies/{key}", h(func(r *http.Request) (int, any, error) {
			in, err := decode[struct {
				Value   json.RawMessage `json:"value"`
				Version int             `json:"version"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			p, err := a.Policy.Put(r.Context(), chi.URLParam(r, "key"), in.Value, in.Version)
			return 200, p, err
		}))
		r.Get("/admin/feature-flags", h(func(r *http.Request) (int, any, error) {
			l, err := a.Policy.Flags(r.Context())
			return 200, list(l), err
		}))
		r.Put("/admin/feature-flags/{key}", h(func(r *http.Request) (int, any, error) {
			in, err := decode[struct {
				Enabled        bool `json:"enabled"`
				RolloutPercent int  `json:"rollout_percent"`
			}](r)
			if err != nil {
				return 0, nil, err
			}
			return 204, nil, a.Policy.SetFlag(r.Context(), chi.URLParam(r, "key"), in.Enabled, in.RolloutPercent)
		}))
	})
}

func (a *API) refreshEventsOf(r *http.Request, articleID uuid.UUID) {
	rows, err := a.DB.Pool.Query(r.Context(), `SELECT DISTINCT event_id FROM event_article_links WHERE article_id=$1`, articleID)
	if err != nil {
		return
	}
	var evs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			evs = append(evs, id)
		}
	}
	rows.Close()
	for _, e := range evs {
		_ = a.Events.Refresh(r.Context(), e, "A source article was quarantined")
	}
}

func (a *API) eventLinks(r *http.Request, id uuid.UUID) ([]map[string]any, error) {
	rows, err := a.DB.Pool.Query(r.Context(), `SELECT l.article_id, a.title, l.relation, l.confidence, l.rationale, l.linked_by, l.linked_at, l.unlinked_at
		FROM event_article_links l JOIN articles a ON a.id=l.article_id WHERE l.event_id=$1 ORDER BY l.linked_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var aid uuid.UUID
		var title, rel, by string
		var conf float32
		var rationale json.RawMessage
		var at, unl any
		if err := rows.Scan(&aid, &title, &rel, &conf, &rationale, &by, &at, &unl); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"article_id": aid, "title": title, "relation": rel, "confidence": conf, "rationale": rationale,
			"linked_by": by, "linked_at": at, "unlinked_at": unl})
	}
	return out, rows.Err()
}
