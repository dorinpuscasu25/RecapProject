package db

import (
	"context"
	"log/slog"
	"time"
)

// ConfigChannel carries cross-process cache invalidations for database-managed configuration
// (AI registry, policies and flags, roles and account status). Payload = scope.
const ConfigChannel = "newsintel_config"

// NotifyConfig announces a configuration change. Call it with the writing transaction so the
// notification is delivered only if the change commits.
func NotifyConfig(ctx context.Context, q Querier, scope string) error {
	_, err := q.Exec(ctx, `SELECT pg_notify($1, $2)`, ConfigChannel, scope)
	return err
}

// ListenConfig delivers configuration-change scopes to fn until ctx ends. After (re)connecting
// it calls fn("*") because notifications sent while disconnected are lost.
func (d *DB) ListenConfig(ctx context.Context, log *slog.Logger, fn func(scope string)) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := d.Pool.Acquire(ctx)
		if err == nil {
			_, err = conn.Exec(ctx, `LISTEN `+ConfigChannel)
			if err == nil {
				fn("*")
				backoff = time.Second
				for {
					n, werr := conn.Conn().WaitForNotification(ctx)
					if werr != nil {
						err = werr
						break
					}
					fn(n.Payload)
				}
			}
			conn.Release()
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("config listener disconnected; retrying", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
