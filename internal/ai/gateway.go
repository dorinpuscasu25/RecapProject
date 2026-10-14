package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dorinpuscasu25/recapproject/internal/platform/crypto"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ratelimit"
)

// Route is a resolved task route.
type Route struct {
	ID              uuid.UUID
	Task            Task
	Mode            string
	Chain           []*Model // primary first
	PromptVersionID *uuid.UUID
	PromptVersion   int
	System          string
	UserTemplate    string
	Temperature     float32
	MaxOutputTokens int
	TimeoutMS       int
	MaxAttempts     int
	DailyBudgetUSD  *float64
	SampleRate      float64
	OnUnavailable   string
	Version         int
}

type snapshot struct {
	models map[uuid.UUID]*Model
	bySlug map[string]*Model
	routes map[string]*Route // task|mode
	at     time.Time
}

// Observer receives execution records (e.g. Langfuse adapter). Must not block.
type Observer interface {
	Observe(ctx context.Context, rec ExecutionRecord)
}

type ExecutionRecord struct {
	ID           uuid.UUID
	Task         Task
	Mode         string
	ModelRef     string
	Status       string
	LatencyMS    int
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	ErrorCat     string
	TraceID      string
	SubjectType  string
	SubjectID    string
}

type Gateway struct {
	db       *db.DB
	sealer   *crypto.Sealer
	limiter  ratelimit.Limiter
	log      *slog.Logger
	observer Observer
	ttl      time.Duration

	mu   sync.RWMutex
	snap *snapshot

	semMu sync.Mutex
	sems  map[uuid.UUID]chan struct{}

	// overrides lets tests inject provider implementations by provider slug.
	overrides map[string]any
}

func NewGateway(d *db.DB, sealer *crypto.Sealer, limiter ratelimit.Limiter, log *slog.Logger) *Gateway {
	return &Gateway{db: d, sealer: sealer, limiter: limiter, log: log, ttl: 30 * time.Second, sems: map[uuid.UUID]chan struct{}{}, overrides: map[string]any{}}
}

func (g *Gateway) SetObserver(o Observer) { g.observer = o }

// OverrideProvider replaces the adapter for a provider slug (tests / fault injection only).
func (g *Gateway) OverrideProvider(slug string, impl any) { g.overrides[slug] = impl }

// Invalidate drops the cached registry (call after admin changes).
func (g *Gateway) Invalidate() {
	g.mu.Lock()
	g.snap = nil
	g.mu.Unlock()
}

var (
	aiCalls = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ai_executions_total", Help: "AI executions by task, model and status."},
		[]string{"task", "model", "status"})
	aiLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "ai_execution_seconds", Help: "AI execution latency.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30, 60}}, []string{"task", "model"})
)

func init() { prometheus.MustRegister(aiCalls, aiLatency) }

