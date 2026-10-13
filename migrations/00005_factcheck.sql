-- +goose Up
-- Claims, evidence, verdicts, editorial reviews, narratives.

CREATE TABLE claims (
    id                 uuid PRIMARY KEY,
    canonical_text     text NOT NULL CHECK (char_length(canonical_text) BETWEEN 3 AND 2000),
    language           text NOT NULL,
    normalized_key     text NOT NULL,                 -- hash of normalized/transliterated canonical text
    checkability       text NOT NULL CHECK (checkability IN ('checkable','opinion','prediction','satire','rhetorical','quote_only')),
    claim_type         text NOT NULL DEFAULT 'statement',  -- statistic | decision | event | attribution | statement
    subject_keys       text[] NOT NULL DEFAULT '{}',   -- entity normalized keys
    time_context       jsonb NOT NULL DEFAULT '{}'::jsonb,
    geo_context        text[] NOT NULL DEFAULT '{}',
    risk_level         text NOT NULL DEFAULT 'low' CHECK (risk_level IN ('low','medium','high')),
    status             text NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending','fact_checked','contested','corrected','withdrawn')),
    first_seen_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at       timestamptz NOT NULL DEFAULT now(),
    occurrence_count   int NOT NULL DEFAULT 0,
    current_verdict_id uuid,                          -- latest reader-visible verdict
    needs_reverification boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    version            int NOT NULL DEFAULT 1,
    tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', immutable_unaccent(canonical_text))) STORED
);
CREATE UNIQUE INDEX claims_normalized_uidx ON claims (normalized_key);
CREATE INDEX claims_tsv_idx ON claims USING gin (tsv);
CREATE INDEX claims_status_idx ON claims (status, last_seen_at DESC);

CREATE TABLE claim_occurrences (
    id                    uuid PRIMARY KEY,
    claim_id              uuid NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
    article_id            uuid NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    event_id              uuid REFERENCES events(id) ON DELETE SET NULL,
    quote_text            text NOT NULL,       -- exact source span text
    start_offset          int NOT NULL CHECK (start_offset >= 0),
    end_offset            int NOT NULL CHECK (end_offset > start_offset),
    attributed_to         text NOT NULL DEFAULT '',
    attribution_entity_id uuid REFERENCES entities(id) ON DELETE SET NULL,
    is_direct_quote       boolean NOT NULL DEFAULT false,
    asserted_at           timestamptz,
    language              text NOT NULL,
    extraction_model_ref  text NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (claim_id, article_id, start_offset)
);
CREATE INDEX claim_occurrences_article_idx ON claim_occurrences (article_id);
CREATE INDEX claim_occurrences_event_idx ON claim_occurrences (event_id);

CREATE TABLE event_claims (
    event_id   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    claim_id   uuid NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, claim_id)
);
CREATE INDEX event_claims_claim_idx ON event_claims (claim_id);

CREATE TABLE claim_evidence (
    id                    uuid PRIMARY KEY,
    claim_id              uuid NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
    evidence_article_id   uuid REFERENCES articles(id) ON DELETE SET NULL,
    evidence_url          text NOT NULL,
    publisher_id          uuid REFERENCES publishers(id) ON DELETE SET NULL,
    source_kind           text NOT NULL CHECK (source_kind IN ('official','newsroom','fact_checker','document','editor_provided','other')),
    relation              text NOT NULL CHECK (relation IN ('supports','contradicts','context','irrelevant')),
    excerpt               text NOT NULL DEFAULT '',
    start_offset          int,
    end_offset            int,
    rationale             text NOT NULL DEFAULT '',
    independence_key      text NOT NULL,       -- syndicated copies share a key and count once
    is_primary_source     boolean NOT NULL DEFAULT false,
    published_at          timestamptz,
    retrieved_at          timestamptz NOT NULL DEFAULT now(),
    assessed_by           text NOT NULL CHECK (assessed_by IN ('model','rule','editor')),
    model_ref             text,
    retrieval_scores      jsonb NOT NULL DEFAULT '{}'::jsonb,
    added_by              uuid,
    superseded_at         timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (start_offset IS NULL OR end_offset >= start_offset)
);
CREATE INDEX claim_evidence_claim_idx ON claim_evidence (claim_id) WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX claim_evidence_article_uidx ON claim_evidence (claim_id, evidence_article_id)
    WHERE evidence_article_id IS NOT NULL AND superseded_at IS NULL;

