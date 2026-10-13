package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/search"
)

// Lang resolves the response language: ?lang= overrides the user's summary language.
func (a *API) Lang(r *http.Request) string {
	switch l := r.URL.Query().Get("lang"); l {
	case "ro", "ru", "en":
		return l
	}
	if p := principal.From(r.Context()); p != nil && p.Kind == principal.KindUser {
		var l string
		if err := a.DB.Pool.QueryRow(r.Context(), `SELECT summary_language FROM user_preferences WHERE user_id=$1`, p.ID).Scan(&l); err == nil && l != "" {
			return l
		}
	}
	return "ro"
}

func (a *API) consumerEvents(r chi.Router) {
	r.Get("/events", h(func(r *http.Request) (int, any, error) {
		cats, err := queryUUIDs(r, "category_id")
		if err != nil {
			return 0, nil, err
		}
		since, err := httpx.QueryTime(r, "since")
		if err != nil {
			return 0, nil, err
		}
		until, err := httpx.QueryTime(r, "until")
		if err != nil {
			return 0, nil, err
		}
		cur, err := httpx.DecodeCursor(r.URL.Query().Get("cursor"))
		if err != nil {
			return 0, nil, err
		}
		f := events.ListFilter{CategoryIDs: cats, Since: since, Until: until, States: queryList(r, "state"), Region: r.URL.Query().Get("region"),
			Lang: a.Lang(r), Limit: httpx.Limit(r, 20, 100)}
		if cur != nil {
			id, _ := uuid.Parse(cur.ID)
			f.Before, f.BeforeID = &cur.T, &id
		}
		list, err := a.Events.List(r.Context(), f)
		if err != nil {
			return 0, nil, err
		}
		out := list
		next := ""
		if len(list) == f.Limit {
			last := list[len(list)-1]
			if last.LastMaterialUpdateAt != nil {
				next = httpx.EncodeCursor(httpx.Cursor{T: *last.LastMaterialUpdateAt, ID: last.ID.String()})
			}
		}
		return 200, items[events.Event]{Items: nonNilEvents(out), NextCursor: next}, nil
	}))
	r.Get("/events/{id}", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "event")
		if err != nil {
			return 0, nil, err
		}
		e, err := a.Events.Get(r.Context(), id, a.Lang(r))
		if err != nil {
			return 0, nil, err
		}
		if e.Status == "merged" && e.MergedInto != nil {
			return 200, map[string]any{"id": e.ID, "status": "merged", "merged_into": e.MergedInto}, nil
		}
		if e.Status != "active" {
			return 0, nil, apperr.Gone("event_withdrawn", "This event has been withdrawn.")
		}
		return 200, e, nil
	}))
	r.Get("/events/{id}/timeline", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "event")
		if err != nil {
			return 0, nil, err
		}
		if _, err := a.Events.Get(r.Context(), id, "ro"); err != nil {
			return 0, nil, err
		}
		tl, err := a.Events.Timeline(r.Context(), id, false)
		return 200, list(tl), err
	}))
	r.Get("/events/{id}/sources", h(func(r *http.Request) (int, any, error) {
		id, err := pathID(r, "id", "event")
		if err != nil {
			return 0, nil, err
		}
		if _, err := a.Events.Get(r.Context(), id, "ro"); err != nil {
			return 0, nil, err
		}
		src, err := a.Events.Sources(r.Context(), id)
		return 200, list(src), err
	}))
	r.Get("/search", h(func(r *http.Request) (int, any, error) {
		from, err := httpx.QueryTime(r, "from")
		if err != nil {
			return 0, nil, err
		}
		to, err := httpx.QueryTime(r, "to")
		if err != nil {
			return 0, nil, err
		}
		q := search.Query{Text: r.URL.Query().Get("q"), Mode: r.URL.Query().Get("mode"), DocTypes: queryList(r, "types"),
			Languages: queryList(r, "language"), CategoryIDs: queryList(r, "category_id"), PublisherIDs: queryList(r, "publisher_id"),
			Regions: queryList(r, "region"), EntityKeys: queryList(r, "entity"), Statuses: queryList(r, "status"), Verdicts: queryList(r, "verdict"),
			From: from, To: to, Limit: httpx.Limit(r, 20, 50), Rerank: r.URL.Query().Get("rerank") == "true"}
		res, err := a.Search.Search(r.Context(), q)
		return 200, res, err
	}))
}

func nonNilEvents(e []events.Event) []events.Event {
	if e == nil {
		return []events.Event{}
	}
	return e
}

// timeQuery parses an optional RFC3339 or YYYY-MM-DD value.
func timeQuery(r *http.Request, name string) (*time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	if !strings.Contains(v, "T") {
		t, err := time.Parse("2006-01-02", v)
		if err == nil {
			return &t, nil
		}
	}
	return httpx.QueryTime(r, name)
}
