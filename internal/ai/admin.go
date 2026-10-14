package ai

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ssrf"
)

// Admin manages providers, models, prompts and routes. Secrets are write-only.
type Admin struct {
	DB     *db.DB
	GW     *Gateway
	Policy ssrf.Policy
}

type ProviderView struct {
	ID             uuid.UUID         `json:"id"`
	Slug           string            `json:"slug"`
	Kind           string            `json:"kind"`
	DisplayName    string            `json:"display_name"`
	BaseURL        string            `json:"base_url"`
	APIKeyEnv      string            `json:"api_key_env"`
	HasStoredKey   bool              `json:"has_stored_key"`
	KeyConfigured  bool              `json:"key_configured"`
	Enabled        bool              `json:"enabled"`
	RateLimitRPM   int               `json:"rate_limit_rpm"`
	MaxConcurrency int               `json:"max_concurrency"`
	TimeoutMS      int               `json:"timeout_ms"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	Version        int               `json:"version"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type ProviderInput struct {
	Slug           *string           `json:"slug,omitempty"`
	Kind           *string           `json:"kind,omitempty"`
	DisplayName    *string           `json:"display_name,omitempty"`
	BaseURL        *string           `json:"base_url,omitempty"`
	APIKeyEnv      *string           `json:"api_key_env,omitempty"`
	APIKey         *string           `json:"api_key,omitempty"` // write-only; stored encrypted
	Enabled        *bool             `json:"enabled,omitempty"`
	RateLimitRPM   *int              `json:"rate_limit_rpm,omitempty"`
	MaxConcurrency *int              `json:"max_concurrency,omitempty"`
	TimeoutMS      *int              `json:"timeout_ms,omitempty"`
	ExtraHeaders   map[string]string `json:"extra_headers,omitempty"`
	Version        *int              `json:"version,omitempty"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)
var envRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)

func (a *Admin) ListProviders(ctx context.Context) ([]ProviderView, error) {
	rows, err := a.DB.Pool.Query(ctx, `SELECT id, slug, kind, display_name, base_url, api_key_env, api_key_enc IS NOT NULL, enabled, rate_limit_rpm,
		max_concurrency, timeout_ms, extra_headers, version, updated_at FROM ai_providers ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProviderView, error) {
		var p ProviderView
		var hdr []byte
		err := r.Scan(&p.ID, &p.Slug, &p.Kind, &p.DisplayName, &p.BaseURL, &p.APIKeyEnv, &p.HasStoredKey, &p.Enabled, &p.RateLimitRPM, &p.MaxConcurrency,
			&p.TimeoutMS, &hdr, &p.Version, &p.UpdatedAt)
		_ = json.Unmarshal(hdr, &p.ExtraHeaders)
		p.KeyConfigured = p.Kind == "heuristic" || p.HasStoredKey || (p.APIKeyEnv != "" && envSet(p.APIKeyEnv))
		redactHeaders(p.ExtraHeaders)
		return p, err
	})
}

func redactHeaders(h map[string]string) {
	for k := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "key") || strings.Contains(lk, "auth") || strings.Contains(lk, "token") {
			h[k] = "[REDACTED]"
		}
	}
}

func (a *Admin) validateProvider(in ProviderInput, creating bool) error {
	var v apperr.Validator
	if creating {
		v.Check(in.Slug != nil && slugRe.MatchString(*in.Slug), "slug", "invalid", "Slug: lowercase letters, digits, - and _.")
		v.Check(in.Kind != nil && (*in.Kind == "openai_compatible" || *in.Kind == "anthropic" || *in.Kind == "gemini" || *in.Kind == "heuristic"),
			"kind", "invalid", "kind must be openai_compatible, anthropic, gemini or heuristic.")
		v.Check(in.DisplayName != nil && *in.DisplayName != "", "display_name", "required", "display_name is required.")
	}
	if in.BaseURL != nil && *in.BaseURL != "" {
		// Local inference servers (vLLM/Ollama) are legitimately private; only scheme/credentials are enforced here.
		if !strings.HasPrefix(*in.BaseURL, "https://") && !strings.HasPrefix(*in.BaseURL, "http://") {
			v.Add("base_url", "invalid", "base_url must be http(s).")
		}
		if strings.Contains(*in.BaseURL, "@") {
			v.Add("base_url", "invalid", "Credentials must not be embedded in base_url.")
		}
	}
	if in.APIKeyEnv != nil && *in.APIKeyEnv != "" {
		v.Check(envRe.MatchString(*in.APIKeyEnv), "api_key_env", "invalid", "Environment variable name must be UPPER_SNAKE_CASE.")
	}
	if in.RateLimitRPM != nil {
		v.Check(*in.RateLimitRPM > 0 && *in.RateLimitRPM <= 100000, "rate_limit_rpm", "out_of_range", "1..100000")
	}
	if in.MaxConcurrency != nil {
		v.Check(*in.MaxConcurrency > 0 && *in.MaxConcurrency <= 256, "max_concurrency", "out_of_range", "1..256")
	}
	if in.TimeoutMS != nil {
		v.Check(*in.TimeoutMS >= 1000 && *in.TimeoutMS <= 600000, "timeout_ms", "out_of_range", "1000..600000")
	}
	return v.Err()
}