-- Versioned verdicts. History is never deleted; a new verdict supersedes the previous one.
CREATE TABLE claim_verdicts (
    id                uuid PRIMARY KEY,
    claim_id          uuid NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
    version           int NOT NULL,
    verdict           text NOT NULL CHECK (verdict IN ('supported','refuted','misleading_context','partially_supported','disputed','unverified','not_checkable','outdated')),
    proposed_verdict  text CHECK (proposed_verdict IN ('supported','refuted','misleading_context','partially_supported','disputed','unverified','not_checkable','outdated')),
    review_status     text NOT NULL CHECK (review_status IN ('automatic_preliminary','pending_review','editor_approved','editor_modified','editor_rejected','superseded')),
    visibility        text NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','internal')),
    headline          jsonb NOT NULL DEFAULT '{}'::jsonb,
    explanation       jsonb NOT NULL DEFAULT '{}'::jsonb,
    limitations       jsonb NOT NULL DEFAULT '[]'::jsonb,
    confidence        real,                    -- internal calibrated signal, not certainty
    evidence_ids      uuid[] NOT NULL DEFAULT '{}',
    rationale         text NOT NULL DEFAULT '',
    signals           jsonb NOT NULL DEFAULT '{}'::jsonb,  -- independent supports/contradicts counts, recency
    policy_version    int NOT NULL,
    model_ref         text,
    assessed_at       timestamptz NOT NULL DEFAULT now(),
    reviewed_by       uuid,
    reviewed_at       timestamptz,
    published_at      timestamptz,
    correction_note   jsonb,
    supersedes_id     uuid REFERENCES claim_verdicts(id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (claim_id, version)
);
CREATE INDEX claim_verdicts_claim_idx ON claim_verdicts (claim_id, version DESC);
ALTER TABLE claims ADD CONSTRAINT claims_current_verdict_fk
    FOREIGN KEY (current_verdict_id) REFERENCES claim_verdicts(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE fact_check_reviews (
    id                  uuid PRIMARY KEY,
    claim_id            uuid NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
    verdict_id          uuid REFERENCES claim_verdicts(id) ON DELETE SET NULL,
    status              text NOT NULL DEFAULT 'open' CHECK (status IN ('open','in_progress','resolved','dismissed')),
    reason              text NOT NULL,
    priority_score      real NOT NULL DEFAULT 0,
    priority_factors    jsonb NOT NULL DEFAULT '{}'::jsonb,   -- reach, uncertainty, impact, harm
    assigned_to         uuid,
    decision            text CHECK (decision IN ('accept','modify','reject','request_more_evidence')),
    decision_verdict    text,
    decision_rationale  text,
    reviewer_id         uuid,
    decided_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    version             int NOT NULL DEFAULT 1
);
CREATE INDEX fcr_open_idx ON fact_check_reviews (priority_score DESC, created_at) WHERE status IN ('open','in_progress');
CREATE UNIQUE INDEX fcr_one_open_per_claim_uidx ON fact_check_reviews (claim_id) WHERE status IN ('open','in_progress');

CREATE TABLE misinformation_narratives (
    id              uuid PRIMARY KEY,
    title           jsonb NOT NULL,
    summary         jsonb NOT NULL DEFAULT '{}'::jsonb,
    why_caution     jsonb NOT NULL DEFAULT '{}'::jsonb,
    status          text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','pending_review','published','archived')),
    languages       text[] NOT NULL DEFAULT '{}',
    first_seen_at   timestamptz,
    last_seen_at    timestamptz,
    created_by      uuid,
    approved_by     uuid,
    approved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    version         int NOT NULL DEFAULT 1
);

CREATE TABLE narrative_claim_links (
    narrative_id  uuid NOT NULL REFERENCES misinformation_narratives(id) ON DELETE CASCADE,
    claim_id      uuid NOT NULL REFERENCES claims(id) ON DELETE CASCADE,
    linked_by     uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (narrative_id, claim_id)
);

-- +goose Down
ALTER TABLE IF EXISTS claims DROP CONSTRAINT IF EXISTS claims_current_verdict_fk;
DROP TABLE IF EXISTS narrative_claim_links, misinformation_narratives, fact_check_reviews, claim_verdicts,
  claim_evidence, event_claims, claim_occurrences, claims;
