// Package audit records append-only audit entries for security-critical, configuration and
// editorial actions. Entries never contain secrets or message bodies.
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/logx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

type Entry struct {
	Action       string
	ResourceType string
	ResourceID   string
	Old          any
	New          any
}

// redactedFields are removed from old/new snapshots before persistence.
var redactedFields = []string{"password_hash", "api_key", "api_key_enc", "token_enc", "code_hmac", "key_hash", "payload_enc"}

// Record writes an audit entry using q (pass the transaction to make it atomic with the change).
func Record(ctx context.Context, q db.Querier, e Entry) error {
	p := principal.From(ctx)
	actorType, actorID := "system", (*uuid.UUID)(nil)
	ipPrefix := ""
	if p != nil && p.Kind != principal.KindSystem {
		actorType = string(p.Kind)
		id := p.ID
		actorID = &id
		ipPrefix = p.IPPrefix
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_logs
		(id, actor_type, actor_id, action, resource_type, resource_id, old_value, new_value, request_id, ip_prefix)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		ids.New(), actorType, actorID, e.Action, e.ResourceType, e.ResourceID,
		snapshot(e.Old), snapshot(e.New), logx.RequestID(ctx), ipPrefix)
	return err
}

func snapshot(v any) []byte {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		for _, f := range redactedFields {
			if _, ok := m[f]; ok {
				m[f] = "[REDACTED]"
			}
		}
		b, _ = json.Marshal(m)
	}
	return b
}

type Log struct {
	ID           uuid.UUID       `json:"id"`
	ActorType    string          `json:"actor_type"`
	ActorID      *uuid.UUID      `json:"actor_id,omitempty"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   *string         `json:"resource_id,omitempty"`
	OldValue     json.RawMessage `json:"old_value,omitempty"`
	NewValue     json.RawMessage `json:"new_value,omitempty"`
	RequestID    *string         `json:"request_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

type Filter struct {
	ResourceType string
	ResourceID   string
	ActorID      *uuid.UUID
	Before       *time.Time
	Limit        int
}

func List(ctx context.Context, q db.Querier, f Filter) ([]Log, error) {
	rows, err := q.Query(ctx, `SELECT id, actor_type, actor_id, action, resource_type, resource_id, old_value, new_value, request_id, created_at
		FROM audit_logs
		WHERE ($1 = '' OR resource_type = $1) AND ($2 = '' OR resource_id = $2)
		  AND ($3::uuid IS NULL OR actor_id = $3) AND ($4::timestamptz IS NULL OR created_at < $4)
		ORDER BY created_at DESC LIMIT $5`, f.ResourceType, f.ResourceID, f.ActorID, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Log
	for rows.Next() {
		var l Log
		if err := rows.Scan(&l.ID, &l.ActorType, &l.ActorID, &l.Action, &l.ResourceType, &l.ResourceID, &l.OldValue, &l.NewValue, &l.RequestID, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
