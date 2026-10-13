package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/config"
	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

// Scenario 12 against real infrastructure. Runs only when the services are provided:
//
//	IT_KAFKA_BROKERS=localhost:9092 IT_QDRANT_URL=http://localhost:6333 IT_OPENSEARCH_URL=http://localhost:9200 go test ./internal/app/ -run Infra
func TestInfraKafkaRedeliveryAndIndexRebuild(t *testing.T) {
	brokers, qdrant, osURL := os.Getenv("IT_KAFKA_BROKERS"), os.Getenv("IT_QDRANT_URL"), os.Getenv("IT_OPENSEARCH_URL")
	if brokers == "" || qdrant == "" || osURL == "" {
		t.Skip("set IT_KAFKA_BROKERS, IT_QDRANT_URL and IT_OPENSEARCH_URL to run the infrastructure integration test")
	}
	run := strings.ToLower(crypto.RandomToken(6))
	run = strings.NewReplacer("-", "x", "_", "x").Replace(run)
	bus.TopicPrefix = "it" + run
	e := testutil.NewEnv(t, testutil.WithConfig(func(c *config.Config) {
		c.BusDriver = "kafka"
		c.KafkaBrokers = strings.Split(brokers, ",")
		c.KafkaClientID = "it-" + run
		c.QdrantURL = qdrant
		c.QdrantPrefix = "it_" + run + "_"
		c.OpenSearchURL = osURL
		c.OpenSearchPrefix = "it-" + run
		c.SchedulerInterval = 500 * time.Millisecond
	}))
	ctx, cancel := context.WithCancel(e.Ctx)
	done := make(chan struct{})
	go func() { _ = e.App.RunWorker(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		cleanupInfra(osURL, qdrant, "it-"+run, "it_"+run+"_")
	})

	count := func(q string) int {
		var n int
		if err := e.App.DB.Pool.QueryRow(e.Ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitFor := func(what string, timeout time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for !cond() {
			if time.Now().After(deadline) {
				var dl string
				_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT coalesce(string_agg(consumer||': '||error, ' | '),'') FROM dead_letters`).Scan(&dl)
				t.Fatalf("timed out waiting for %s (dead letters: %s)", what, dl)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	admin := e.Admin("infra-admin@example.com", "super_admin")
	_, ro := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	_, ru := e.SetupSource(admin, testutil.FixtureSource{Name: "Новости Пример", Domain: "novosti-primer.md", Feed: "novosti-primer.xml", Languages: []string{"ru"}})
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+ro+"/crawl", admin, nil), 202)
	e.MustStatus(e.Do("POST", "/v1/admin/sources/"+ru+"/crawl", admin, nil), 202)
	waitFor("articles to be enriched and linked through Kafka", 90*time.Second, func() bool {
		return count(`SELECT count(*) FROM articles`) > 0 && count(`SELECT count(*) FROM articles WHERE status NOT IN ('enriched','duplicate','quarantined')`) == 0 &&
			count(`SELECT count(*) FROM articles a WHERE a.status='enriched' AND NOT EXISTS (SELECT 1 FROM event_article_links l WHERE l.article_id=a.id)`) == 0
	})
	lag := func() int64 {
		cl, err := kgo.NewClient(kgo.SeedBrokers(strings.Split(brokers, ",")...))
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		var groups []string
		for _, c := range e.App.Consumers() {
			groups = append(groups, bus.TopicPrefix+"."+c.Name)
		}
		lags, err := kadm.NewClient(cl).Lag(e.Ctx, groups...)
		if err != nil {
			return -1
		}
		var total int64
		for _, l := range lags.Sorted() {
			if l.Error() != nil {
				return -1
			}
			total += l.Lag.Total()
		}
		return total
	}
	settled := func() bool {
		return count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0 && lag() == 0
	}
	waitFor("consumers to settle", 60*time.Second, settled)

	snapshot := func() string {
		return fmt.Sprintf("articles=%d versions=%d events=%d links=%d claims=%d occurrences=%d updates=%d",
			count(`SELECT count(*) FROM articles`), count(`SELECT count(*) FROM article_versions`), count(`SELECT count(*) FROM events`),
			count(`SELECT count(*) FROM event_article_links`), count(`SELECT count(*) FROM claims`), count(`SELECT count(*) FROM claim_occurrences`),
			count(`SELECT count(*) FROM event_updates`))
	}
	before := snapshot()

	// At-least-once redelivery: every outbox message is relayed to Kafka again with its original ID.
	n := count(`WITH r AS (UPDATE outbox_events SET published_at=NULL RETURNING 1) SELECT count(*) FROM r`)
	if n < 10 {
		t.Fatalf("expected a meaningful number of messages to redeliver, got %d", n)
	}
	waitFor("redelivered messages to be consumed", 90*time.Second, settled)
	if after := snapshot(); after != before {
		t.Fatalf("redelivery duplicated state:\nbefore %s\nafter  %s", before, after)
	}
	if dl := count(`SELECT count(*) FROM dead_letters`); dl != 0 {
		t.Fatalf("redelivery produced %d dead letters", dl)
	}

	// Search runs on OpenSearch + Qdrant (not degraded).
	user, _, _ := e.RegisterVerified("infra-reader@example.com")
	search := func() *testutil.Resp { return e.Do("GET", "/v1/search?q=tarif%20energie%20electric%C4%83", user, nil) }
	waitFor("OpenSearch to serve results", 30*time.Second, func() bool { r := search(); return r.Status == 200 && len(r.Items()) > 0 })
	if r := search(); strings.Contains(string(r.Raw), "postgres_fts") {
		t.Fatalf("search should not be degraded: %s", r.Raw)
	}

	// Lose the derived indices entirely, then rebuild them from canonical PostgreSQL records.
	cleanupInfra(osURL, qdrant, "it-"+run, "it_"+run+"_")
	if r := search(); r.Status == 200 && len(r.Items()) > 0 && !strings.Contains(string(r.Raw), "degraded") {
		t.Fatalf("search unexpectedly healthy after index loss: %s", r.Raw)
	}
	job := e.MustStatus(e.Do("POST", "/v1/admin/search/reindex", admin, map[string]any{}), 202)
	waitFor("reindex job", 60*time.Second, func() bool {
		j := e.Do("GET", "/v1/admin/jobs/"+job.Str("job_id"), admin, nil)
		if j.Str("status") != "succeeded" {
			t.Log("reindex job:", string(j.Raw))
		}
		return j.Str("status") == "succeeded"
	})
	waitFor("rebuilt indices to serve results", 30*time.Second, func() bool {
		r := search()
		return r.Status == 200 && len(r.Items()) > 0 && !strings.Contains(string(r.Raw), "degraded\":[\"")
	})
	// Vectors were rebuilt too: every active Qdrant collection holds points again.
	rows, err := e.App.DB.Pool.Query(e.Ctx, `SELECT collection_name FROM vector_collections WHERE status='active'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	points := 0
	for _, n := range names {
		resp, err := http.Get(strings.TrimSuffix(qdrant, "/") + "/collections/" + n)
		if err != nil {
			t.Fatal(err)
		}
		var info struct {
			Result struct {
				PointsCount int `json:"points_count"`
			} `json:"result"`
		}
		_ = jsonDecode(resp, &info)
		resp.Body.Close()
		points += info.Result.PointsCount
	}
	if len(names) == 0 || points == 0 {
		t.Fatalf("qdrant collections not rebuilt: %v points=%d", names, points)
	}
	cols := e.MustStatus(e.Do("GET", "/v1/admin/search/collections", admin, nil), 200)
	if !strings.Contains(string(cols.Raw), `"active"`) {
		t.Fatalf("collections: %s", cols.Raw)
	}
}

// cleanupInfra deletes the test's OpenSearch indices and Qdrant collections.
func cleanupInfra(osURL, qdrant, osPrefix, qPrefix string) {
	do := func(method, url string) {
		req, _ := http.NewRequest(method, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	do(http.MethodDelete, strings.TrimSuffix(osURL, "/")+"/"+osPrefix+"-*")
	resp, err := http.Get(strings.TrimSuffix(qdrant, "/") + "/collections")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var body struct {
		Result struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"result"`
	}
	_ = jsonDecode(resp, &body)
	for _, c := range body.Result.Collections {
		if strings.HasPrefix(c.Name, qPrefix) {
			do(http.MethodDelete, strings.TrimSuffix(qdrant, "/")+"/collections/"+c.Name)
		}
	}
}

func jsonDecode(resp *http.Response, v any) error { return json.NewDecoder(resp.Body).Decode(v) }
