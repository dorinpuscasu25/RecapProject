// Package api wires domain services into the public, consumer and admin REST API.
package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

// handlerFunc returns (status, body, error); errors render as problem+json.
type handlerFunc func(r *http.Request) (int, any, error)

func h(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, body, err := fn(r)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if status == http.StatusNoContent {
			httpx.NoContent(w)
			return
		}
		httpx.WriteJSON(w, status, body)
	}
}

func pathID(r *http.Request, name, resource string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, apperr.NotFound(resource+"_not_found", strings.ReplaceAll(resource, "_", " ")+" not found.")
	}
	return id, nil
}

func queryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, apperr.Validation(apperr.FieldError{Field: name, Code: "invalid_uuid", Message: "Must be a UUID."})
	}
	return &id, nil
}

func queryUUIDs(r *http.Request, name string) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, raw := range r.URL.Query()[name] {
		for _, v := range strings.Split(raw, ",") {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			id, err := uuid.Parse(v)
			if err != nil {
				return nil, apperr.Validation(apperr.FieldError{Field: name, Code: "invalid_uuid", Message: "Must be UUIDs."})
			}
			out = append(out, id)
		}
	}
	return out, nil
}

func queryInt(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func queryList(r *http.Request, name string) []string {
	var out []string
	for _, raw := range r.URL.Query()[name] {
		for _, v := range strings.Split(raw, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

func actorPtr(r *http.Request) *uuid.UUID {
	if p := principal.From(r.Context()); p != nil && p.Kind == principal.KindUser {
		id := p.ID
		return &id
	}
	return nil
}

func decode[T any](r *http.Request) (T, error) {
	var v T
	err := httpx.DecodeJSON(r, &v)
	return v, err
}

type items[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func list[T any](v []T) items[T] {
	if v == nil {
		v = []T{}
	}
	return items[T]{Items: v}
}
