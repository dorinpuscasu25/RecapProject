-- +goose Up
-- Personalization, briefings, audio, chat, notifications.

CREATE TABLE user_preferences (
    user_id                   uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    languages                 text[] NOT NULL DEFAULT '{ro}',
    summary_language          text NOT NULL DEFAULT 'ro' CHECK (summary_language IN ('ro','ru','en')),
    regions                   text[] NOT NULL DEFAULT '{}',
    audio_enabled             boolean NOT NULL DEFAULT false,
    briefing_minutes          int NOT NULL DEFAULT 5 CHECK (briefing_minutes IN (2,5,10)),
    personalization_consent   boolean NOT NULL DEFAULT true,
    onboarding_completed_at   timestamptz,
    updated_at                timestamptz NOT NULL DEFAULT now(),
    version                   int NOT NULL DEFAULT 1
);

-- Follows and mutes over categories, entities, keywords, regions, events, publishers.
CREATE TABLE user_interests (
    id             uuid PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    interest_type  text NOT NULL CHECK (interest_type IN ('category','entity','keyword','region','publisher')),
    target_key     text NOT NULL,          -- uuid for category/entity/publisher, normalized text otherwise
    mode           text NOT NULL CHECK (mode IN ('follow','mute')),
    weight         real NOT NULL DEFAULT 1 CHECK (weight BETWEEN 0 AND 5),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, interest_type, target_key)
);
CREATE INDEX user_interests_user_idx ON user_interests (user_id, interest_type);

CREATE TABLE followed_events (
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id    uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, event_id)
);
CREATE INDEX followed_events_event_idx ON followed_events (event_id);

-- Per-user event state: last seen version, acknowledgement, bookmarks.
CREATE TABLE user_event_state (
    user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id         uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    seen_version     int NOT NULL DEFAULT 0,
    seen_at          timestamptz,
    bookmarked_at    timestamptz,
    dismissed_at     timestamptz,
    PRIMARY KEY (user_id, event_id)
);
CREATE INDEX user_event_state_bookmarks_idx ON user_event_state (user_id, bookmarked_at DESC) WHERE bookmarked_at IS NOT NULL;

