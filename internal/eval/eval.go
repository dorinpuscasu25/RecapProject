// Package eval runs versioned evaluation suites against whatever model the AI routes currently
// select, records per-case results and aggregate metrics, and enforces quality gates. Numbers
// reported here are measurements on a small synthetic corpus, not claims of production accuracy.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/taxonomy"
)

// Suites maps a suite name to its AI task and dataset file under the eval directory.
var Suites = map[string]struct {
	Task ai.Task
	File string
}{
	"classification": {ai.TaskClassification, "classification.json"},
	"claims":         {ai.TaskClaimExtraction, "claims.json"},
	"answers":        {ai.TaskChatAnswering, "answers.json"},
	"retrieval":      {ai.TaskEmbeddings, "retrieval.json"},
}

// Dataset is the on-disk format: a gate (minimum metric values) plus cases.
type Dataset struct {
	Version     string             `json:"version"`
	Description string             `json:"description"`
	Gates       map[string]float64 `json:"gates"`
	Cases       []json.RawMessage  `json:"cases"`
}

type Runner struct {
	DB  *db.DB
	GW  *ai.Gateway
	Tax *taxonomy.Service
	Dir string
}

type Report struct {
	RunID     uuid.UUID          `json:"run_id"`
	Suite     string             `json:"suite"`
	Model     string             `json:"model"`
	Dataset   string             `json:"dataset"`
	Cases     int                `json:"cases"`
	Passed    int                `json:"passed"`
	Metrics   map[string]float64 `json:"metrics"`
	Gates     map[string]float64 `json:"gates"`
	GateFails []string           `json:"gate_failures"`
	Failures  []string           `json:"failed_cases"`
}

// OK reports whether every gate was met.
func (r *Report) OK() bool { return len(r.GateFails) == 0 }

type caseResult struct {
	id       string
	expected any
	actual   any
	scores   map[string]float64
	passed   bool
	model    string
}