func (g *Gateway) load(ctx context.Context) (*snapshot, error) {
	g.mu.RLock()
	s := g.snap
	g.mu.RUnlock()
	if s != nil && time.Since(s.at) < g.ttl {
		return s, nil
	}
	s = &snapshot{models: map[uuid.UUID]*Model{}, bySlug: map[string]*Model{}, routes: map[string]*Route{}, at: time.Now()}
	providers := map[uuid.UUID]*Provider{}
	rows, err := g.db.Pool.Query(ctx, `SELECT id, slug, kind, base_url, api_key_env, api_key_enc, enabled, rate_limit_rpm, max_concurrency, timeout_ms, extra_headers FROM ai_providers`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p Provider
		var env string
		var enc, hdr []byte
		if err := rows.Scan(&p.ID, &p.Slug, &p.Kind, &p.BaseURL, &env, &enc, &p.Enabled, &p.RateLimitRPM, &p.MaxConcurrency, &p.TimeoutMS, &hdr); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(hdr, &p.ExtraHeaders)
		switch {
		case env != "":
			p.APIKey = os.Getenv(env)
		case enc != nil && g.sealer != nil:
			if k, err := g.sealer.Open(enc); err == nil {
				p.APIKey = string(k)
			}
		}
		providers[p.ID] = &p
	}
	rows.Close()
	rows, err = g.db.Pool.Query(ctx, `SELECT id, provider_id, slug, model_name, model_version, kind, supports_structured, supports_json_mode, languages,
		context_window, max_output_tokens, COALESCE(embedding_dims,0), input_price_per_mtok::float8, output_price_per_mtok::float8, price_per_mchar::float8, enabled, metadata
		FROM ai_models`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m Model
		var meta []byte
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.Slug, &m.Name, &m.Version, &m.Kind, &m.SupportsStructured, &m.SupportsJSONMode, &m.Languages,
			&m.ContextWindow, &m.MaxOutputTokens, &m.EmbeddingDims, &m.InputPricePerMTok, &m.OutputPricePerMTok, &m.PricePerMChar, &m.Enabled, &meta); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(meta, &m.Metadata)
		m.Provider = providers[m.ProviderID]
		s.models[m.ID] = &m
		s.bySlug[m.Slug] = &m
	}
	rows.Close()
	rows, err = g.db.Pool.Query(ctx, `SELECT r.id, r.task, r.mode, r.primary_model_id, r.fallback_model_ids, r.prompt_version_id, COALESCE(pv.version,0),
		COALESCE(pv.system_prompt,''), COALESCE(pv.user_template,''), r.temperature, r.max_output_tokens, r.timeout_ms, r.max_attempts,
		r.daily_budget_usd::float8, r.sample_rate, r.on_unavailable, r.version
		FROM ai_task_routes r LEFT JOIN prompt_versions pv ON pv.id = r.prompt_version_id WHERE r.enabled`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r Route
		var primary uuid.UUID
		var fallbacks []uuid.UUID
		var task string
		if err := rows.Scan(&r.ID, &task, &r.Mode, &primary, &fallbacks, &r.PromptVersionID, &r.PromptVersion, &r.System, &r.UserTemplate,
			&r.Temperature, &r.MaxOutputTokens, &r.TimeoutMS, &r.MaxAttempts, &r.DailyBudgetUSD, &r.SampleRate, &r.OnUnavailable, &r.Version); err != nil {
			rows.Close()
			return nil, err
		}
		r.Task = Task(task)
		for _, id := range append([]uuid.UUID{primary}, fallbacks...) {
			if m := s.models[id]; m != nil {
				r.Chain = append(r.Chain, m)
			}
		}
		s.routes[task+"|"+r.Mode] = &r
	}
	rows.Close()
	g.mu.Lock()
	g.snap = s
	g.mu.Unlock()
	return s, nil
}

// RouteFor returns the production route for a task.
func (g *Gateway) RouteFor(ctx context.Context, t Task) (*Route, error) {
	s, err := g.load(ctx)
	if err != nil {
		return nil, err
	}
	r := s.routes[string(t)+"|production"]
	if r == nil || len(r.Chain) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, t)
	}
	return r, nil
}

// ModelBySlug resolves a model (eval harness / experiments).
func (g *Gateway) ModelBySlug(ctx context.Context, slug string) (*Model, error) {
	s, err := g.load(ctx)
	if err != nil {
		return nil, err
	}
	m := s.bySlug[slug]
	if m == nil {
		return nil, fmt.Errorf("unknown model %q", slug)
	}
	return m, nil
}

func (g *Gateway) chat(m *Model) (ChatProvider, error) {
	if o, ok := g.overrides[m.Provider.Slug].(ChatProvider); ok {
		return o, nil
	}
	hc := defaultHTTPClient(m.Provider.TimeoutMS)
	switch m.Provider.Kind {
	case "heuristic":
		return Heuristic{}, nil
	case "openai_compatible":
		return &OpenAICompatible{P: m.Provider, Client: hc}, nil
	case "anthropic":
		if m.Provider.APIKey == "" {
			return nil, &ProviderError{Category: ErrNotConfigured, Msg: "anthropic API key not configured"}
		}
		return NewAnthropic(m.Provider, hc), nil
	case "gemini":
		if m.Provider.APIKey == "" {
			return nil, &ProviderError{Category: ErrNotConfigured, Msg: "gemini API key not configured"}
		}
		return &Gemini{P: m.Provider, Client: hc}, nil
	}
	return nil, &ProviderError{Category: ErrUnsupported, Msg: "unknown provider kind " + m.Provider.Kind}
}

