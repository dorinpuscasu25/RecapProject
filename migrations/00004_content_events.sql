-- +goose Up
-- Articles, entities, events.

CREATE TABLE articles (
    id                     uuid PRIMARY KEY,
    publisher_id           uuid NOT NULL REFERENCES publishers(id) ON DELETE RESTRICT,
    source_endpoint_id     uuid REFERENCES source_endpoints(id) ON DELETE SET NULL,
    canonical_url          text NOT NULL,
    canonical_url_hash     bytea NOT NULL,
    original_url           text NOT NULL,
    language               text NOT NULL DEFAULT 'und',
    language_confidence    real,
    title                  text NOT NULL,
    byline                 text NOT NULL DEFAULT '',
    description            text NOT NULL DEFAULT '',
    body_text              text,                 -- only if permitted (allow_store_full_text)
    body_chars             int NOT NULL DEFAULT 0,
    raw_object_key         text,                 -- only if permitted (allow_store_raw)
    text_hash              bytea NOT NULL,       -- SHA-256 of normalized title+body
    simhash                bigint NOT NULL,      -- 64-bit simhash for near-duplicate detection
    content_type           text NOT NULL DEFAULT 'news'
                           CHECK (content_type IN ('news','opinion','satire','press_release','official','analysis','unknown')),
    published_at           timestamptz,
    modified_at            timestamptz,
    first_seen_at          timestamptz NOT NULL DEFAULT now(),
    fetched_at             timestamptz NOT NULL DEFAULT now(),
    extractor_version      text NOT NULL,
    extraction_confidence  real NOT NULL DEFAULT 0,
    status                 text NOT NULL DEFAULT 'extracted'
                           CHECK (status IN ('extracted','enriching','enriched','failed','quarantined','under_review','withdrawn','duplicate')),
    duplicate_of           uuid REFERENCES articles(id) ON DELETE SET NULL,
    duplicate_kind         text CHECK (duplicate_kind IN ('exact','syndicated','near')),
    revision               int NOT NULL DEFAULT 1,
    is_correction          boolean NOT NULL DEFAULT false,
    retracted_at           timestamptz,
    tombstoned_at          timestamptz,
    geographies            text[] NOT NULL DEFAULT '{}',
    tags                   text[] NOT NULL DEFAULT '{}',
    enrichment             jsonb NOT NULL DEFAULT '{}'::jsonb,  -- model refs, taxonomy version, timings
    processing_attempts    int NOT NULL DEFAULT 0,
    last_error             text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    version                int NOT NULL DEFAULT 1,
    tsv tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', immutable_unaccent(coalesce(title,''))), 'A') ||
        setweight(to_tsvector('simple', immutable_unaccent(coalesce(description,''))), 'B') ||
        setweight(to_tsvector('simple', immutable_unaccent(left(coalesce(body_text,''), 20000))), 'C')
    ) STORED
);
CREATE UNIQUE INDEX articles_canonical_url_uidx ON articles (canonical_url_hash);
CREATE INDEX articles_text_hash_idx ON articles (text_hash);
CREATE INDEX articles_publisher_time_idx ON articles (publisher_id, published_at DESC);
CREATE INDEX articles_status_idx ON articles (status, updated_at);
CREATE INDEX articles_published_idx ON articles (published_at DESC);
CREATE INDEX articles_tsv_idx ON articles USING gin (tsv);

CREATE TABLE article_versions (
    id           uuid PRIMARY KEY,
    article_id   uuid NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    revision     int NOT NULL,
    title        text NOT NULL,
    body_text    text,
    text_hash    bytea NOT NULL,
    change_kind  text NOT NULL CHECK (change_kind IN ('initial','update','correction','retraction','tombstone')),
    captured_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (article_id, revision)
);

CREATE TABLE article_translations (
    article_id   uuid NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    language     text NOT NULL,
    title        text NOT NULL,
    body_text    text,
    model_ref    text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (article_id, language)
);

CREATE TABLE article_categories (
    article_id        uuid NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    category_id       uuid NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    source            text NOT NULL CHECK (source IN ('ai','editorial','rule')),
    is_primary        boolean NOT NULL DEFAULT false,
    confidence        real,
    model_ref         text,
    taxonomy_version  bigint,
    assigned_by       uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (article_id, category_id, source)
);
CREATE INDEX article_categories_category_idx ON article_categories (category_id);