// Run executes one suite and persists eval_runs/eval_results.
func (r *Runner) Run(ctx context.Context, suite string, actor *uuid.UUID) (*Report, error) {
	def, ok := Suites[suite]
	if !ok {
		return nil, fmt.Errorf("unknown suite %q (known: classification, claims, answers, retrieval)", suite)
	}
	raw, err := os.ReadFile(filepath.Join(r.Dir, def.File))
	if err != nil {
		return nil, err
	}
	var ds Dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, fmt.Errorf("dataset %s: %w", def.File, err)
	}
	r.GW.Invalidate()
	route, err := r.GW.RouteFor(ctx, def.Task)
	if err != nil {
		return nil, err
	}
	model := route.Chain[0].Ref()
	runID := ids.New()
	if _, err := r.DB.Pool.Exec(ctx, `INSERT INTO eval_runs (id, name, suite, task, model_slug, dataset, config, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		runID, suite+"@"+time.Now().UTC().Format(time.RFC3339), suite, string(def.Task), model, def.File+"@"+ds.Version,
		map[string]any{"gates": ds.Gates, "route_id": route.ID}, actor); err != nil {
		return nil, err
	}
	var results []caseResult
	var metrics map[string]float64
	switch suite {
	case "classification":
		results, metrics, err = r.classification(ctx, ds.Cases)
	case "claims":
		results, metrics, err = r.claims(ctx, ds.Cases)
	case "answers":
		results, metrics, err = r.answers(ctx, ds.Cases)
	case "retrieval":
		results, metrics, err = r.retrieval(ctx, raw)
	}
	if err != nil {
		_, _ = r.DB.Pool.Exec(ctx, `UPDATE eval_runs SET status='failed', error=$2, finished_at=now() WHERE id=$1`, runID, err.Error())
		return nil, err
	}
	rep := &Report{RunID: runID, Suite: suite, Model: model, Dataset: def.File + "@" + ds.Version, Cases: len(results), Metrics: metrics, Gates: ds.Gates,
		GateFails: []string{}, Failures: []string{}}
	for _, c := range results {
		if c.passed {
			rep.Passed++
		} else {
			rep.Failures = append(rep.Failures, c.id)
		}
		exp, _ := json.Marshal(c.expected)
		act, _ := json.Marshal(c.actual)
		sc, _ := json.Marshal(c.scores)
		if _, err := r.DB.Pool.Exec(ctx, `INSERT INTO eval_results (run_id, case_id, expected, actual, scores, passed) VALUES ($1,$2,$3,$4,$5,$6)`,
			runID, c.id, exp, act, sc, c.passed); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(ds.Gates))
	for k := range ds.Gates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := metrics[k]; !ok || v+1e-9 < ds.Gates[k] {
			rep.GateFails = append(rep.GateFails, fmt.Sprintf("%s=%.3f < %.3f", k, metrics[k], ds.Gates[k]))
		}
	}
	metrics["gates_passed"] = 0
	if rep.OK() {
		metrics["gates_passed"] = 1
	}
	if _, err := r.DB.Pool.Exec(ctx, `UPDATE eval_runs SET status='succeeded', metrics=$2, finished_at=now() WHERE id=$1`, runID, metrics); err != nil {
		return nil, err
	}
	return rep, nil
}

func opts(suite, id string) ai.CallOpts {
	return ai.CallOpts{SubjectType: "eval", SubjectID: suite + ":" + id}
}

// ---------- classification ----------

type classCase struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Text     string   `json:"text"`
	Language string   `json:"language"`
	Expected []string `json:"expected"`
}

func (r *Runner) classification(ctx context.Context, raw []json.RawMessage) ([]caseResult, map[string]float64, error) {
	cats, ver, err := r.Tax.Active(ctx)
	if err != nil {
		return nil, nil, err
	}
	slugByID := map[uuid.UUID]string{}
	for _, c := range cats {
		slugByID[c.ID] = c.Slug
	}
	parentOf := map[string]string{}
	for _, c := range cats {
		if c.ParentID != nil {
			parentOf[c.Slug] = slugByID[*c.ParentID]
		}
	}
	// Compare ancestor-closed sets: "electricity-prices" implies "energy" on both sides.
	closure := func(in []string) map[string]bool {
		out := map[string]bool{}
		for _, s := range in {
			for s != "" && !out[s] {
				out[s] = true
				s = parentOf[s]
			}
		}
		return out
	}
	var defs []ai.CategoryDef
	for _, c := range cats {
		if !c.AutoClassify {
			continue
		}
		d := ai.CategoryDef{Slug: c.Slug, Names: c.Names, Description: c.EditorialDescription, Hints: c.ClassificationHints, Aliases: c.Aliases,
			Positive: c.PositiveExamples, Negative: c.NegativeExamples, Boost: c.Boost}
		if c.ParentID != nil {
			d.ParentSlug = slugByID[*c.ParentID]
		}
		defs = append(defs, d)
	}
	var tp, fp, fn, exact float64
	var out []caseResult
	for _, rc := range raw {
		var c classCase
		if err := json.Unmarshal(rc, &c); err != nil {
			return nil, nil, err
		}
		in := &ai.ClassifyInput{Title: c.Title, Text: c.Text, Language: c.Language, Categories: defs, TaxonomyVersion: ver}
		var o ai.ClassifyOutput
		_, err := r.GW.GenerateStructured(ctx, ai.TaskClassification, in, &o, opts("classification", c.ID))
		if err != nil {
			if errors.Is(err, ai.ErrDeferred) {
				return nil, nil, fmt.Errorf("model unavailable for case %s: %w", c.ID, err)
			}
			return nil, nil, err
		}
		var gotList []string
		for _, a := range o.Assignments {
			gotList = append(gotList, a.Slug)
		}
		got, want := closure(gotList), closure(c.Expected)
		var ctp, cfp, cfn float64
		for s := range want {
			if got[s] {
				ctp++
			} else {
				cfn++
			}
		}
		for s := range got {
			if !want[s] {
				cfp++
			}
		}
		tp, fp, fn = tp+ctp, fp+cfp, fn+cfn
		pass := cfn == 0 && cfp == 0
		if pass {
			exact++
		}
		out = append(out, caseResult{id: c.ID, expected: c.Expected, actual: gotList, passed: pass,
			scores: map[string]float64{"tp": ctp, "fp": cfp, "fn": cfn}})
	}
	p, rc := ratio(tp, tp+fp), ratio(tp, tp+fn)
	return out, map[string]float64{"precision": p, "recall": rc, "f1": f1(p, rc), "exact_match": ratio(exact, float64(len(out)))}, nil
}

// ---------- claims ----------

type claimCase struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	Language string `json:"language"`
	// Expected checkable claims, identified by a substring of their quote.
	Checkable []string `json:"checkable"`
	// Spans that must NOT be extracted as checkable (opinions, predictions, rhetoric).
	NotCheckable []string `json:"not_checkable"`
}

func (r *Runner) claims(ctx context.Context, raw []json.RawMessage) ([]caseResult, map[string]float64, error) {
	var hit, want, falseCheckable, guarded, spanOK, spans float64
	var out []caseResult
	for _, rc := range raw {
		var c claimCase
		if err := json.Unmarshal(rc, &c); err != nil {
			return nil, nil, err
		}
		in := &ai.ClaimInput{Title: c.Title, Text: c.Text, Language: c.Language, ContentType: "news", MaxClaims: 10}
		var o ai.ClaimOutput
		if _, err := r.GW.GenerateStructured(ctx, ai.TaskClaimExtraction, in, &o, opts("claims", c.ID)); err != nil {
			return nil, nil, err
		}
		var checkable []string
		for _, cl := range o.Claims {
			spans++
			if cl.Start >= 0 && cl.End <= len(c.Text) && cl.Start < cl.End && c.Text[cl.Start:cl.End] == cl.Quote {
				spanOK++
			}
			if cl.Checkability == "checkable" {
				checkable = append(checkable, cl.Quote)
			}
		}
		pass := true
		for _, w := range c.Checkable {
			want++
			if containsAny(checkable, w) {
				hit++
			} else {
				pass = false
			}
		}
		for _, n := range c.NotCheckable {
			guarded++
			if containsAny(checkable, n) {
				falseCheckable++
				pass = false
			}
		}
		out = append(out, caseResult{id: c.ID, expected: map[string]any{"checkable": c.Checkable, "not_checkable": c.NotCheckable},
			actual: o.Claims, passed: pass, scores: map[string]float64{"claims": float64(len(o.Claims))}})
	}
	return out, map[string]float64{
		"checkable_recall":        ratio(hit, want),
		"non_checkable_rejection": 1 - ratio(falseCheckable, guarded),
		"span_fidelity":           ratioOr1(spanOK, spans),
	}, nil
}

func containsAny(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(strings.ToLower(s), strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// ---------- grounded answers ----------

type answerCase struct {
	ID            string          `json:"id"`
	Question      string          `json:"question"`
	Language      string          `json:"language"`
	Context       []ai.ContextDoc `json:"context"`
	ScopeDefault  bool            `json:"scope_default"`
	ExpectAbstain bool            `json:"expect_abstain"`
	MustCite      []string        `json:"must_cite"`
}

func (r *Runner) answers(ctx context.Context, raw []json.RawMessage) ([]caseResult, map[string]float64, error) {
	var abstainOK, n, citeValid, cites, required, requiredHit, uncertainty, answered float64
	var out []caseResult
	for _, rc := range raw {
		var c answerCase
		if err := json.Unmarshal(rc, &c); err != nil {
			return nil, nil, err
		}
		in := &ai.AnswerInput{Question: c.Question, Level: "short", Language: c.Language, Now: time.Now().UTC().Format(time.RFC3339),
			Context: c.Context, ScopeDefault: c.ScopeDefault}
		var o ai.AnswerOutput
		if _, err := r.GW.GenerateStructured(ctx, ai.TaskChatAnswering, in, &o, opts("answers", c.ID)); err != nil {
			return nil, nil, err
		}
		n++
		refs := map[string]bool{}
		for _, d := range c.Context {
			refs[d.Ref] = true
		}
		pass := o.Abstained == c.ExpectAbstain
		if pass {
			abstainOK++
		}
		got := map[string]bool{}
		for _, ref := range o.Citations {
			cites++
			if refs[ref] {
				citeValid++
			} else {
				pass = false // citation to something that was never retrieved
			}
			got[ref] = true
		}
		for _, ref := range c.MustCite {
			required++
			if got[ref] {
				requiredHit++
			} else {
				pass = false
			}
		}
		if !o.Abstained {
			answered++
			if strings.TrimSpace(o.Uncertainty) != "" {
				uncertainty++
			}
		}
		out = append(out, caseResult{id: c.ID, expected: map[string]any{"abstain": c.ExpectAbstain, "must_cite": c.MustCite}, actual: o, passed: pass,
			scores: map[string]float64{"citations": float64(len(o.Citations))}})
	}
	return out, map[string]float64{
		"abstention_accuracy":      ratio(abstainOK, n),
		"citation_validity":        ratioOr1(citeValid, cites),
		"required_citation_recall": ratioOr1(requiredHit, required),
		"uncertainty_stated":       ratioOr1(uncertainty, answered),
	}, nil
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func ratioOr1(a, b float64) float64 {
	if b == 0 {
		return 1
	}
	return a / b
}

func f1(p, r float64) float64 {
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// ---------- retrieval (embedding models) ----------

// retrieval ranks a small multilingual corpus for each query with the routed embedding model and
// reports recall@k, MRR and nDCG@10. The dataset format is shared with services/ml (app.evaluate).
func (r *Runner) retrieval(ctx context.Context, raw []byte) ([]caseResult, map[string]float64, error) {
	var ds struct {
		Corpus []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"corpus"`
		Queries []struct {
			Query    string   `json:"query"`
			Relevant []string `json:"relevant"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, nil, err
	}
	m, err := r.GW.EmbeddingModel(ctx)
	if err != nil {
		return nil, nil, err
	}
	texts := make([]string, len(ds.Corpus))
	for i, d := range ds.Corpus {
		texts[i] = d.Text
	}
	docs, err := r.GW.EmbedTexts(ctx, m, texts, opts("retrieval", "corpus"))
	if err != nil {
		return nil, nil, err
	}
	var out []caseResult
	var r1, r5, mrr, ndcg float64
	for qi, q := range ds.Queries {
		qv, err := r.GW.EmbedTexts(ctx, m, []string{q.Query}, opts("retrieval", fmt.Sprint(qi)))
		if err != nil {
			return nil, nil, err
		}
		idx := make([]int, len(docs))
		score := make([]float64, len(docs))
		for i := range docs {
			idx[i], score[i] = i, dot(qv[0], docs[i])
		}
		sort.SliceStable(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
		rel := map[string]bool{}
		for _, id := range q.Relevant {
			rel[id] = true
		}
		var ranked []string
		hit1, hit5, rr, dcg, idcg := 0.0, 0.0, 0.0, 0.0, 0.0
		for rank, i := range idx {
			id := ds.Corpus[i].ID
			ranked = append(ranked, id)
			if !rel[id] {
				continue
			}
			if rank < 1 {
				hit1++
			}
			if rank < 5 {
				hit5++
			}
			if rr == 0 {
				rr = 1 / float64(rank+1)
			}
			if rank < 10 {
				dcg += 1 / math.Log2(float64(rank+2))
			}
		}
		for i := 0; i < len(rel) && i < 10; i++ {
			idcg += 1 / math.Log2(float64(i+2))
		}
		n := float64(len(rel))
		r1 += hit1 / n
		r5 += hit5 / n
		mrr += rr
		if idcg > 0 {
			ndcg += dcg / idcg
		}
		if len(ranked) > 5 {
			ranked = ranked[:5]
		}
		out = append(out, caseResult{id: fmt.Sprintf("q%02d", qi+1), expected: q.Relevant, actual: ranked, passed: rr == 1,
			scores: map[string]float64{"rr": rr}})
	}
	n := float64(len(ds.Queries))
	return out, map[string]float64{"recall@1": r1 / n, "recall@5": r5 / n, "mrr": mrr / n, "ndcg@10": ndcg / n}, nil
}

func dot(a, b []float32) float64 {
	var s, na, nb float64
	for i := range a {
		if i < len(b) {
			s += float64(a[i]) * float64(b[i])
			na += float64(a[i]) * float64(a[i])
			nb += float64(b[i]) * float64(b[i])
		}
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return s / math.Sqrt(na*nb)
}

// ---------- listing ----------

type RunView struct {
	ID         uuid.UUID          `json:"id"`
	Suite      string             `json:"suite"`
	Task       string             `json:"task"`
	Model      string             `json:"model"`
	Dataset    string             `json:"dataset"`
	Status     string             `json:"status"`
	Metrics    map[string]float64 `json:"metrics"`
	Error      *string            `json:"error,omitempty"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
}

// Runs lists recent evaluation runs (optionally for one suite).
func Runs(ctx context.Context, d *db.DB, suite string, limit int) ([]RunView, error) {
	rows, err := d.Pool.Query(ctx, `SELECT id, suite, task, model_slug, dataset, status, metrics, error, started_at, finished_at FROM eval_runs
		WHERE ($1='' OR suite=$1) ORDER BY started_at DESC LIMIT $2`, suite, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunView{}
	for rows.Next() {
		var v RunView
		var m []byte
		if err := rows.Scan(&v.ID, &v.Suite, &v.Task, &v.Model, &v.Dataset, &v.Status, &m, &v.Error, &v.StartedAt, &v.FinishedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(m, &v.Metrics)
		out = append(out, v)
	}
	return out, rows.Err()
}

type ResultView struct {
	CaseID   string          `json:"case_id"`
	Passed   bool            `json:"passed"`
	Expected json.RawMessage `json:"expected"`
	Actual   json.RawMessage `json:"actual"`
	Scores   json.RawMessage `json:"scores"`
}

// Results returns the per-case results of one run.
func Results(ctx context.Context, d *db.DB, runID uuid.UUID) ([]ResultView, error) {
	rows, err := d.Pool.Query(ctx, `SELECT case_id, passed, expected, coalesce(actual,'null'::jsonb), scores FROM eval_results WHERE run_id=$1 ORDER BY case_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResultView{}
	for rows.Next() {
		var v ResultView
		if err := rows.Scan(&v.CaseID, &v.Passed, &v.Expected, &v.Actual, &v.Scores); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
