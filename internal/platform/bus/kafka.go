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
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
)

// KafkaDriver relays the outbox to Kafka and runs consumer groups.
type KafkaDriver struct {
	DB           *db.DB
	Brokers      []string
	ClientID     string
	Log          *slog.Logger
	PollInterval time.Duration
	Batch        int
	Partitions   int32
	Replication  int16
}

func (k *KafkaDriver) defaults() {
	if k.PollInterval == 0 {
		k.PollInterval = 250 * time.Millisecond
	}
	if k.Batch == 0 {
		k.Batch = 200
	}
	if k.Partitions == 0 {
		k.Partitions = 6
	}
	if k.Replication == 0 {
		k.Replication = 1
	}
	if k.Log == nil {
		k.Log = slog.Default()
	}
}

// EnsureTopics creates main and DLQ topics if missing.
func (k *KafkaDriver) EnsureTopics(ctx context.Context) error {
	k.defaults()
	cl, err := kgo.NewClient(kgo.SeedBrokers(k.Brokers...), kgo.ClientID(k.ClientID+"-admin"))
	if err != nil {
		return err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	var topics []string
	for _, t := range AllTopics() {
		topics = append(topics, t, t+".dlq")
	}
	res, err := adm.CreateTopics(ctx, k.Partitions, k.Replication, map[string]*string{"retention.ms": ptr("604800000")}, topics...)
	if err != nil {
		return err
	}
	for _, r := range res.Sorted() {
		if r.Err != nil && !errors.Is(r.Err, kerr.TopicAlreadyExists) {
			return fmt.Errorf("create topic %s: %w", r.Topic, r.Err)
		}
	}
	return nil
}

func ptr(s string) *string { return &s }

func (k *KafkaDriver) Run(ctx context.Context, consumers []*Consumer) error {
	k.defaults()
	if err := k.EnsureTopics(ctx); err != nil {
		k.Log.Warn("ensure topics failed (continuing)", "error", err)
	}
	producer, err := kgo.NewClient(kgo.SeedBrokers(k.Brokers...), kgo.ClientID(k.ClientID+"-relay"),
		kgo.RequiredAcks(kgo.AllISRAcks()), kgo.ProducerLinger(5*time.Millisecond))
	if err != nil {
		return err
	}
	defer producer.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			n, err := k.RelayOnce(ctx, producer)
			if err != nil && ctx.Err() == nil {
				k.Log.Error("kafka relay failed", "error", err)
			}
			if n == 0 || err != nil {
				select {
				case <-ctx.Done():
				case <-time.After(k.PollInterval):
				}
			}
		}
	}()
	for _, c := range consumers {
		wg.Add(1)
		go func(c *Consumer) {
			defer wg.Done()
			if err := k.consume(ctx, c, producer); err != nil && ctx.Err() == nil {
				k.Log.Error("kafka consumer stopped", "consumer", c.Name, "error", err)
			}
		}(c)
	}
	wg.Wait()
	return ctx.Err()
}

