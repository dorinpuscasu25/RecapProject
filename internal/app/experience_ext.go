package app

import (
	"context"

	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/feed"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
)

type feedExt struct{}

func (feedExt) consumers() []*bus.Consumer           { return nil }
func (feedExt) jobHandlers() map[string]jobs.Handler { return nil }
func (feedExt) tick(context.Context)                 {}

func init() {
	extensionCtors = append(extensionCtors, func(ctx context.Context, a *App, m *modules) (extension, error) {
		var cache feed.Cache = feed.NewMemoryCache()
		if a.Redis != nil {
			cache = feed.RedisCache{C: a.Redis}
		}
		m.Feed = feed.NewService(a.DB, m.Events, a.Taxonomy, m.Policy, cache)
		return feedExt{}, nil
	})
	apiWirers = append(apiWirers, func(m *modules, a *api.API) {
		a.Mods.Experience = append(a.Mods.Experience, &feed.Handler{Svc: m.Feed, Lang: a.Lang})
	})
}