func (a *Admin) UpsertProvider(ctx context.Context, id *uuid.UUID, in ProviderInput) (*ProviderView, error) {
	if err := a.validateProvider(in, id == nil); err != nil {
		return nil, err
	}
	var enc []byte
	if in.APIKey != nil && *in.APIKey != "" {
		if a.GW.sealer == nil {
			return nil, apperr.Unavailable("secrets_unavailable", "Secret storage is not configured.")
		}
		enc = a.GW.sealer.Seal([]byte(*in.APIKey))
	}
	hdr, _ := json.Marshal(in.ExtraHeaders)
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if id == nil {
			nid := ids.New()
			id = &nid
			_, err := tx.Exec(ctx, `INSERT INTO ai_providers (id, slug, kind, display_name, base_url, api_key_env, api_key_enc, enabled, rate_limit_rpm, max_concurrency, timeout_ms, extra_headers)
				VALUES ($1,$2,$3,$4,COALESCE($5,''),COALESCE($6,''),$7,COALESCE($8,false),COALESCE($9,60),COALESCE($10,4),COALESCE($11,60000),COALESCE($12,'{}'::jsonb))`,
				nid, *in.Slug, *in.Kind, *in.DisplayName, in.BaseURL, in.APIKeyEnv, enc, in.Enabled, in.RateLimitRPM, in.MaxConcurrency, in.TimeoutMS, nullJSON(in.ExtraHeaders, hdr))
			if db.IsUniqueViolation(err) {
				return apperr.Conflict("provider_slug_taken", "A provider with this slug exists.")
			}
			if err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE ai_providers SET display_name=COALESCE($2,display_name), base_url=COALESCE($3,base_url), api_key_env=COALESCE($4,api_key_env),
				api_key_enc=COALESCE($5,api_key_enc), enabled=COALESCE($6,enabled), rate_limit_rpm=COALESCE($7,rate_limit_rpm), max_concurrency=COALESCE($8,max_concurrency),
				timeout_ms=COALESCE($9,timeout_ms), extra_headers=COALESCE($10,extra_headers), version=version+1, updated_at=now()
				WHERE id=$1 AND ($11::int IS NULL OR version=$11)`, *id, in.DisplayName, in.BaseURL, in.APIKeyEnv, enc, in.Enabled, in.RateLimitRPM,
				in.MaxConcurrency, in.TimeoutMS, nullJSON(in.ExtraHeaders, hdr), in.Version)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return apperr.VersionConflict("provider")
			}
		}
		redacted := in
		if redacted.APIKey != nil {
			s := "[REDACTED]"
			redacted.APIKey = &s
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "ai.provider.upsert", ResourceType: "ai_provider", ResourceID: id.String(), New: redacted})
	})
	if err != nil {
		return nil, err
	}
	a.GW.Invalidate()
	_ = db.NotifyConfig(ctx, a.DB.Pool, "ai") // other processes (workers, API replicas)
	list, err := a.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		if p.ID == *id {
			return &p, nil
		}
	}
	return nil, apperr.NotFound("provider_not_found", "Provider not found.")
}

func nullJSON(m map[string]string, b []byte) any {
	if m == nil {
		return nil
	}
	return b
}

type ModelView struct {
	ID                 uuid.UUID      `json:"id"`
	ProviderID         uuid.UUID      `json:"provider_id"`
	ProviderSlug       string         `json:"provider_slug"`
	Slug               string         `json:"slug"`
	ModelName          string         `json:"model_name"`
	ModelVersion       string         `json:"model_version"`
	Kind               string         `json:"kind"`
	SupportsStructured bool           `json:"supports_structured"`
	SupportsJSONMode   bool           `json:"supports_json_mode"`
	Languages          []string       `json:"languages"`
	ContextWindow      int            `json:"context_window"`
	MaxOutputTokens    int            `json:"max_output_tokens"`
	EmbeddingDims      *int           `json:"embedding_dims,omitempty"`
	InputPricePerMTok  float64        `json:"input_price_per_mtok"`
	OutputPricePerMTok float64        `json:"output_price_per_mtok"`
	PricePerMChar      float64        `json:"price_per_mchar"`
	Enabled            bool           `json:"enabled"`
	Metadata           map[string]any `json:"metadata"`
	Version            int            `json:"version"`
}

type ModelInput struct {
	ProviderID         *uuid.UUID     `json:"provider_id,omitempty"`
	Slug               *string        `json:"slug,omitempty"`
	ModelName          *string        `json:"model_name,omitempty"`
	ModelVersion       *string        `json:"model_version,omitempty"`
	Kind               *string        `json:"kind,omitempty"`
	SupportsStructured *bool          `json:"supports_structured,omitempty"`
	SupportsJSONMode   *bool          `json:"supports_json_mode,omitempty"`
	Languages          []string       `json:"languages,omitempty"`
	ContextWindow      *int           `json:"context_window,omitempty"`
	MaxOutputTokens    *int           `json:"max_output_tokens,omitempty"`
	EmbeddingDims      *int           `json:"embedding_dims,omitempty"`
	InputPricePerMTok  *float64       `json:"input_price_per_mtok,omitempty"`
	OutputPricePerMTok *float64       `json:"output_price_per_mtok,omitempty"`
	PricePerMChar      *float64       `json:"price_per_mchar,omitempty"`
	Enabled            *bool          `json:"enabled,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	Version            *int           `json:"version,omitempty"`
}

func (a *Admin) ListModels(ctx context.Context) ([]ModelView, error) {
	rows, err := a.DB.Pool.Query(ctx, `SELECT m.id, m.provider_id, p.slug, m.slug, m.model_name, m.model_version, m.kind, m.supports_structured, m.supports_json_mode,
		m.languages, m.context_window, m.max_output_tokens, m.embedding_dims, m.input_price_per_mtok::float8, m.output_price_per_mtok::float8, m.price_per_mchar::float8,
		m.enabled, m.metadata, m.version FROM ai_models m JOIN ai_providers p ON p.id=m.provider_id ORDER BY p.slug, m.slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ModelView, error) {
		var m ModelView
		var meta []byte
		err := r.Scan(&m.ID, &m.ProviderID, &m.ProviderSlug, &m.Slug, &m.ModelName, &m.ModelVersion, &m.Kind, &m.SupportsStructured, &m.SupportsJSONMode,
			&m.Languages, &m.ContextWindow, &m.MaxOutputTokens, &m.EmbeddingDims, &m.InputPricePerMTok, &m.OutputPricePerMTok, &m.PricePerMChar, &m.Enabled, &meta, &m.Version)
		_ = json.Unmarshal(meta, &m.Metadata)
		return m, err
	})
}

var modelSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{1,126}$`)

func (a *Admin) UpsertModel(ctx context.Context, id *uuid.UUID, in ModelInput) (*ModelView, error) {
	var v apperr.Validator
	if id == nil {
		v.Check(in.ProviderID != nil, "provider_id", "required", "provider_id is required.")
		v.Check(in.Slug != nil && modelSlugRe.MatchString(*in.Slug), "slug", "invalid", "Invalid model slug.")
		v.Check(in.ModelName != nil && *in.ModelName != "", "model_name", "required", "model_name is required.")
		v.Check(in.Kind != nil && (*in.Kind == "chat" || *in.Kind == "embedding" || *in.Kind == "rerank" || *in.Kind == "tts" || *in.Kind == "stt"),
			"kind", "invalid", "kind must be chat, embedding, rerank, tts or stt.")
		if in.Kind != nil && *in.Kind == "embedding" {
			v.Check(in.EmbeddingDims != nil && *in.EmbeddingDims > 0 && *in.EmbeddingDims <= 8192, "embedding_dims", "required", "Embedding models need embedding_dims (1..8192).")
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	meta, _ := json.Marshal(in.Metadata)
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if id == nil {
			nid := ids.New()
			id = &nid
			_, err := tx.Exec(ctx, `INSERT INTO ai_models (id, provider_id, slug, model_name, model_version, kind, supports_structured, supports_json_mode, languages,
				context_window, max_output_tokens, embedding_dims, input_price_per_mtok, output_price_per_mtok, price_per_mchar, enabled, metadata)
				VALUES ($1,$2,$3,$4,COALESCE($5,''),$6,COALESCE($7,false),COALESCE($8,false),COALESCE($9::text[],'{ro,ru,en}'::text[]),COALESCE($10,8192),COALESCE($11,2048),$12,
				COALESCE($13,0),COALESCE($14,0),COALESCE($15,0),COALESCE($16,true),COALESCE($17,'{}'::jsonb))`,
				nid, *in.ProviderID, *in.Slug, *in.ModelName, in.ModelVersion, *in.Kind, in.SupportsStructured, in.SupportsJSONMode, in.Languages, in.ContextWindow,
				in.MaxOutputTokens, in.EmbeddingDims, in.InputPricePerMTok, in.OutputPricePerMTok, in.PricePerMChar, in.Enabled, nullMeta(in.Metadata, meta))
			if db.IsUniqueViolation(err) {
				return apperr.Conflict("model_slug_taken", "A model with this slug exists.")
			}
			if db.IsForeignKeyViolation(err) {
				return apperr.ValidationMsg("provider_not_found", "Unknown provider_id.")
			}
			if err != nil {
				return err
			}
		} else {
			if in.EmbeddingDims != nil {
				return apperr.ValidationMsg("embedding_dims_immutable", "Embedding dimensions are immutable; create a new model and migrate collections.")
			}
			tag, err := tx.Exec(ctx, `UPDATE ai_models SET model_name=COALESCE($2,model_name), model_version=COALESCE($3,model_version),
				supports_structured=COALESCE($4,supports_structured), supports_json_mode=COALESCE($5,supports_json_mode), languages=COALESCE($6::text[],languages),
				context_window=COALESCE($7,context_window), max_output_tokens=COALESCE($8,max_output_tokens), input_price_per_mtok=COALESCE($9,input_price_per_mtok),
				output_price_per_mtok=COALESCE($10,output_price_per_mtok), price_per_mchar=COALESCE($11,price_per_mchar), enabled=COALESCE($12,enabled),
				metadata=COALESCE($13,metadata), version=version+1, updated_at=now() WHERE id=$1 AND ($14::int IS NULL OR version=$14)`,
				*id, in.ModelName, in.ModelVersion, in.SupportsStructured, in.SupportsJSONMode, in.Languages, in.ContextWindow, in.MaxOutputTokens,
				in.InputPricePerMTok, in.OutputPricePerMTok, in.PricePerMChar, in.Enabled, nullMeta(in.Metadata, meta), in.Version)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return apperr.VersionConflict("model")
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "ai.model.upsert", ResourceType: "ai_model", ResourceID: id.String(), New: in})
	})
	if err != nil {
		return nil, err
	}
	a.GW.Invalidate()
	_ = db.NotifyConfig(ctx, a.DB.Pool, "ai") // other processes (workers, API replicas)
	list, err := a.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range list {
		if m.ID == *id {
			return &m, nil
		}
	}
	return nil, apperr.NotFound("model_not_found", "Model not found.")
}

func nullMeta(m map[string]any, b []byte) any {
	if m == nil {
		return nil
	}
	return b
}

type RouteView struct {
	ID               uuid.UUID   `json:"id"`
	Task             string      `json:"task"`
	Mode             string      `json:"mode"`
	PrimaryModelID   uuid.UUID   `json:"primary_model_id"`
	PrimaryModel     string      `json:"primary_model"`
	FallbackModelIDs []uuid.UUID `json:"fallback_model_ids"`
	PromptVersionID  *uuid.UUID  `json:"prompt_version_id,omitempty"`
	Temperature      float32     `json:"temperature"`
	MaxOutputTokens  int         `json:"max_output_tokens"`
	TimeoutMS        int         `json:"timeout_ms"`
	MaxAttempts      int         `json:"max_attempts"`
	QualityThreshold *float32    `json:"quality_threshold,omitempty"`
	DailyBudgetUSD   *float64    `json:"daily_budget_usd,omitempty"`
	SampleRate       float32     `json:"sample_rate"`
	OnUnavailable    string      `json:"on_unavailable"`
	Enabled          bool        `json:"enabled"`
	Version          int         `json:"version"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

type RouteInput struct {
	Task             *string     `json:"task,omitempty"`
	Mode             *string     `json:"mode,omitempty"`
	PrimaryModelID   *uuid.UUID  `json:"primary_model_id,omitempty"`
	FallbackModelIDs []uuid.UUID `json:"fallback_model_ids,omitempty"`
	PromptVersionID  *uuid.UUID  `json:"prompt_version_id,omitempty"`
	Temperature      *float32    `json:"temperature,omitempty"`
	MaxOutputTokens  *int        `json:"max_output_tokens,omitempty"`
	TimeoutMS        *int        `json:"timeout_ms,omitempty"`
	MaxAttempts      *int        `json:"max_attempts,omitempty"`
	QualityThreshold *float32    `json:"quality_threshold,omitempty"`
	DailyBudgetUSD   *float64    `json:"daily_budget_usd,omitempty"`
	SampleRate       *float32    `json:"sample_rate,omitempty"`
	OnUnavailable    *string     `json:"on_unavailable,omitempty"`
	Enabled          *bool       `json:"enabled,omitempty"`
	Version          *int        `json:"version,omitempty"`
}

const routeCols = `r.id, r.task, r.mode, r.primary_model_id, m.slug, r.fallback_model_ids, r.prompt_version_id, r.temperature, r.max_output_tokens, r.timeout_ms,
	r.max_attempts, r.quality_threshold, r.daily_budget_usd::float8, r.sample_rate, r.on_unavailable, r.enabled, r.version, r.updated_at`

func scanRoute(row pgx.Row) (RouteView, error) {
	var r RouteView
	err := row.Scan(&r.ID, &r.Task, &r.Mode, &r.PrimaryModelID, &r.PrimaryModel, &r.FallbackModelIDs, &r.PromptVersionID, &r.Temperature, &r.MaxOutputTokens,
		&r.TimeoutMS, &r.MaxAttempts, &r.QualityThreshold, &r.DailyBudgetUSD, &r.SampleRate, &r.OnUnavailable, &r.Enabled, &r.Version, &r.UpdatedAt)
	return r, err
}

func (a *Admin) ListRoutes(ctx context.Context) ([]RouteView, error) {
	rows, err := a.DB.Pool.Query(ctx, `SELECT `+routeCols+` FROM ai_task_routes r JOIN ai_models m ON m.id=r.primary_model_id ORDER BY r.task, r.mode`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RouteView, error) { return scanRoute(r) })
}

// UpsertRoute creates or changes a task route. Models must exist and match the task's kind.
func (a *Admin) UpsertRoute(ctx context.Context, id *uuid.UUID, in RouteInput) (*RouteView, error) {
	var out RouteView
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		var cur RouteView
		if id != nil {
			var err error
			cur, err = scanRoute(tx.QueryRow(ctx, `SELECT `+routeCols+` FROM ai_task_routes r JOIN ai_models m ON m.id=r.primary_model_id WHERE r.id=$1 FOR UPDATE OF r`, *id))
			if db.IsNoRows(err) {
				return apperr.NotFound("route_not_found", "Route not found.")
			}
			if err != nil {
				return err
			}
			if in.Version != nil && *in.Version != cur.Version {
				return apperr.VersionConflict("route")
			}
		} else {
			cur = RouteView{ID: ids.New(), Mode: "production", MaxOutputTokens: 1500, TimeoutMS: 60000, MaxAttempts: 2, SampleRate: 1, OnUnavailable: "defer", Enabled: true}
		}
		old := cur
		if in.Task != nil {
			if id != nil && *in.Task != cur.Task {
				return apperr.ValidationMsg("route_task_immutable", "A route's task cannot change.")
			}
			cur.Task = *in.Task
		}
		if in.Mode != nil {
			cur.Mode = *in.Mode
		}
		if in.PrimaryModelID != nil {
			cur.PrimaryModelID = *in.PrimaryModelID
		}
		if in.FallbackModelIDs != nil {
			cur.FallbackModelIDs = in.FallbackModelIDs
		}
		if in.PromptVersionID != nil {
			cur.PromptVersionID = in.PromptVersionID
		}
		if in.Temperature != nil {
			cur.Temperature = *in.Temperature
		}
		if in.MaxOutputTokens != nil {
			cur.MaxOutputTokens = *in.MaxOutputTokens
		}
		if in.TimeoutMS != nil {
			cur.TimeoutMS = *in.TimeoutMS
		}
		if in.MaxAttempts != nil {
			cur.MaxAttempts = *in.MaxAttempts
		}
		if in.QualityThreshold != nil {
			cur.QualityThreshold = in.QualityThreshold
		}
		if in.DailyBudgetUSD != nil {
			cur.DailyBudgetUSD = in.DailyBudgetUSD
		}
		if in.SampleRate != nil {
			cur.SampleRate = *in.SampleRate
		}
		if in.OnUnavailable != nil {
			cur.OnUnavailable = *in.OnUnavailable
		}
		if in.Enabled != nil {
			cur.Enabled = *in.Enabled
		}
		var v apperr.Validator
		v.Check(ValidTask(cur.Task), "task", "invalid", "Unknown task.")
		v.Check(cur.Mode == "production" || cur.Mode == "shadow" || cur.Mode == "experiment", "mode", "invalid", "mode must be production, shadow or experiment.")
		v.Check(cur.OnUnavailable == "defer" || cur.OnUnavailable == "fail", "on_unavailable", "invalid", "defer or fail")
		v.Check(cur.MaxAttempts >= 1 && cur.MaxAttempts <= 5, "max_attempts", "out_of_range", "1..5")
		v.Check(cur.SampleRate >= 0 && cur.SampleRate <= 1, "sample_rate", "out_of_range", "0..1")
		v.Check(cur.Temperature >= 0 && cur.Temperature <= 2, "temperature", "out_of_range", "0..2")
		if err := v.Err(); err != nil {
			return err
		}
		wantKind := "chat"
		switch Task(cur.Task) {
		case TaskEmbeddings:
			wantKind = "embedding"
		case TaskTTS:
			wantKind = "tts"
		}
		for _, mid := range append([]uuid.UUID{cur.PrimaryModelID}, cur.FallbackModelIDs...) {
			var kind string
			var dims *int
			if err := tx.QueryRow(ctx, `SELECT kind, embedding_dims FROM ai_models WHERE id=$1`, mid).Scan(&kind, &dims); err != nil {
				return apperr.ValidationMsg("route_model_not_found", "Unknown model id "+mid.String())
			}
			if kind != wantKind {
				return apperr.ValidationMsg("route_model_kind", "Model "+mid.String()+" is "+kind+"; task needs "+wantKind+".")
			}
		}
		if Task(cur.Task) == TaskEmbeddings && len(cur.FallbackModelIDs) > 0 {
			return apperr.ValidationMsg("embedding_fallback_forbidden", "Embedding routes cannot have fallbacks: vectors from different models are not comparable. Use a collection migration.")
		}
		if cur.PromptVersionID != nil {
			var task string
			if err := tx.QueryRow(ctx, `SELECT t.task FROM prompt_versions v JOIN prompt_templates t ON t.id=v.template_id WHERE v.id=$1`, *cur.PromptVersionID).Scan(&task); err != nil {
				return apperr.ValidationMsg("prompt_version_not_found", "Unknown prompt version.")
			}
			if task != cur.Task {
				return apperr.ValidationMsg("prompt_task_mismatch", "Prompt version belongs to task "+task+".")
			}
		}
		fb := cur.FallbackModelIDs
		if fb == nil {
			fb = []uuid.UUID{}
		}
		var err error
		if id == nil {
			_, err = tx.Exec(ctx, `INSERT INTO ai_task_routes (id, task, mode, primary_model_id, fallback_model_ids, prompt_version_id, temperature, max_output_tokens,
				timeout_ms, max_attempts, quality_threshold, daily_budget_usd, sample_rate, on_unavailable, enabled)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, cur.ID, cur.Task, cur.Mode, cur.PrimaryModelID, fb, cur.PromptVersionID, cur.Temperature,
				cur.MaxOutputTokens, cur.TimeoutMS, cur.MaxAttempts, cur.QualityThreshold, cur.DailyBudgetUSD, cur.SampleRate, cur.OnUnavailable, cur.Enabled)
		} else {
			_, err = tx.Exec(ctx, `UPDATE ai_task_routes SET mode=$2, primary_model_id=$3, fallback_model_ids=$4, prompt_version_id=$5, temperature=$6, max_output_tokens=$7,
				timeout_ms=$8, max_attempts=$9, quality_threshold=$10, daily_budget_usd=$11, sample_rate=$12, on_unavailable=$13, enabled=$14, version=version+1, updated_at=now()
				WHERE id=$1`, cur.ID, cur.Mode, cur.PrimaryModelID, fb, cur.PromptVersionID, cur.Temperature, cur.MaxOutputTokens, cur.TimeoutMS, cur.MaxAttempts,
				cur.QualityThreshold, cur.DailyBudgetUSD, cur.SampleRate, cur.OnUnavailable, cur.Enabled)
		}
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("route_exists", "An enabled route for this task and mode already exists.")
		}
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "ai.route.upsert", ResourceType: "ai_task_route", ResourceID: cur.ID.String(), Old: old, New: cur}); err != nil {
			return err
		}
		out, err = scanRoute(tx.QueryRow(ctx, `SELECT `+routeCols+` FROM ai_task_routes r JOIN ai_models m ON m.id=r.primary_model_id WHERE r.id=$1`, cur.ID))
		return err
	})
	if err != nil {
		return nil, err
	}
	a.GW.Invalidate()
	_ = db.NotifyConfig(ctx, a.DB.Pool, "ai") // other processes (workers, API replicas)
	return &out, nil
}

