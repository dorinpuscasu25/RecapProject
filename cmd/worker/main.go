// Command worker runs the outbox relay, message consumers, durable jobs, mail delivery and schedulers.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dorinpuscasu25/recapproject/internal/api"
	"github.com/dorinpuscasu25/recapproject/internal/app"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/logx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/telemetry"
)

func main() {
	log := logx.New("newsintel-worker", os.Stdout)
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := telemetry.Setup(ctx, cfg.ServiceName+"-worker", cfg.OTLPEndpoint)
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
	metricsAddr := os.Getenv("WORKER_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9091"
	}
	ms := &http.Server{Addr: metricsAddr, Handler: api.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := ms.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener failed", "error", err)
		}
	}()
	log.Info("worker started", "bus_driver", cfg.BusDriver)
	_ = a.RunWorker(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ms.Shutdown(sctx)
	log.Info("worker stopped")
}
