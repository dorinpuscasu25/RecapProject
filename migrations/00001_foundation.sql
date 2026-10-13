-- +goose Up
-- Foundation: extensions, audit, outbox, consumer dedupe, deliveries, jobs, feature flags, policies.

CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- Immutable wrapper so unaccent can be used in generated columns / indexes.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION immutable_unaccent(text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT AS $$ SELECT public.unaccent('public.unaccent'::regdictionary, $1) $$;
-- +goose StatementEnd

-- Audit log: append-only record of security-critical and editorial/config changes.
CREATE TABLE audit_logs (
    id             uuid PRIMARY KEY,
    actor_type     text NOT NULL CHECK (actor_type IN ('user','service','system')),
    actor_id       uuid,
    action         text NOT NULL,
    resource_type  text NOT NULL,
    resource_id    text,
    old_value      jsonb,
    new_value      jsonb,
    request_id     text,
    ip_prefix      text,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_resource_idx ON audit_logs (resource_type, resource_id, created_at DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_id, created_at DESC);
CREATE INDEX audit_logs_created_idx ON audit_logs (created_at DESC);

-- Transactional outbox. A row is written in the same transaction as the state change.
CREATE TABLE outbox_events (
    id              uuid PRIMARY KEY,               -- message id (idempotency key for consumers)
    topic           text NOT NULL,
    event_type      text NOT NULL,                  -- e.g. article.enriched.v1
    aggregate_type  text NOT NULL,
    aggregate_id    text NOT NULL,
    partition_key   text NOT NULL,
    payload         jsonb NOT NULL,
    headers         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    available_at    timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    attempts        int NOT NULL DEFAULT 0,
    last_error      text
);
CREATE INDEX outbox_unpublished_idx ON outbox_events (available_at, created_at) WHERE published_at IS NULL;

-- Per-consumer deliveries used by BUS_DRIVER=postgres (durable in-database queue).
CREATE TABLE message_deliveries (
    message_id      uuid NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    consumer        text NOT NULL,
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','processing','succeeded','failed','dead')),
    attempts        int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz,
    last_error      text,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, consumer)
);
CREATE INDEX message_deliveries_ready_idx ON message_deliveries (consumer, next_attempt_at) WHERE status IN ('queued','failed');

-- Inbound idempotency: a consumer records the message id in the same tx as its effects.
CREATE TABLE consumer_dedupe (
    consumer      text NOT NULL,
    message_id    uuid NOT NULL,
    processed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, message_id)
);
CREATE INDEX consumer_dedupe_processed_idx ON consumer_dedupe (processed_at);

-- Dead letters (both bus drivers). Replayable via newsctl / admin jobs API.
CREATE TABLE dead_letters (
    id           uuid PRIMARY KEY,
    consumer     text NOT NULL,
    message_id   uuid NOT NULL,
    topic        text NOT NULL,
    event_type   text NOT NULL,
    payload      jsonb NOT NULL,
    headers      jsonb NOT NULL DEFAULT '{}'::jsonb,
    error        text NOT NULL,
    attempts     int NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    replayed_at  timestamptz,
    UNIQUE (consumer, message_id)
);

-- Durable job runner (workflow steps, reprocessing, briefings, scheduled reports).
CREATE TABLE jobs (
    id               uuid PRIMARY KEY,
    kind             text NOT NULL,
    payload          jsonb NOT NULL DEFAULT '{}'::jsonb,
    status           text NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued','processing','succeeded','failed','quarantined','cancelled')),
    priority         int NOT NULL DEFAULT 100,
    attempts         int NOT NULL DEFAULT 0,
    max_attempts     int NOT NULL DEFAULT 5,
    run_at           timestamptz NOT NULL DEFAULT now(),
    locked_by        text,
    locked_until     timestamptz,
    idempotency_key  text UNIQUE,
    last_error       text,
    result           jsonb,
    created_by       uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz
);
CREATE INDEX jobs_ready_idx ON jobs (priority, run_at) WHERE status = 'queued';
CREATE INDEX jobs_kind_status_idx ON jobs (kind, status, created_at DESC);

CREATE TABLE feature_flags (
    key              text PRIMARY KEY,
    enabled          boolean NOT NULL DEFAULT false,
    rollout_percent  int NOT NULL DEFAULT 100 CHECK (rollout_percent BETWEEN 0 AND 100),
    description      text NOT NULL DEFAULT '',
    updated_by       uuid,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Database-managed editorial/scoring policies (event matching thresholds, fact-check sufficiency,
-- ranking weights, notification budgets). Versioned with optimistic concurrency.
CREATE TABLE editorial_policies (
    key          text PRIMARY KEY,
    value        jsonb NOT NULL,
    description  text NOT NULL DEFAULT '',
    version      int NOT NULL DEFAULT 1,
    updated_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Product analytics (bounded, minimized). Optional sink; ClickHouse in larger setups.
CREATE TABLE analytics_events (
    id           uuid PRIMARY KEY,
    event_name   text NOT NULL,
    subject_hash text,
    properties   jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX analytics_events_name_time_idx ON analytics_events (event_name, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS analytics_events, editorial_policies, feature_flags, jobs, dead_letters,
    consumer_dedupe, message_deliveries, outbox_events, audit_logs;
DROP FUNCTION IF EXISTS immutable_unaccent(text);
