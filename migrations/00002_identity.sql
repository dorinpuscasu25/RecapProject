-- +goose Up
-- Identity, sessions, RBAC, email delivery.

CREATE TABLE roles (
    code         text PRIMARY KEY,
    description  text NOT NULL,
    is_staff     boolean NOT NULL DEFAULT false
);

CREATE TABLE permissions (
    code         text PRIMARY KEY,
    description  text NOT NULL
);

CREATE TABLE role_permissions (
    role_code        text NOT NULL REFERENCES roles(code) ON DELETE CASCADE,
    permission_code  text NOT NULL REFERENCES permissions(code) ON DELETE CASCADE,
    PRIMARY KEY (role_code, permission_code)
);

CREATE TABLE users (
    id                  uuid PRIMARY KEY,
    first_name          text NOT NULL CHECK (char_length(first_name) BETWEEN 1 AND 100),
    last_name           text NOT NULL CHECK (char_length(last_name) BETWEEN 1 AND 100),
    email               text NOT NULL CHECK (char_length(email) <= 254),
    email_normalized    text NOT NULL CHECK (char_length(email_normalized) <= 254),
    phone_e164          text NOT NULL CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
    phone_verified_at   timestamptz,
    password_hash       text NOT NULL,
    email_verified_at   timestamptz,
    status              text NOT NULL DEFAULT 'pending_verification'
                        CHECK (status IN ('pending_verification','active','suspended','disabled','deletion_pending')),
    locale              text NOT NULL DEFAULT 'ro' CHECK (locale ~ '^[a-z]{2}(-[A-Z]{2})?$'),
    timezone            text NOT NULL DEFAULT 'Europe/Chisinau',
    marketing_opt_in    boolean NOT NULL DEFAULT false,
    password_changed_at timestamptz NOT NULL DEFAULT now(),
    deletion_requested_at timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    version             int NOT NULL DEFAULT 1,
    CONSTRAINT users_active_requires_verified CHECK (status <> 'active' OR email_verified_at IS NOT NULL)
);
CREATE UNIQUE INDEX users_email_normalized_uidx ON users (email_normalized);
CREATE INDEX users_status_idx ON users (status);

CREATE TABLE user_roles (
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_code   text NOT NULL REFERENCES roles(code),
    granted_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    granted_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role_code)
);

