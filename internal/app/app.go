// Package app is the composition root: it builds infrastructure clients and domain services
// from configuration and exposes the HTTP handler, bus consumers, job handlers and schedulers.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/identity"
	"github.com/dorinpuscasu25/recapproject/internal/ingest"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/mail"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ssrf"
	"github.com/dorinpuscasu25/recapproject/internal/platform/storage"
	"github.com/dorinpuscasu25/recapproject/internal/sources"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

type App struct {
	Cfg     *config.Config
	Log     *slog.Logger
	DB      *db.DB
	Redis   redis.UniversalClient
	Limiter ratelimit.Limiter
	Store   storage.Store
	Sealer  *crypto.Sealer
	Mailer  mail.Provider

	Identity *identity.Service
	Taxonomy *taxonomy.Service
	Sources  *sources.Service
	Ingest   *ingest.Service

	mods *modules
	api  *api.API
}

// Options allow tests to override infrastructure.
type Options struct {
	Limiter ratelimit.Limiter
	Mailer  mail.Provider
	Store   storage.Store
}

func New(ctx context.Context, cfg *config.Config, log *slog.Logger, opts Options) (*App, error) {
	d, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return nil, err
	}
	a := &App{Cfg: cfg, Log: log, DB: d}
	a.Sealer, err = crypto.NewSealer(cfg.SecretsEncryptionKey)
	if err != nil {
		return nil, err
	}
	// Redis (optional): rate limiting and caches fall back to in-process when absent.
	switch {
	case opts.Limiter != nil:
		a.Limiter = opts.Limiter
	case cfg.RedisURL != "":
		ro, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("parse REDIS_URL: %w", err)
		}
		a.Redis = redis.NewClient(ro)
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := a.Redis.Ping(pctx).Err(); err != nil {
			log.Warn("redis unavailable; using in-process rate limiter", "error", err)
			a.Limiter = ratelimit.NewMemory()
		} else {
			a.Limiter = ratelimit.NewRedis(a.Redis)
		}
		cancel()
	default:
		a.Limiter = ratelimit.NewMemory()
	}
	// Object storage.
	switch {
	case opts.Store != nil:
		a.Store = opts.Store
	case cfg.StorageDriver == "s3":
		s3, err := storage.NewS3(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3Region, cfg.S3UseSSL)
		if err != nil {
			return nil, fmt.Errorf("object storage: %w", err)
		}
		a.Store = s3
	default:
		a.Store = &storage.Filesystem{Root: cfg.StorageDir}
	}
	// Mail provider.
	switch {
	case opts.Mailer != nil:
		a.Mailer = opts.Mailer
	case cfg.MailDriver == "smtp":
		a.Mailer = &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.MailFrom, StartTLS: cfg.SMTPStartTLS}
	default:
		a.Mailer = mail.DevSink{DB: d}
	}

	policy := ssrf.Policy{AllowPrivate: cfg.CrawlAllowPrivate}
	a.Identity = identity.NewService(d, cfg.Auth, a.Sealer, a.Limiter, log)
	a.Taxonomy = taxonomy.NewService(d)
	a.Sources = sources.NewService(d, policy)
	fetcher := ingest.NewFetcher(policy, cfg.CrawlUserAgent, cfg.FixtureDir, a.Limiter, cfg.CrawlLiveEnabled)
	a.Ingest = ingest.NewService(d, a.Sources, fetcher, a.Store, log)

	a.mods, err = newModules(ctx, a)
	if err != nil {
		return nil, err
	}
	a.api = &api.API{Cfg: cfg, DB: d, Log: log, Identity: a.Identity, Taxonomy: a.Taxonomy, Sources: a.Sources, Ingest: a.Ingest,
		Policy: a.mods.Policy, AI: a.mods.AI, Search: a.mods.Search, Enrich: a.mods.Enrich, Events: a.mods.Events}
	a.mods.wireAPI(a.api)
	return a, nil
}

func (a *App) Close() {
	if a.Redis != nil {
		_ = a.Redis.Close()
	}
	a.DB.Close()
}

// Handler returns the public HTTP handler.
func (a *App) Handler() http.Handler { return a.api.Router() }

// Consumers returns all bus consumers run by the worker.
func (a *App) Consumers() []*bus.Consumer {
	cs := []*bus.Consumer{
		{Name: "ingest-poller", Types: []string{bus.SourcePollRequested}, Handle: a.Ingest.HandlePoll, Concurrency: 2, Timeout: 5 * time.Minute},
		{Name: "ingest-fetcher", Types: []string{bus.ArticleDiscovered}, Handle: a.Ingest.HandleDiscovered, Concurrency: 4},
	}
	return append(cs, a.mods.consumers()...)
}

