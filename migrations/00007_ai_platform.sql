-- +goose Up
-- Provider-agnostic AI platform: providers, models (with capabilities), prompts, task routes,
-- execution accounting, vector collection registry and offline evaluations.

CREATE TABLE ai_providers (
    id               uuid PRIMARY KEY,
    slug             text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9_-]{1,62}$'),
    kind             text NOT NULL CHECK (kind IN ('openai_compatible','anthropic','gemini','heuristic')),
    display_name     text NOT NULL,
    base_url         text NOT NULL DEFAULT '',
    api_key_env      text NOT NULL DEFAULT '',   -- name of env var holding the key (preferred)
    api_key_enc      bytea,                      -- or AES-GCM encrypted key stored via admin API
    enabled          boolean NOT NULL DEFAULT true,
    rate_limit_rpm   int NOT NULL DEFAULT 60 CHECK (rate_limit_rpm > 0),
    max_concurrency  int NOT NULL DEFAULT 4 CHECK (max_concurrency > 0),
    timeout_ms       int NOT NULL DEFAULT 60000 CHECK (timeout_ms > 0),
    extra_headers    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by       uuid,
    updated_by       uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    version          int NOT NULL DEFAULT 1
);

-- ai_model_configs + model_capabilities are merged: one row per callable model with capability flags.
CREATE TABLE ai_models (
    id                        uuid PRIMARY KEY,
    provider_id               uuid NOT NULL REFERENCES ai_providers(id) ON DELETE RESTRICT,
    slug                      text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9_.:-]{1,126}$'),
    model_name                text NOT NULL,          -- provider's model identifier
    model_version             text NOT NULL DEFAULT '',
    kind                      text NOT NULL CHECK (kind IN ('chat','embedding','rerank','tts','stt')),
    supports_structured       boolean NOT NULL DEFAULT false,  -- native JSON-schema constrained output
    supports_json_mode        boolean NOT NULL DEFAULT false,
    languages                 text[] NOT NULL DEFAULT '{ro,ru,en}',
    context_window            int NOT NULL DEFAULT 8192,
    max_output_tokens         int NOT NULL DEFAULT 2048,
    embedding_dims            int,
    input_price_per_mtok      numeric(12,6) NOT NULL DEFAULT 0,
    output_price_per_mtok     numeric(12,6) NOT NULL DEFAULT 0,
    price_per_mchar           numeric(12,6) NOT NULL DEFAULT 0, -- TTS
    rate_limit_rpm            int,
    max_concurrency           int,
    enabled                   boolean NOT NULL DEFAULT true,
    metadata                  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    version                   int NOT NULL DEFAULT 1,
    CHECK (kind <> 'embedding' OR embedding_dims IS NOT NULL)
);