func (g *Gateway) embedder(m *Model) (EmbeddingProvider, error) {
	if o, ok := g.overrides[m.Provider.Slug].(EmbeddingProvider); ok {
		return o, nil
	}
	hc := defaultHTTPClient(m.Provider.TimeoutMS)
	switch m.Provider.Kind {
	case "heuristic":
		return Heuristic{}, nil
	case "openai_compatible":
		return &OpenAICompatible{P: m.Provider, Client: hc}, nil
	case "gemini":
		if m.Provider.APIKey == "" {
			return nil, &ProviderError{Category: ErrNotConfigured, Msg: "gemini API key not configured"}
		}
		return &Gemini{P: m.Provider, Client: hc}, nil
	}
	return nil, &ProviderError{Category: ErrUnsupported, Msg: "provider kind has no embeddings: " + m.Provider.Kind}
}

func (g *Gateway) speaker(m *Model) (SpeechProvider, error) {
	if o, ok := g.overrides[m.Provider.Slug].(SpeechProvider); ok {
		return o, nil
	}
	switch m.Provider.Kind {
	case "openai_compatible":
		if m.Provider.APIKey == "" && strings.Contains(m.Provider.BaseURL, "openai.com") {
			return nil, &ProviderError{Category: ErrNotConfigured, Msg: "TTS API key not configured"}
		}
		return &OpenAICompatible{P: m.Provider, Client: defaultHTTPClient(m.Provider.TimeoutMS)}, nil
	}
	return nil, &ProviderError{Category: ErrNotConfigured, Msg: "no TTS-capable provider configured"}
}

// usable reports whether a model may be attempted now.
func (g *Gateway) usable(m *Model, kind string) error {
	if m.Provider == nil || !m.Provider.Enabled || !m.Enabled {
		return &ProviderError{Category: ErrNotConfigured, Msg: "provider or model disabled"}
	}
	if m.Kind != kind {
		return &ProviderError{Category: ErrUnsupported, Msg: fmt.Sprintf("model kind %s cannot serve %s", m.Kind, kind)}
	}
	return nil
}

