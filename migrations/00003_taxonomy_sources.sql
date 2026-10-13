-- +goose Up
-- Taxonomy and dynamic source configuration.

CREATE TABLE categories (
    id                     uuid PRIMARY KEY,
    slug                   citext NOT NULL CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) <= 80),
    parent_id              uuid REFERENCES categories(id) ON DELETE RESTRICT,
    names                  jsonb NOT NULL,          -- {"ro": "...", "ru": "...", "en": "..."}
    descriptions           jsonb NOT NULL DEFAULT '{}'::jsonb,
    editorial_description  text NOT NULL DEFAULT '',
    icon_key               text NOT NULL DEFAULT '',
    display_order          int NOT NULL DEFAULT 0,
    status                 text NOT NULL DEFAULT 'active' CHECK (status IN ('active','hidden','archived','merged')),
    classification_hints   text[] NOT NULL DEFAULT '{}',
    positive_examples      text[] NOT NULL DEFAULT '{}',
    negative_examples      text[] NOT NULL DEFAULT '{}',
    routing_labels         text[] NOT NULL DEFAULT '{}',
    aliases                text[] NOT NULL DEFAULT '{}',
    boost                  real NOT NULL DEFAULT 0 CHECK (boost BETWEEN -1 AND 1),
    auto_classify          boolean NOT NULL DEFAULT true,
    created_by             uuid,
    updated_by             uuid,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    version                int NOT NULL DEFAULT 1,
    CONSTRAINT categories_not_self_parent CHECK (parent_id IS NULL OR parent_id <> id),
    CONSTRAINT categories_names_ro CHECK (names ? 'ro')
);
CREATE UNIQUE INDEX categories_slug_uidx ON categories (slug);
CREATE INDEX categories_parent_idx ON categories (parent_id, display_order);

-- Merged/renamed categories keep old ids resolvable (subscriptions are not broken).
CREATE TABLE category_redirects (
    from_category_id  uuid PRIMARY KEY REFERENCES categories(id) ON DELETE CASCADE,
    to_category_id    uuid NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    reason            text NOT NULL DEFAULT '',
    created_by        uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (from_category_id <> to_category_id)
);