CREATE TABLE prompt_templates (
    id           uuid PRIMARY KEY,
    task         text NOT NULL,
    name         text NOT NULL UNIQUE,
    description  text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE prompt_versions (
    id             uuid PRIMARY KEY,
    template_id    uuid NOT NULL REFERENCES prompt_templates(id) ON DELETE CASCADE,
    version        int NOT NULL,
    system_prompt  text NOT NULL,
    user_template  text NOT NULL,
    output_schema  jsonb,                  -- JSON schema for structured tasks
    status         text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active','retired')),
    notes          text NOT NULL DEFAULT '',
    created_by     uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (template_id, version)
);

CREATE TABLE ai_task_routes (
    id                  uuid PRIMARY KEY,
    task                text NOT NULL,
    mode                text NOT NULL DEFAULT 'production' CHECK (mode IN ('production','shadow','experiment')),
    primary_model_id    uuid NOT NULL REFERENCES ai_models(id) ON DELETE RESTRICT,
    fallback_model_ids  uuid[] NOT NULL DEFAULT '{}',
    prompt_version_id   uuid REFERENCES prompt_versions(id) ON DELETE RESTRICT,
    temperature         real NOT NULL DEFAULT 0 CHECK (temperature BETWEEN 0 AND 2),
    max_output_tokens   int NOT NULL DEFAULT 1500 CHECK (max_output_tokens > 0),
    timeout_ms          int NOT NULL DEFAULT 60000 CHECK (timeout_ms > 0),
    max_attempts        int NOT NULL DEFAULT 2 CHECK (max_attempts BETWEEN 1 AND 5),
    quality_threshold   real,
    daily_budget_usd    numeric(12,4),
    sample_rate         real NOT NULL DEFAULT 1 CHECK (sample_rate BETWEEN 0 AND 1),  -- shadow/experiment sampling
    on_unavailable      text NOT NULL DEFAULT 'defer' CHECK (on_unavailable IN ('defer','fail')),
    enabled             boolean NOT NULL DEFAULT true,
    updated_by          uuid,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    version             int NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX ai_task_routes_task_mode_uidx ON ai_task_routes (task, mode) WHERE enabled;

CREATE TABLE ai_executions (
    id                  uuid PRIMARY KEY,
    task                text NOT NULL,
    route_id            uuid,
    mode                text NOT NULL DEFAULT 'production',
    provider_slug       text NOT NULL,
    model_slug          text NOT NULL,
    model_version       text NOT NULL DEFAULT '',
    prompt_version_id   uuid,
    status              text NOT NULL CHECK (status IN ('succeeded','failed','invalid_output','deferred','skipped_budget')),
    attempt             int NOT NULL DEFAULT 1,
    is_fallback         boolean NOT NULL DEFAULT false,
    input_tokens        int NOT NULL DEFAULT 0,
    output_tokens       int NOT NULL DEFAULT 0,
    latency_ms          int NOT NULL DEFAULT 0,
    cost_usd            numeric(14,8) NOT NULL DEFAULT 0,
    error_category      text,
    error_message       text,
    trace_id            text,
    subject_type        text,
    subject_id          text,
    request_hash        text,
    output              jsonb,           -- structured output (no raw prompts stored by default)
    experiment_id       uuid,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_executions_task_time_idx ON ai_executions (task, created_at DESC);
CREATE INDEX ai_executions_subject_idx ON ai_executions (subject_type, subject_id);
CREATE INDEX ai_executions_model_day_idx ON ai_executions (model_slug, created_at);

-- Qdrant collection registry: one collection per doc type + embedding model + dimension.
CREATE TABLE vector_collections (
    id               uuid PRIMARY KEY,
    doc_type         text NOT NULL CHECK (doc_type IN ('article','event','claim')),
    model_id         uuid NOT NULL REFERENCES ai_models(id) ON DELETE RESTRICT,
    collection_name  text NOT NULL UNIQUE,
    dims             int NOT NULL CHECK (dims > 0),
    status           text NOT NULL DEFAULT 'building' CHECK (status IN ('building','active','retired')),
    backfill_job_id  uuid,
    backfilled_count int NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    activated_at     timestamptz,
    retired_at       timestamptz
);
CREATE UNIQUE INDEX vector_collections_active_uidx ON vector_collections (doc_type) WHERE status = 'active';

CREATE TABLE eval_runs (
    id            uuid PRIMARY KEY,
    name          text NOT NULL,
    suite         text NOT NULL,
    task          text NOT NULL,
    model_slug    text NOT NULL,
    dataset       text NOT NULL,
    config        jsonb NOT NULL DEFAULT '{}'::jsonb,
    metrics       jsonb NOT NULL DEFAULT '{}'::jsonb,
    status        text NOT NULL DEFAULT 'processing' CHECK (status IN ('processing','succeeded','failed')),
    error         text,
    created_by    uuid,
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);
CREATE INDEX eval_runs_suite_idx ON eval_runs (suite, started_at DESC);

CREATE TABLE eval_results (
    run_id     uuid NOT NULL REFERENCES eval_runs(id) ON DELETE CASCADE,
    case_id    text NOT NULL,
    expected   jsonb NOT NULL,
    actual     jsonb,
    scores     jsonb NOT NULL DEFAULT '{}'::jsonb,
    passed     boolean NOT NULL,
    PRIMARY KEY (run_id, case_id)
);

-- +goose Down
DROP TABLE IF EXISTS eval_results, eval_runs, vector_collections, ai_executions, ai_task_routes, prompt_versions,
  prompt_templates, ai_models, ai_providers;
