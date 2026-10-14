// Package seed installs idempotent baseline data: editorial policies, feature flags, AI
// providers/models/routes/prompts, an entity gazetteer and (optionally) a Moldova-focused
// category taxonomy. Everything seeded here is editable or replaceable through admin APIs.
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/ai"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
)

type Options struct {
	Categories bool
	Entities   bool
	// MLServiceURL registers the project's Python ML service (services/ml) as provider `local-ml`
	// with BGE-M3 embeddings and a multilingual reranker. On a fresh install (no vector
	// collections yet) the embeddings and reranking routes point at it; afterwards switching the
	// embedding model must go through an embedding migration.
	MLServiceURL string
}

func Run(ctx context.Context, d *db.DB, o Options) error {
	return d.Tx(ctx, func(tx pgx.Tx) error {
		if err := policies(ctx, tx); err != nil {
			return fmt.Errorf("policies: %w", err)
		}
		if err := aiPlatform(ctx, tx, o.MLServiceURL); err != nil {
			return fmt.Errorf("ai: %w", err)
		}
		if o.Entities {
			if err := entities(ctx, tx); err != nil {
				return fmt.Errorf("entities: %w", err)
			}
		}
		if o.Categories {
			if err := categories(ctx, tx); err != nil {
				return fmt.Errorf("categories: %w", err)
			}
		}
		return nil
	})
}

// DefaultPolicies are versioned, DB-managed policy documents.
var DefaultPolicies = map[string]struct {
	Desc  string
	Value any
}{
	"event_matching": {"Event candidate retrieval and matching thresholds", map[string]any{
		"time_window_hours": 120, "max_candidates": 12, "min_candidate_score": 0.22, "same_story_score": 0.5,
		"ambiguous_low": 0.3, "use_llm_for_ambiguous": true, "require_shared_anchor": true,
		"weights": map[string]any{"semantic": 0.45, "entities": 0.25, "facts": 0.2, "time": 0.1}}},
	"factcheck": {"Evidence sufficiency and review gates (policy_version increments on change)", map[string]any{
		"policy_version": 1, "min_independent_supporting": 2, "min_independent_supporting_with_primary": 1,
		"min_independent_contradicting": 1, "outdated_after_days": 365, "evidence_window_days": 30,
		"review_required_verdicts": []string{"refuted", "misleading_context", "disputed"},
		"review_confidence_below":  0.6,
		"high_risk_terms": []string{"alegeri", "vot", "fraudă", "război", "armată", "vaccin", "epidemie", "terorism", "corupție",
			"выборы", "фальсификац", "война", "армия", "вакцин", "эпидеми", "терроризм", "коррупц"}}},
	"ranking": {"Feed ranking weights (no click-through optimization)", map[string]any{
		"weights":                 map[string]any{"relevance": 0.35, "recency": 0.25, "importance": 0.2, "novelty": 0.15, "source_diversity": 0.05},
		"recency_half_life_hours": 18, "disputed_penalty": 0.1, "max_per_category_ratio": 0.5}},
	"notifications": {"Notification anti-spam and breaking-news policy", map[string]any{
		"daily_budget": 6, "update_min_severity": "major", "update_cooldown_minutes": 60, "digest_lookback_hours": 24, "min_importance_default": 0.3,
		"breaking_min_importance": 0.8, "breaking_min_sources": 2, "breaking_require_no_dispute": true, "breaking_max_age_minutes": 180,
		"breaking_min_confidence": 0.6}},
	"chat":      {"Chat retention and context bounds", map[string]any{"retention_days": 30, "max_history_turns": 6, "max_context_docs": 8}},
	"search":    {"Hybrid retrieval", map[string]any{"rrf_k": 60, "lexical_k": 50, "semantic_k": 50, "rerank_top": 20}},
	"briefings": {"Briefing composition", map[string]any{"events_per_minute": 1.2, "max_events": 12, "words_per_minute": 150}},
}