func (g *Gateway) acquire(ctx context.Context, m *Model) (func(), error) {
	g.semMu.Lock()
	sem, ok := g.sems[m.Provider.ID]
	if !ok {
		n := m.Provider.MaxConcurrency
		if n <= 0 {
			n = 4
		}
		sem = make(chan struct{}, n)
		g.sems[m.Provider.ID] = sem
	}
	g.semMu.Unlock()
	if m.Provider.Kind != "heuristic" && g.limiter != nil {
		if ok, retry, _ := g.limiter.Allow(ctx, "ai:"+m.Provider.Slug, m.Provider.RateLimitRPM, time.Minute); !ok {
			return nil, &ProviderError{Category: ErrTransient, Msg: fmt.Sprintf("provider rate limit reached; retry in %s", retry)}
		}
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *Gateway) budgetOK(ctx context.Context, r *Route) bool {
	if r.DailyBudgetUSD == nil {
		return true
	}
	var spent float64
	_ = g.db.Pool.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd),0)::float8 FROM ai_executions WHERE route_id=$1 AND created_at > now() - interval '24 hours'`, r.ID).Scan(&spent)
	return spent < *r.DailyBudgetUSD
}

// CallOpts carries provenance and overrides for one call.
type CallOpts struct {
	SubjectType string
	SubjectID   string
	TraceID     string
	// ModelSlug forces a specific model (evaluation/experiments); bypasses the route chain.
	ModelSlug    string
	Mode         string // production (default) | experiment
	ExperimentID *uuid.UUID
}

// GenerateStructured runs a structured task, decoding and validating into out.
func (g *Gateway) GenerateStructured(ctx context.Context, task Task, input any, out any, o CallOpts) (*Execution, error) {
	schema := SchemaFor(task)
	if schema == nil {
		return nil, fmt.Errorf("task %s has no structured schema", task)
	}
	exec, err := g.run(ctx, task, input, schema, o, func(text string) error {
		return decodeValidate(text, schema, input, out)
	})
	if err == nil && o.Mode == "" {
		g.maybeShadow(task, input, schema, o)
	}
	return exec, err
}

// GenerateText runs an unstructured task.
func (g *Gateway) GenerateText(ctx context.Context, task Task, input any, o CallOpts) (string, *Execution, error) {
	var result string
	exec, err := g.run(ctx, task, input, nil, o, func(text string) error {
		if strings.TrimSpace(text) == "" {
			return errors.New("empty output")
		}
		result = text
		return nil
	})
	return result, exec, err
}

func decodeValidate(text string, schema map[string]any, input, out any) error {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	var generic any
	if err := json.Unmarshal([]byte(text), &generic); err != nil {
		return fmt.Errorf("output is not valid JSON: %w", err)
	}
	if err := ValidateSchema(schema, generic); err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode output: %w", err)
	}
	if v, ok := out.(Validator); ok {
		if err := v.Validate(input); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gateway) run(ctx context.Context, task Task, input any, schema map[string]any, o CallOpts, accept func(string) error) (*Execution, error) {
	var route *Route
	var chain []*Model
	if o.ModelSlug != "" {
		m, err := g.ModelBySlug(ctx, o.ModelSlug)
		if err != nil {
			return nil, err
		}
		chain = []*Model{m}
		route, _ = g.RouteFor(ctx, task)
		if route == nil {
			route = &Route{Task: task, MaxAttempts: 2, MaxOutputTokens: 1500, TimeoutMS: 60000, OnUnavailable: "fail"}
		}
	} else {
		var err error
		route, err = g.RouteFor(ctx, task)
		if err != nil {
			return nil, err
		}
		chain = route.Chain
	}
	mode := o.Mode
	if mode == "" {
		mode = "production"
	}
	system, user := route.System, route.UserTemplate
	if user == "" {
		for _, dp := range DefaultPrompts {
			if dp.Task == task {
				system, user = dp.System, dp.User
			}
		}
	}
	prompt, err := RenderPrompt(user, input)
	if err != nil {
		return nil, err
	}
	reqHash := sha256.Sum256([]byte(system + "\x00" + prompt))
	if mode == "production" && route.ID != uuid.Nil && !g.budgetOK(ctx, route) {
		g.record(ctx, route, &Model{Slug: "-", Provider: &Provider{Slug: "-"}}, mode, task, "skipped_budget", 0, Usage{}, 0, "budget", "daily budget exhausted", o, nil, false, hex.EncodeToString(reqHash[:8]))
		if route.OnUnavailable == "fail" {
			return nil, &ProviderError{Category: ErrBudget, Msg: "daily budget exhausted"}
		}
		return nil, fmt.Errorf("%w: budget exhausted", ErrDeferred)
	}
	var lastErr error
	for i, m := range chain {
		if err := g.usable(m, "chat"); err != nil {
			lastErr = err
			continue
		}
		prov, err := g.chat(m)
		if err != nil {
			lastErr = err
			continue
		}
		attempts := route.MaxAttempts
		if attempts <= 0 {
			attempts = 1
		}
		msgs := []Message{{Role: "user", Content: prompt}}
		for a := 1; a <= attempts; a++ {
			release, err := g.acquire(ctx, m)
			if err != nil {
				lastErr = err
				break
			}
			timeout := time.Duration(route.TimeoutMS) * time.Millisecond
			if timeout <= 0 {
				timeout = time.Minute
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			start := time.Now()
			maxTok := route.MaxOutputTokens
			if m.MaxOutputTokens > 0 && maxTok > m.MaxOutputTokens {
				maxTok = m.MaxOutputTokens
			}
			resp, err := prov.Generate(cctx, ProviderRequest{Model: m, Task: task, System: system, Messages: msgs,
				Schema: schema, SchemaName: string(task), Temperature: route.Temperature, MaxTokens: maxTok, Payload: input})
			cancel()
			release()
			lat := int(time.Since(start).Milliseconds())
			if err == nil {
				if verr := accept(resp.Text); verr != nil {
					err = &ProviderError{Category: ErrInvalid, Msg: verr.Error()}
					g.record(ctx, route, m, mode, task, "invalid_output", a, resp.Usage, lat, string(ErrInvalid), verr.Error(), o, nil, i > 0, hex.EncodeToString(reqHash[:8]))
					lastErr = err
					// Repair attempt: show the model its output and the validation error.
					msgs = []Message{{Role: "user", Content: prompt}, {Role: "assistant", Content: resp.Text},
						{Role: "user", Content: "That output was rejected by validation: " + verr.Error() +
							"\nReturn a corrected JSON object only, following the schema and the rules above."}}
					continue
				}
				id := g.record(ctx, route, m, mode, task, "succeeded", a, resp.Usage, lat, "", "", o, json.RawMessage(resp.Text), i > 0, hex.EncodeToString(reqHash[:8]))
				return &Execution{ID: id, Task: task, ModelRef: m.Ref(), PromptVersion: route.PromptVersionID, IsFallback: i > 0, Attempts: a,
					LatencyMS: lat, CostUSD: cost(m, resp.Usage)}, nil
			}
			cat := categoryOf(err)
			g.record(ctx, route, m, mode, task, "failed", a, Usage{}, lat, string(cat), err.Error(), o, nil, i > 0, hex.EncodeToString(reqHash[:8]))
			lastErr = err
			if cat != ErrTransient && cat != ErrInvalid {
				break // permanent/unsupported: go to next model
			}
			if cat == ErrTransient && a < attempts {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(a*a) * 300 * time.Millisecond):
				}
			}
		}
		if g.log != nil {
			g.log.Warn("ai model failed; trying fallback", "task", task, "model", m.Slug, "error", lastErr)
		}
	}
	if route.OnUnavailable == "fail" || o.ModelSlug != "" {
		if lastErr == nil {
			lastErr = errors.New("no usable model")
		}
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w (%s): %v", ErrDeferred, task, lastErr)
}

func cost(m *Model, u Usage) float64 {
	return float64(u.InputTokens)/1e6*m.InputPricePerMTok + float64(u.OutputTokens)/1e6*m.OutputPricePerMTok + float64(u.Characters)/1e6*m.PricePerMChar
}

func (g *Gateway) record(ctx context.Context, r *Route, m *Model, mode string, task Task, status string, attempt int, u Usage, lat int, errCat, errMsg string,
	o CallOpts, output json.RawMessage, fallback bool, reqHash string) uuid.UUID {
	id := ids.New()
	var routeID *uuid.UUID
	if r != nil && r.ID != uuid.Nil {
		routeID = &r.ID
	}
	var pv *uuid.UUID
	if r != nil {
		pv = r.PromptVersionID
	}
	c := 0.0
	if m.Provider != nil && m.Provider.Slug != "-" {
		c = cost(m, u)
	}
	if len(errMsg) > 1000 {
		errMsg = errMsg[:1000]
	}
	var outPtr any
	if output != nil && json.Valid(output) {
		outPtr = []byte(output)
	}
	var errCatPtr, errMsgPtr *string
	if errCat != "" {
		errCatPtr, errMsgPtr = &errCat, &errMsg
	}
	_, err := g.db.Pool.Exec(context.WithoutCancel(ctx), `INSERT INTO ai_executions (id, task, route_id, mode, provider_slug, model_slug, model_version,
		prompt_version_id, status, attempt, is_fallback, input_tokens, output_tokens, latency_ms, cost_usd, error_category, error_message, trace_id,
		subject_type, subject_id, request_hash, output, experiment_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		id, string(task), routeID, mode, providerSlug(m), m.Slug, m.Version, pv, status, max(attempt, 1), fallback, u.InputTokens, u.OutputTokens, lat, c,
		errCatPtr, errMsgPtr, nullStr(o.TraceID), nullStr(o.SubjectType), nullStr(o.SubjectID), reqHash, outPtr, o.ExperimentID)
	if err != nil && g.log != nil {
		g.log.Error("record ai execution", "error", err)
	}
	aiCalls.WithLabelValues(string(task), m.Slug, status).Inc()
	aiLatency.WithLabelValues(string(task), m.Slug).Observe(float64(lat) / 1000)
	if g.observer != nil {
		g.observer.Observe(ctx, ExecutionRecord{ID: id, Task: task, Mode: mode, ModelRef: providerSlug(m) + "/" + m.Slug, Status: status, LatencyMS: lat,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: c, ErrorCat: errCat, TraceID: o.TraceID, SubjectType: o.SubjectType, SubjectID: o.SubjectID})
	}
	return id
}

