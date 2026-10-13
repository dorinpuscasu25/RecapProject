// Package ratelimit provides fixed-window rate limiting backed by Redis (shared across replicas)
// with an in-memory implementation for single-process development and tests.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type Limiter interface {
	// Allow increments the counter for key and reports whether the request is within limit.
	Allow(ctx context.Context, key string, limit int, window time.Duration) (ok bool, retryAfter time.Duration, err error)
}

// Redis implements Limiter with an atomic INCR + PEXPIRE script.
type Redis struct{ c redis.UniversalClient }

func NewRedis(c redis.UniversalClient) *Redis { return &Redis{c: c} }

var script = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
return {n, ttl}
`)

func (l *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	res, err := script.Run(ctx, l.c, []string{"rl:" + key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return true, 0, err // fail open on cache outage; callers log the error
	}
	if int(res[0]) > limit {
		return false, time.Duration(res[1]) * time.Millisecond, nil
	}
	return true, 0, nil
}

// Memory implements Limiter in-process.
type Memory struct {
	mu      sync.Mutex
	windows map[string]*window
	now     func() time.Time
}

type window struct {
	count int
	reset time.Time
}

func NewMemory() *Memory { return &Memory{windows: map[string]*window{}, now: time.Now} }

func (m *Memory) Allow(_ context.Context, key string, limit int, win time.Duration) (bool, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	w, ok := m.windows[key]
	if !ok || now.After(w.reset) {
		w = &window{reset: now.Add(win)}
		m.windows[key] = w
	}
	w.count++
	if w.count > limit {
		return false, w.reset.Sub(now), nil
	}
	if len(m.windows) > 100_000 { // bound memory
		for k, v := range m.windows {
			if now.After(v.reset) {
				delete(m.windows, k)
			}
		}
	}
	return true, 0, nil
}

// Noop never limits (used only when explicitly disabled in tests).
type Noop struct{}

func (Noop) Allow(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	return true, 0, nil
}
