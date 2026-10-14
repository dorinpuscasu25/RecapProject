// Command api serves the public, consumer and admin REST API.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // IANA zones (Europe/Chisinau) without OS tzdata

	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/app"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/logx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/telemetry"
)

func main() {
	log := logx.New("newsintel-api", os.Stdout)
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	if cfg.Auth.EphemeralKeys {
		log.Warn("AUTH_SIGNING_KEYS not set: using an ephemeral signing key (development only; tokens reset on restart)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := telemetry.Setup(ctx, cfg.ServiceName+"-api", cfg.OTLPEndpoint)
	if err != nil {
		log.Warn("tracing disabled", "error", err)
	}
	defer shutdownTracing(context.Background())

	a, err := app.New(ctx, cfg, log, app.Options{})
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer a.Close()

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 120 * time.Second}
	internal := &http.Server{Addr: cfg.AdminHTTPAddr, Handler: api.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := internal.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener failed", "error", err)
		}
	}()
	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	_ = internal.Shutdown(sctx)
	log.Info("api stopped")
}