func providerSlug(m *Model) string {
	if m.Provider == nil {
		return "-"
	}
	return m.Provider.Slug
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// maybeShadow runs the shadow route (if any) asynchronously on a sample; results are only
// recorded in ai_executions (mode=shadow) and never returned or persisted elsewhere.
func (g *Gateway) maybeShadow(task Task, input any, schema map[string]any, o CallOpts) {
	s, err := g.load(context.Background())
	if err != nil {
		return
	}
	r := s.routes[string(task)+"|shadow"]
	if r == nil || len(r.Chain) == 0 || rand.Float64() >= r.SampleRate {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		m := r.Chain[0]
		if g.usable(m, "chat") != nil {
			return
		}
		prov, err := g.chat(m)
		if err != nil {
			return
		}
		system, user := r.System, r.UserTemplate
		if user == "" {
			for _, dp := range DefaultPrompts {
				if dp.Task == task {
					system, user = dp.System, dp.User
				}
			}
		}
		prompt, err := RenderPrompt(user, input)
		if err != nil {
			return
		}
		start := time.Now()
		resp, err := prov.Generate(ctx, ProviderRequest{Model: m, Task: task, System: system, Messages: []Message{{Role: "user", Content: prompt}},
			Schema: schema, SchemaName: string(task), Temperature: r.Temperature, MaxTokens: r.MaxOutputTokens, Payload: input})
		lat := int(time.Since(start).Milliseconds())
		so := o
		so.Mode = "shadow"
		if err != nil {
			g.record(ctx, r, m, "shadow", task, "failed", 1, Usage{}, lat, string(categoryOf(err)), err.Error(), so, nil, false, "")
			return
		}
		g.record(ctx, r, m, "shadow", task, "succeeded", 1, resp.Usage, lat, "", "", so, json.RawMessage(resp.Text), false, "")
	}()
}

// ---------- Embeddings, rerank, TTS ----------

// EmbeddingModel returns the active embedding model for the embeddings route.
func (g *Gateway) EmbeddingModel(ctx context.Context) (*Model, error) {
	r, err := g.RouteFor(ctx, TaskEmbeddings)
	if err != nil {
		return nil, err
	}
	for _, m := range r.Chain {
		if g.usable(m, "embedding") == nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("%w: no usable embedding model", ErrDeferred)
}

// EmbedTexts embeds with a specific model (embedding spaces are not interchangeable, so there
// is no silent fallback to a different model — callers choose the model/collection explicitly).
func (g *Gateway) EmbedTexts(ctx context.Context, m *Model, texts []string, o CallOpts) ([][]float32, error) {
	if err := g.usable(m, "embedding"); err != nil {
		return nil, err
	}
	p, err := g.embedder(m)
	if err != nil {
		return nil, err
	}
	release, err := g.acquire(ctx, m)
	if err != nil {
		return nil, err
	}
	defer release()
	start := time.Now()
	out, u, err := p.Embed(ctx, m, texts)
	lat := int(time.Since(start).Milliseconds())
	route, _ := g.RouteFor(ctx, TaskEmbeddings)
	if err != nil {
		g.record(ctx, route, m, "production", TaskEmbeddings, "failed", 1, u, lat, string(categoryOf(err)), err.Error(), o, nil, false, "")
		return nil, err
	}
	if m.Provider.Kind != "heuristic" {
		g.record(ctx, route, m, "production", TaskEmbeddings, "succeeded", 1, u, lat, "", "", o, nil, false, "")
	}
	return out, nil
}

// Rerank scores documents against a query. Uses the reranking route when configured with a
// chat model (pointwise relevance via embeddings is the deterministic fallback).
// Rerank scores docs against query. It uses the `reranking` route when it has a usable model of
// kind "rerank" (e.g. the ML service's cross-encoder); otherwise it falls back to cosine similarity
// on the active embedding model. The second result reports which path was used.
func (g *Gateway) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if r, err := g.RouteFor(ctx, TaskReranking); err == nil {
		for _, m := range r.Chain {
			if g.usable(m, "rerank") != nil {
				continue
			}
			p, err := g.reranker(m)
			if err != nil {
				continue
			}
			release, err := g.acquire(ctx, m)
			if err != nil {
				continue
			}
			start := time.Now()
			scores, u, err := p.Rerank(ctx, m, query, docs)
			release()
			lat := int(time.Since(start).Milliseconds())
			if err != nil {
				g.record(ctx, r, m, "production", TaskReranking, "failed", 1, Usage{}, lat, string(categoryOf(err)), err.Error(), CallOpts{SubjectType: "rerank"}, nil, false, "")
				continue
			}
			g.record(ctx, r, m, "production", TaskReranking, "succeeded", 1, u, lat, "", "", CallOpts{SubjectType: "rerank"}, nil, false, "")
			return scores, nil
		}
	}
	m, err := g.EmbeddingModel(ctx)
	if err != nil {
		return nil, err
	}
	vecs, err := g.EmbedTexts(ctx, m, append([]string{query}, docs...), CallOpts{SubjectType: "rerank"})
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(docs))
	for i := range docs {
		out[i] = cosine(vecs[0], vecs[i+1])
	}
	return out, nil
}

