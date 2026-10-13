// Package mail implements transactional email: an encrypted outbox (email_deliveries), a delivery
// worker with retry/backoff/dead-lettering, provider adapters (SMTP, dev sink), and localized
// templates. One-time codes are encrypted at rest and purged once sent or expired.
package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
)

// Message is a rendered email.
type Message struct {
	To             string
	Subject        string
	Text           string
	IdempotencyKey string
}

// Provider sends a rendered message. Implementations must be safe for concurrent use.
type Provider interface {
	Name() string
	Send(ctx context.Context, m Message) (providerMessageID string, err error)
}

// PermanentError indicates the message can never be delivered (e.g. invalid recipient).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Email is an outbox request.
type Email struct {
	UserID         *uuid.UUID
	To             string
	Template       string
	Locale         string
	Data           map[string]string
	IdempotencyKey string
	ExpiresAt      *time.Time // do not send after this time (e.g. code expiry)
}

// Enqueue writes an encrypted delivery row in the caller's transaction.
func Enqueue(ctx context.Context, q db.Querier, sealer *crypto.Sealer, e Email) error {
	if _, ok := templates[e.Template]; !ok {
		return fmt.Errorf("unknown email template %q", e.Template)
	}
	b, err := json.Marshal(e.Data)
	if err != nil {
		return err
	}
	if e.IdempotencyKey == "" {
		e.IdempotencyKey = ids.New().String()
	}
	_, err = q.Exec(ctx, `INSERT INTO email_deliveries (id, idempotency_key, user_id, to_address, template, locale, payload_enc, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (idempotency_key) DO NOTHING`,
		ids.New(), e.IdempotencyKey, e.UserID, e.To, e.Template, e.Locale, sealer.Seal(b), e.ExpiresAt)
	return err
}

var deliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "email_deliveries_total", Help: "Email delivery outcomes by template.",
}, []string{"template", "outcome"})

func init() { prometheus.MustRegister(deliveries) }

// Worker delivers queued emails.
type Worker struct {
	DB       *db.DB
	Provider Provider
	Sealer   *crypto.Sealer
	Log      *slog.Logger
	Batch    int
}

// RunOnce processes up to Batch due deliveries and returns how many were attempted.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	if w.Batch == 0 {
		w.Batch = 20
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	// Expire undeliverable time-bound messages first (codes past expiry must not be sent).
	if _, err := w.DB.Pool.Exec(ctx, `UPDATE email_deliveries SET status='expired', payload_enc=NULL
		WHERE status IN ('queued','failed') AND expires_at IS NOT NULL AND expires_at < now()`); err != nil {
		return 0, err
	}
	type item struct {
		id                    uuid.UUID
		key, to, tmpl, locale string
		payload               []byte
		attempts, maxAttempts int
	}
	var items []item
	rows, err := w.DB.Pool.Query(ctx, `
		WITH next AS (
			SELECT id FROM email_deliveries
			WHERE (status IN ('queued','failed') AND next_attempt_at <= now())
			   OR (status = 'sending' AND locked_until < now())
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		UPDATE email_deliveries e SET status='sending', attempts=e.attempts+1, locked_until=now()+interval '2 minutes'
		FROM next WHERE e.id = next.id
		RETURNING e.id, e.idempotency_key, e.to_address, e.template, e.locale, e.payload_enc, e.attempts, e.max_attempts`, w.Batch)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.key, &it.to, &it.tmpl, &it.locale, &it.payload, &it.attempts, &it.maxAttempts); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, it := range items {
		err := w.deliver(ctx, it.key, it.to, it.tmpl, it.locale, it.payload)
		log := w.Log.With("delivery_id", it.id, "template", it.tmpl, "attempt", it.attempts)
		switch {
		case err == nil:
			deliveries.WithLabelValues(it.tmpl, "sent").Inc()
		case isPermanent(err) || it.attempts >= it.maxAttempts:
			deliveries.WithLabelValues(it.tmpl, "dead").Inc()
			log.Error("email delivery dead-lettered", "error", err)
			_, _ = w.DB.Pool.Exec(ctx, `UPDATE email_deliveries SET status='dead', payload_enc=NULL, last_error=$2, locked_until=NULL WHERE id=$1`,
				it.id, trunc(err.Error()))
		default:
			deliveries.WithLabelValues(it.tmpl, "retry").Inc()
			backoff := time.Duration(1<<min(it.attempts, 8)) * 5 * time.Second
			log.Warn("email delivery failed; retrying", "error", err, "backoff", backoff.String())
			_, _ = w.DB.Pool.Exec(ctx, `UPDATE email_deliveries SET status='failed', last_error=$2, locked_until=NULL,
				next_attempt_at = now() + $3::interval WHERE id=$1`, it.id, trunc(err.Error()), fmt.Sprintf("%d milliseconds", backoff.Milliseconds()))
		}
		if err == nil {
			// handled inside deliver
			continue
		}
	}
	return len(items), nil
}

func (w *Worker) deliver(ctx context.Context, key, to, tmpl, locale string, sealed []byte) error {
	if sealed == nil {
		return &PermanentError{Err: errors.New("payload missing")}
	}
	plain, err := w.Sealer.Open(sealed)
	if err != nil {
		return &PermanentError{Err: fmt.Errorf("decrypt payload: %w", err)}
	}
	var data map[string]string
	if err := json.Unmarshal(plain, &data); err != nil {
		return &PermanentError{Err: err}
	}
	subject, body, err := Render(tmpl, locale, data)
	if err != nil {
		return &PermanentError{Err: err}
	}
	pid, err := w.Provider.Send(ctx, Message{To: to, Subject: subject, Text: body, IdempotencyKey: key})
	if err != nil {
		return err
	}
	_, err = w.DB.Pool.Exec(ctx, `UPDATE email_deliveries SET status='sent', sent_at=now(), payload_enc=NULL, provider=$2,
		provider_message_id=$3, last_error=NULL, locked_until=NULL WHERE idempotency_key=$1`, key, w.Provider.Name(), pid)
	return err
}

func isPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

func trunc(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}

// DevSink stores messages in dev_mail_messages for local inspection. Refused in production by config.
type DevSink struct{ DB *db.DB }

func (DevSink) Name() string { return "devsink" }

func (d DevSink) Send(ctx context.Context, m Message) (string, error) {
	id := ids.New()
	_, err := d.DB.Pool.Exec(ctx, `INSERT INTO dev_mail_messages (id, to_address, subject, body_text) VALUES ($1,$2,$3,$4)`,
		id, m.To, m.Subject, m.Text)
	return id.String(), err
}

// DevMessage is a dev-sink email.
type DevMessage struct {
	ID        uuid.UUID `json:"id"`
	To        string    `json:"to"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// DevMailbox lists dev-sink messages for an address (newest first).
func DevMailbox(ctx context.Context, q db.Querier, to string, limit int) ([]DevMessage, error) {
	rows, err := q.Query(ctx, `SELECT id, to_address, subject, body_text, created_at FROM dev_mail_messages
		WHERE lower(to_address) = lower($1) ORDER BY created_at DESC LIMIT $2`, strings.TrimSpace(to), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DevMessage, error) {
		var m DevMessage
		err := r.Scan(&m.ID, &m.To, &m.Subject, &m.Body, &m.CreatedAt)
		return m, err
	})
}
