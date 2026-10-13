# Testing

## Running

```bash
docker compose up -d postgres                 # integration tests need PostgreSQL
go test ./...                                 # unit + integration
scripts/test-report.sh                        # full run with a pass/fail summary and the integration matrix
make test-infra                               # Kafka + Qdrant + OpenSearch (needs the full local stack)
go run ./cmd/newsctl eval                     # quality gates for the currently routed models
```

Integration tests create a database per test by cloning a migrated template
(`TEST_DATABASE_ADMIN_URL`, default `postgres://newsintel:newsintel@localhost:5432/postgres`), run the
real HTTP router over `httptest`, and drain the PostgreSQL bus and job runner synchronously, so
each test exercises API → outbox → consumers → jobs → API. `TEST_LOG=1` prints application logs.

Fixtures are synthetic. `FIXTURE_TIME_ANCHOR` keeps their age relative to "now" constant, so recency
windows (catch-up, digests, breaking alerts) behave the same on any day.

Last full run in the development container: **70 top-level tests passed, 0 failed, 0 skipped**,
with Kafka, Qdrant and OpenSearch exercised.

## Acceptance scenarios

| # | Scenario | Test |
|---|---|---|
| 1 | Admin onboards a source (publisher, permission, endpoint, polling, test, enable, health) without code changes | `internal/api` `TestSourceAdminLifecycle`; list pages over HTTP: `internal/ingest` `TestHTMLListEndpointOverHTTP` |
| 2 | Admin adds Energy → Electricity prices (ro/ru); new articles are classified into it; correction and reclassification | `internal/api` `TestDynamicCategoriesClassifyCorrectReclassify` |
| 3 | Three articles in two languages about one decision form one event with several citations; a separate decision stays separate | `internal/events` `TestEventGroupingNoveltyAndTimeline` |
| 4 | A second-day article with no new facts causes no update; a verified change creates a version and timeline entry | `internal/events` `TestEventGroupingNoveltyAndTimeline` |
| 5 | Switching a route's model changes the provider; failures fall back or defer; an embedding migration is versioned and keeps search working | `internal/api` `TestModelRoutingFallbackDeferAndEmbeddingMigration`, `TestInvalidOutputRepairRetry`; adapters: `internal/ai` contract tests |
| 6 | No evidence → `unverified`; contrary evidence → proposed `refuted` held for review | `internal/factcheck` `TestFactCheckingWorkflow`, policy unit tests |
| 7 | Misinformation watch shows claim, review status, evidence, citations, dates and caution | `internal/factcheck` `TestFactCheckingWorkflow` |
| 8 | Correcting a published verdict updates the explanation and keeps history | `internal/factcheck` `TestFactCheckingWorkflow` |
| 9 | Technology + Chișinău vs agriculture users get different feeds; catch-up since last acknowledgement | `internal/feed` `TestPersonalizedFeedAndCatchUp` |
| 10 | Five-minute briefing with ordered events and citations; TTS swappable by configuration; no fabricated audio | `internal/briefings` `TestBriefingAndAudio` |
| 11 | "Why is this important for a Moldova business?" → grounded answer with links and uncertainty; abstention when not covered | `internal/briefings` `TestGroundedChat` |
| 12 | Kafka redelivery does not duplicate; OpenSearch and Qdrant rebuild from canonical records | `internal/app` `TestInfraKafkaRedeliveryAndIndexRebuild` (real services), `internal/events` `TestRedeliveryIsIdempotent` |
| 13 | SSRF via URL or redirect blocked; ordinary users cannot reach admin controls | `internal/api` `TestSSRFAndAdminRBAC`, `TestRoutesMatchContractAndPermissions`; `internal/platform/ssrf` tests; `internal/identity` `TestRBACAndPrivilegeEscalation` |
| 14 | Clear pass/fail report and the list of environment-dependent integrations | `scripts/test-report.sh` |

Other coverage: identity (registration validation, OTP lockout/expiry/resend, enumeration safety,
concurrent verification, refresh replay, token tampering and algorithm confusion, rate limits,
password reset and change, email change, export and deletion), notifications (DST spring-forward
and fall-back, quiet hours, schedules over HTTP, dedupe, invalid tokens, breaking-alert policy),
APNs/FCM and AI adapter wire contracts, analytics and Langfuse exporters, evaluation gates,
OpenAPI drift and route/permission parity, NLP (transliteration, facts, claims, SimHash, gazetteer),
ingestion (feed formats, entity-expansion defence, robots, canonicalization, extraction profiles).

## Environment-dependent integrations

| Integration | How it is tested here | Not exercised |
|---|---|---|
| PostgreSQL | All integration tests | — |
| Kafka | `TestInfra…` with `IT_KAFKA_BROKERS` | — when unset (test skips) |
| Qdrant / OpenSearch | `TestInfra…` with `IT_QDRANT_URL` / `IT_OPENSEARCH_URL`; otherwise in-memory vectors and PostgreSQL FTS (reported as degraded) | — when unset |
| OpenAI-compatible / Anthropic / Gemini | Wire-format contract tests against local servers | Live calls (need keys) |
| TTS | Fake speech provider routed through the real gateway | Live synthesis |
| APNs / FCM | Contract tests (JWT signing, OAuth, error mapping) | Live delivery (needs credentials) |
| SMTP | Dev mail sink | Real SMTP (use Mailpit locally) |
| S3 / MinIO | Filesystem driver | S3 presigning against MinIO |
| Redis | In-memory limiter and cache | Redis-backed limiter (used when `REDIS_URL` is set) |
| ClickHouse / Langfuse | Contract tests | Live ingestion |

## Evaluation

`newsctl eval` runs three suites over `fixtures/eval/*.json` against the currently routed model and
fails (exit 3) when a gate is not met. Baseline with the local rules (synthetic data):

| Suite | Metrics (local-rules) | Gates |
|---|---|---|
| classification (14 cases) | precision 1.00, recall 0.95, F1 0.97, exact match 0.93 | F1 ≥ 0.6, recall ≥ 0.6 |
| claims (8 cases) | checkable recall 1.00, non-checkable rejection 1.00, span fidelity 1.00 | 0.6 / 0.8 / 1.0 |
| answers (7 cases) | abstention accuracy 0.86, citation validity 1.00, required-citation recall 0.80, uncertainty stated 1.00 | 0.8 / 1.0 / 0.7 / 1.0 |

These are measurements on a small synthetic corpus, not accuracy claims. The rule-based baseline
fails one answer case (paraphrase "growth"/"grew"); its safety property (no injected citation) holds.