-- "What did I miss?" cursor: explicit acknowledgement time, distinct from last login.
CREATE TABLE user_feed_cursors (
    user_id           uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    acknowledged_at   timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE briefings (
    id                 uuid PRIMARY KEY,
    user_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status             text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','processing','ready','failed')),
    trigger            text NOT NULL DEFAULT 'on_demand' CHECK (trigger IN ('on_demand','scheduled')),
    language           text NOT NULL,
    duration_minutes   int NOT NULL CHECK (duration_minutes IN (2,5,10)),
    category_ids       uuid[] NOT NULL DEFAULT '{}',
    region             text,
    tone               text NOT NULL DEFAULT 'neutral',
    since              timestamptz NOT NULL,
    until              timestamptz NOT NULL,
    title              text NOT NULL DEFAULT '',
    text               text,
    estimated_seconds  int,
    cache_fingerprint  text NOT NULL,
    model_ref          text,
    prompt_version     text,
    generation_version int NOT NULL DEFAULT 1,
    error_code         text,
    error_message      text,
    requested_at       timestamptz NOT NULL DEFAULT now(),
    started_at         timestamptz,
    completed_at       timestamptz
);
CREATE INDEX briefings_user_idx ON briefings (user_id, requested_at DESC);
CREATE INDEX briefings_fingerprint_idx ON briefings (cache_fingerprint) WHERE status = 'ready';

CREATE TABLE briefing_items (
    briefing_id    uuid NOT NULL REFERENCES briefings(id) ON DELETE CASCADE,
    position       int NOT NULL,
    event_id       uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    event_version  int NOT NULL,
    section_text   text NOT NULL,
    citations      jsonb NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (briefing_id, position)
);

CREATE TABLE audio_assets (
    id                uuid PRIMARY KEY,
    owner_type        text NOT NULL CHECK (owner_type IN ('briefing','event_narration')),
    owner_id          uuid NOT NULL,
    user_id           uuid REFERENCES users(id) ON DELETE CASCADE,
    language          text NOT NULL,
    voice             text NOT NULL DEFAULT '',
    provider          text,
    model             text,
    format            text NOT NULL DEFAULT 'mp3',
    status            text NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued','processing','ready','failed','provider_unavailable','cancelled')),
    object_key        text,
    duration_ms       int,
    size_bytes        bigint,
    checksum_sha256   text,
    segment_manifest  jsonb NOT NULL DEFAULT '[]'::jsonb,
    characters        int NOT NULL DEFAULT 0,
    cost_estimate_usd numeric(12,6),
    error             text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz
);
CREATE INDEX audio_assets_owner_idx ON audio_assets (owner_type, owner_id, created_at DESC);

CREATE TABLE chat_sessions (
    id               uuid PRIMARY KEY,
    user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope_type       text NOT NULL DEFAULT 'global' CHECK (scope_type IN ('global','event','briefing','article','claim')),
    scope_id         uuid,
    language         text NOT NULL DEFAULT 'ro',
    title            text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_message_at  timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL
);
CREATE INDEX chat_sessions_user_idx ON chat_sessions (user_id, last_message_at DESC);
CREATE INDEX chat_sessions_expires_idx ON chat_sessions (expires_at);

CREATE TABLE chat_messages (
    id           uuid PRIMARY KEY,
    session_id   uuid NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role         text NOT NULL CHECK (role IN ('user','assistant')),
    content      text NOT NULL,
    citations    jsonb NOT NULL DEFAULT '[]'::jsonb,
    retrieval    jsonb NOT NULL DEFAULT '{}'::jsonb,
    abstained    boolean NOT NULL DEFAULT false,
    model_ref    text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_messages_session_idx ON chat_messages (session_id, created_at);

CREATE TABLE notification_schedules (
    id                    uuid PRIMARY KEY,
    user_id               uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                  text NOT NULL DEFAULT 'default',
    kind                  text NOT NULL CHECK (kind IN ('daily','weekly','custom')),
    local_times           text[] NOT NULL,                -- ["08:00","19:30"]
    weekdays              int[] NOT NULL DEFAULT '{1,2,3,4,5,6,7}',  -- ISO weekday 1=Mon
    timezone              text NOT NULL DEFAULT 'Europe/Chisinau',
    quiet_start           text,                            -- "22:00"
    quiet_end             text,                            -- "07:00"
    importance_threshold  real NOT NULL DEFAULT 0.3 CHECK (importance_threshold BETWEEN 0 AND 1),
    category_ids          uuid[] NOT NULL DEFAULT '{}',
    language              text NOT NULL DEFAULT 'ro',
    digest_size           int NOT NULL DEFAULT 5 CHECK (digest_size BETWEEN 1 AND 20),
    enabled               boolean NOT NULL DEFAULT true,
    next_run_at           timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);
CREATE INDEX notification_schedules_due_idx ON notification_schedules (next_run_at) WHERE enabled;

CREATE TABLE device_tokens (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform      text NOT NULL CHECK (platform IN ('ios','android')),
    provider      text NOT NULL CHECK (provider IN ('apns','fcm')),
    token_hash    bytea NOT NULL UNIQUE,
    token_enc     bytea NOT NULL,       -- AES-GCM encrypted; never logged
    app_version   text NOT NULL DEFAULT '',
    locale        text NOT NULL DEFAULT 'ro',
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','invalid')),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz
);
CREATE INDEX device_tokens_user_idx ON device_tokens (user_id) WHERE status = 'active';

CREATE TABLE notification_deliveries (
    id                   uuid PRIMARY KEY,
    user_id              uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    schedule_id          uuid REFERENCES notification_schedules(id) ON DELETE SET NULL,
    occurrence_key       text NOT NULL,     -- e.g. 2026-10-13T08:00@Europe/Chisinau
    device_token_id      uuid REFERENCES device_tokens(id) ON DELETE SET NULL,
    kind                 text NOT NULL CHECK (kind IN ('digest','breaking','event_update')),
    payload              jsonb NOT NULL,
    status               text NOT NULL DEFAULT 'queued'
                         CHECK (status IN ('queued','sent','failed','skipped','provider_unavailable','invalid_token')),
    skip_reason          text,
    attempts             int NOT NULL DEFAULT 0,
    error                text,
    provider_message_id  text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    sent_at              timestamptz
);
CREATE UNIQUE INDEX notification_deliveries_dedupe_uidx
    ON notification_deliveries (user_id, coalesce(schedule_id, '00000000-0000-0000-0000-000000000000'::uuid), occurrence_key,
                                coalesce(device_token_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE INDEX notification_deliveries_user_idx ON notification_deliveries (user_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS notification_deliveries, device_tokens, notification_schedules, chat_messages, chat_sessions,
  audio_assets, briefing_items, briefings, user_feed_cursors, user_event_state, followed_events, user_interests,
  user_preferences;
