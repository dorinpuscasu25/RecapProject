package analytics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type memSink struct {
	mu     sync.Mutex
	events []Event
}

func (m *memSink) Name() string { return "mem" }
func (m *memSink) Write(_ context.Context, b []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, b...)
	return nil
}

func TestRecorderPseudonymousAndFlushOnClose(t *testing.T) {
	s := &memSink{}
	r := NewRecorder(s, []byte("0123456789abcdef0123456789abcdef"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	u := uuid.New()
	for i := 0; i < 5; i++ {
		r.Track("api_request", u, map[string]any{"route": "/v1/feed"})
	}
	r.Close()
	r.Track("after_close", u, nil) // must not panic
	if len(s.events) != 5 {
		t.Fatalf("flushed %d events", len(s.events))
	}
	if h := s.events[0].SubjectHash; h == "" || strings.Contains(h, u.String()) || h != r.Subject(u) {
		t.Fatalf("subject must be a stable pseudonym: %q", h)
	}
}

func TestClickHouseContract(t *testing.T) {
	var query, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	c := ClickHouse{URL: srv.URL + "/?database=newsintel"}
	err := c.Write(context.Background(), []Event{{ID: uuid.New(), Name: "a", Properties: map[string]any{"k": 1}}, {ID: uuid.New(), Name: "b"}})
	if err != nil || query != "INSERT INTO analytics_events FORMAT JSONEachRow" || strings.Count(body, "\n") != 2 || !strings.Contains(body, `"event_name":"a"`) {
		t.Fatalf("query=%q body=%q err=%v", query, body, err)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer failing.Close()
	if err := (ClickHouse{URL: failing.URL}).Write(context.Background(), []Event{{ID: uuid.New(), Name: "x"}}); err == nil {
		t.Fatal("server errors must surface")
	}
}
