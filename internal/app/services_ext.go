package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/briefings"
	"github.com/dorinpuscasu25/recapproject/internal/chat"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/notifications"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
)

// experienceExt wires briefings/audio, grounded chat and notifications. It is registered after
// feed and fact-checking (file order), which it depends on.
type experienceExt struct {
	a  *App
	br *briefings.Service
	nt *notifications.Service
}

func init() {
	extensionCtors = append(extensionCtors, func(ctx context.Context, a *App, m *modules) (extension, error) {
		if m.Feed == nil || m.FactCheck == nil {
			return nil, errors.New("experience modules require feed and fact-check modules")
		}
		m.Briefings = briefings.NewService(a.DB, m.AI, m.Events, m.Feed, m.Policy, a.Store, a.Limiter, a.Log, a.Cfg.StorageURLTTL)
		m.Chat = chat.NewService(a.DB, m.AI, m.Events, m.FactCheck, m.Search, m.Policy, a.Limiter)
		m.Notifications = notifications.NewService(a.DB, m.Feed, m.Events, m.Policy, a.Sealer, a.Cfg.Auth.CodeHMACSecret, pushProviders(a), a.Log)
		return &experienceExt{a: a, br: m.Briefings, nt: m.Notifications}, nil
	})
	apiWirers = append(apiWirers, func(m *modules, a *api.API) {
		a.Mods.Experience = append(a.Mods.Experience,
			&briefings.Handler{Svc: m.Briefings},
			&chat.Handler{Svc: m.Chat, Lang: a.Lang},
			&notifications.Handler{Svc: m.Notifications})
	})
}

// pushProviders builds the configured push adapters. Anything not configured is an explicit
// Unconfigured provider, so deliveries record provider_unavailable instead of pretending to send.
func pushProviders(a *App) map[string]notifications.PushProvider {
	out := map[string]notifications.PushProvider{"apns": notifications.Unconfigured{Provider: "apns"}, "fcm": notifications.Unconfigured{Provider: "fcm"}}
	drivers := strings.Split(a.Cfg.PushDriver, ",")
	for _, d := range drivers {
		switch strings.TrimSpace(d) {
		case "apns":
			p, err := notifications.NewAPNs(a.Cfg.APNSKeyID, a.Cfg.APNSTeamID, a.Cfg.APNSTopic, a.Cfg.APNSKeyPEM, a.Cfg.APNSProduction)
			if err != nil {
				a.Log.Warn("APNs not configured; iOS pushes will be recorded as provider_unavailable", "error", err)
				continue
			}
			out["apns"] = p
		case "fcm":
			p, err := notifications.NewFCM(a.Cfg.FCMProjectID, a.Cfg.FCMServiceAccount)
			if err != nil {
				a.Log.Warn("FCM not configured; Android pushes will be recorded as provider_unavailable", "error", err)
				continue
			}
			out["fcm"] = p
		}
	}
	return out
}

func (e *experienceExt) consumers() []*bus.Consumer {
	return []*bus.Consumer{
		{Name: "notification-event-updates", Types: []string{bus.EventUpdated}, Handle: e.nt.HandleEventUpdated, Concurrency: 1, MaxAttempts: 8},
		{Name: "notification-sender", Types: []string{bus.NotificationRequested}, Handle: e.nt.HandleRequested, Concurrency: 4, Timeout: time.Minute, MaxAttempts: 6},
	}
}

func (e *experienceExt) jobHandlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{briefings.JobGenerate: e.br.RunGenerate, briefings.JobAudio: e.br.RunAudio}
}

func (e *experienceExt) tick(ctx context.Context) {
	if n, err := e.nt.Tick(ctx); err != nil {
		e.a.Log.Error("notification scheduler tick failed", "error", err)
	} else if n > 0 {
		e.a.Log.Info("notification schedules processed", "count", n)
	}
}

// AIGateway exposes the gateway (operational tooling and tests swap providers through it).
func (a *App) AIGateway() *ai.Gateway { return a.mods.AI }

// NotificationService exposes the notification module (scheduler tooling and tests).
func (a *App) NotificationService() *notifications.Service { return a.mods.Notifications }
