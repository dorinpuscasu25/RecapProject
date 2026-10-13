package feed

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

func encodeCursor(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Handler mounts personalization routes.
type Handler struct {
	Svc  *Service
	Lang func(*http.Request) string
}

type fn func(r *http.Request, user uuid.UUID) (int, any, error)

func wrap(f fn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principal.From(r.Context())
		st, body, err := f(r, p.ID)
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

func eventID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, apperr.NotFound("event_not_found", "Event not found.")
	}
	return id, nil
}

func (h *Handler) MountConsumer(r chi.Router) {
	r.Get("/users/me/preferences", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		p, err := h.Svc.GetPreferences(r.Context(), u)
		return 200, p, err
	}))
	r.Put("/users/me/preferences", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in PreferencesInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		p, err := h.Svc.PutPreferences(r.Context(), u, in)
		return 200, p, err
	}))
	r.Get("/users/me/interests", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		l, err := h.Svc.Interests(r.Context(), u)
		return 200, map[string]any{"items": nonNilI(l)}, err
	}))
	r.Post("/users/me/interests", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in InterestInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		x, err := h.Svc.AddInterest(r.Context(), u, in)
		return 201, x, err
	}))
	r.Delete("/users/me/interests/{iid}", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := uuid.Parse(chi.URLParam(r, "iid"))
		if err != nil {
			return 0, nil, apperr.NotFound("interest_not_found", "Interest not found.")
		}
		return 204, nil, h.Svc.RemoveInterest(r.Context(), u, id)
	}))
	r.Put("/users/me/categories", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in struct {
			CategoryIDs []uuid.UUID `json:"category_ids"`
		}
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		l, err := h.Svc.SetCategories(r.Context(), u, in.CategoryIDs)
		return 200, map[string]any{"items": nonNilI(l)}, err
	}))
	r.Get("/users/me/bookmarks", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		l, err := h.Svc.Bookmarks(r.Context(), u, h.Lang(r))
		return 200, map[string]any{"items": l}, err
	}))
	for _, op := range []string{"follow", "unfollow"} {
		r.Post("/events/{id}/"+op, wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
			id, err := eventID(r)
			if err != nil {
				return 0, nil, err
			}
			return 204, nil, h.Svc.Follow(r.Context(), u, id, op == "follow")
		}))
	}
	r.Post("/events/{id}/seen", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := eventID(r)
		if err != nil {
			return 0, nil, err
		}
		var in struct {
			Version *int `json:"version"`
		}
		if r.ContentLength > 0 {
			if err := httpx.DecodeJSON(r, &in); err != nil {
				return 0, nil, err
			}
		}
		return 204, nil, h.Svc.MarkSeen(r.Context(), u, id, in.Version)
	}))
	r.Put("/events/{id}/bookmark", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := eventID(r)
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, h.Svc.Bookmark(r.Context(), u, id, true)
	}))
	r.Delete("/events/{id}/bookmark", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := eventID(r)
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, h.Svc.Bookmark(r.Context(), u, id, false)
	}))
	r.Post("/events/{id}/dismiss", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := eventID(r)
		if err != nil {
			return 0, nil, err
		}
		return 204, nil, h.Svc.Dismiss(r.Context(), u, id)
	}))
	r.Get("/feed", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		p, err := h.Svc.Feed(r.Context(), u, h.Lang(r), r.URL.Query().Get("cursor"), httpx.Limit(r, 20, 50))
		return 200, p, err
	}))
	r.Get("/catch-up", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		since, err := httpx.QueryTime(r, "since")
		if err != nil {
			return 0, nil, err
		}
		c, err := h.Svc.CatchUp(r.Context(), u, h.Lang(r), since, httpx.Limit(r, 20, 50))
		return 200, c, err
	}))
	r.Post("/catch-up/acknowledge", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in struct {
			At *string `json:"acknowledged_at"`
		}
		if r.ContentLength > 0 {
			if err := httpx.DecodeJSON(r, &in); err != nil {
				return 0, nil, err
			}
		}
		var at *timeT
		if in.At != nil {
			t, err := parseTime(*in.At)
			if err != nil {
				return 0, nil, apperr.Validation(apperr.FieldError{Field: "acknowledged_at", Code: "invalid_time", Message: "RFC3339 required."})
			}
			at = &t
		}
		t, err := h.Svc.Acknowledge(r.Context(), u, at)
		return 200, map[string]any{"acknowledged_at": t}, err
	}))
}

func (h *Handler) MountAdmin(chi.Router, func(string) func(http.HandlerFunc) http.HandlerFunc) {}

func nonNilI(l []Interest) []Interest {
	if l == nil {
		return []Interest{}
	}
	return l
}

type timeT = time.Time

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }
