// Package policy loads database-managed editorial/scoring policies and feature flags with a short
// cache, and updates them with optimistic concurrency and audit.
package policy

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

type Store struct {
	db  *db.DB
	mu  sync.RWMutex
	c   map[string]cached
	ttl time.Duration
}

type cached struct {
	raw []byte
	ver int
	at  time.Time
}

func New(d *db.DB) *Store { return &Store{db: d, c: map[string]cached{}, ttl: 15 * time.Second} }

// Get decodes policy key into dst. Missing policies leave dst unchanged (callers set defaults).
func (s *Store) Get(ctx context.Context, key string, dst any) (int, error) {
	s.mu.RLock()
	c, ok := s.c[key]
	s.mu.RUnlock()
	if !ok || time.Since(c.at) > s.ttl {
		err := s.db.Pool.QueryRow(ctx, `SELECT value, version FROM editorial_policies WHERE key=$1`, key).Scan(&c.raw, &c.ver)
		if db.IsNoRows(err) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		c.at = time.Now()
		s.mu.Lock()
		s.c[key] = c
		s.mu.Unlock()
	}
	return c.ver, json.Unmarshal(c.raw, dst)
}

type Policy struct {
	Key         string          `json:"key"`
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
	Version     int             `json:"version"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func (s *Store) List(ctx context.Context) ([]Policy, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT key, value, description, version, updated_at FROM editorial_policies ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Policy, error) {
		var p Policy
		err := r.Scan(&p.Key, &p.Value, &p.Description, &p.Version, &p.UpdatedAt)
		return p, err
	})
}

// Put replaces a policy value; version must match the current version (409 otherwise).
func (s *Store) Put(ctx context.Context, key string, value json.RawMessage, version int) (*Policy, error) {
	if !json.Valid(value) || len(value) == 0 || value[0] != '{' {
		return nil, apperr.ValidationMsg("policy_invalid", "Policy value must be a JSON object.")
	}
	var out Policy
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var old json.RawMessage
		var cur int
		err := tx.QueryRow(ctx, `SELECT value, version FROM editorial_policies WHERE key=$1 FOR UPDATE`, key).Scan(&old, &cur)
		if db.IsNoRows(err) {
			return apperr.NotFound("policy_not_found", "Policy not found.")
		}
		if err != nil {
			return err
		}
		if cur != version {
			return apperr.VersionConflict("policy")
		}
		var by any
		if p := principal.From(ctx); p != nil && p.Kind == principal.KindUser {
			by = p.ID
		}
		if err := tx.QueryRow(ctx, `UPDATE editorial_policies SET value=$2, version=version+1, updated_by=$3, updated_at=now() WHERE key=$1
			RETURNING key, value, description, version, updated_at`, key, value, by).Scan(&out.Key, &out.Value, &out.Description, &out.Version, &out.UpdatedAt); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "policy.update", ResourceType: "policy", ResourceID: key, Old: map[string]any{"value": old}, New: map[string]any{"value": value}})
	})
	s.mu.Lock()
	delete(s.c, key)
	s.mu.Unlock()
	return &out, err
}

// FlagEnabled evaluates a feature flag with deterministic percentage rollout per subject.
func (s *Store) FlagEnabled(ctx context.Context, key, subject string) bool {
	var enabled bool
	var pct int
	if err := s.db.Pool.QueryRow(ctx, `SELECT enabled, rollout_percent FROM feature_flags WHERE key=$1`, key).Scan(&enabled, &pct); err != nil {
		return false
	}
	return Rollout(enabled, pct, key, subject)
}

// Rollout is the pure bucketing function (unit-tested).
func Rollout(enabled bool, pct int, key, subject string) bool {
	if !enabled || pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	h := fnv.New32a()
	h.Write([]byte(key + ":" + subject))
	return int(h.Sum32()%100) < pct
}

type Flag struct {
	Key            string    `json:"key"`
	Enabled        bool      `json:"enabled"`
	RolloutPercent int       `json:"rollout_percent"`
	Description    string    `json:"description"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (s *Store) Flags(ctx context.Context) ([]Flag, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT key, enabled, rollout_percent, description, updated_at FROM feature_flags ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Flag, error) {
		var f Flag
		err := r.Scan(&f.Key, &f.Enabled, &f.RolloutPercent, &f.Description, &f.UpdatedAt)
		return f, err
	})
}

func (s *Store) SetFlag(ctx context.Context, key string, enabled bool, pct int) error {
	if pct < 0 || pct > 100 {
		return apperr.ValidationMsg("flag_invalid", "rollout_percent must be 0..100")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE feature_flags SET enabled=$2, rollout_percent=$3, updated_at=now() WHERE key=$1`, key, enabled, pct)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.NotFound("flag_not_found", "Feature flag not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "feature_flag.update", ResourceType: "feature_flag", ResourceID: key, New: map[string]any{"enabled": enabled, "rollout_percent": pct}})
	})
}
