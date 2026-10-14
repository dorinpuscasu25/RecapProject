# Architecture Decision Log

Each entry: context → decision → consequences. Newest entries are appended at the bottom.
Status values: `accepted`, `superseded`, `deferred`.

---

## ADR-001 — Go monorepo with a small number of binaries (accepted)

**Context.** The product has 11 logical modules. Microservice proliferation would add operational
cost without benefit at this stage.

**Decision.** One Go module (`github.com/dorinpuscasu25/recapproject`) with domain packages under
`internal/` and four binaries:

| Binary | Role |
|---|---|
| `cmd/api` | Public + admin REST API (stateless, horizontally scalable) |
| `cmd/worker` | Outbox relay, message consumers (enrichment, events, fact-check, indexing, mail, notifications), scheduler, durable job runner |
| `cmd/migrate` | Applies embedded SQL migrations (goose) |
| `cmd/newsctl` | Operator CLI: bootstrap admin, seed, fixture ingestion, replay, reindex, offline evaluation |

Module boundaries are enforced by package layout and by the rule that modules only talk to each
other synchronously through narrow Go interfaces, and asynchronously through versioned messages.

**Consequences.** Any module can later be split into its own binary because it already
communicates via interfaces/messages.

## ADR-002 — chi router instead of Gin (accepted)

chi (`github.com/go-chi/chi/v5`) is `net/http`-native: handlers are plain `http.Handler`s,
middleware is standard, and there is no custom context type leaking into domain code. Gin's
performance advantage is irrelevant for this workload (I/O bound on DB/AI calls).

## ADR-003 — pgx with hand-written typed repositories, goose migrations (accepted)

**Decision.** `pgx/v5` pool, explicit parameterized SQL in per-module `store.go` files returning
typed structs. Migrations are plain SQL embedded via `embed.FS` and applied by goose.

**Why not sqlc now.** sqlc adds a code-generation step that must be kept in sync with migrations.
The typed-repository pattern gives equivalent type safety at the call site with all SQL visible
and reviewed in one place per module. Moving a store to sqlc later is mechanical (queries are
already isolated). Recorded as a possible follow-up.

## ADR-004 — No pgvector; Qdrant for vectors, OpenSearch for lexical (accepted)

PostgreSQL is the system of record and stores **no vectors**. Embedding vectors live only in Qdrant
(one collection per `{doc_type}_{embedding_model_slug}_{dim}` with an alias per doc type).
OpenSearch holds lexical projections. Both are rebuildable from Postgres (`newsctl reindex`).

**Graceful degradation.** When OpenSearch is not configured/reachable, the lexical retriever falls
back to PostgreSQL full-text search (`tsvector` generated column, `simple` config, unaccented) and
responses carry `"degraded": ["lexical:postgres_fts"]`. When Qdrant is unavailable the semantic
retriever is skipped and the response states `"degraded": ["semantic:unavailable"]`. This is a
transparent fallback, not a fake result.

## ADR-005 — Kafka via franz-go; Postgres outbox; pluggable bus for local runs (accepted)

**Decision.** All domain state changes that other modules care about are written to
`outbox_events` in the same transaction as the state change. The outbox relay publishes to Kafka
(`franz-go`, idempotent producer, key = aggregate id). Consumers record
`(consumer_group, message_id)` in `consumer_dedupe` inside the same transaction as their side
effects → effectively-once domain effects.

`BUS_DRIVER=kafka|postgres`. In `postgres` mode the relay dispatches the outbox rows directly to
the same in-process consumers (same dedupe, retry and DLQ semantics, rows locked with
`FOR UPDATE SKIP LOCKED`). This lets the full pipeline run and be tested without a Kafka broker;
it is a real durable queue, not a mock. Production intent is `kafka`.

**Topology.** A few versioned topics grouped by aggregate rather than one per event type:
see `docs/event-processing.md`. Every message has an envelope with `type` (e.g.
`article.enriched.v1`), so the 17 logical contracts from the brief map onto 9 physical topics plus
DLQs.

## ADR-006 — Temporal deferred; Postgres-backed durable job runner (accepted; re-evaluated in ADR-020)

**Context.** Temporal is recommended for multi-step workflows (briefings, reprocessing,
review-dependent flows). Running a Temporal cluster is a significant dependency for a first
version, and none of the current workflows need long-lived timers or signals that a job table
cannot express.

