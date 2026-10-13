package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
)

var (
	msgProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "bus_messages_processed_total", Help: "Messages processed by consumer and outcome.",
	}, []string{"consumer", "type", "outcome"})
	outboxLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bus_outbox_unpublished", Help: "Unpublished outbox rows at last relay tick.",
	})
)

func init() { prometheus.MustRegister(msgProcessed, outboxLag) }

// PostgresDriver delivers outbox messages to in-process consumers through message_deliveries.
type PostgresDriver struct {
	DB           *db.DB
	Log          *slog.Logger
	PollInterval time.Duration
	Batch        int
	Lease        time.Duration
	WorkerID     string
}

func (p *PostgresDriver) defaults() {
	if p.PollInterval == 0 {
		p.PollInterval = 250 * time.Millisecond
	}
	if p.Batch == 0 {
		p.Batch = 50
	}
	if p.Lease == 0 {
		p.Lease = 5 * time.Minute
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
}

// Run starts the relay and one loop per consumer; returns when ctx is cancelled.
func (p *PostgresDriver) Run(ctx context.Context, consumers []*Consumer) error {
	p.defaults()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.loop(ctx, func() (int, error) { return p.RelayOnce(ctx, consumers) })
	}()
	for _, c := range consumers {
		n := c.Concurrency
		if n <= 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(c *Consumer) {
				defer wg.Done()
				p.loop(ctx, func() (int, error) { return p.ConsumeOnce(ctx, c) })
			}(c)
		}
	}
	wg.Wait()
	return ctx.Err()
}

func (p *PostgresDriver) loop(ctx context.Context, step func() (int, error)) {
	for ctx.Err() == nil {
		n, err := step()
		if err != nil && ctx.Err() == nil {
			p.Log.Error("bus loop step failed", "error", err)
		}
		if n == 0 || err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.PollInterval):
			}
		}
	}
}