func (g *Gateway) reranker(m *Model) (RerankProvider, error) {
	if o, ok := g.overrides[m.Provider.Slug].(RerankProvider); ok {
		return o, nil
	}
	if m.Provider.Kind == "openai_compatible" {
		return &OpenAICompatible{P: m.Provider, Client: defaultHTTPClient(m.Provider.TimeoutMS)}, nil
	}
	return nil, &ProviderError{Category: ErrUnsupported, Msg: "provider kind has no rerank endpoint: " + m.Provider.Kind}
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

func sqrt(x float64) float64 {
	z := x
	for i := 0; i < 30; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// ErrTTSUnavailable means no TTS provider is configured; callers must report
// provider_unavailable and never fabricate audio.
var ErrTTSUnavailable = errors.New("tts provider unavailable")

// TextToSpeech synthesizes audio through the TTS route.
func (g *Gateway) TextToSpeech(ctx context.Context, req SpeechRequest, o CallOpts) (*SpeechResult, *Model, error) {
	r, err := g.RouteFor(ctx, TaskTTS)
	if err != nil {
		return nil, nil, ErrTTSUnavailable
	}
	var lastErr error = ErrTTSUnavailable
	for _, m := range r.Chain {
		if g.usable(m, "tts") != nil {
			continue
		}
		p, err := g.speaker(m)
		if err != nil {
			lastErr = err
			continue
		}
		release, err := g.acquire(ctx, m)
		if err != nil {
			lastErr = err
			continue
		}
		start := time.Now()
		res, err := p.Speak(ctx, m, req)
		release()
		lat := int(time.Since(start).Milliseconds())
		if err != nil {
			g.record(ctx, r, m, "production", TaskTTS, "failed", 1, Usage{Characters: len([]rune(req.Text))}, lat, string(categoryOf(err)), err.Error(), o, nil, false, "")
			lastErr = err
			continue
		}
		g.record(ctx, r, m, "production", TaskTTS, "succeeded", 1, res.Usage, lat, "", "", o, nil, false, "")
		return res, m, nil
	}
	var pe *ProviderError
	if errors.As(lastErr, &pe) && (pe.Category == ErrNotConfigured || pe.Category == ErrUnsupported) {
		return nil, nil, ErrTTSUnavailable
	}
	return nil, nil, lastErr
}

var lookupEnv = os.Getenv