type PromptVersionView struct {
	ID           uuid.UUID       `json:"id"`
	TemplateID   uuid.UUID       `json:"template_id"`
	Task         string          `json:"task"`
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	SystemPrompt string          `json:"system_prompt"`
	UserTemplate string          `json:"user_template"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Status       string          `json:"status"`
	Notes        string          `json:"notes"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (a *Admin) ListPrompts(ctx context.Context, task string) ([]PromptVersionView, error) {
	rows, err := a.DB.Pool.Query(ctx, `SELECT v.id, t.id, t.task, t.name, v.version, v.system_prompt, v.user_template, v.output_schema, v.status, v.notes, v.created_at
		FROM prompt_versions v JOIN prompt_templates t ON t.id=v.template_id WHERE ($1='' OR t.task=$1) ORDER BY t.task, v.version DESC`, task)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PromptVersionView, error) {
		var p PromptVersionView
		err := r.Scan(&p.ID, &p.TemplateID, &p.Task, &p.Name, &p.Version, &p.SystemPrompt, &p.UserTemplate, &p.OutputSchema, &p.Status, &p.Notes, &p.CreatedAt)
		return p, err
	})
}

type PromptInput struct {
	Task         string `json:"task"`
	SystemPrompt string `json:"system_prompt"`
	UserTemplate string `json:"user_template"`
	Notes        string `json:"notes"`
	Activate     bool   `json:"activate"`
}

