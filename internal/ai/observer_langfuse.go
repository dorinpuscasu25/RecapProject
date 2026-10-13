package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Langfuse forwards execution metadata (task, model, latency, tokens, cost, status) to the
// Langfuse ingestion API. Prompt and completion text are never sent: crawled content and user
// questions stay inside this system. Delivery is best-effort and bounded.
type Langfuse struct {
	Host, PublicKey, SecretKey string
	Client                     *http.Client
	Log                        *slog.Logger
	ch                         chan ExecutionRecord
	done                       chan struct{}
	once                       sync.Once
}

func NewLangfuse(host, public, secret string, log *slog.Logger) *Langfuse {
	l := &Langfuse{Host: strings.TrimSuffix(host, "/"), PublicKey: public, SecretKey: secret, Log: log,
		Client: &http.Client{Timeout: 10 * time.Second}, ch: make(chan ExecutionRecord, 1024), done: make(chan struct{})}
	go l.loop()
	return l
}

func (l *Langfuse) Observe(_ context.Context, rec ExecutionRecord) {
	select {
	case l.ch <- rec:
	default: // never block the AI path
	}
}

func (l *Langfuse) loop() {
	defer close(l.done)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var batch []ExecutionRecord
	for {
		select {
		case r, ok := <-l.ch:
			if !ok {
				l.send(batch)
				return
			}
			batch = append(batch, r)
			if len(batch) >= 50 {
				l.send(batch)
				batch = nil
			}
		case <-t.C:
			l.send(batch)
			batch = nil
		}
	}
}

// Events builds the ingestion batch (exported for contract tests).
func (l *Langfuse) Events(batch []ExecutionRecord, now time.Time) []map[string]any {
	var events []map[string]any
	for _, r := range batch {
		end := now.UTC()
		start := end.Add(-time.Duration(r.LatencyMS) * time.Millisecond)
		level := "DEFAULT"
		if r.Status != "succeeded" {
			level = "ERROR"
		}
		trace := r.TraceID
		if trace == "" {
			trace = r.ID.String()
		}
		events = append(events, map[string]any{
			"id": r.ID.String() + "-trace", "type": "trace-create", "timestamp": end.Format(time.RFC3339Nano),
			"body": map[string]any{"id": trace, "name": string(r.Task), "metadata": map[string]any{"subject_type": r.SubjectType, "subject_id": r.SubjectID}},
		}, map[string]any{
			"id": r.ID.String(), "type": "generation-create", "timestamp": end.Format(time.RFC3339Nano),
			"body": map[string]any{"id": r.ID.String(), "traceId": trace, "name": string(r.Task), "model": r.ModelRef,
				"startTime": start.Format(time.RFC3339Nano), "endTime": end.Format(time.RFC3339Nano), "level": level, "statusMessage": r.ErrorCat,
				"usage":    map[string]any{"input": r.InputTokens, "output": r.OutputTokens, "unit": "TOKENS", "totalCost": r.CostUSD},
				"metadata": map[string]any{"mode": r.Mode, "status": r.Status}},
		})
	}
	return events
}

func (l *Langfuse) send(batch []ExecutionRecord) {
	if len(batch) == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"batch": l.Events(batch, time.Now())})
	req, err := http.NewRequest(http.MethodPost, l.Host+"/api/public/ingestion", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.SetBasicAuth(l.PublicKey, l.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.Client.Do(req)
	if err != nil {
		l.Log.Warn("langfuse export failed", "records", len(batch), "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusMultiStatus {
		l.Log.Warn("langfuse export rejected", "status", fmt.Sprint(resp.StatusCode), "records", len(batch))
	}
}

// Close flushes pending records.
func (l *Langfuse) Close() {
	l.once.Do(func() { close(l.ch) })
	<-l.done
}
