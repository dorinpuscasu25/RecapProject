# Implementation plan and status

Status values: **done** (implemented and verified by tests), **partially done** (works, with
listed gaps), **blocked** (needs something outside this repository). Evidence refers to tests
described in [testing.md](testing.md).

## Phase A — Foundations: **done**

Monorepo with four binaries, configuration with production safety checks, PostgreSQL schema and
embedded migrations, transactional outbox with PostgreSQL and Kafka drivers, durable jobs, audit
log, problem+json errors, request ids, structured logs, tracing hooks, metrics.
Identity: email/password registration, 6-digit OTP (HMAC-stored, expiry, attempt limits, resend
limits), Argon2id, EdDSA access tokens with key ids, rotating refresh tokens with replay detection,
sessions, password reset and change, email change, export and deletion, RBAC with fine-grained
permissions and service accounts. Source and category admin CRUD with audit.
*Evidence:* identity integration tests (registration through logout, lockout, enumeration safety,
token tampering, RBAC, rate limits, export and deletion); contract test of route permissions.

## Phase B — Ingestion vertical slice: **done**

Scheduler, RSS/Atom/JSON Feed/sitemap parsing (entity-expansion safe), HTML list pages, article
extraction (profiles, readability heuristics, OpenGraph, JSON-LD), robots.txt, SSRF-safe fetching,
canonicalization, dedupe (URL, hash, SimHash, syndication), revisions, crawl runs and health
(partial runs and degraded states), source test and preview, moderation, fixture ingestion.
*Evidence:* `TestSourceAdminLifecycle`, `TestHTMLListEndpointOverHTTP`, ingestion and SSRF unit
tests, `newsctl ingest-fixtures`.

## Phase C — AI flexibility: **done**

Provider-agnostic gateway with database routes (primary, fallbacks, prompt versions, budgets,
shadow mode, defer/fail), typed tasks with schema and semantic validation and repair retries,
classification against the live taxonomy, embeddings, execution accounting, deterministic local
provider, and adapters for OpenAI-compatible APIs, Anthropic (official SDK) and Gemini.
*Evidence:* `TestModelRoutingFallbackDeferAndEmbeddingMigration`, `TestInvalidOutputRepairRetry`,
adapter contract tests, evaluation harness.

## Phase D — Event intelligence and search: **done**

Qdrant and OpenSearch indexing with stale-write protection, candidate retrieval and anchored
matching, event creation and versioning, novelty and contradiction holds, timelines, merge (reversible)
and split, hybrid retrieval with RRF, reindex and embedding migrations with cutover and rollback.
*Evidence:* `TestEventGroupingNoveltyAndTimeline`, `TestRedeliveryIsIdempotent`,
`TestInfraKafkaRedeliveryAndIndexRebuild` (real Kafka, Qdrant, OpenSearch).

## Phase E — Claims and fact-checking: **done**

Claim extraction with exact spans and checkability, evidence retrieval with independence clusters,
deterministic verdict rules defaulting to unverified, review gates, separate publish permission,
corrections with history, narratives, trends, re-verification on new primary evidence.
*Evidence:* `TestFactCheckingWorkflow`, policy unit tests, `TestBreakingAlertsArePolicyGated`.

## Phase F — Users and personalization: **done**

Preferences, interests (follow/mute), category onboarding, event follow/seen/bookmark/dismiss, ranked
feed with diversity and stable cursors, catch-up against acknowledgements, cache.
*Evidence:* `TestPersonalizedFeedAndCatchUp`.

## Phase G — Briefings, chat, audio, notifications: **done** (live providers credential-gated)

Briefing generation jobs with ordered, cited items and number checks; narration audio via a TTS
route with per-event segment reuse; grounded chat and explanations with server-side citation
mapping and abstention; DST-correct schedules, quiet hours, budgets, dedupe, followed-event updates,
policy-gated breaking alerts; APNs and FCM adapters.
*Evidence:* `TestBriefingAndAudio`, `TestGroundedChat`, `TestScheduledNotifications`,
`TestBreakingAlertsArePolicyGated`, push contract tests, schedule unit tests.
*Not verified live:* TTS, APNs and FCM need real credentials.

## Phase H — Hardening and evaluation: **done** (with listed gaps)

Replay and backfill, SSRF, RBAC and prompt-injection validation, observability (metrics, tracing,
Langfuse metadata export, analytics), evaluation harness with gates (classification, claims,
answers, retrieval), generated OpenAPI contract with drift and permission tests, read-path load test
(`cmd/loadtest`), static security analysis (gosec clean), compose end-to-end script, CI workflow.
*Evidence:* [testing.md](testing.md#stack-load-and-security-checks).
Gaps: ingestion throughput and soak tests; evaluation data is small and synthetic; govulncheck has
not run outside CI.

## Phase I — Complete backend architecture: **done** (live checks externally blocked)

Python ML service (embeddings, reranking, experiments, evaluation) integrated through the AI
gateway (ADR-021); full compose stack with Go API, worker, init, ML, PostgreSQL, Kafka, OpenSearch,
Qdrant, Redis, SeaweedFS S3 (ADR-022) and Mailpit, with healthchecks and generated secrets;
cross-process configuration invalidation (ADR-023); job leases with heartbeats, fencing and
cancellation (ADR-020, Temporal not adopted); real-source verification tooling with a Moldovan RO/RU
manifest; data export download; security fixes.
*Evidence:* [audit.md](audit.md).
*Externally blocked:* real model weights (HuggingFace) and the Moldovan sites are unreachable from
the build environment; remote LLM, TTS and push need credentials.

## Requirement notes

- **Temporal** was evaluated and not adopted (ADR-020); the PostgreSQL job runner has leases,
  heartbeats, fencing and cancellation.
- **Kafka** is the bus in the compose stack; the PostgreSQL bus remains for single-process
  development and tests.
- **pgvector** is not used anywhere.
- **Mobile apps** are not part of this repository yet.
