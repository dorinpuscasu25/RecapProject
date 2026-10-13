package app

import (
	"context"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/enrich"
	"github.com/dorinpuscasu25/recapproject/internal/events"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/policy"
	"github.com/dorinpuscasu25/recapproject/internal/search"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

// modules groups the intelligence/experience modules (AI, enrichment, events, fact-checking,
// search, personalization, briefings, chat, notifications).
type modules struct {
	app    *App
	Policy *policy.Store
	AI     *ai.Gateway
	Search *search.Service
	Enrich *enrich.Service
	Events *events.Service
	ext    []extension
}

// extension lets later modules register consumers, jobs and periodic work.
type extension interface {
	consumers() []*bus.Consumer
	jobHandlers() map[string]jobs.Handler
	tick(ctx context.Context)
}

func newModules(ctx context.Context, a *App) (*modules, error) {
	m := &modules{app: a}
	m.Policy = policy.New(a.DB)
	m.AI = ai.NewGateway(a.DB, a.Sealer, a.Limiter, a.Log)
	var vec search.VectorStore
	if a.Cfg.QdrantURL != "" {
		vec = search.NewQdrant(a.Cfg.QdrantURL, a.Cfg.QdrantAPIKey)
	} else {
		a.Log.Warn("QDRANT_URL not set: using in-memory vector store (development only, not persistent; rebuild with `newsctl reindex`)")
		vec = search.NewMemory()
	}
	var lex search.LexicalIndex
	if a.Cfg.OpenSearchURL != "" {
		lex = search.NewOpenSearch(a.Cfg.OpenSearchURL, a.Cfg.OpenSearchUser, a.Cfg.OpenSearchPassword, a.Cfg.OpenSearchPrefix)
		ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := lex.Ensure(ectx); err != nil {
			a.Log.Warn("opensearch unavailable at startup; lexical queries will degrade", "error", err)
		}
		cancel()
	} else {
		lex = &search.PostgresFTS{DB: a.DB}
	}
	m.Search = search.NewService(a.DB, m.AI, vec, lex, m.Policy, a.Log, a.Cfg.QdrantPrefix)
	m.Enrich = enrich.NewService(a.DB, m.AI, a.Taxonomy, m.Policy, a.Log)
	m.Events = events.NewService(a.DB, m.AI, m.Search, m.Policy, a.Log)
	for _, ctor := range extensionCtors {
		ext, err := ctor(ctx, a, m)
		if err != nil {
			return nil, err
		}
		m.ext = append(m.ext, ext)
	}
	return m, nil
}

// extensionCtors is populated by files in this package for each experience module.
var extensionCtors []func(ctx context.Context, a *App, m *modules) (extension, error)

func (m *modules) consumers() []*bus.Consumer {
	cs := []*bus.Consumer{
		{Name: "enricher", Types: []string{bus.ArticleEnrichmentRequested}, Handle: m.Enrich.HandleEnrichment, Concurrency: 2, Timeout: 5 * time.Minute, MaxAttempts: 8},
		{Name: "event-resolver", Types: []string{bus.EventResolutionRequested}, Handle: m.Events.HandleResolution, Concurrency: 1, Timeout: 5 * time.Minute, MaxAttempts: 8},
		{Name: "search-indexer", Types: []string{bus.ArticleEnriched, bus.EventCreated, bus.EventUpdated, bus.ClaimVerdictUpdated,
			bus.SearchIndexRequested, bus.ContentDeleted}, Handle: m.Search.HandleMessage, Concurrency: 2, MaxAttempts: 10},
	}
	for _, e := range m.ext {
		cs = append(cs, e.consumers()...)
	}
	return cs
}

func (m *modules) jobHandlers() map[string]jobs.Handler {
	hs := map[string]jobs.Handler{
		taxonomy.JobReclassify:   m.Enrich.RunReclassify,
		search.JobReindex:        m.Search.RunReindex,
		search.JobVectorBackfill: m.Search.RunBackfill,
	}
	for _, e := range m.ext {
		for k, v := range e.jobHandlers() {
			hs[k] = v
		}
	}
	return hs
}

func (m *modules) tick(ctx context.Context) {
	for _, e := range m.ext {
		e.tick(ctx)
	}
}

// wireAPI hands module route mounters to the API (set by later modules).
func (m *modules) wireAPI(a *api.API) {
	for _, w := range apiWirers {
		w(m, a)
	}
}

var apiWirers []func(m *modules, a *api.API)