// CreatePromptVersion appends a new immutable prompt version for the task's template. The
// output schema is pinned to the task contract. Templates are validated by test-rendering.
func (a *Admin) CreatePromptVersion(ctx context.Context, in PromptInput) (*PromptVersionView, error) {
	if !ValidTask(in.Task) || SchemaFor(Task(in.Task)) == nil && Task(in.Task) != TaskExplanation {
		return nil, apperr.ValidationMsg("prompt_task_invalid", "Prompts can be versioned for structured generation tasks only.")
	}
	if !strings.Contains(in.SystemPrompt, "source_material") {
		return nil, apperr.ValidationMsg("prompt_missing_guard", "System prompt must reference the <source_material> untrusted-data boundary.")
	}
	if _, err := RenderPrompt(in.UserTemplate, sampleInput(Task(in.Task))); err != nil {
		return nil, apperr.ValidationMsg("prompt_template_invalid", err.Error())
	}
	var out PromptVersionView
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		var tmpl uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM prompt_templates WHERE task=$1 ORDER BY created_at LIMIT 1`, in.Task).Scan(&tmpl); err != nil {
			tmpl = ids.New()
			if _, err := tx.Exec(ctx, `INSERT INTO prompt_templates (id, task, name) VALUES ($1,$2,$3)`, tmpl, in.Task, in.Task); err != nil {
				return err
			}
		}
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM prompt_versions WHERE template_id=$1`, tmpl).Scan(&next); err != nil {
			return err
		}
		schema, _ := json.Marshal(SchemaFor(Task(in.Task)))
		status := "draft"
		if in.Activate {
			status = "active"
			if _, err := tx.Exec(ctx, `UPDATE prompt_versions SET status='retired' WHERE template_id=$1 AND status='active'`, tmpl); err != nil {
				return err
			}
		}
		id := ids.New()
		if _, err := tx.Exec(ctx, `INSERT INTO prompt_versions (id, template_id, version, system_prompt, user_template, output_schema, status, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, tmpl, next, in.SystemPrompt, in.UserTemplate, schema, status, in.Notes); err != nil {
			return err
		}
		if in.Activate {
			if _, err := tx.Exec(ctx, `UPDATE ai_task_routes SET prompt_version_id=$2, version=version+1, updated_at=now() WHERE task=$1 AND mode='production'`, in.Task, id); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "ai.prompt.version_created", ResourceType: "prompt_version", ResourceID: id.String(),
			New: map[string]any{"task": in.Task, "version": next, "activated": in.Activate}}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT v.id, t.id, t.task, t.name, v.version, v.system_prompt, v.user_template, v.output_schema, v.status, v.notes, v.created_at
			FROM prompt_versions v JOIN prompt_templates t ON t.id=v.template_id WHERE v.id=$1`, id).
			Scan(&out.ID, &out.TemplateID, &out.Task, &out.Name, &out.Version, &out.SystemPrompt, &out.UserTemplate, &out.OutputSchema, &out.Status, &out.Notes, &out.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	a.GW.Invalidate()
	_ = db.NotifyConfig(ctx, a.DB.Pool, "ai") // other processes (workers, API replicas)
	return &out, nil
}

