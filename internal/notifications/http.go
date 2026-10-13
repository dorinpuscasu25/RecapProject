package notifications

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

// Handler mounts notification schedule, device and delivery-history routes.
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

func (h *Handler) MountConsumer(r chi.Router) {
	r.Get("/notifications/schedules", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		l, err := h.Svc.Schedules(r.Context(), u)
		return 200, map[string]any{"items": l}, err
	}))
	// PUT /notifications/schedules manages the user's "default" schedule; named schedules allow several.
	r.Put("/notifications/schedules", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in ScheduleInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		v, err := h.Svc.PutSchedule(r.Context(), u, "default", in)
		return 200, v, err
	}))
	r.Put("/notifications/schedules/{name}", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in ScheduleInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		v, err := h.Svc.PutSchedule(r.Context(), u, chi.URLParam(r, "name"), in)
		return 200, v, err
	}))
	r.Delete("/notifications/schedules/{name}", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		return http.StatusNoContent, nil, h.Svc.DeleteSchedule(r.Context(), u, chi.URLParam(r, "name"))
	}))
	r.Get("/notifications/deliveries", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		l, err := h.Svc.Deliveries(r.Context(), u, limit)
		if l == nil {
			l = []DeliveryView{}
		}
		return 200, map[string]any{"items": l}, err
	}))
	r.Get("/devices", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		l, err := h.Svc.Devices(r.Context(), u)
		if l == nil {
			l = []Device{}
		}
		return 200, map[string]any{"items": l}, err
	}))
	r.Post("/devices", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		var in DeviceInput
		if err := httpx.DecodeJSON(r, &in); err != nil {
			return 0, nil, err
		}
		d, err := h.Svc.RegisterDevice(r.Context(), u, in)
		return http.StatusCreated, d, err
	}))
	r.Delete("/devices/{id}", wrap(func(r *http.Request, u uuid.UUID) (int, any, error) {
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			return 0, nil, apperr.NotFound("device_not_found", "Device not found.")
		}
		return http.StatusNoContent, nil, h.Svc.RevokeDevice(r.Context(), u, id)
	}))
}

func (h *Handler) MountAdmin(chi.Router, func(string) func(http.HandlerFunc) http.HandlerFunc) {}
