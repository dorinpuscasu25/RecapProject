package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	apidocs "github.com/dorinpuscasu25/recapproject/api"
	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/analytics"
	"github.com/dorinpuscasu25/recapproject/internal/enrich"
	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/identity"
	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/mail"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/search"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"

	"log/slog"
)

// API holds the services exposed over HTTP.
type API struct {
	Cfg       *config.Config
	DB        *db.DB
	Log       *slog.Logger
	Identity  *identity.Service
	Taxonomy  *taxonomy.Service
	Sources   *sources.Service
	Ingest    *ingest.Service
	Policy    *policy.Store
	AI        *ai.Gateway
	Search    *search.Service
	Enrich    *enrich.Service
	Events    *events.Service
	Analytics *analytics.Recorder
	Mods      Modules
}

// usage records consumer API usage as pseudonymous analytics events: route pattern, method,
// status and language only (never query text, bodies or identifiers from the path).
func (a *API) usage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if a.Analytics == nil {
			return
		}
		route := ""
		if rc := chi.RouteContext(r.Context()); rc != nil {
			route = rc.RoutePattern()
		}
		var user uuid.UUID
		if p := principal.From(r.Context()); p != nil {
			user = p.ID
		}
		a.Analytics.Track("api_request", user, map[string]any{"route": route, "method": r.Method, "status": sw.status, "lang": r.URL.Query().Get("lang")})
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Modules holds experience modules added after the core (fact-checking, feed, briefings, chat,
// notifications). Nil entries are simply not mounted.
type Modules struct {
	FactCheck  FactCheckAPI
	Experience []ExperienceAPI
}

// FactCheckAPI and ExperienceAPI let those modules mount their own routes.
type FactCheckAPI interface {
	MountConsumer(r chi.Router)
	MountAdmin(r chi.Router, perm func(string) func(http.HandlerFunc) http.HandlerFunc)
}

type ExperienceAPI interface {
	MountConsumer(r chi.Router)
	MountAdmin(r chi.Router, perm func(string) func(http.HandlerFunc) http.HandlerFunc)
}

// Router builds the HTTP handler.
func (a *API) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.WithLogger(a.Log), httpx.Recoverer, httpx.Tracing(a.Cfg.ServiceName), httpx.AccessLog,
		httpx.SecurityHeaders, httpx.CORS(a.Cfg.CORSOrigins), httpx.BodyLimit(a.Cfg.MaxBodyBytes))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", a.ready)
	r.Get("/openapi.yaml", serveOpenAPI)

	idh := &identity.Handler{Svc: a.Identity}
	r.Route("/v1", func(r chi.Router) {
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteError(w, r, notFound())
		})
		// Unauthenticated identity endpoints.
		idh.PublicRoutes(r)

		// Development-only mail sink (never mounted in production; config refuses devsink there).
		if a.Cfg.MailDriver == "devsink" && !a.Cfg.IsProduction() {
			r.Get("/dev/mailbox", h(func(r *http.Request) (int, any, error) {
				msgs, err := mail.DevMailbox(r.Context(), a.DB.Pool, r.URL.Query().Get("to"), 20)
				return 200, list(msgs), err
			}))
		}

		// Everything below requires an authenticated, active principal.
		r.Group(func(r chi.Router) {
			r.Use(idh.Authenticate)

			// Self-service (human users only).
			r.Group(func(r chi.Router) {
				r.Use(identity.RequireUser, identity.Require("me:manage"))
				idh.AuthedRoutes(r)
			})

			// Consumer news APIs: verified users with news:read.
			r.Group(func(r chi.Router) {
				r.Use(identity.RequireUser, identity.Require("news:read"), a.usage)
				a.consumerCategories(r)
				a.consumerEvents(r)
				if a.Mods.FactCheck != nil {
					a.Mods.FactCheck.MountConsumer(r)
				}
				for _, x := range a.Mods.Experience {
					x.MountConsumer(r)
				}
			})

			// Admin / editorial APIs: per-group permissions.
			r.Group(func(r chi.Router) {
				r.Use(identity.Require("users:manage"))
				idh.AdminRoutes(r)
			})
			r.Group(func(r chi.Router) {
				r.Use(identity.Require("categories:write"))
				a.adminCategories(r)
			})
			r.Group(func(r chi.Router) {
				r.Use(identity.Require("sources:read"))
				a.adminSources(r, requirePerm("sources:write"))
			})
			r.Group(func(r chi.Router) {
				r.Use(identity.Require("jobs:manage"))
				a.adminOps(r)
			})
			r.Group(func(r chi.Router) {
				r.Use(identity.Require("audit:read"))
				a.adminAudit(r)
			})
			a.adminContent(r)
			if a.Mods.FactCheck != nil {
				a.Mods.FactCheck.MountAdmin(r, requirePerm)
			}
			for _, x := range a.Mods.Experience {
				x.MountAdmin(r, requirePerm)
			}
		})
	})
	return r
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"postgres": "ok"}
	status := 200
	if err := a.DB.Pool.Ping(ctx); err != nil {
		checks["postgres"] = "unavailable"
		status = 503
	}
	httpx.WriteJSON(w, status, map[string]any{"status": map[bool]string{true: "ready", false: "not_ready"}[status == 200], "checks": checks})
}

// MetricsHandler serves Prometheus metrics (mounted on the internal admin listener).
func MetricsHandler() http.Handler { return promhttp.Handler() }

// ReprocessRequest selects articles to re-run through enrichment.
type ReprocessRequest = ingest.ReprocessRequest

func (a *API) enqueueReprocess(r *http.Request, in ReprocessRequest) (uuid.UUID, error) {
	return a.Ingest.RequestReprocess(r.Context(), in, actorPtr(r))
}

var _ = jobs.Enqueue

func notFound() error { return apperr.NotFound("route_not_found", "No such endpoint.") }

func serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(apidocs.OpenAPI)
}
