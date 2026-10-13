# Data model

PostgreSQL 16 holds all canonical state; migrations live in `migrations/` (goose, embedded in the
binaries). IDs are UUIDv7 (time-ordered). Timestamps are `timestamptz` in UTC; scheduling uses IANA
time zones. Optimistic concurrency uses a `version` column (`409` on mismatch). There is no
pgvector: vectors live in Qdrant and are rebuildable.

## 00001 foundation

| Table | Purpose |
|---|---|
| `audit_logs` | Who did what, when, from where; old/new values (secrets redacted) |
| `outbox_events` | Transactional outbox (envelope, topic, partition key, published_at) |
| `message_deliveries` | PostgreSQL bus: one row per consumer and message, claimed with SKIP LOCKED |
| `consumer_dedupe` | (consumer, message_id) processed markers for idempotency |
| `dead_letters` | Exhausted messages with error, attempts and replay marker |
| `jobs` | Durable jobs: kind, payload, status, attempts, run_at, idempotency key, result |
| `feature_flags` | Flags with rollout percentage (deterministic per subject) |
| `editorial_policies` | Versioned JSON policies (`event_matching`, `factcheck`, `ranking`, `notifications`, `chat`, `search`, `briefings`) |
| `analytics_events` | Local pseudonymous usage events (ClickHouse optional) |

## 00002 identity

`users` (normalized email, E.164 phone, Argon2id hash, `status` pending_verification → active →
suspended/disabled/deletion_pending; active requires a verified email), `roles`, `permissions`,
`role_permissions`, `user_roles`, `email_verification_challenges` and `password_reset_challenges`
(HMAC of the code, expiry, attempts, consumed), `auth_sessions`, `auth_refresh_token_families` and
`auth_refresh_tokens` (rotation, replay detection revokes the family), `email_deliveries` (mail
outbox; bodies encrypted), `dev_mail_messages` (development sink), `user_terms_acceptances`,
`user_security_events`, `service_accounts` (machine identities with hashed keys),
`user_data_requests` (export/deletion).

## 00003 taxonomy and sources

`categories` (slug, parent, names/descriptions per language, status, classification hints,
aliases, positive/negative examples, boost, auto_classify, version), `category_redirects` (merge
targets), `taxonomy_versions`; `publishers` (type: newsroom, government, public_institution,
fact_checker, other), `source_permissions` (legal basis, crawl/store/summarize/embed/snippet/audio
permissions, robots and terms review, attribution, per publisher or endpoint), `source_endpoints`
(type, entry URL, polling, limits, extraction profile, allowed hosts, crawl window, health state),
`extraction_profiles` (selectors with fixtures), `source_crawl_runs` (counts, errors, status:
succeeded, partial, failed, skipped), `source_health`, `blocked_urls`.

## 00004 content and events

`articles` (canonical URL, language and confidence, title, body, content type, published/modified/
first-seen times, status pipeline, dedupe link, revision, correction/retraction/tombstone markers),
`article_versions` (revision history), `article_translations`, `article_categories` (source: ai,
editorial, rule; primary; confidence; model and taxonomy version), `entities` and
`article_entities`, `article_facts` (typed, normalized keys with offsets), `events` (titles and
summaries per language, type, state, status, occurrence and publication times, importance,
confidence, counts, citation coverage, editorial status, version), `event_versions` (snapshots),
`event_article_links` (relation origin/same_story/follow_up/related, confidence, rationale with
scores and thresholds, linked_by, unlink), `event_updates` (kind, what changed, severity,
`requires_approval`, approval), `event_entities`, `event_categories`, `event_relationships`,
`event_merge_history`, `event_narrations` (cached per event version and language),
`search_index_state` (last indexed version per document), `content_removal_requests`.

## 00005 fact-checking

`claims` (canonical text, language, normalized key, checkability, type, risk level, status,
current reader-visible verdict), `claim_occurrences` (exact quote spans with offsets, article,
event, attribution, extraction model), `event_claims`, `claim_evidence` (evidence article or
external URL, relation supports/contradicts/context, independence cluster, primary flag,
retrieval scores, superseded marker), `claim_verdicts` (verdict, proposed verdict, review status,
confidence, explanation per language, limitations, policy version, model refs, version),
`fact_check_reviews` (queue items with priority, decision, reviewer, publisher),
`misinformation_narratives`, `narrative_claim_links`.

## 00006 users and experience

`user_preferences`, `user_interests` (category, entity, keyword, region or publisher; follow or
mute; weight), `followed_events`, `user_event_state` (seen version, bookmark, dismissed),
`user_feed_cursors` (catch-up acknowledgement), `briefings` and `briefing_items` (ordered,
with citations), `audio_assets` (status, provider, model, segment manifest, checksum, cost),
`chat_sessions` and `chat_messages` (retention-bounded), `notification_schedules`,
`device_tokens` (HMAC hash for lookup, AES-GCM ciphertext for delivery, status),
`notification_deliveries` (unique per user, schedule, occurrence and device; status queued, sent,
failed, skipped with reason, provider_unavailable, invalid_token).

## 00007 AI platform

`ai_providers` (kind, base URL, API key env name or encrypted key, limits, timeout, headers),
`ai_models` (kind chat/embedding/rerank/tts/stt, capabilities, languages, limits, embedding
dimensions (immutable), prices), `prompt_templates` and `prompt_versions`, `ai_task_routes` (task,
mode production/shadow, primary and fallback models, prompt version, temperature, limits,
attempts, quality threshold, daily budget, sample rate, on_unavailable defer/fail),
`ai_executions` (every call: model, prompt version, tokens, characters, cost, latency, status,
error category, fallback flag, subject), `vector_collections` (doc type, model, dims, status
building/active/retired, backfill count), `eval_runs` and `eval_results`.

## Retention and privacy

- Unverified accounts, expired challenges and old sessions are purged by the worker's TTL cleanup.
- Chat sessions expire after `chat.retention_days`. Deletion requests remove personal rows and
  anonymize audit references.
- Device tokens are never returned or logged; analytics subjects are HMAC pseudonyms.
- Raw article snapshots are stored only when the publisher permission allows it.
