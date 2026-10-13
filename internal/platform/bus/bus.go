// Package bus implements versioned domain messages, the transactional outbox, and two delivery
// drivers: Kafka (production intent) and PostgreSQL (durable in-database queue for local runs).
//
// Delivery is at-least-once. Effectively-once domain effects are achieved by consumers recording
// (consumer, message_id) in consumer_dedupe inside the same transaction as their effects
// (see Delivery.Once).
package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/logx"
)

// Message types (versioned contracts). See docs/event-processing.md.
const (
	SourcePollRequested        = "source.poll.requested.v1"
	ArticleDiscovered          = "article.discovered.v1"
	ArticleFetched             = "article.fetched.v1"
	ArticleExtracted           = "article.extracted.v1"
	ArticleEnrichmentRequested = "article.enrichment.requested.v1"
	ArticleEnriched            = "article.enriched.v1"
	EventResolutionRequested   = "event.resolution.requested.v1"
	EventCreated               = "event.created.v1"
	EventUpdated               = "event.updated.v1"
	ClaimVerificationRequested = "claim.verification.requested.v1"
	ClaimVerdictUpdated        = "claim.verdict.updated.v1"
	SearchIndexRequested       = "search.index.requested.v1"
	BriefingRequested          = "briefing.requested.v1"
	BriefingReady              = "briefing.ready.v1"
	AudioRequested             = "audio.requested.v1"
	NotificationRequested      = "notification.requested.v1"
	ContentDeleted             = "content.deleted.v1"
)

// TopicPrefix is prepended to physical Kafka topic names.
var TopicPrefix = "newsintel"

// topicGroups maps a message type's aggregate prefix to a physical topic. A handful of topics
// grouped by aggregate keeps operational overhead low while the envelope `type` keeps contracts
// explicit.
var topicGroups = map[string]string{
	"source":       "source.v1",
	"article":      "article.v1",
	"event":        "event.v1",
	"claim":        "claim.v1",
	"search":       "search.v1",
	"briefing":     "briefing.v1",
	"audio":        "briefing.v1",
	"notification": "notification.v1",
	"content":      "content.v1",
}

// TopicFor returns the physical topic for a message type.
func TopicFor(msgType string) string {
	agg, _, _ := strings.Cut(msgType, ".")
	g, ok := topicGroups[agg]
	if !ok {
		g = "misc.v1"
	}
	return TopicPrefix + "." + g
}

// AllTopics lists every physical topic (used for provisioning and DLQ creation).
func AllTopics() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range topicGroups {
		t := TopicPrefix + "." + g
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Message is the envelope delivered to consumers.
type Message struct {
	ID            uuid.UUID         `json:"id"`
	Type          string            `json:"type"`
	Topic         string            `json:"topic"`
	Key           string            `json:"key"`
	AggregateType string            `json:"aggregate_type"`
	AggregateID   string            `json:"aggregate_id"`
	OccurredAt    time.Time         `json:"occurred_at"`
	Payload       json.RawMessage   `json:"payload"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// Decode unmarshals the payload.
func (m Message) Decode(v any) error {
	if err := json.Unmarshal(m.Payload, v); err != nil {
		return fmt.Errorf("decode %s payload: %w", m.Type, err)
	}
	return nil
}

// Draft is a message to be written to the outbox.
type Draft struct {
	Type          string
	AggregateType string
	AggregateID   string
	Key           string // partition key; defaults to AggregateID
	Payload       any
	Delay         time.Duration
}

// Publish writes a message to the transactional outbox using q (normally the caller's tx).
func Publish(ctx context.Context, q db.Querier, d Draft) (uuid.UUID, error) {
	payload, err := json.Marshal(d.Payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal %s: %w", d.Type, err)
	}
	key := d.Key
	if key == "" {
		key = d.AggregateID
	}
	headers := map[string]string{"schema": d.Type}
	if rid := logx.RequestID(ctx); rid != "" {
		headers["request_id"] = rid
	}
	hb, _ := json.Marshal(headers)
	id := ids.New()
	_, err = q.Exec(ctx, `INSERT INTO outbox_events
		(id, topic, event_type, aggregate_type, aggregate_id, partition_key, payload, headers, available_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now() + $9::interval)`,
		id, TopicFor(d.Type), d.Type, d.AggregateType, d.AggregateID, key, payload, hb,
		fmt.Sprintf("%d milliseconds", d.Delay.Milliseconds()))
	return id, err
}

// Handler processes one message. Return a *Permanent error to dead-letter immediately.
type Handler func(ctx context.Context, d *Delivery) error

// Consumer subscribes a handler to message types under a stable consumer-group name.
type Consumer struct {
	Name        string
	Types       []string
	Handle      Handler
	MaxAttempts int           // default 6
	Concurrency int           // default 1
	Timeout     time.Duration // per message; default 2m
}

func (c *Consumer) maxAttempts() int {
	if c.MaxAttempts <= 0 {
		return 6
	}
	return c.MaxAttempts
}

func (c *Consumer) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 2 * time.Minute
	}
	return c.Timeout
}

func (c *Consumer) wants(t string) bool {
	for _, x := range c.Types {
		if x == t {
			return true
		}
	}
	return false
}

// Delivery wraps a message with idempotency helpers.
type Delivery struct {
	Message  Message
	Consumer string
	Attempt  int
	db       *db.DB
}

// Once runs fn in a transaction that also records the message as processed by this consumer.
// If the message was already processed, fn is not called and nil is returned.
func (d *Delivery) Once(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return d.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO consumer_dedupe (consumer, message_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			d.Consumer, d.Message.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // duplicate delivery
		}
		return fn(tx)
	})
}

// AlreadyProcessed reports whether this consumer has already recorded the message.
func (d *Delivery) AlreadyProcessed(ctx context.Context) (bool, error) {
	var exists bool
	err := d.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM consumer_dedupe WHERE consumer=$1 AND message_id=$2)`,
		d.Consumer, d.Message.ID).Scan(&exists)
	return exists, err
}

// Permanent marks an error as non-retryable (poison message → dead letter).
type Permanent struct{ Err error }

func (p *Permanent) Error() string { return "permanent: " + p.Err.Error() }
func (p *Permanent) Unwrap() error { return p.Err }

func PermanentErr(err error) error { return &Permanent{Err: err} }

// Backoff returns exponential backoff with a cap.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second * time.Duration(1<<min(attempt, 10))
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// Driver runs the relay and consumers.
type Driver interface {
	Run(ctx context.Context, consumers []*Consumer) error
}

// deadLetter persists a poison message for inspection and replay.
func deadLetter(ctx context.Context, q db.Querier, consumer string, m Message, attempts int, cause error) error {
	hb, _ := json.Marshal(m.Headers)
	_, err := q.Exec(ctx, `INSERT INTO dead_letters (id, consumer, message_id, topic, event_type, payload, headers, error, attempts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (consumer, message_id) DO UPDATE SET error = EXCLUDED.error, attempts = EXCLUDED.attempts, replayed_at = NULL`,
		ids.New(), consumer, m.ID, m.Topic, m.Type, []byte(m.Payload), hb, truncate(cause.Error(), 2000), attempts)
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
