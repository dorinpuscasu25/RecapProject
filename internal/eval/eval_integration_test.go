package eval_test

import (
	"path/filepath"
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/eval"
	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

// Quality gates for the default (rule-based) routes. A route change that regresses below a gate
// fails here; per-case results are persisted and visible through the admin API.
func TestEvalSuitesMeetGates(t *testing.T) {
	e := testutil.NewEnv(t)
	r := &eval.Runner{DB: e.App.DB, GW: e.App.AIGateway(), Tax: e.App.Taxonomy, Dir: filepath.Join(testutil.RepoRoot(), "fixtures", "eval")}
	for _, s := range []string{"classification", "claims", "answers", "retrieval"} {
		rep, err := r.Run(e.Ctx, s, nil)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if !rep.OK() {
			t.Errorf("%s gates failed: %v (metrics %v, failed cases %v)", s, rep.GateFails, rep.Metrics, rep.Failures)
		}
		t.Logf("%s on %s: %d/%d cases, metrics %v", s, rep.Model, rep.Passed, rep.Cases, rep.Metrics)
	}
	admin := e.Admin("eval-admin@example.com", "analyst")
	runs := e.MustStatus(e.Do("GET", "/v1/admin/ai/evaluations?suite=answers", admin, nil), 200)
	if len(runs.Items()) != 1 {
		t.Fatalf("eval runs: %s", runs.Raw)
	}
	id := runs.Items()[0].(map[string]any)["id"].(string)
	res := e.MustStatus(e.Do("GET", "/v1/admin/ai/evaluations/"+id, admin, nil), 200)
	if len(res.Get("results").([]any)) != 7 {
		t.Fatalf("eval results: %s", res.Raw)
	}
	user, _, _ := e.RegisterVerified("eval-user@example.com")
	e.MustStatus(e.Do("GET", "/v1/admin/ai/evaluations", user, nil), 403)
}