// RelayOnce fans unpublished outbox rows out to subscribed consumers. Exposed for tests/CLI.
func (p *PostgresDriver) RelayOnce(ctx context.Context, consumers []*Consumer) (int, error) {
	p.defaults()
	n := 0
	err := p.DB.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, event_type FROM outbox_events
			WHERE published_at IS NULL AND available_at <= now()
			ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, p.Batch)
		if err != nil {
			return err
		}
		type row struct {
			id  uuid.UUID
			typ string
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.typ); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range batch {
			for _, c := range consumers {
				if !c.wants(r.typ) {
					continue
				}
				if _, err := tx.Exec(ctx, `INSERT INTO message_deliveries (message_id, consumer) VALUES ($1,$2) ON CONFLICT DO NOTHING`, r.id, c.Name); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE id = $1`, r.id); err != nil {
				return err
			}
		}
		n = len(batch)
		return nil
	})
	if err == nil {
		var pending int64
		_ = p.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending)
		outboxLag.Set(float64(pending))
	}
	return n, err
}

type claimed struct {
	msg      Message
	attempts int
}

// ConsumeOnce claims and processes up to Batch deliveries for consumer c.
func (p *PostgresDriver) ConsumeOnce(ctx context.Context, c *Consumer) (int, error) {
	p.defaults()
	rows, err := p.DB.Pool.Query(ctx, `
		WITH next AS (
			SELECT message_id FROM message_deliveries
			WHERE consumer = $1 AND (
			      (status IN ('queued','failed') AND next_attempt_at <= now())
			   OR (status = 'processing' AND locked_until < now()))
			ORDER BY next_attempt_at
			LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE message_deliveries d
		SET status = 'processing', attempts = d.attempts + 1, locked_until = now() + $3::interval, updated_at = now()
		FROM next, outbox_events o
		WHERE d.message_id = next.message_id AND d.consumer = $1 AND o.id = d.message_id
		RETURNING o.id, o.topic, o.event_type, o.partition_key, o.aggregate_type, o.aggregate_id, o.created_at,
		          o.payload, o.headers, d.attempts`,
		c.Name, 1, fmt.Sprintf("%d seconds", int(p.Lease.Seconds())))
	if err != nil {
		return 0, err
	}
	var batch []claimed
	for rows.Next() {
		var cl claimed
		var headers []byte
		if err := rows.Scan(&cl.msg.ID, &cl.msg.Topic, &cl.msg.Type, &cl.msg.Key, &cl.msg.AggregateType,
			&cl.msg.AggregateID, &cl.msg.OccurredAt, &cl.msg.Payload, &headers, &cl.attempts); err != nil {
			rows.Close()
			return 0, err
		}
		_ = json.Unmarshal(headers, &cl.msg.Headers)
		batch = append(batch, cl)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, cl := range batch {
		p.process(ctx, c, cl)
	}
	return len(batch), nil
}

func (p *PostgresDriver) process(ctx context.Context, c *Consumer, cl claimed) {
	d := &Delivery{Message: cl.msg, Consumer: c.Name, Attempt: cl.attempts, db: p.DB}
	hctx, cancel := context.WithTimeout(ctx, c.timeout())
	err := safeHandle(hctx, c.Handle, d)
	cancel()
	log := p.Log.With("consumer", c.Name, "type", cl.msg.Type, "message_id", cl.msg.ID, "attempt", cl.attempts)
	if err == nil {
		msgProcessed.WithLabelValues(c.Name, cl.msg.Type, "succeeded").Inc()
		_, uerr := p.DB.Pool.Exec(ctx, `UPDATE message_deliveries SET status='succeeded', locked_until=NULL, last_error=NULL, updated_at=now()
			WHERE message_id=$1 AND consumer=$2`, cl.msg.ID, c.Name)
		if uerr != nil {
			log.Error("mark delivery succeeded", "error", uerr)
		}
		return
	}
	var perm *Permanent
	if errors.As(err, &perm) || cl.attempts >= c.maxAttempts() {
		msgProcessed.WithLabelValues(c.Name, cl.msg.Type, "dead_lettered").Inc()
		log.Error("message dead-lettered", "error", err)
		_ = p.DB.Tx(ctx, func(tx pgx.Tx) error {
			if err := deadLetter(ctx, tx, c.Name, cl.msg, cl.attempts, err); err != nil {
				return err
			}
			_, e := tx.Exec(ctx, `UPDATE message_deliveries SET status='dead', locked_until=NULL, last_error=$3, updated_at=now()
				WHERE message_id=$1 AND consumer=$2`, cl.msg.ID, c.Name, truncate(err.Error(), 2000))
			return e
		})
		return
	}
	msgProcessed.WithLabelValues(c.Name, cl.msg.Type, "retry").Inc()
	log.Warn("message processing failed; will retry", "error", err)
	_, _ = p.DB.Pool.Exec(ctx, `UPDATE message_deliveries SET status='failed', locked_until=NULL, last_error=$3,
		next_attempt_at = now() + $4::interval, updated_at=now() WHERE message_id=$1 AND consumer=$2`,
		cl.msg.ID, c.Name, truncate(err.Error(), 2000), fmt.Sprintf("%d milliseconds", Backoff(cl.attempts).Milliseconds()))
}

func safeHandle(ctx context.Context, h Handler, d *Delivery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = PermanentErr(fmt.Errorf("handler panic: %v", r))
		}
	}()
	return h(ctx, d)
}

// Drain runs relay + consumers synchronously until no work remains (tests and fixture CLI).
// Messages that are scheduled for retry in the future are not waited for.
func (p *PostgresDriver) Drain(ctx context.Context, consumers []*Consumer, maxRounds int) error {
	p.defaults()
	for i := 0; i < maxRounds; i++ {
		total := 0
		n, err := p.RelayOnce(ctx, consumers)
		if err != nil {
			return err
		}
		total += n
		for _, c := range consumers {
			for {
				m, err := p.ConsumeOnce(ctx, c)
				if err != nil {
					return err
				}
				total += m
				if m == 0 {
					break
				}
			}
		}
		if total == 0 {
			return nil
		}
	}
	return fmt.Errorf("bus drain did not settle after %d rounds", maxRounds)
}

// ReplayDeadLetter re-enqueues a dead-lettered message for its consumer.
func ReplayDeadLetter(ctx context.Context, d *db.DB, id uuid.UUID) error {
	return d.Tx(ctx, func(tx pgx.Tx) error {
		var consumer string
		var msgID uuid.UUID
		err := tx.QueryRow(ctx, `UPDATE dead_letters SET replayed_at = now() WHERE id=$1 AND replayed_at IS NULL
			RETURNING consumer, message_id`, id).Scan(&consumer, &msgID)
		if err != nil {
			return err
		}
		// Clear dedupe so the consumer reprocesses, then requeue (pg driver) — Kafka driver
		// re-publishes via the outbox copy below.
		if _, err := tx.Exec(ctx, `DELETE FROM consumer_dedupe WHERE consumer=$1 AND message_id=$2`, consumer, msgID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE message_deliveries SET status='queued', attempts=0, next_attempt_at=now(), last_error=NULL
			WHERE message_id=$1 AND consumer=$2`, msgID, consumer)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Kafka mode: re-publish a copy targeted at this consumer.
			_, err = tx.Exec(ctx, `INSERT INTO outbox_events (id, topic, event_type, aggregate_type, aggregate_id, partition_key, payload, headers)
				SELECT $3, topic, event_type, aggregate_type, aggregate_id, partition_key, payload,
				       headers || jsonb_build_object('replay_for', $2::text, 'original_id', $1::text)
				FROM outbox_events WHERE id=$1`, msgID, consumer, uuid.Must(uuid.NewV7()))
		}
		return err
	})
}