CREATE TABLE entities (
    id              uuid PRIMARY KEY,
    kind            text NOT NULL CHECK (kind IN ('person','organization','location','event','other')),
    canonical_name  text NOT NULL,
    normalized_key  text NOT NULL,      -- transliterated, unaccented, lower-case
    aliases         text[] NOT NULL DEFAULT '{}',
    external_ids    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (kind, normalized_key)
);
CREATE INDEX entities_name_trgm_idx ON entities USING gin (normalized_key gin_trgm_ops);

CREATE TABLE article_entities (
    article_id     uuid NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    entity_id      uuid NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    mention_count  int NOT NULL DEFAULT 1,
    salience       real NOT NULL DEFAULT 0,
    PRIMARY KEY (article_id, entity_id)
);
CREATE INDEX article_entities_entity_idx ON article_entities (entity_id);

-- Extracted atomic facts (numbers, dates, decisions) used for change/novelty detection.
CREATE TABLE article_facts (
    id            uuid PRIMARY KEY,
    article_id    uuid NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    fact_type     text NOT NULL CHECK (fact_type IN ('number','date','money','percent','decision','quote','statement')),
    fact_key      text NOT NULL,         -- normalized comparable key
    text          text NOT NULL,
    start_offset  int NOT NULL CHECK (start_offset >= 0),
    end_offset    int NOT NULL CHECK (end_offset >= start_offset),
    value         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX article_facts_article_idx ON article_facts (article_id);

-- Events: the principal knowledge unit.
CREATE TABLE events (
    id                       uuid PRIMARY KEY,
    titles                   jsonb NOT NULL,                 -- {"ro": "...", "ru": "..."}
    summaries                jsonb NOT NULL DEFAULT '{}'::jsonb, -- {"ro": {"short","normal","detailed","citations":[...]}}
    event_type               text NOT NULL DEFAULT 'general',
    state                    text NOT NULL DEFAULT 'developing'
                             CHECK (state IN ('developing','ongoing','resolved','corrected','disputed')),
    status                   text NOT NULL DEFAULT 'active' CHECK (status IN ('active','merged','withdrawn')),
    merged_into              uuid REFERENCES events(id) ON DELETE SET NULL,
    occurred_start           timestamptz,
    occurred_end             timestamptz,
    earliest_published_at    timestamptz,
    last_material_update_at  timestamptz,
    locations                text[] NOT NULL DEFAULT '{}',
    primary_category_id      uuid REFERENCES categories(id) ON DELETE SET NULL,
    importance               real NOT NULL DEFAULT 0 CHECK (importance BETWEEN 0 AND 1),
    editorial_status         text NOT NULL DEFAULT 'automatic' CHECK (editorial_status IN ('automatic','reviewed','locked')),
    confidence               real NOT NULL DEFAULT 0,
    quality                  jsonb NOT NULL DEFAULT '{}'::jsonb,
    article_count            int NOT NULL DEFAULT 0,
    source_count             int NOT NULL DEFAULT 0,
    citation_coverage        real NOT NULL DEFAULT 0,
    languages                text[] NOT NULL DEFAULT '{}',
    fingerprint              jsonb NOT NULL DEFAULT '{}'::jsonb,  -- entity keys, fact keys, used for candidate filtering
    version                  int NOT NULL DEFAULT 1,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    search_text              text NOT NULL DEFAULT '',
    tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', immutable_unaccent(search_text))) STORED
);
CREATE INDEX events_update_idx ON events (last_material_update_at DESC) WHERE status = 'active';
CREATE INDEX events_category_idx ON events (primary_category_id, last_material_update_at DESC);
CREATE INDEX events_tsv_idx ON events USING gin (tsv);

CREATE TABLE event_versions (
    id            uuid PRIMARY KEY,
    event_id      uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    version       int NOT NULL,
    snapshot      jsonb NOT NULL,
    change_reason text NOT NULL,
    created_by    uuid,
    model_ref     text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, version)
);

CREATE TABLE event_article_links (
    event_id     uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    article_id   uuid NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    relation     text NOT NULL CHECK (relation IN ('origin','same_story','follow_up','related')),
    confidence   real NOT NULL DEFAULT 0,
    rationale    jsonb NOT NULL DEFAULT '{}'::jsonb,   -- candidate scores, thresholds, model/index versions
    linked_by    text NOT NULL DEFAULT 'system',       -- system | editor:<uuid>
    linked_at    timestamptz NOT NULL DEFAULT now(),
    unlinked_at  timestamptz,
    PRIMARY KEY (event_id, article_id)
);
-- An article belongs to at most one event as coverage (origin/same_story/follow_up).
CREATE UNIQUE INDEX eal_article_primary_uidx ON event_article_links (article_id)
    WHERE unlinked_at IS NULL AND relation IN ('origin','same_story','follow_up');