// JobHandlers returns durable job handlers.
func (a *App) JobHandlers() map[string]jobs.Handler {
	hs := map[string]jobs.Handler{
		ingest.JobReprocess: a.Ingest.RunReprocess,
		identity.JobUserExport: func(ctx context.Context, j *jobs.Job) (any, error) {
			var p struct {
				RequestID string `json:"request_id"`
				UserID    string `json:"user_id"`
			}
			if err := json.Unmarshal(j.Payload, &p); err != nil {
				return nil, jobs.Permanent(err)
			}
			uid, _ := uuid.Parse(p.UserID)
			rid, _ := uuid.Parse(p.RequestID)
			data, err := a.Identity.BuildExport(ctx, uid)
			if err != nil {
				return nil, err
			}
			b, _ := json.MarshalIndent(data, "", "  ")
			key := fmt.Sprintf("exports/%s/%s.json", uid, rid)
			if err := a.Store.Put(ctx, key, b, "application/json"); err != nil {
				return nil, err
			}
			_, err = a.DB.Pool.Exec(ctx, `UPDATE user_data_requests SET status='succeeded', result_object_key=$2, completed_at=now() WHERE id=$1`, rid, key)
			return map[string]string{"object_key": key}, err
		},
		identity.JobUserDeletion: func(ctx context.Context, j *jobs.Job) (any, error) {
			var p struct {
				RequestID string `json:"request_id"`
				UserID    string `json:"user_id"`
			}
			if err := json.Unmarshal(j.Payload, &p); err != nil {
				return nil, jobs.Permanent(err)
			}
			uid, _ := uuid.Parse(p.UserID)
			rid, _ := uuid.Parse(p.RequestID)
			return nil, a.Identity.PurgeUser(ctx, uid, rid)
		},
	}
	for k, v := range a.mods.jobHandlers() {
		hs[k] = v
	}
	return hs
}

// BusDriver returns the configured message driver.
func (a *App) BusDriver() bus.Driver {
	if a.Cfg.BusDriver == "kafka" {
		return &bus.KafkaDriver{DB: a.DB, Brokers: a.Cfg.KafkaBrokers, ClientID: a.Cfg.KafkaClientID, Log: a.Log}
	}
	return &bus.PostgresDriver{DB: a.DB, Log: a.Log}
}

// RunWorker runs the bus, job runner, mail worker and schedulers until ctx is done.
func (a *App) RunWorker(ctx context.Context) error {
	var wg sync.WaitGroup
	run := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Log.Info("worker component started", "component", name)
			fn()
			a.Log.Info("worker component stopped", "component", name)
		}()
	}
	run("bus", func() {
		if err := a.BusDriver().Run(ctx, a.Consumers()); err != nil && ctx.Err() == nil {
			a.Log.Error("bus driver stopped", "error", err)
		}
	})
	runner := &jobs.Runner{DB: a.DB, Log: a.Log, Handlers: a.JobHandlers(), Concurrency: a.Cfg.WorkerConcurrency}
	run("jobs", func() { runner.Run(ctx) })
	mw := &mail.Worker{DB: a.DB, Provider: a.Mailer, Sealer: a.Sealer, Log: a.Log}
	run("mail", func() { loop(ctx, time.Second, func() { _, _ = mw.RunOnce(ctx) }) })
	run("scheduler", func() {
		loop(ctx, a.Cfg.SchedulerInterval, func() {
			if n, err := a.Ingest.ScheduleDue(ctx); err != nil {
				a.Log.Error("source scheduler", "error", err)
			} else if n > 0 {
				a.Log.Info("scheduled source polls", "count", n)
			}
			a.mods.tick(ctx)
		})
	})
	run("cleanup", func() {
		loop(ctx, time.Hour, func() {
			if res, err := a.Identity.CleanupExpired(ctx); err != nil {
				a.Log.Error("ttl cleanup", "error", err)
			} else {
				a.Log.Info("ttl cleanup", "deleted", res)
			}
		})
	})
	wg.Wait()
	return nil
}

func loop(ctx context.Context, every time.Duration, fn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	fn()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// Drain synchronously processes outbox messages, jobs and mail until quiescent (tests, CLI).
func (a *App) Drain(ctx context.Context) error {
	pg := &bus.PostgresDriver{DB: a.DB, Log: a.Log}
	runner := &jobs.Runner{DB: a.DB, Log: a.Log, Handlers: a.JobHandlers()}
	mw := &mail.Worker{DB: a.DB, Provider: a.Mailer, Sealer: a.Sealer, Log: a.Log}
	for i := 0; i < 50; i++ {
		if err := pg.Drain(ctx, a.Consumers(), 100); err != nil {
			return err
		}
		ran, err := runner.RunOnce(ctx)
		if err != nil {
			return err
		}
		n, err := mw.RunOnce(ctx)
		if err != nil {
			return err
		}
		var pending int
		if err := a.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND available_at <= now()`).Scan(&pending); err != nil {
			return err
		}
		if !ran && n == 0 && pending == 0 {
			return nil
		}
	}
	return fmt.Errorf("drain did not settle")
}
