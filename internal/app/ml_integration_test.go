package app_test

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

// Go ↔ Python ML service over real HTTP. Runs when IT_ML_URL points at a running services/ml
// instance (any backend; CI uses ML_EMBEDDING_BACKEND=hash with ML_EMBEDDING_DIMS=1024):
//
//	IT_ML_URL=http://localhost:8000 go test ./internal/app/ -run MLService -v
func TestMLServiceEmbeddingsAndRerank(t *testing.T) {
	url := os.Getenv("IT_ML_URL")
	if url == "" {
		t.Skip("set IT_ML_URL to run the ML service integration test")
	}
	if resp, err := http.Get(strings.TrimSuffix(url, "/") + "/readyz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("ML service not ready at %s: %v", url, err)
	}
	e := testutil.NewEnv(t, testutil.WithMLService(url))
	admin := e.Admin("ml-admin@example.com", "super_admin")
	_, ro := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	_, ru := e.SetupSource(admin, testutil.FixtureSource{Name: "Новости Пример", Domain: "novosti-primer.md", Feed: "novosti-primer.xml", Languages: []string{"ru"}})
	e.Crawl(admin, ro)
	e.Crawl(admin, ru)

	// Every article and event was embedded by the ML service into 1024-dim collections.
	cols := e.MustStatus(e.Do("GET", "/v1/admin/search/collections", admin, nil), 200)
	if !strings.Contains(string(cols.Raw), `"bge-m3"`) || !strings.Contains(string(cols.Raw), `"dims":1024`) {
		t.Fatalf("collections should use bge-m3: %s", cols.Raw)
	}
	var ok, failed int
	if err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FILTER (WHERE status='succeeded'), count(*) FILTER (WHERE status<>'succeeded')
		FROM ai_executions WHERE provider_slug='local-ml' AND model_slug='bge-m3'`).Scan(&ok, &failed); err != nil {
		t.Fatal(err)
	}
	if ok == 0 || failed != 0 {
		t.Fatalf("bge-m3 executions: ok=%d failed=%d", ok, failed)
	}

	// Search with reranking goes through the cross-encoder route.
	user, _, _ := e.RegisterVerified("ml-user@example.com")
	r := e.MustStatus(e.Do("GET", "/v1/search?q=tarif%20energie%20electric%C4%83&rerank=true", user, nil), 200)
	if len(r.Items()) == 0 {
		t.Fatalf("search: %s", r.Raw)
	}
	var reranks int
	if err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM ai_executions WHERE task='reranking' AND model_slug='multilingual-reranker'
		AND status='succeeded'`).Scan(&reranks); err != nil {
		t.Fatal(err)
	}
	if reranks == 0 {
		t.Fatal("rerank route was not used")
	}
}