CREATE INDEX eal_event_idx ON event_article_links (event_id) WHERE unlinked_at IS NULL;

CREATE TABLE event_updates (
    id                     uuid PRIMARY KEY,
    event_id               uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    event_version          int NOT NULL,
    update_kind            text NOT NULL CHECK (update_kind IN ('created','material_update','correction','contradiction','status_change','merge','split')),
    what_changed           jsonb NOT NULL,          -- {"ro": "...", "facts":[...]}
    affected_claim_ids     uuid[] NOT NULL DEFAULT '{}',
    supporting_article_ids uuid[] NOT NULL DEFAULT '{}',
    severity               text NOT NULL DEFAULT 'minor' CHECK (severity IN ('minor','major','critical')),
    requires_approval      boolean NOT NULL DEFAULT false,
    approved_by            uuid,
    approved_at            timestamptz,
    occurred_at            timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX event_updates_event_idx ON event_updates (event_id, created_at);
CREATE INDEX event_updates_time_idx ON event_updates (created_at DESC);

CREATE TABLE event_entities (
    event_id   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    entity_id  uuid NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    weight     real NOT NULL DEFAULT 1,
    PRIMARY KEY (event_id, entity_id)
);
CREATE INDEX event_entities_entity_idx ON event_entities (entity_id);

CREATE TABLE event_categories (
    event_id      uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    category_id   uuid NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    source        text NOT NULL CHECK (source IN ('ai','editorial','rule','derived')),
    is_primary    boolean NOT NULL DEFAULT false,
    confidence    real,
    assigned_by   uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, category_id, source)
);
CREATE INDEX event_categories_category_idx ON event_categories (category_id);

CREATE TABLE event_relationships (
    from_event_id  uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    to_event_id    uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    relation       text NOT NULL CHECK (relation IN ('related','follow_up','same_subject','merged_into','split_from')),
    confidence     real NOT NULL DEFAULT 0,
    created_by     text NOT NULL DEFAULT 'system',
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (from_event_id, to_event_id, relation),
    CHECK (from_event_id <> to_event_id)
);

CREATE TABLE event_merge_history (
    id                uuid PRIMARY KEY,
    operation         text NOT NULL CHECK (operation IN ('merge','split','unmerge')),
    source_event_ids  uuid[] NOT NULL,
    target_event_ids  uuid[] NOT NULL,
    article_ids       uuid[] NOT NULL DEFAULT '{}',
    reason            text NOT NULL,
    payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
    performed_by      uuid,
    reverted_by       uuid,
    reverted_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

-- Reusable canonical narration per event version and language (shared across users' briefings).
CREATE TABLE event_narrations (
    event_id       uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    event_version  int NOT NULL,
    language       text NOT NULL,
    style          text NOT NULL DEFAULT 'neutral',
    text           text NOT NULL,
    citations      jsonb NOT NULL DEFAULT '[]'::jsonb,
    model_ref      text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, event_version, language, style)
);

-- Projection bookkeeping for OpenSearch/Qdrant: guards against stale writes and supports backfill.
CREATE TABLE search_index_state (
    doc_type         text NOT NULL,
    doc_id           uuid NOT NULL,
    backend          text NOT NULL,          -- opensearch | qdrant:<collection>
    indexed_version  int NOT NULL,
    schema_version   int NOT NULL,
    indexed_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_type, doc_id, backend)
);

CREATE TABLE content_removal_requests (
    id            uuid PRIMARY KEY,
    target_type   text NOT NULL CHECK (target_type IN ('article','publisher','url')),
    target_value  text NOT NULL,
    reason        text NOT NULL,
    legal_basis   text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','processing','succeeded','failed','rejected')),
    requested_by  uuid,
    processed_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS content_removal_requests, search_index_state, event_narrations, event_merge_history,
  event_relationships, event_categories, event_entities, event_updates, event_article_links, event_versions,
  events, article_facts, article_entities, entities, article_categories, article_translations,
  article_versions, articles;
