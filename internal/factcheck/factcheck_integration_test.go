package factcheck_test

import (
	"strings"
	"testing"

	"github.com/dorinpuscasu25/recapproject/internal/testutil"
)

func claimIDByQuote(t *testing.T, e *testutil.Env, fragment string) string {
	t.Helper()
	var id string
	if err := e.App.DB.Pool.QueryRow(e.Ctx, `SELECT claim_id::text FROM claim_occurrences WHERE quote_text LIKE '%'||$1||'%' LIMIT 1`, fragment).Scan(&id); err != nil {
		var all string
		_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT coalesce(string_agg(quote_text, ' || '),'') FROM claim_occurrences`).Scan(&all)
		t.Fatalf("no claim containing %q (claims: %s): %v", fragment, all, err)
	}
	return id
}

// Scenarios 6, 7, 8: unverified is not false, refuted is gated by review, the monitor shows
// evidence and caution, corrections keep history.
func TestFactCheckingWorkflow(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin("factcheck-admin@example.com", "super_admin")
	_, rapid := e.SetupSource(admin, testutil.FixtureSource{Name: "Portal Rapid", Domain: "rapid-portal.md", Feed: "rapid-portal.xml", Languages: []string{"ro"}})
	_, mf := e.SetupSource(admin, testutil.FixtureSource{Name: "Ministerul Finanțelor exemplu", Domain: "mf-exemplu.md", Type: "government", Feed: "mf-exemplu.xml", FetchPages: true, Languages: []string{"ro"}})
	_, ro := e.SetupSource(admin, testutil.FixtureSource{Name: "Știri Exemplu", Domain: "stiri-exemplu.md", Feed: "stiri-exemplu.xml", Languages: []string{"ro"}})
	_, off := e.SetupSource(admin, testutil.FixtureSource{Name: "ANRE exemplu", Domain: "anre-exemplu.md", Type: "government", Feed: "anre-exemplu.xml", FetchPages: true, Languages: []string{"ro"}})
	e.Crawl(admin, rapid)
	e.Crawl(admin, mf)
	e.Crawl(admin, off)
	e.Crawl(admin, ro)
	// Re-verify after all evidence exists (new evidence reopens claims automatically in production).
	pension := claimIDByQuote(t, e, "reducerea pensiilor cu 30% pentru toți pensionarii")
	airport := claimIDByQuote(t, e, "aeroport internațional")
	tariff := claimIDByQuote(t, e, "3,05 lei la 3,51 lei")
	for _, c := range []string{pension, airport, tariff} {
		e.MustStatus(e.Do("POST", "/v1/admin/claims/"+c+"/reverify", admin, nil), 200)
	}
	e.Drain()
	user, _, _ := e.RegisterVerified("reader-fc@example.com")

	// Scenario 6a: no corroboration → unverified (not false).
	fc := e.MustStatus(e.Do("GET", "/v1/fact-checks/"+airport+"?lang=ro", user, nil), 200)
	if v := fc.Str("fact_check", "verdict"); v != "unverified" {
		t.Fatalf("uncorroborated claim must be unverified, got %s: %s", v, fc.Raw)
	}
	// Satire produces no checkable claims.
	var satireClaims int
	_ = e.App.DB.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM claim_occurrences o JOIN articles a ON a.id=o.article_id WHERE a.title LIKE 'Pamflet%'`).Scan(&satireClaims)
	if satireClaims != 0 {
		t.Fatalf("satire must not yield claims, got %d", satireClaims)
	}

	// Supported: official primary source with matching figures.
	fc = e.MustStatus(e.Do("GET", "/v1/fact-checks/"+tariff, user, nil), 200)
	if v := fc.Str("fact_check", "verdict"); v != "supported" {
		dossier := e.MustStatus(e.Do("GET", "/v1/admin/claims/"+tariff, admin, nil), 200)
		t.Fatalf("tariff claim should be supported by the official source, got %s; dossier: %s", v, dossier.Raw)
	}

	// Scenario 6b: direct contrary evidence → proposed refuted, but readers see unverified + pending_review.
	fc = e.MustStatus(e.Do("GET", "/v1/fact-checks/"+pension, user, nil), 200)
	if fc.Str("fact_check", "verdict") != "unverified" || fc.Str("fact_check", "review_status") != "pending_review" {
		dossier := e.MustStatus(e.Do("GET", "/v1/admin/claims/"+pension, admin, nil), 200)
		t.Fatalf("refutation must be held for review: %s\ndossier: %s", fc.Raw, dossier.Raw)
	}
	if strings.Contains(string(fc.Raw), `"relation":"contradicts"`) {
		t.Fatal("unreviewed negative evidence relation must not be exposed")
	}
	// Ordinary users cannot review.
	e.MustStatus(e.Do("GET", "/v1/admin/fact-checks/queue", user, nil), 403)
	q := e.MustStatus(e.Do("GET", "/v1/admin/fact-checks/queue", admin, nil), 200)
	var reviewID string
	for _, it := range q.Items() {
		m := it.(map[string]any)
		if m["claim_id"] == pension {
			reviewID = m["id"].(string)
			if m["proposed_verdict"] != "refuted" {
				t.Fatalf("proposed verdict: %v", m)
			}
		}
	}
	if reviewID == "" {
		t.Fatalf("pension claim missing from review queue: %s", q.Raw)
	}
	// A fact-checker without publish permission cannot publish a refuted label alone.
	fcUser := e.Admin("checker@example.com", "fact_checker")
	r := e.MustStatus(e.Do("POST", "/v1/admin/fact-checks/"+reviewID+"/decision", fcUser, map[string]any{"decision": "accept",
		"rationale": "Official denial by the Ministry contradicts the claim directly."}), 200)
	if r.Str("status") != "in_progress" {
		t.Fatalf("decision should await publication: %s", r.Raw)
	}
	if v := e.MustStatus(e.Do("GET", "/v1/fact-checks/"+pension, user, nil), 200).Str("fact_check", "verdict"); v != "unverified" {
		t.Fatalf("still unpublished, readers must see unverified, got %s", v)
	}
	e.MustStatus(e.Do("POST", "/v1/admin/fact-checks/"+reviewID+"/publish", admin, nil), 204)
	fc = e.MustStatus(e.Do("GET", "/v1/fact-checks/"+pension+"?lang=ro", user, nil), 200)
	if fc.Str("fact_check", "verdict") != "refuted" || fc.Str("fact_check", "review_status") != "editor_approved" {
		t.Fatalf("published refutation: %s", fc.Raw)
	}
	ev := fc.Get("fact_check", "evidence").([]any)
	hasPrimary := false
	for _, x := range ev {
		m := x.(map[string]any)
		if m["relation"] == "contradicts" && m["primary_source"] == true && m["url"] != "" {
			hasPrimary = true
		}
	}
	if !hasPrimary {
		t.Fatalf("reader must see the contradicting primary evidence: %s", fc.Raw)
	}

	// Scenario 7: misinformation monitor shows claim, review status, evidence, dates and why caution.
	tr := e.MustStatus(e.Do("GET", "/v1/misinformation/trends?lang=ro", user, nil), 200)
	claims := tr.Get("claims").([]any)
	if len(claims) == 0 {
		t.Fatalf("monitor should list the reviewed refuted claim: %s", tr.Raw)
	}
	top := claims[0].(map[string]any)
	if top["claim_id"] != pension || top["why_caution"] == "" || top["review_status"] != "editor_approved" || len(top["evidence"].([]any)) == 0 {
		t.Fatalf("monitor entry incomplete: %v", top)
	}
	if strings.Contains(strings.ToLower(string(tr.Raw)), "fake media") {
		t.Fatal("monitor must not label outlets")
	}

	// Scenario 8: correction with visible history.
	e.MustStatus(e.Do("POST", "/v1/admin/claims/"+pension+"/correct", admin, map[string]any{"verdict": "refuted",
		"explanation":     map[string]string{"ro": "Ministerul a confirmat oficial că nu există nicio decizie de reducere a pensiilor."},
		"correction_note": map[string]string{"ro": "Explicația a fost actualizată cu referința la comunicatul oficial."},
		"rationale":       "Clarified explanation"}), 200)
	fc = e.MustStatus(e.Do("GET", "/v1/fact-checks/"+pension+"?lang=ro", user, nil), 200)
	if fc.Str("fact_check", "correction", "ro") == "" || !strings.Contains(fc.Str("fact_check", "explanation"), "confirmat oficial") {
		t.Fatalf("correction not visible: %s", fc.Raw)
	}
	hist := fc.Get("history").([]any)
	if len(hist) < 3 {
		t.Fatalf("history must keep previous verdicts: %s", fc.Raw)
	}
	first := hist[0].(map[string]any)
	if first["verdict"] != "unverified" {
		t.Fatalf("first public version should be the preliminary unverified: %v", first)
	}

	// Narratives can only be published when every claim is editor-reviewed.
	n := e.MustStatus(e.Do("POST", "/v1/admin/narratives", admin, map[string]any{"title": map[string]string{"ro": "Zvonuri despre tăierea pensiilor"},
		"summary": map[string]string{"ro": "Afirmații despre reducerea pensiilor"}, "why_caution": map[string]string{"ro": "Contrazise de documente oficiale."},
		"claim_ids": []string{pension, airport}}), 201)
	e.MustStatus(e.Do("POST", "/v1/admin/narratives/"+n.Str("id")+"/publish", admin, nil), 422)
}