**Decision.** `internal/jobs` implements a durable job runner on PostgreSQL (`jobs` table, leases
with `FOR UPDATE SKIP LOCKED`, attempts, exponential backoff, `queued → processing → succeeded |
failed | quarantined`). Workflows are written as idempotent step functions behind a
`jobs.Handler` interface so they can be ported 1:1 to Temporal activities. Kafka is still used for
domain events; jobs are not a Kafka replacement.

**Consequences.** One less moving part locally. A Temporal adapter is listed in `HANDOFF.md`
as remaining work; nothing in domain code depends on the job runner implementation.

## ADR-007 — First-party identity with Ed25519 JWT access tokens + opaque rotating refresh tokens (accepted)

- Passwords: Argon2id (`m=64 MiB, t=3, p=2, salt=16B, key=32B`; configurable; tests use
  lower cost). Encoded in PHC string format so parameters can evolve.
- Email codes: 6 digits from `crypto/rand` (rejection sampling), stored as
  `HMAC-SHA256(server_secret, purpose|challenge_id|code)`, compared in constant time.
- Access token: JWT signed with **EdDSA (Ed25519)** only; the parser is configured with
  `WithValidMethods(["EdDSA"])` to prevent algorithm confusion; `kid` header selects the key from
  a key ring for rotation; validates `iss`, `aud`, `exp`, `iat`, `sub`, `jti`. 15 min default.
- Refresh token: 32 random bytes, base64url, stored as SHA-256 hash; belongs to a token family
  (`auth_refresh_token_families`) bound to a session (`auth_sessions`). Each refresh rotates; a
  reused (already-rotated) token revokes the whole family and session.
- Flow choice: **`verify-email` activates the account and issues a session** (same as a login) to
  avoid a second password entry right after registration.
- Service accounts authenticate with hashed API keys (`service_accounts` table, `X-API-Key`),
  never with the human login flow. See `docs/architecture.md` §Security.
- The identity boundary (`identity.TokenIssuer`, `identity.Authenticator`) is OIDC-compatible: a
  future OIDC federation adapter can mint the same principal type.

## ADR-008 — Provider-agnostic AI gateway with a deterministic heuristic provider (accepted)

**Decision.** `internal/ai` defines task-level operations (`GenerateStructured`, `GenerateText`,
`EmbedTexts`, `Rerank`, `TextToSpeech`). Routing (`ai_task_routes`) chooses primary + fallback
models per task from the database. Adapters: OpenAI-compatible (also covers vLLM, Ollama,
OpenRouter, Azure-style gateways), Anthropic Messages API, Google Gemini `generateContent`, and a
`heuristic` provider.

The `heuristic` provider is a **real rule-based baseline**, not a mock: language detection by
script/stop-words, taxonomy classification using DB-managed hints/aliases/examples, sentence-level
claim extraction with attribution patterns, and a deterministic hashed character n-gram embedding
(`local-hash-v1`, Cyrillic transliterated to Latin first so RO/RU share features). Its outputs are
marked `provider=heuristic` in `ai_executions` and provenance fields, so they are never presented
as LLM output. It is the default for local development and is what the evaluation harness uses as
a baseline.

A separate `fake` provider exists **only in test code** to simulate outages and malformed JSON.

## ADR-009 — Structured output validation (accepted)

Every structured task declares a JSON Schema (stored in `prompt_versions.output_schema`) and a Go
type. Output is validated by (1) JSON decoding with `DisallowUnknownFields`, (2) a minimal JSON
Schema validator (`internal/ai/schema`) for required fields/enums/types, (3) task-specific semantic
validation (offset bounds, citation ids exist in the provided context, enum membership against the
live taxonomy). Failures retry up to `route.max_attempts`, then the work item is quarantined.

## ADR-010 — Fact-check verdicts are deterministic-rule gated (accepted)

Models may *propose* evidence relations and verdicts, but the final automated verdict is computed by
deterministic sufficiency rules (`internal/factcheck/policy.go`): independent source clusters
(syndication collapsed), primary/official evidence weight, recency, contradiction balance.
Default without sufficient evidence is `unverified`. `refuted`, `misleading_context` and
`disputed` in high-risk contexts are stored as `proposed` and hidden from readers (shown as
`unverified` with `review_status=pending_review`) until an editor approves.