func sampleInput(t Task) any {
	switch t {
	case TaskClassification:
		return &ClassifyInput{Title: "t", Text: "x", Categories: []CategoryDef{{Slug: "a", Names: map[string]string{"ro": "A"}}}}
	case TaskClaimExtraction:
		return &ClaimInput{Title: "t", Text: "x"}
	case TaskEntityExtraction:
		return &EntityInput{Text: "x"}
	case TaskEventEquivalence:
		return &EquivalenceInput{}
	case TaskEventSynthesis:
		return &SynthesisInput{Sources: []SourceSnippet{{Ref: "a1"}}}
	case TaskNoveltyDetection:
		return &NoveltyInput{}
	case TaskEvidenceAssessment:
		return &EvidenceInput{Evidence: []EvidenceItem{{Ref: "e1"}}}
	case TaskExplanation, TaskChatAnswering:
		return &AnswerInput{Context: []ContextDoc{{Ref: "e1"}}}
	case TaskScriptGeneration:
		return &ScriptInput{Sections: []ScriptSection{{Ref: "s1"}}}
	}
	return map[string]any{}
}

type ExecutionView struct {
	ID            uuid.UUID `json:"id"`
	Task          string    `json:"task"`
	Mode          string    `json:"mode"`
	ProviderSlug  string    `json:"provider_slug"`
	ModelSlug     string    `json:"model_slug"`
	Status        string    `json:"status"`
	Attempt       int       `json:"attempt"`
	IsFallback    bool      `json:"is_fallback"`
	InputTokens   int       `json:"input_tokens"`
	OutputTokens  int       `json:"output_tokens"`
	LatencyMS     int       `json:"latency_ms"`
	CostUSD       float64   `json:"cost_usd"`
	ErrorCategory *string   `json:"error_category,omitempty"`
	ErrorMessage  *string   `json:"error_message,omitempty"`
	SubjectType   *string   `json:"subject_type,omitempty"`
	SubjectID     *string   `json:"subject_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func (a *Admin) ListExecutions(ctx context.Context, task, status string, limit int) ([]ExecutionView, error) {
	rows, err := a.DB.Pool.Query(ctx, `SELECT id, task, mode, provider_slug, model_slug, status, attempt, is_fallback, input_tokens, output_tokens, latency_ms,
		cost_usd::float8, error_category, error_message, subject_type, subject_id, created_at FROM ai_executions
		WHERE ($1='' OR task=$1) AND ($2='' OR status=$2) ORDER BY created_at DESC LIMIT $3`, task, status, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ExecutionView, error) {
		var e ExecutionView
		err := r.Scan(&e.ID, &e.Task, &e.Mode, &e.ProviderSlug, &e.ModelSlug, &e.Status, &e.Attempt, &e.IsFallback, &e.InputTokens, &e.OutputTokens, &e.LatencyMS,
			&e.CostUSD, &e.ErrorCategory, &e.ErrorMessage, &e.SubjectType, &e.SubjectID, &e.CreatedAt)
		return e, err
	})
}

// UsageSummary aggregates cost/latency per task and model over a window.
func (a *Admin) UsageSummary(ctx context.Context, hours int) ([]map[string]any, error) {
	rows, err := a.DB.Pool.Query(ctx, `SELECT task, model_slug, count(*), count(*) FILTER (WHERE status='succeeded'), sum(input_tokens), sum(output_tokens),
		sum(cost_usd)::float8, percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms)
		FROM ai_executions WHERE created_at > now() - make_interval(hours => $1) GROUP BY task, model_slug ORDER BY task, model_slug`, hours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var task, model string
		var n, ok int
		var in, outT *int64
		var cost *float64
		var p50, p95 *float64
		if err := rows.Scan(&task, &model, &n, &ok, &in, &outT, &cost, &p50, &p95); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"task": task, "model": model, "calls": n, "succeeded": ok, "input_tokens": in, "output_tokens": outT,
			"cost_usd": cost, "latency_p50_ms": p50, "latency_p95_ms": p95})
	}
	return out, rows.Err()
}

func envSet(name string) bool { return lookupEnv(name) != "" }