-- Email verification challenges (signup + email change). Codes are stored as HMAC only.
CREATE TABLE email_verification_challenges (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose         text NOT NULL CHECK (purpose IN ('signup','email_change')),
    target_email    text NOT NULL,
    target_email_normalized text NOT NULL,
    code_hmac       bytea NOT NULL,
    attempts        int NOT NULL DEFAULT 0,
    max_attempts    int NOT NULL,
    expires_at      timestamptz NOT NULL,
    consumed_at     timestamptz,
    invalidated_at  timestamptz,
    invalidated_reason text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX evc_user_purpose_idx ON email_verification_challenges (user_id, purpose, created_at DESC);
CREATE INDEX evc_expires_idx ON email_verification_challenges (expires_at);
-- At most one live challenge per user and purpose.
CREATE UNIQUE INDEX evc_one_active_uidx ON email_verification_challenges (user_id, purpose)
    WHERE consumed_at IS NULL AND invalidated_at IS NULL;

CREATE TABLE password_reset_challenges (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hmac       bytea NOT NULL,
    attempts        int NOT NULL DEFAULT 0,
    max_attempts    int NOT NULL,
    expires_at      timestamptz NOT NULL,
    consumed_at     timestamptz,
    invalidated_at  timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX prc_user_idx ON password_reset_challenges (user_id, created_at DESC);
CREATE INDEX prc_expires_idx ON password_reset_challenges (expires_at);
CREATE UNIQUE INDEX prc_one_active_uidx ON password_reset_challenges (user_id)
    WHERE consumed_at IS NULL AND invalidated_at IS NULL;

-- A session represents one signed-in device.
CREATE TABLE auth_sessions (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_label    text NOT NULL DEFAULT '' CHECK (char_length(device_label) <= 100),
    user_agent      text NOT NULL DEFAULT '' CHECK (char_length(user_agent) <= 200),
    ip_prefix       text NOT NULL DEFAULT '',  -- truncated (/24 IPv4, /48 IPv6) for privacy
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    revoke_reason   text
);
CREATE INDEX auth_sessions_user_idx ON auth_sessions (user_id, created_at DESC);
CREATE INDEX auth_sessions_expires_idx ON auth_sessions (expires_at);

CREATE TABLE auth_refresh_token_families (
    id                  uuid PRIMARY KEY,
    session_id          uuid NOT NULL UNIQUE REFERENCES auth_sessions(id) ON DELETE CASCADE,
    user_id             uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    absolute_expires_at timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    revoked_at          timestamptz,
    revoke_reason       text
);

CREATE TABLE auth_refresh_tokens (
    id           uuid PRIMARY KEY,
    family_id    uuid NOT NULL REFERENCES auth_refresh_token_families(id) ON DELETE CASCADE,
    token_hash   bytea NOT NULL UNIQUE,     -- SHA-256 of the opaque token
    parent_id    uuid REFERENCES auth_refresh_tokens(id) ON DELETE SET NULL,
    issued_at    timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    rotated_at   timestamptz,               -- set when exchanged; a second use = replay
    revoked_at   timestamptz
);
CREATE INDEX auth_refresh_tokens_family_idx ON auth_refresh_tokens (family_id);
CREATE INDEX auth_refresh_tokens_expires_idx ON auth_refresh_tokens (expires_at);

-- Transactional email outbox. Payload (may contain a one-time code) is encrypted and purged after send.
CREATE TABLE email_deliveries (
    id                  uuid PRIMARY KEY,
    idempotency_key     text NOT NULL UNIQUE,
    user_id             uuid REFERENCES users(id) ON DELETE SET NULL,
    to_address          text NOT NULL,
    template            text NOT NULL,
    locale              text NOT NULL DEFAULT 'ro',
    payload_enc         bytea,
    status              text NOT NULL DEFAULT 'queued'
                        CHECK (status IN ('queued','sending','sent','failed','dead','expired')),
    attempts            int NOT NULL DEFAULT 0,
    max_attempts        int NOT NULL DEFAULT 6,
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    locked_until        timestamptz,
    expires_at          timestamptz,
    last_error          text,
    provider            text,
    provider_message_id text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    sent_at             timestamptz
);
CREATE INDEX email_deliveries_ready_idx ON email_deliveries (next_attempt_at) WHERE status IN ('queued','failed');
CREATE INDEX email_deliveries_user_idx ON email_deliveries (user_id, created_at DESC);

-- Development mail sink (MAIL_DRIVER=devsink only; refused in production config).
CREATE TABLE dev_mail_messages (
    id           uuid PRIMARY KEY,
    to_address   text NOT NULL,
    subject      text NOT NULL,
    body_text    text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dev_mail_messages_to_idx ON dev_mail_messages (lower(to_address), created_at DESC);

CREATE TABLE user_terms_acceptances (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    document        text NOT NULL CHECK (document IN ('terms','privacy','marketing')),
    policy_version  text NOT NULL,
    accepted        boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX uta_user_idx ON user_terms_acceptances (user_id, document, created_at DESC);

CREATE TABLE user_security_events (
    id           uuid PRIMARY KEY,
    user_id      uuid REFERENCES users(id) ON DELETE CASCADE,
    event_type   text NOT NULL,
    ip_prefix    text NOT NULL DEFAULT '',
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX use_user_idx ON user_security_events (user_id, created_at DESC);
CREATE INDEX use_created_idx ON user_security_events (created_at);

-- Machine identities. Authenticate with X-API-Key (hashed at rest), never via human login.
CREATE TABLE service_accounts (
    id            uuid PRIMARY KEY,
    name          text NOT NULL UNIQUE,
    key_prefix    text NOT NULL UNIQUE,
    key_hash      bytea NOT NULL,
    roles         text[] NOT NULL DEFAULT '{service}',
    created_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz
);

CREATE TABLE user_data_requests (
    id                 uuid PRIMARY KEY,
    user_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind               text NOT NULL CHECK (kind IN ('export','deletion')),
    status             text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','processing','succeeded','failed','cancelled')),
    result_object_key  text,
    requested_at       timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz
);
CREATE INDEX udr_user_idx ON user_data_requests (user_id, requested_at DESC);

-- Seed roles and permissions. Privileged roles are never assignable from registration.
INSERT INTO roles (code, description, is_staff) VALUES
 ('super_admin','Full control including role management', true),
 ('admin','Platform administration: sources, categories, AI configuration', true),
 ('editor','Editorial and fact-check review', true),
 ('fact_checker','Fact-check review', true),
 ('analyst','Read-only analytics, health and audit inspection', true),
 ('user','Verified consumer account', false),
 ('service','Machine identity for internal automation', false);

INSERT INTO permissions (code, description) VALUES
 ('news:read','Read consumer news, events, search, fact-checks'),
 ('me:manage','Manage own profile, preferences, sessions'),
 ('sources:read','Read publishers, sources, crawl runs'),
 ('sources:write','Create/edit sources, publishers, extraction profiles, trigger crawls'),
 ('categories:write','Manage taxonomy'),
 ('articles:moderate','Review/reprocess/quarantine/tombstone articles'),
 ('events:edit','Merge/split/correct events'),
 ('factcheck:review','Review claims and verdicts'),
 ('factcheck:publish','Publish verdicts and narratives'),
 ('ai:configure','Manage AI providers, models, routes, prompts'),
 ('ai:evaluate','Run and inspect evaluations'),
 ('jobs:manage','Inspect and control jobs, replay dead letters'),
 ('audit:read','Read audit logs and metrics'),
 ('users:manage','Manage users and roles'),
 ('policies:write','Manage editorial policies and feature flags');

INSERT INTO role_permissions (role_code, permission_code)
SELECT 'super_admin', code FROM permissions;
INSERT INTO role_permissions (role_code, permission_code) VALUES
 ('admin','news:read'),('admin','me:manage'),('admin','sources:read'),('admin','sources:write'),
 ('admin','categories:write'),('admin','articles:moderate'),('admin','events:edit'),
 ('admin','factcheck:review'),('admin','ai:configure'),('admin','ai:evaluate'),
 ('admin','jobs:manage'),('admin','audit:read'),('admin','policies:write'),
 ('editor','news:read'),('editor','me:manage'),('editor','sources:read'),('editor','articles:moderate'),
 ('editor','events:edit'),('editor','factcheck:review'),('editor','factcheck:publish'),
 ('fact_checker','news:read'),('fact_checker','me:manage'),('fact_checker','factcheck:review'),
 ('fact_checker','sources:read'),
 ('analyst','news:read'),('analyst','me:manage'),('analyst','sources:read'),('analyst','audit:read'),
 ('analyst','ai:evaluate'),
 ('user','news:read'),('user','me:manage'),
 ('service','sources:read'),('service','jobs:manage');

-- +goose Down
DROP TABLE IF EXISTS user_data_requests, service_accounts, user_security_events, user_terms_acceptances,
  dev_mail_messages, email_deliveries, auth_refresh_tokens, auth_refresh_token_families, auth_sessions,
  password_reset_challenges, email_verification_challenges, user_roles, users, role_permissions,
  permissions, roles;
