// Package analytics records product usage events. Recording never blocks or fails a request:
// events go through a bounded buffer and are dropped (and counted) when the sink lags. Subjects
// are pseudonymous (HMAC of the user id); properties must never contain free text a user typed.
package analytics

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
)

type Event struct {
	ID          uuid.UUID      `json:"id"`
	Name        string         `json:"event_name"`
	SubjectHash string         `json:"subject_hash,omitempty"`
	Properties  map[string]any `json:"properties"`
	OccurredAt  time.Time      `json:"occurred_at"`
}

// Sink persists a batch of events.
type Sink interface {
	Write(ctx context.Context, batch []Event) error
	Name() string
}

type Noop struct{}

func (Noop) Write(context.Context, []Event) error { return nil }
func (Noop) Name() string                         { return "noop" }

// Postgres writes to the bounded local analytics_events table.
type Postgres struct{ DB *db.DB }

func (p Postgres) Name() string { return "postgres" }
func (p Postgres) Write(ctx context.Context, batch []Event) error {
	for _, e := range batch {
		props, _ := json.Marshal(e.Properties)
		if _, err := p.DB.Pool.Exec(ctx, `INSERT INTO analytics_events (id, event_name, subject_hash, properties, occurred_at) VALUES ($1,$2,nullif($3,''),$4,$5)`,
			e.ID, e.Name, e.SubjectHash, props, e.OccurredAt); err != nil {
			return err
		}
	}
	return nil
}

// ClickHouse inserts via the HTTP interface (JSONEachRow). Expected table:
//
//	CREATE TABLE analytics_events (id UUID, event_name LowCardinality(String), subject_hash String,
//	  properties String, occurred_at DateTime64(3, 'UTC')) ENGINE = MergeTree ORDER BY (event_name, occurred_at)
type ClickHouse struct {
	URL    string // e.g. http://user:pass@localhost:8123/?database=newsintel
	Table  string
	Client *http.Client
}

func (c ClickHouse) Name() string { return "clickhouse" }
func (c ClickHouse) Write(ctx context.Context, batch []Event) error {
	var buf bytes.Buffer
	for _, e := range batch {
		props, _ := json.Marshal(e.Properties)
		row, _ := json.Marshal(map[string]any{"id": e.ID, "event_name": e.Name, "subject_hash": e.SubjectHash, "properties": string(props),
			"occurred_at": e.OccurredAt.UTC().Format("2006-01-02 15:04:05.000")})
		buf.Write(row)
		buf.WriteByte('\n')
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return err
	}
	q := u.Query()
	table := c.Table
	if table == "" {
		table = "analytics_events"
	}
	q.Set("query", "INSERT INTO "+table+" FORMAT JSONEachRow")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &buf)
	if err != nil {
		return err
	}
	cl := c.Client
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("clickhouse insert: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

var (
	dropped = promauto.NewCounter(prometheus.CounterOpts{Name: "analytics_events_dropped_total", Help: "Analytics events dropped because the buffer was full or the sink failed."})
	written = promauto.NewCounter(prometheus.CounterOpts{Name: "analytics_events_written_total", Help: "Analytics events written."})
)

// Recorder buffers events and flushes them asynchronously.
type Recorder struct {
	sink    Sink
	hashKey []byte
	log     *slog.Logger
	ch      chan Event
	done    chan struct{}
	once    sync.Once
	mu      sync.RWMutex // guards ch against send-after-close
	closed  bool
	every   time.Duration
}

func NewRecorder(sink Sink, hashKey []byte, log *slog.Logger) *Recorder {
	r := &Recorder{sink: sink, hashKey: hashKey, log: log, ch: make(chan Event, 2048), done: make(chan struct{}), every: 2 * time.Second}
	go r.loop()
	return r
}

// Subject returns the pseudonymous subject for a user id.
func (r *Recorder) Subject(user uuid.UUID) string {
	if user == uuid.Nil {
		return ""
	}
	return hex.EncodeToString(crypto.HMAC(r.hashKey, "analytics-subject", user.String()))[:32]
}

// Track enqueues an event; it never blocks.
func (r *Recorder) Track(name string, user uuid.UUID, props map[string]any) {
	if r == nil {
		return
	}
	if props == nil {
		props = map[string]any{}
	}
	e := Event{ID: ids.New(), Name: name, SubjectHash: r.Subject(user), Properties: props, OccurredAt: time.Now().UTC()}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.ch <- e:
	default:
		dropped.Inc()
	}
}

func (r *Recorder) loop() {
	defer close(r.done)
	t := time.NewTicker(r.every)
	defer t.Stop()
	var batch []Event
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.sink.Write(ctx, batch); err != nil {
			dropped.Add(float64(len(batch)))
			r.log.Warn("analytics sink write failed; batch dropped", "sink", r.sink.Name(), "events", len(batch), "error", err)
		} else {
			written.Add(float64(len(batch)))
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 200 {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// Close flushes buffered events.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.ch)
		r.mu.Unlock()
	})
	<-r.done
}