func policies(ctx context.Context, tx pgx.Tx) error {
	for k, v := range DefaultPolicies {
		b, _ := json.Marshal(v.Value)
		if _, err := tx.Exec(ctx, `INSERT INTO editorial_policies (key, value, description) VALUES ($1,$2,$3) ON CONFLICT (key) DO NOTHING`, k, b, v.Desc); err != nil {
			return err
		}
	}
	flags := map[string]string{
		"live_crawling":                "Scheduler polls non-fixture sources (also requires CRAWL_LIVE_ENABLED)",
		"llm_event_equivalence":        "Use the event_equivalence route for ambiguous candidates",
		"personalized_vector_interest": "Consent-based vector interest matching",
		"breaking_notifications":       "Send breaking-news pushes (subject to policy)",
	}
	for k, d := range flags {
		enabled := k == "llm_event_equivalence"
		if _, err := tx.Exec(ctx, `INSERT INTO feature_flags (key, enabled, description) VALUES ($1,$2,$3) ON CONFLICT (key) DO NOTHING`, k, enabled, d); err != nil {
			return err
		}
	}
	return nil
}

func upsertProvider(ctx context.Context, tx pgx.Tx, slug, kind, name, baseURL, keyEnv string, enabled bool) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO ai_providers (id, slug, kind, display_name, base_url, api_key_env, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (slug) DO UPDATE SET slug=EXCLUDED.slug RETURNING id`,
		ids.New(), slug, kind, name, baseURL, keyEnv, enabled).Scan(&id)
	return id, err
}

type modelSeed struct {
	slug, name, version, kind string
	structured                bool
	dims                      *int
	inPrice, outPrice         float64
	enabled                   bool
	ctxWindow, maxOut         int
}

func upsertModel(ctx context.Context, tx pgx.Tx, provider uuid.UUID, m modelSeed) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO ai_models (id, provider_id, slug, model_name, model_version, kind, supports_structured, supports_json_mode,
		embedding_dims, input_price_per_mtok, output_price_per_mtok, enabled, context_window, max_output_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (slug) DO UPDATE SET slug=EXCLUDED.slug RETURNING id`,
		ids.New(), provider, m.slug, m.name, m.version, m.kind, m.structured, m.dims, m.inPrice, m.outPrice, m.enabled, m.ctxWindow, m.maxOut).Scan(&id)
	return id, err
}

