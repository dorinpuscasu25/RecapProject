package app

import (
	"context"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/factcheck"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
)

type factcheckExt struct {
	m   *modules
	svc *factcheck.Service
}

func init() {
	extensionCtors = append(extensionCtors, func(ctx context.Context, a *App, m *modules) (extension, error) {
		m.FactCheck = factcheck.NewService(a.DB, m.AI, m.Search, m.Policy, a.Log)
		return &factcheckExt{m: m, svc: m.FactCheck}, nil
	})
	apiWirers = append(apiWirers, func(m *modules, a *api.API) {
		a.Mods.FactCheck = &factcheck.Handler{Svc: m.FactCheck, Lang: a.Lang, SSRF: a.Sources.SSRFPolicy()}
	})
}

func (f *factcheckExt) consumers() []*bus.Consumer {
	return []*bus.Consumer{{Name: "fact-checker", Types: []string{bus.ClaimVerificationRequested, bus.ContentDeleted},
		Handle: f.svc.HandleVerification, Concurrency: 2, Timeout: 5 * time.Minute, MaxAttempts: 8}}
}

func (f *factcheckExt) jobHandlers() map[string]jobs.Handler { return nil }

// tick is a no-op: reopened claims publish claim.verification.requested when flagged.
func (f *factcheckExt) tick(ctx context.Context) {}
