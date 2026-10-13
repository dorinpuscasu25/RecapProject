package factcheck

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ssrf"
)

// Handler mounts fact-check routes (implements api.FactCheckAPI).
type Handler struct {
	Svc  *Service
	Lang func(*http.Request) string
	SSRF ssrf.Policy
}

type fn func(r *http.Request) (int, any, error)

func wrap(f fn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, body, err := f(r)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if st == http.StatusNoContent {
			httpx.NoContent(w)
			return
		}
		httpx.WriteJSON(w, st, body)
	}
}

func id(r *http.Request, name, res string) (uuid.UUID, error) {
	v, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, apperr.NotFound(res+"_not_found", "Not found.")
	}
	return v, nil
}

func items[T any](v []T) map[string]any {
	if v == nil {
		v = []T{}
	}
	return map[string]any{"items": v}
}

func (h *Handler) MountConsumer(r chi.Router) {
	r.Get("/fact-checks", wrap(func(r *http.Request) (int, any, error) {
		var verdicts []string
		if v := r.URL.Query().Get("verdict"); v != "" {
			verdicts = strings.Split(v, ",")
		}
		before, err := httpx.QueryTime(r, "before")
		if err != nil {
			return 0, nil, err
		}
		l, err := h.Svc.List(r.Context(), ListFilter{Verdicts: verdicts, Lang: h.Lang(r), Before: before, Limit: httpx.Limit(r, 20, 100)})
		return 200, items(l), err
	}))
	r.Get("/fact-checks/{id}", wrap(func(r *http.Request) (int, any, error) {
		cid, err := id(r, "id", "fact_check")
		if err != nil {
			return 0, nil, err
		}
		fc, err := h.Svc.Get(r.Context(), cid, h.Lang(r))
		if err != nil {
			return 0, nil, err
		}
		hist, err := h.Svc.History(r.Context(), cid)
		if err != nil {
			return 0, nil, err
		}
		return 200, map[string]any{"fact_check": fc, "history": hist}, nil
	}))
	r.Get("/events/{id}/claims", wrap(func(r *http.Request) (int, any, error) {
		eid, err := id(r, "id", "event")
		if err != nil {
			return 0, nil, err
		}
		l, err := h.Svc.List(r.Context(), ListFilter{EventID: &eid, Lang: h.Lang(r), Limit: 100})
		return 200, items(l), err
	}))
	r.Get("/misinformation/trends", wrap(func(r *http.Request) (int, any, error) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		t, err := h.Svc.Trends(r.Context(), days, h.Lang(r))
		return 200, t, err
	}))
}

func (h *Handler) MountAdmin(r chi.Router, perm func(string) func(http.HandlerFunc) http.HandlerFunc) {
	review := perm("factcheck:review")
	publish := perm("factcheck:publish")
	r.Get("/admin/fact-checks/queue", review(wrap(func(r *http.Request) (int, any, error) {
		l, err := h.Svc.Queue(r.Context(), r.URL.Query().Get("status"), httpx.Limit(r, 50, 500))
		return 200, items(l), err
	})))
	r.Get("/admin/claims/{id}", review(wrap(func(r *http.Request) (int, any, error) {
		cid, err := id(r, "id", "claim")
		if err != nil {
			return 0, nil, err
		}
		d, err := h.Svc.ClaimDossier(r.Context(), cid)
		return 200, d, err
	})))
	r.Post("/admin/claims/{id}/evidence", review(wrap(func(r *http.Request) (int, any, error) {
		cid, err := id(r, "id", "claim")
		if err != nil {
			return 0, nil, err
		}
		var in EvidenceInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		if err := h.Svc.AddEvidence(r.Context(), cid, in, h.SSRF); err != nil {
			return 0, nil, err
		}
		d, err := h.Svc.ClaimDossier(r.Context(), cid)
		return 201, d, err
	})))
	r.Post("/admin/claims/{id}/reverify", review(wrap(func(r *http.Request) (int, any, error) {
		cid, err := id(r, "id", "claim")
		if err != nil {
			return 0, nil, err
		}
		if err := h.Svc.Verify(r.Context(), cid, "editor_requested"); err != nil {
			return 0, nil, err
		}
		d, err := h.Svc.ClaimDossier(r.Context(), cid)
		return 200, d, err
	})))
	r.Post("/admin/claims/{id}/status", review(wrap(func(r *http.Request) (int, any, error) {
		cid, err := id(r, "id", "claim")
		if err != nil {
			return 0, nil, err
		}
		var in struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		return 204, nil, h.Svc.SetClaimStatus(r.Context(), cid, in.Status, in.Reason)
	})))
	r.Post("/admin/fact-checks/{id}/decision", review(wrap(func(r *http.Request) (int, any, error) {
		rid, err := id(r, "id", "review")
		if err != nil {
			return 0, nil, err
		}
		var in DecisionInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		it, err := h.Svc.Decide(r.Context(), rid, in)
		return 200, it, err
	})))
	r.Post("/admin/fact-checks/{id}/publish", publish(wrap(func(r *http.Request) (int, any, error) {
		rid, err := id(r, "id", "review")
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, h.Svc.Publish(r.Context(), rid)
	})))
	r.Post("/admin/claims/{id}/correct", publish(wrap(func(r *http.Request) (int, any, error) {
		cid, err := id(r, "id", "claim")
		if err != nil {
			return 0, nil, err
		}
		var in CorrectionInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		if err := h.Svc.Correct(r.Context(), cid, in); err != nil {
			return 0, nil, err
		}
		fc, err := h.Svc.Get(r.Context(), cid, h.Lang(r))
		return 200, fc, err
	})))
	r.Post("/admin/narratives", review(wrap(func(r *http.Request) (int, any, error) {
		var in NarrativeInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		nid, err := h.Svc.CreateNarrative(r.Context(), in)
		return 201, map[string]any{"id": nid, "status": "draft"}, err
	})))
	r.Post("/admin/narratives/{id}/publish", publish(wrap(func(r *http.Request) (int, any, error) {
		nid, err := id(r, "id", "narrative")
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, h.Svc.PublishNarrative(r.Context(), nid)
	})))
}
