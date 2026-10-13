package briefings

import (
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

// Handler mounts briefing and audio routes.
type Handler struct{ Svc *Service }

type fn func(r *http.Request, user uuid.UUID) (int, any, error)

func wrap(f fn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, body, err := f(r, principal.From(r.Context()).ID)
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

func briefingID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, apperr.NotFound("briefing_not_found", "Briefing not found.")
	}
	return id, nil
}

func (h *Handler) MountConsumer(r chi.Router) {
	r.Post("/briefings", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in Request
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		b, reused, err := h.Svc.Create(r.Context(), u, in, "on_demand")
		if err != nil {
			return 0, nil, err
		}
		if reused {
			return http.StatusOK, b, nil
		}
		return http.StatusAccepted, b, nil
	}))
	r.Get("/briefings", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		l, err := h.Svc.List(r.Context(), u, limit)
		return 200, map[string]any{"items": nonNilB(l)}, err
	}))
	r.Get("/briefings/latest", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		b, err := h.Svc.Latest(r.Context(), u)
		return 200, b, err
	}))
	r.Get("/briefings/{id}", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := briefingID(r)
		if err != nil {
			return 0, nil, err
		}
		b, err := h.Svc.Get(r.Context(), u, id)
		return 200, b, err
	}))
	r.Post("/briefings/{id}/audio", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := briefingID(r)
		if err != nil {
			return 0, nil, err
		}
		var in struct {
			Voice string `json:"voice,omitempty"`
		}
		if r.ContentLength != 0 {
			if err := httpx.DecodeJSON(r, &in); err != nil {
				return 0, nil, err
			}
		}
		a, err := h.Svc.RequestAudio(r.Context(), u, id, in.Voice)
		return http.StatusAccepted, a, err
	}))
	r.Get("/briefings/{id}/audio", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := briefingID(r)
		if err != nil {
			return 0, nil, err
		}
		a, err := h.Svc.GetAudio(r.Context(), u, id)
		return 200, a, err
	}))
	r.Delete("/briefings/{id}/audio", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := briefingID(r)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, h.Svc.CancelAudio(r.Context(), u, id)
	}))
	r.Get("/briefings/{id}/audio/stream", func(w http.ResponseWriter, r *http.Request) {
		id, err := briefingID(r)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		rc, n, ctype, err := h.Svc.OpenAudio(r.Context(), principal.From(r.Context()).ID, id)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "private, no-store")
		if n > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		}
		_, _ = io.Copy(w, rc)
	})
}

func (h *Handler) MountAdmin(chi.Router, func(string) func(http.HandlerFunc) http.HandlerFunc) {}

func nonNilB(l []Briefing) []Briefing {
	if l == nil {
		return []Briefing{}
	}
	return l
}