## ADR-011 — Object storage via minio-go with a local filesystem driver (accepted)

`STORAGE_DRIVER=s3|filesystem`. Raw snapshots and audio are never publicly addressable; audio is
streamed through an authenticated endpoint or served via short-lived presigned URLs (S3 mode).

## ADR-012 — Telemetry (accepted)

`log/slog` JSON logs with request/correlation ids; OpenTelemetry tracing with OTLP/HTTP exporter
when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (no-op otherwise); Prometheus metrics at `/metrics` on a
separate internal listener. AI observability via a `ai.Observer` interface with a Langfuse
adapter when configured.

## ADR-013 — Analytics optional (accepted)

Product analytics events go through `analytics.Sink` with `noop`, `postgres` (bounded
`analytics_events` table for local) and `clickhouse` (HTTP `JSONEachRow`) drivers. Analytics
failures never fail API or ingestion paths (fire-and-forget with bounded buffer).

## ADR-014 — Error model (accepted)

All errors are `application/problem+json` (RFC 9457) with a stable machine-readable `code`,
`request_id`, and optional `errors[]` field-level details. Status mapping: 400 malformed,
401 unauthenticated, 403 forbidden, 404, 409 version conflict / duplicate, 422 domain validation,
429 rate-limited (with `Retry-After`), 503 dependency unavailable / provider unavailable.

## ADR-015 — OpenAPI generated from Go types (accepted)

The contract is produced by `internal/apispec` from the actual request/response types (reflection)
and an operation table (access level, permissions, statuses, query parameters), written by
`go run ./cmd/openapi-gen`. Tests fail when the committed file differs from the generator, when a
mounted route is undocumented or a documented one is not mounted, and when a documented permission
is not the one the middleware enforces (403 problems carry `details.required_permission`).
Hand-written specs drift; generated ones stay honest.

## ADR-016 — Fixture time anchor (accepted)

Synthetic fixtures have fixed dates, but feed, catch-up, digest and breaking logic use windows
relative to now. `FIXTURE_TIME_ANCHOR` (dev/test only, refused in production) shifts fixture item
dates by `now − anchor`, keeping relative ages constant so tests do not depend on the calendar day.

## ADR-017 — Breaking alerts are policy-gated and corroboration-based (accepted)

Breaking pushes require the `breaking_notifications` flag and an event that is fresh, sufficiently
sourced and confident, with all checkable claims assessed, at least one corroborated, and nothing
disputed or awaiting review. Eligibility is re-evaluated on verdict updates and an alert is sent at
most once per event. Unresolved rumours are therefore never pushed as confirmed news.

## ADR-018 — Evaluation harness with regression gates (accepted)

`newsctl eval` measures classification, claim extraction and grounded answering for the currently
routed models on versioned datasets, persists per-case results, and fails on gate regressions. The
numbers are measurements on small synthetic sets, not accuracy claims.

## ADR-019 — Unsupported source modes are rejected (accepted)

`official_api` endpoints, headless rendering and JavaScript-required sources are rejected at
validation with an explanation instead of being accepted and failing or being silently ignored.
HTML list pages are supported through link selectors.

## ADR-020 — Temporal re-evaluated: PostgreSQL jobs kept, with leases, heartbeats and fencing (accepted; supersedes the "revisit" of ADR-006)

**Question.** The specification allows Temporal "where justified" for durable workflows. Is it
needed to meet the durability requirements?

**Workflows in this system and what they need:**

| Workflow | Shape | Needs |
|---|---|---|
| Briefing → audio | two independent jobs (`briefing.generate`, `audio.generate`), each idempotent | retries, visibility, explicit `provider_unavailable` |
| Reindex, vector backfill, reprocess, reclassify | single long job, idempotent upserts | crash recovery, no duplicate concurrent runs, cancellation, progress |
| Data export / deletion | single job | retries, audit |
| Fact-check review | human-in-the-loop state machine (`fact_check_reviews`) | durable state, not timers |
| Digests and notifications | periodic scheduler + per-occurrence dedupe | DST-correct timing, at-most-once per occurrence |
| Ingestion and enrichment | event-driven consumers over the outbox | at-least-once + idempotency (Kafka/PG bus) |