// RelayOnce publishes a batch of outbox rows and marks them published only after broker acks.
func (k *KafkaDriver) RelayOnce(ctx context.Context, producer *kgo.Client) (int, error) {
	k.defaults()
	n := 0
	err := k.DB.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, topic, event_type, partition_key, aggregate_type, aggregate_id, created_at, payload, headers
			FROM outbox_events WHERE published_at IS NULL AND available_at <= now()
			ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, k.Batch)
		if err != nil {
			return err
		}
		var recs []*kgo.Record
		var idList []uuid.UUID
		for rows.Next() {
			var m Message
			var headers []byte
			if err := rows.Scan(&m.ID, &m.Topic, &m.Type, &m.Key, &m.AggregateType, &m.AggregateID, &m.OccurredAt, &m.Payload, &headers); err != nil {
				rows.Close()
				return err
			}
			_ = json.Unmarshal(headers, &m.Headers)
			env, _ := json.Marshal(m)
			recs = append(recs, &kgo.Record{
				Topic: m.Topic, Key: []byte(m.Key), Value: env,
				Headers: []kgo.RecordHeader{{Key: "type", Value: []byte(m.Type)}, {Key: "message_id", Value: []byte(m.ID.String())}},
			})
			idList = append(idList, m.ID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		if err := producer.ProduceSync(ctx, recs...).FirstErr(); err != nil {
			return fmt.Errorf("produce: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE id = ANY($1)`, idList)
		n = len(recs)
		return err
	})
	return n, err
}

func (k *KafkaDriver) consume(ctx context.Context, c *Consumer, producer *kgo.Client) error {
	topicSet := map[string]bool{}
	for _, t := range c.Types {
		topicSet[TopicFor(t)] = true
	}
	var topics []string
	for t := range topicSet {
		topics = append(topics, t)
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(k.Brokers...), kgo.ClientID(k.ClientID+"-"+c.Name),
		kgo.ConsumerGroup(TopicPrefix+"."+c.Name), kgo.ConsumeTopics(topics...),
		kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(500*time.Millisecond))
	if err != nil {
		return err
	}
	defer cl.Close()
	for ctx.Err() == nil {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() {
			return nil
		}
		fetches.EachError(func(t string, p int32, err error) {
			if ctx.Err() == nil {
				k.Log.Warn("kafka fetch error", "topic", t, "partition", p, "error", err)
			}
		})
		var done []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			var m Message
			if err := json.Unmarshal(r.Value, &m); err != nil {
				k.Log.Error("undecodable kafka record; skipping", "topic", r.Topic, "offset", r.Offset)
				done = append(done, r)
				return
			}
			if !c.wants(m.Type) {
				done = append(done, r)
				return
			}
			if rf := m.Headers["replay_for"]; rf != "" && rf != c.Name {
				done = append(done, r)
				return
			}
			if orig := m.Headers["original_id"]; orig != "" {
				if id, err := uuid.Parse(orig); err == nil {
					m.ID = id // replay keeps the original idempotency key (dedupe row was cleared)
				}
			}
			k.handleWithRetry(ctx, c, m, producer)
			done = append(done, r)
		})
		if len(done) > 0 && ctx.Err() == nil {
			if err := cl.CommitRecords(ctx, done...); err != nil {
				k.Log.Warn("commit offsets failed", "consumer", c.Name, "error", err)
			}
		}
	}
	return nil
}

func (k *KafkaDriver) handleWithRetry(ctx context.Context, c *Consumer, m Message, producer *kgo.Client) {
	for attempt := 1; ; attempt++ {
		d := &Delivery{Message: m, Consumer: c.Name, Attempt: attempt, db: k.DB}
		hctx, cancel := context.WithTimeout(ctx, c.timeout())
		err := safeHandle(hctx, c.Handle, d)
		cancel()
		if err == nil {
			msgProcessed.WithLabelValues(c.Name, m.Type, "succeeded").Inc()
			return
		}
		var perm *Permanent
		if errors.As(err, &perm) || attempt >= c.maxAttempts() || ctx.Err() != nil {
			msgProcessed.WithLabelValues(c.Name, m.Type, "dead_lettered").Inc()
			k.Log.Error("message dead-lettered", "consumer", c.Name, "type", m.Type, "message_id", m.ID, "error", err)
			if derr := deadLetter(context.WithoutCancel(ctx), k.DB.Pool, c.Name, m, attempt, err); derr != nil {
				k.Log.Error("persist dead letter failed", "error", derr)
			}
			env, _ := json.Marshal(m)
			producer.Produce(context.WithoutCancel(ctx), &kgo.Record{Topic: m.Topic + ".dlq", Key: []byte(m.Key), Value: env,
				Headers: []kgo.RecordHeader{{Key: "consumer", Value: []byte(c.Name)}, {Key: "error", Value: []byte(truncate(err.Error(), 500))}}}, nil)
			return
		}
		msgProcessed.WithLabelValues(c.Name, m.Type, "retry").Inc()
		// Bounded in-consumer backoff keeps partition order; long outages end in the DLQ.
		wait := time.Duration(attempt*attempt) * 500 * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