func aiPlatform(ctx context.Context, tx pgx.Tx, mlURL string) error {
	heur, err := upsertProvider(ctx, tx, "heuristic", "heuristic", "Deterministic rule-based baseline (local)", "", "", true)
	if err != nil {
		return err
	}
	dims := ai.HashEmbeddingDims
	rules, err := upsertModel(ctx, tx, heur, modelSeed{slug: "local-rules", name: "local-rules", version: "1", kind: "chat", structured: true, enabled: true, ctxWindow: 1 << 20, maxOut: 1 << 16})
	if err != nil {
		return err
	}
	embed, err := upsertModel(ctx, tx, heur, modelSeed{slug: "local-hash-embed-384", name: "local-hash-v1", version: "1", kind: "embedding", dims: &dims, enabled: true, ctxWindow: 1 << 20, maxOut: 0})
	if err != nil {
		return err
	}
	// Remote providers are seeded DISABLED: enabling requires an API key env var and an admin action.
	anth, err := upsertProvider(ctx, tx, "anthropic", "anthropic", "Anthropic Claude", "", "ANTHROPIC_API_KEY", false)
	if err != nil {
		return err
	}
	if _, err := upsertModel(ctx, tx, anth, modelSeed{slug: "claude-opus-5-5", name: "claude-opus-5-5", version: "5.5", kind: "chat", structured: true,
		inPrice: 4, outPrice: 20, enabled: true, ctxWindow: 1_000_000, maxOut: 16000}); err != nil {
		return err
	}
	if _, err := upsertProvider(ctx, tx, "openai", "openai_compatible", "OpenAI-compatible endpoint", "https://api.openai.com/v1", "OPENAI_API_KEY", false); err != nil {
		return err
	}
	if _, err := upsertProvider(ctx, tx, "gemini", "gemini", "Google Gemini", "https://generativelanguage.googleapis.com/v1beta", "GEMINI_API_KEY", false); err != nil {
		return err
	}
	if _, err := upsertProvider(ctx, tx, "local-vllm", "openai_compatible", "Local vLLM / Ollama (OpenAI-compatible)", "http://localhost:8000/v1", "", false); err != nil {
		return err
	}
	// Prompt templates + active versions; routes pin the version.
	for _, p := range ai.DefaultPrompts {
		var tmplID uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO prompt_templates (id, task, name) VALUES ($1,$2,$3) ON CONFLICT (name) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
			ids.New(), string(p.Task), p.Name).Scan(&tmplID); err != nil {
			return err
		}
		schema, _ := json.Marshal(ai.SchemaFor(p.Task))
		var pvID uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO prompt_versions (id, template_id, version, system_prompt, user_template, output_schema, status)
			VALUES ($1,$2,1,$3,$4,$5,'active') ON CONFLICT (template_id, version) DO UPDATE SET version=EXCLUDED.version RETURNING id`,
			ids.New(), tmplID, p.System, p.User, schema).Scan(&pvID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ai_task_routes (id, task, mode, primary_model_id, prompt_version_id, max_output_tokens, timeout_ms, max_attempts)
			SELECT $1,$2,'production',$3,$4,4000,60000,2 WHERE NOT EXISTS (SELECT 1 FROM ai_task_routes WHERE task=$2 AND mode='production')`,
			ids.New(), string(p.Task), rules, pvID); err != nil {
			return err
		}
	}
	if mlURL != "" {
		base := strings.TrimSuffix(mlURL, "/")
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		ml, err := upsertProvider(ctx, tx, "local-ml", "openai_compatible", "Project ML service (BGE-M3 embeddings, multilingual reranker)", base, "ML_API_KEY", true)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ai_providers SET base_url=$2, enabled=true WHERE id=$1`, ml, base); err != nil {
			return err
		}
		mlDims := 1024
		bge, err := upsertModel(ctx, tx, ml, modelSeed{slug: "bge-m3", name: "bge-m3", version: "BAAI/bge-m3", kind: "embedding", dims: &mlDims, enabled: true, ctxWindow: 8192})
		if err != nil {
			return err
		}
		rr, err := upsertModel(ctx, tx, ml, modelSeed{slug: "multilingual-reranker", name: "multilingual-reranker", version: "jinaai/jina-reranker-v2-base-multilingual",
			kind: "rerank", enabled: true, ctxWindow: 1024})
		if err != nil {
			return err
		}
		// Fresh install only: nothing has been embedded yet, so no migration is needed.
		if _, err := tx.Exec(ctx, `INSERT INTO ai_task_routes (id, task, mode, primary_model_id, on_unavailable)
			SELECT $1,'embeddings','production',$2,'defer' WHERE NOT EXISTS (SELECT 1 FROM ai_task_routes WHERE task='embeddings' AND mode='production')`, ids.New(), bge); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ai_task_routes SET primary_model_id=$1, version=version+1, updated_at=now()
			WHERE task='embeddings' AND mode='production' AND primary_model_id=$2 AND NOT EXISTS (SELECT 1 FROM vector_collections)`, bge, embed); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ai_task_routes (id, task, mode, primary_model_id, on_unavailable, timeout_ms)
			SELECT $1,'reranking','production',$2,'fail',30000 WHERE NOT EXISTS (SELECT 1 FROM ai_task_routes WHERE task='reranking' AND mode='production')`,
			ids.New(), rr); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ai_task_routes SET primary_model_id=$1, version=version+1, updated_at=now()
			WHERE task='reranking' AND mode='production' AND primary_model_id=$2`, rr, rules); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO ai_task_routes (id, task, mode, primary_model_id, on_unavailable)
		SELECT $1,'embeddings','production',$2,'defer' WHERE NOT EXISTS (SELECT 1 FROM ai_task_routes WHERE task='embeddings' AND mode='production')`, ids.New(), embed)
	return err
}

type entitySeed struct {
	kind, name string
	aliases    []string
}

// Gazetteer of institutions and places (public bodies only); extendable via the entities table.
var entitySeeds = []entitySeed{
	{"location", "Republica Moldova", []string{"Moldova", "Молдова", "Республика Молдова", "RM"}},
	{"location", "Chișinău", []string{"Chisinau", "Кишинев", "Кишинёв", "capitala"}},
	{"location", "Bălți", []string{"Balti", "Бельцы"}},
	{"location", "Comrat", []string{"Комрат"}},
	{"location", "Găgăuzia", []string{"UTA Găgăuzia", "Гагаузия", "АТО Гагаузия"}},
	{"location", "Transnistria", []string{"regiunea transnistreană", "stânga Nistrului", "Приднестровье"}},
	{"location", "Tiraspol", []string{"Тирасполь"}},
	{"location", "Cahul", []string{"Кагул"}},
	{"location", "Ungheni", []string{"Унгены"}},
	{"location", "Orhei", []string{"Орхей"}},
	{"location", "Ucraina", []string{"Украина"}},
	{"location", "România", []string{"Romania", "Румыния"}},
	{"location", "Rusia", []string{"Россия", "Federația Rusă"}},
	{"organization", "Uniunea Europeană", []string{"UE", "Евросоюз", "Европейский союз", "ЕС"}},
	{"organization", "Parlamentul Republicii Moldova", []string{"Parlamentul", "Legislativul", "Парламент", "парламент Молдовы"}},
	{"organization", "Guvernul Republicii Moldova", []string{"Guvernul", "Executivul", "Cabinetul de miniștri", "Правительство", "правительство Молдовы"}},
	{"organization", "Președinția Republicii Moldova", []string{"Președinția", "Президентура", "Администрация президента"}},
	{"organization", "Banca Națională a Moldovei", []string{"BNM", "Национальный банк Молдовы", "НБМ"}},
	{"organization", "Biroul Național de Statistică", []string{"BNS", "Национальное бюро статистики", "НБС"}},
	{"organization", "Comisia Electorală Centrală", []string{"CEC", "Центральная избирательная комиссия", "ЦИК"}},
	{"organization", "Curtea Constituțională", []string{"Конституционный суд"}},
	{"organization", "Curtea Supremă de Justiție", []string{"CSJ", "Высшая судебная палата", "ВСП"}},
	{"organization", "Procuratura Generală", []string{"Генеральная прокуратура"}},
	{"organization", "Procuratura Anticorupție", []string{"PA", "Антикоррупционная прокуратура"}},
	{"organization", "Centrul Național Anticorupție", []string{"CNA", "Национальный антикоррупционный центр", "НЦБК"}},
	{"organization", "Agenția Națională pentru Reglementare în Energetică", []string{"ANRE", "НАРЭ", "Национальное агентство по регулированию в энергетике"}},
	{"organization", "Ministerul Energiei", []string{"Министерство энергетики"}},
	{"organization", "Ministerul Finanțelor", []string{"Министерство финансов", "Минфин"}},
	{"organization", "Ministerul Educației și Cercetării", []string{"Ministerul Educației", "Министерство образования"}},
	{"organization", "Ministerul Sănătății", []string{"Министерство здравоохранения"}},
	{"organization", "Ministerul Agriculturii și Industriei Alimentare", []string{"Ministerul Agriculturii", "Министерство сельского хозяйства"}},
	{"organization", "Primăria Chișinău", []string{"Primăria municipiului Chișinău", "Примэрия Кишинева", "мэрия Кишинева"}},
	{"organization", "Energocom", []string{"Энергоком"}},
	{"organization", "Moldovagaz", []string{"Молдовагаз"}},
	{"person", "Maia Sandu", []string{"Майя Санду", "președinta Maia Sandu"}},
}

func entities(ctx context.Context, tx pgx.Tx) error {
	for _, e := range entitySeeds {
		key := gazKey(e.name)
		if _, err := tx.Exec(ctx, `INSERT INTO entities (id, kind, canonical_name, normalized_key, aliases) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (kind, normalized_key) DO NOTHING`, ids.New(), e.kind, e.name, key, e.aliases); err != nil {
			return err
		}
	}
	return nil
}
