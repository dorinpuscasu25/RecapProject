// Package logx provides structured JSON logging with correlation ids carried in context.
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyLogger
)

// New returns a JSON slog logger. Level is read from LOG_LEVEL (debug|info|warn|error).
func New(service string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	lvl := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl, ReplaceAttr: redact})
	return slog.New(h).With("service", service)
}

// sensitiveKeys are never written to logs even if a caller accidentally passes them.
var sensitiveKeys = map[string]struct{}{
	"password": {}, "code": {}, "token": {}, "refresh_token": {}, "access_token": {},
	"authorization": {}, "api_key": {}, "secret": {}, "device_token": {}, "otp": {},
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, "[REDACTED]")
	}
	return a
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, l)
}

// From returns the context logger (enriched with request id) or the default logger.
func From(ctx context.Context) *slog.Logger {
	l, ok := ctx.Value(keyLogger).(*slog.Logger)
	if !ok {
		l = slog.Default()
	}
	if id := RequestID(ctx); id != "" {
		return l.With("request_id", id)
	}
	return l
}