**What the PostgreSQL job runner provides (verified by `internal/jobs` tests):** durable state in
the source-of-truth database (enqueued in the same transaction as the business change),
idempotency keys, priorities, exponential backoff, quarantine after max attempts, leases with
**heartbeats** (a long job keeps its lease; no duplicate concurrent execution), **crash recovery**
(an expired lease is reclaimed), **fencing** (a worker that lost its lease cannot overwrite the new
owner's outcome; its handler is cancelled), **cooperative cancellation** of running jobs, admin
visibility (`/v1/admin/jobs`, retry, cancel) and metrics.

**What Temporal would add:** deterministic multi-step workflows with event-history replay, durable
timers and signals, saga-style compensation, workflow versioning and a workflow UI.

**Decision.** None of today's workflows is a long multi-step orchestration with compensation across
external systems; each is a single idempotent step or a database state machine. Temporal would add
a stateful cluster (server, its own database, UI) and a second programming model without closing a
real gap. Kept: PostgreSQL jobs. The gaps that did exist (no heartbeat, no fencing, no cancellation
of running jobs) were fixed in the runner.

**Revisit when** a workflow needs several external side effects with compensation (e.g.
distributing to partners and rolling back), long waits with many timers per entity, or
orchestration across separately deployed services. Job handlers keep a narrow interface
(`func(ctx, *Job) (any, error)`) so such a workflow can move to Temporal activities without
touching domain code.

**Known limitation.** Long jobs are not checkpointed: a reclaimed backfill restarts from the
beginning (safe because writes are idempotent, but repeated work).

## ADR-021 — Python ML service behind the AI gateway (accepted)

**Context.** Multilingual embeddings (Romanian, Russian, cross-language queries) and cross-encoder
reranking need model runtimes that are mature in Python and not in Go. Business logic must not be
split between two languages.

**Decision.** `services/ml` is a stateless FastAPI model server. It exposes OpenAI-compatible
`/v1/embeddings`, Cohere/Jina-compatible `/v1/rerank`, readiness and Prometheus metrics. Go reaches
it only through the AI gateway as an ordinary `openai_compatible` provider (`local-ml`), with
routes `embeddings` and `reranking`; it is therefore swappable for any compatible endpoint (a hosted
API, vLLM, TEI) by changing a route. The default runtime is ONNX via fastembed (no PyTorch, small
image, CPU-friendly): BGE-M3 (1024 dimensions, CLS pooling, normalized) and
`jina-reranker-v2-base-multilingual`. sentence-transformers is an optional backend; a deterministic
`hash` backend exists for tests and offline use only and is labelled as such.

Classification and entity extraction endpoints exist as **experiments** for evaluation; production
classification, entity linking, event matching, verdicts and ranking stay in Go.

**Consequences.** One more container and a model cache volume. Vector collections are named per
model and dimension (`<doc>__bge_m3__1024`), so switching models goes through the embedding
migration (build, backfill, cutover, rollback) and never mixes vector spaces. When the service is
down, embeddings defer (no fabricated vectors) and search reports `degraded`.

## ADR-022 — SeaweedFS for local S3 (accepted)

**Context.** MinIO no longer publishes community container images; the compose stack still needs an
S3-compatible store for exports, audio and raw snapshots.

**Decision.** Use SeaweedFS (`server -s3`, Apache-2.0) locally with static identities in
`deploy/seaweedfs/s3.json`. The application uses the generic S3 API (minio-go client), so any S3
service works in other environments via `S3_*` settings. `S3_PUBLIC_ENDPOINT` signs presigned URLs
for the address clients reach.

## ADR-023 — Cross-process configuration invalidation with LISTEN/NOTIFY (accepted)

**Context.** API, worker and CLI each cache the AI registry, editorial policies and session/permission
data. With time-based expiry only, an admin change could take up to the TTL to reach the worker.

**Decision.** Admin writes call `pg_notify('newsintel_config', scope)` in the writing transaction
(policies) or right after it commits (AI registry, identity); scopes are `ai`, `policy`,
`identity` and `*`. Each process holds a dedicated listening connection and
invalidates the matching caches; on reconnect it invalidates everything, since notifications may
have been missed. TTL expiry remains as a safety net. No extra infrastructure is needed, and
notifications are delivered only after commit, so readers never reload uncommitted configuration.
`TestConfigChangesPropagateAcrossProcesses` and `scripts/verify-dynamic-config.sh` verify it.