-- Monotonic taxonomy version; bumped on every category change; used to invalidate classifier caches.
CREATE TABLE taxonomy_versions (
    version     bigserial PRIMARY KEY,
    summary     text NOT NULL,
    changed_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE publishers (
    id                 uuid PRIMARY KEY,
    name               text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    canonical_domain   text NOT NULL CHECK (canonical_domain ~ '^[a-z0-9.-]+\.[a-z]{2,}$'),
    country            text NOT NULL DEFAULT 'MD' CHECK (country ~ '^[A-Z]{2}$'),
    primary_languages  text[] NOT NULL DEFAULT '{ro}',
    publisher_type     text NOT NULL CHECK (publisher_type IN ('newsroom','government','public_institution','fact_checker','other')),
    metadata           jsonb NOT NULL DEFAULT '{}'::jsonb,
    status             text NOT NULL DEFAULT 'pending_permission'
                       CHECK (status IN ('active','paused','disabled','archived','pending_permission')),
    created_by         uuid,
    updated_by         uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    version            int NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX publishers_domain_uidx ON publishers (canonical_domain);

CREATE TABLE extraction_profiles (
    id                   uuid PRIMARY KEY,
    name                 text NOT NULL UNIQUE,
    title_selectors      text[] NOT NULL DEFAULT '{}',
    content_selectors    text[] NOT NULL DEFAULT '{}',
    date_selectors       text[] NOT NULL DEFAULT '{}',
    canonical_selectors  text[] NOT NULL DEFAULT '{}',
    author_selectors     text[] NOT NULL DEFAULT '{}',
    cleanup_selectors    text[] NOT NULL DEFAULT '{}',
    pagination           jsonb NOT NULL DEFAULT '{}'::jsonb,
    fixtures             jsonb NOT NULL DEFAULT '[]'::jsonb,  -- [{name, html, expected:{title, author, contains:[...]}}]
    created_by           uuid,
    updated_by           uuid,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    version              int NOT NULL DEFAULT 1
);

CREATE TABLE source_endpoints (
    id                       uuid PRIMARY KEY,
    publisher_id             uuid NOT NULL REFERENCES publishers(id) ON DELETE RESTRICT,
    name                     text NOT NULL DEFAULT '',
    endpoint_type            text NOT NULL CHECK (endpoint_type IN ('rss','atom','json_feed','sitemap','html_list','official_api','fixture')),
    base_url                 text NOT NULL,
    entry_url                text NOT NULL,
    status                   text NOT NULL DEFAULT 'paused'
                             CHECK (status IN ('enabled','paused','disabled','archived','pending_permission')),
    poll_interval_seconds    int NOT NULL DEFAULT 900 CHECK (poll_interval_seconds BETWEEN 60 AND 604800),
    priority                 int NOT NULL DEFAULT 100,
    languages                text[] NOT NULL DEFAULT '{}',
    tags                     text[] NOT NULL DEFAULT '{}',
    category_hints           uuid[] NOT NULL DEFAULT '{}',
    allowed_category_ids     uuid[] NOT NULL DEFAULT '{}',
    allowed_hosts            text[] NOT NULL DEFAULT '{}',
    url_blocklist            text[] NOT NULL DEFAULT '{}',
    fetch_mode               text NOT NULL DEFAULT 'http' CHECK (fetch_mode IN ('http','headless')),
    js_required              boolean NOT NULL DEFAULT false,
    fetch_article_pages      boolean NOT NULL DEFAULT true,
    extraction_profile_id    uuid REFERENCES extraction_profiles(id) ON DELETE SET NULL,
    max_requests_per_minute  int NOT NULL DEFAULT 10 CHECK (max_requests_per_minute BETWEEN 1 AND 600),
    concurrency_limit        int NOT NULL DEFAULT 2 CHECK (concurrency_limit BETWEEN 1 AND 16),
    timeout_seconds          int NOT NULL DEFAULT 20 CHECK (timeout_seconds BETWEEN 1 AND 120),
    max_items_per_poll       int NOT NULL DEFAULT 50 CHECK (max_items_per_poll BETWEEN 1 AND 1000),
    retry_policy             jsonb NOT NULL DEFAULT '{"max_attempts":3,"base_backoff_seconds":30}'::jsonb,
    crawl_window             jsonb NOT NULL DEFAULT '{}'::jsonb,   -- {"timezone":"Europe/Chisinau","start":"06:00","end":"23:00"}
    etag                     text,
    last_modified            text,
    health_state             text NOT NULL DEFAULT 'unknown' CHECK (health_state IN ('unknown','healthy','degraded','failing')),
    consecutive_failures     int NOT NULL DEFAULT 0,
    last_success_at          timestamptz,
    last_error               text,
    next_poll_at             timestamptz,
    created_by               uuid,
    updated_by               uuid,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    version                  int NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX source_endpoints_entry_uidx ON source_endpoints (entry_url) WHERE status <> 'archived';
CREATE INDEX source_endpoints_due_idx ON source_endpoints (next_poll_at) WHERE status = 'enabled';
CREATE INDEX source_endpoints_publisher_idx ON source_endpoints (publisher_id);

CREATE TABLE source_permissions (
    id                     uuid PRIMARY KEY,
    publisher_id           uuid NOT NULL REFERENCES publishers(id) ON DELETE CASCADE,
    source_endpoint_id     uuid REFERENCES source_endpoints(id) ON DELETE CASCADE,
    legal_basis            text NOT NULL DEFAULT '',
    permission_status      text NOT NULL DEFAULT 'pending'
                           CHECK (permission_status IN ('pending','granted','restricted','denied','revoked')),
    allow_crawl            boolean NOT NULL DEFAULT false,
    allow_store_full_text  boolean NOT NULL DEFAULT false,
    allow_store_raw        boolean NOT NULL DEFAULT false,
    allow_summarize        boolean NOT NULL DEFAULT false,
    allow_embed            boolean NOT NULL DEFAULT false,
    allow_audio            boolean NOT NULL DEFAULT false,
    allow_snippets         boolean NOT NULL DEFAULT false,
    max_snippet_chars      int NOT NULL DEFAULT 300 CHECK (max_snippet_chars BETWEEN 0 AND 2000),
    robots_review_status   text NOT NULL DEFAULT 'unreviewed' CHECK (robots_review_status IN ('unreviewed','compliant','restricted','blocked')),
    terms_review_status    text NOT NULL DEFAULT 'unreviewed' CHECK (terms_review_status IN ('unreviewed','compliant','restricted','blocked')),
    retention_days         int CHECK (retention_days IS NULL OR retention_days > 0),
    attribution_text       text NOT NULL DEFAULT '',
    reviewed_at            timestamptz,
    reviewed_by            uuid,
    notes                  text NOT NULL DEFAULT '',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    version                int NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX source_permissions_scope_uidx
    ON source_permissions (publisher_id, coalesce(source_endpoint_id, '00000000-0000-0000-0000-000000000000'::uuid));

CREATE TABLE source_crawl_runs (
    id                   uuid PRIMARY KEY,
    source_endpoint_id   uuid NOT NULL REFERENCES source_endpoints(id) ON DELETE CASCADE,
    trigger              text NOT NULL CHECK (trigger IN ('schedule','manual','reprocess','fixture')),
    status               text NOT NULL DEFAULT 'processing'
                         CHECK (status IN ('queued','processing','succeeded','partial','failed','skipped')),
    started_at           timestamptz NOT NULL DEFAULT now(),
    completed_at         timestamptz,
    http_status          int,
    not_modified         boolean NOT NULL DEFAULT false,
    discovered_count     int NOT NULL DEFAULT 0,
    fetched_count        int NOT NULL DEFAULT 0,
    accepted_count       int NOT NULL DEFAULT 0,
    duplicate_count      int NOT NULL DEFAULT 0,
    rejected_count       int NOT NULL DEFAULT 0,
    error_count          int NOT NULL DEFAULT 0,
    errors               jsonb NOT NULL DEFAULT '[]'::jsonb,
    duration_ms          int,
    requested_by         uuid
);
CREATE INDEX source_crawl_runs_endpoint_idx ON source_crawl_runs (source_endpoint_id, started_at DESC);

CREATE TABLE source_health (
    source_endpoint_id     uuid PRIMARY KEY REFERENCES source_endpoints(id) ON DELETE CASCADE,
    last_http_status       int,
    runs_24h               int NOT NULL DEFAULT 0,
    failed_runs_24h        int NOT NULL DEFAULT 0,
    parse_coverage         real,   -- share of fetched items with title+body extracted
    duplicate_rate         real,
    freshness_lag_seconds  bigint, -- now - newest article published_at
    anomalies              jsonb NOT NULL DEFAULT '[]'::jsonb,
    updated_at             timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE blocked_urls (
    id          uuid PRIMARY KEY,
    match_type  text NOT NULL CHECK (match_type IN ('url','prefix','domain')),
    value       text NOT NULL,
    reason      text NOT NULL DEFAULT '',
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (match_type, value)
);

-- +goose Down
DROP TABLE IF EXISTS blocked_urls, source_health, source_crawl_runs, source_permissions, source_endpoints,
  extraction_profiles, publishers, taxonomy_versions, category_redirects, categories;
