package app

import (
	"context"

	"github.com/go-chi/chi/v5"

	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
)

// modules groups the intelligence/experience modules (AI, enrichment, events, fact-checking,
// search, personalization, briefings, chat, notifications).
type modules struct {
	app *App
}

func newModules(ctx context.Context, a *App) (*modules, error) {
	return &modules{app: a}, nil
}

func (m *modules) MountConsumer(r chi.Router) {}
func (m *modules) MountAdmin(r chi.Router)    {}
func (m *modules) consumers() []*bus.Consumer { return nil }
func (m *modules) jobHandlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{}
}
func (m *modules) tick(ctx context.Context) {}
