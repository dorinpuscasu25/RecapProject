package chat

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

// Handler mounts chat and event-explanation routes.
type Handler struct {
	Svc  *Service
	Lang func(*http.Request) string
}

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

func param(r *http.Request, code, msg string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, apperr.NotFound(code, msg)
	}
	return id, nil
}

func (h *Handler) MountConsumer(r chi.Router) {
	r.Post("/chat/sessions", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in CreateInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		s, err := h.Svc.Create(r.Context(), u, in)
		return http.StatusCreated, s, err
	}))
	r.Get("/chat/sessions", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		l, err := h.Svc.List(r.Context(), u)
		if l == nil {
			l = []Session{}
		}
		return 200, map[string]any{"items": l}, err
	}))
	r.Get("/chat/sessions/{id}", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := param(r, "chat_session_not_found", "Chat session not found.")
		if err != nil {
			return 0, nil, err
		}
		s, err := h.Svc.Get(r.Context(), u, id)
		return 200, s, err
	}))
	r.Delete("/chat/sessions/{id}", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := param(r, "chat_session_not_found", "Chat session not found.")
		if err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, h.Svc.Delete(r.Context(), u, id)
	}))
	r.Post("/chat/sessions/{id}/messages", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := param(r, "chat_session_not_found", "Chat session not found.")
		if err != nil {
			return 0, nil, err
		}
		var in struct {
			Content string `json:"content"`
			Level   string `json:"level,omitempty"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		m, err := h.Svc.Ask(r.Context(), u, id, in.Content, in.Level)
		return http.StatusCreated, m, err
	}))
	r.Get("/events/{id}/explain", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := param(r, "event_not_found", "Event not found.")
		if err != nil {
			return 0, nil, err
		}
		q := r.URL.Query()
		if len([]rune(q.Get("question"))) > 500 {
			return 0, nil, apperr.Validation(apperr.FieldError{Field: "question", Code: "too_long", Message: "At most 500 characters."})
		}
		m, err := h.Svc.Explain(r.Context(), id, h.Lang(r), q.Get("level"), q.Get("question"))
		return 200, m, err
	}))
}

func (h *Handler) MountAdmin(chi.Router, func(string) func(http.HandlerFunc) http.HandlerFunc) {}
