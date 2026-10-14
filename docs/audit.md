# Implementation audit

Audit of the backend against the master specification and the follow-up "complete the backend"
brief. Date: 2026-10-14, branch `claude/admiring-fermat-vdqpa4`.

## Status vocabulary

| Status | Meaning |
|---|---|
| **Integration-tested** | Implemented and exercised end to end against real services (PostgreSQL, Kafka, OpenSearch, Qdrant, Redis, S3, Mailpit, ML service) by Go integration tests or `scripts/e2e.sh` on the compose stack |
| **Implemented** | Implemented and covered by unit, contract or in-process integration tests, not by a live external service |
| **Externally blocked** | Code is complete and tested against local substitutes; the live check needs credentials or network access this environment does not have |
| **Incomplete** | Missing or partial; listed with what is left |
| **Not planned** | Deliberately out of scope, with the reason |

No feature is reported as working when it isn't. Dev and test substitutes are named: the
`heuristic` provider ("local rules"), the ML service's `hash` backend, the dev mail sink and the
filesystem storage driver.

## Matrix

### Platform and infrastructure

| Requirement | Status | Evidence |
|---|---|---|
| Go monorepo: API, worker, migrate, operator CLI | Integration-tested | `cmd/*`; compose stack runs all four (`scripts/e2e.sh`) |
| PostgreSQL as source of truth, embedded migrations, no pgvector | Integration-tested | `migrations/`; every integration test; `grep -r pgvector` finds only docs |
| Transactional outbox; Kafka bus (and PostgreSQL bus for development) | Integration-tested | `TestInfraKafkaRedeliveryAndIndexRebuild` (real Kafka); compose runs `BUS_DRIVER=kafka`; e2e checks consumer groups and zero dead letters |
| OpenSearch lexical search | Integration-tested | `TestInfra…`; e2e checks the event index and non-degraded search |
| Qdrant vectors with versioned collections | Integration-tested | `TestInfra…`; e2e checks the `*__bge_m3__1024` collection fed by the ML service |
| Redis rate limits and caches | Integration-tested | Compose sets `REDIS_URL`; the e2e load test runs through Redis-backed limits (0 rate-limit errors at 325 req/s) |
| Object storage (S3 API) | Integration-tested | e2e: data export job → SeaweedFS → presigned download (`S3_PUBLIC_ENDPOINT`) |
| Durable jobs: leases, heartbeats, fenced outcomes, cancellation, crash recovery | Integration-tested | `internal/jobs` `TestLongJobKeepsLeaseNoDuplicate`, `TestCrashedWorkerJobIsRecovered`, `TestLostLeaseFencesOutcome`, `TestCancelRunningJob` |
| Temporal | Not planned | ADR-020: PostgreSQL jobs cover the workflows; trade-offs and migration triggers documented |
| Configuration changes without restart, across processes | Integration-tested | `TestConfigChangesPropagateAcrossProcesses` (LISTEN/NOTIFY); `scripts/verify-dynamic-config.sh` on compose |
| Docker Compose: all services, healthchecks, init, generated secrets | Integration-tested | `docker-compose.yml`, `scripts/setup.sh`, `scripts/init.sh`; e2e 23/23 |
| Observability: JSON logs, Prometheus, OpenTelemetry, Langfuse metadata | Implemented | Metrics endpoints checked by e2e; OTLP and Langfuse via contract tests |
| CI | Implemented | `.github/workflows/ci.yml` (gofmt, vet, OpenAPI drift, tests, gosec, govulncheck, ruff, pytest, compose e2e); first run happens on GitHub |

### Identity and account

| Requirement | Status | Evidence |
|---|---|---|
| Registration, email OTP verification, login, refresh rotation, logout | Integration-tested | Identity integration tests; e2e `demo.sh` reads the real OTP from Mailpit over SMTP |
| Password reset and change, email change, sessions | Implemented | Identity integration tests (dev mail sink) |
| RBAC, staff permissions, service accounts | Implemented | `TestRBACAndPrivilegeEscalation`, `TestRoutesMatchContractAndPermissions` |
| Data export and deletion | Integration-tested | `TestDataExportAndDeletion` (now including download and owner check); e2e S3 round trip |
| First administrator without manual steps | Integration-tested | `setup.sh` generates `ADMIN_*`; `init` runs `newsctl create-admin`; e2e logs in as that admin |

### Sources, ingestion and taxonomy

| Requirement | Status | Evidence |
|---|---|---|
| Add, edit, test, enable, disable sources at runtime | Integration-tested | `TestSourceAdminLifecycle`; `verify-dynamic-config.sh` on the running stack |
| RSS, Atom, JSON Feed, sitemap, HTML list pages; extraction; robots; SSRF safety | Implemented | Ingestion unit tests, `TestHTMLListEndpointOverHTTP`, SSRF tests |
| Real Moldovan RO and RU sources | **Externally blocked** | `config/sources/moldova.json` (14 endpoints) and `newsctl verify-sources` are ready. In this environment DNS for every site fails (egress policy); see [reports/sources-blocked-2026-10-14.md](reports/sources-blocked-2026-10-14.md). Run it where the sites are reachable; see [Real sources](#real-sources) |
| Categories as data (ro/ru/en), reclassification | Integration-tested | `TestDynamicCategoriesClassifyCorrectReclassify`; the live script adds a category and sees the worker use it |
| JavaScript-rendered sites, `official_api` endpoints | Not planned | Rejected at validation (ADR-019) |
| Raw HTML snapshots in object storage | Implemented | Stored only when the publisher permission has `allow_store_raw` and article pages are fetched; fixtures do neither |

### AI platform and ML service

| Requirement | Status | Evidence |
|---|---|---|
| Provider-agnostic gateway, routes, fallbacks, prompts, budgets, shadow mode | Implemented | `TestModelRoutingFallbackDeferAndEmbeddingMigration`, `TestInvalidOutputRepairRetry` |
| Switch chat or embedding model without code changes | Integration-tested | `verify-dynamic-config.sh` switches the classification route live; embedding migration test |
| Safe vector migrations (build, backfill, cutover, rollback) | Implemented | Embedding migration test; collections are named per model and dimension |
| Python ML service: embeddings, reranking | Integration-tested | `services/ml` (FastAPI); `TestMLServiceEmbeddingsAndRerank` (`IT_ML_URL`); compose routes `embeddings` and `reranking` to it (60+ embedding calls, rerank in search) |
| BGE-M3 and multilingual reranker weights | **Externally blocked** | Default backend is `fastembed` with BGE-M3 (ONNX) and `jina-reranker-v2-base-multilingual`. HuggingFace is blocked here, so the stack ran with the deterministic `hash` backend. `tests/test_models.py` (`ML_TEST_REAL_MODELS=1`) checks real models |
| Semantic classification and entity extraction experiments | Implemented (experimental) | `/v1/classify` (embedding similarity to label descriptions); `/v1/entities` (transformers NER, off by default). Production classification and entities stay in Go so business rules are not duplicated |
| Model evaluation | Implemented | `newsctl eval` suites: classification, claims, answers, retrieval (RO/RU, cross-language); `python -m app.evaluate` for any OpenAI-compatible embedding endpoint |
| Remote LLMs (Anthropic, OpenAI, Gemini, OpenAI-compatible) | **Externally blocked** | Wire contract tests; needs API keys |
| Narration TTS | **Externally blocked** | Gateway path tested with a fake speech provider; needs a TTS provider and route |

### Intelligence and experience

| Requirement | Status | Evidence |
|---|---|---|
| Event grouping across languages, novelty, versions, timelines, merge and split | Integration-tested | `TestEventGroupingNoveltyAndTimeline`, `TestRedeliveryIsIdempotent`; compose pipeline (fixtures → 9 events) |
| Claims and fact-checking with review and publish gates, corrections | Integration-tested | `TestFactCheckingWorkflow`; compose pipeline produces claims with verdicts |
| Hybrid search (OpenSearch + Qdrant, RRF, rerank) | Integration-tested | e2e: `degraded: []`, rerank through the ML service |
| Personalized feed and catch-up | Integration-tested | `TestPersonalizedFeedAndCatchUp`; e2e demo and load test |
| Briefings | Integration-tested | `TestBriefingAndAudio`; demo requests one on the stack |
| Grounded chat with citations and abstention | Integration-tested | `TestGroundedChat`; demo on the stack. Chat scope query parameterized (security review) |
| Notifications: schedules, quiet hours, DST, budgets, breaking alerts | Implemented | `TestScheduledNotifications`, `TestBreakingAlertsArePolicyGated`, schedule unit tests |
| Push delivery (APNs, FCM) | **Externally blocked** | Contract tests; needs Apple and Firebase credentials |
| Email delivery | Integration-tested | SMTP to Mailpit in compose; a real SMTP relay needs credentials |

### Quality, security and performance

| Requirement | Status | Evidence |
|---|---|---|
| Acceptance scenarios 1–14 | Integration-tested | [testing.md](testing.md) |
| Static security analysis | Implemented | gosec: 0 findings (17 annotated false positives, G104 excluded). Fixed: chat SQL concatenation, unchecked narrowing of numeric config |
| Dependency vulnerabilities | **Externally blocked** | `govulncheck` cannot reach vuln.go.dev here; runs in CI |
| Load test | Integration-tested | `cmd/loadtest`; [reports/load-2026-10-14.md](reports/load-2026-10-14.md): 373 req/s, p95 268 ms, 0 errors (32 workers, 4 vCPU) |
| Ingestion throughput test | Incomplete | Only the read API is load-tested. An ingestion benchmark needs a larger synthetic corpus |
| Evaluation data | Incomplete | Small synthetic datasets; add reviewed Moldovan cases before using the gates for model selection |
| OpenAPI contract | Implemented | Generated from Go types; drift and route/permission tests |
| Mobile apps | Not planned | Out of scope for this repository at this stage |

## Gaps found and fixed in this pass

| Gap | Fix |
|---|---|
| No Python ML service | `services/ml`: FastAPI, BGE-M3 embeddings, multilingual reranker, metrics, readiness, auth, evaluation CLI |
| Gateway reranked with embedding cosine only | `RerankProvider` and a `reranking` route; falls back to cosine when no rerank model is usable |
| Compose ran infrastructure only | API, worker, init, ML and storage services; healthchecks and dependency ordering; `setup.sh` generates secrets and an admin |
| Config caches were per process (TTL only) | PostgreSQL LISTEN/NOTIFY invalidation across API, worker and CLI |
| Long jobs could be picked up twice; a stale worker could overwrite results | Lease heartbeats, fenced completion, cancellation of running jobs |
| Source tests shared one global rate-limit bucket | Unsaved endpoints are limited per host |
| Data exports could not be downloaded | `download_url` (presigned S3 or authenticated stream) and `/v1/users/me/data-requests/{id}/download` |
| Presigned URLs pointed at the internal S3 hostname | `S3_PUBLIC_ENDPOINT` |
| A quarantined export stayed `queued` | Marked `failed` on the last attempt |
| Chat briefing scope built SQL by concatenation; withdrawn claims were valid scopes | Parameterized; withdrawn claims excluded |
| Negative or oversized numeric env values wrapped silently | Range-checked; Argon2 memory floor enforced in production |
| Local providers inherited a 60 rpm limit, so search degraded under load | Seeded limits for local providers |
| `verify-dynamic-config.sh` failed on re-runs | Archives earlier test categories |

## Remaining work

1. **Real sources.** Run `newsctl verify-sources` where the Moldovan sites are reachable, check
   permissions with each publisher, and fix extraction profiles where needed. The Go fetcher dials
   directly and ignores `HTTPS_PROXY` (this keeps the SSRF dial check); add an explicit crawl proxy
   if one is required.
2. **Real models.** Start the ML service with network access to HuggingFace (or a mounted model
   cache), compare `newsctl eval -suite retrieval` for `hash` and BGE-M3, then run the embedding
   migration.
3. **Keys.** Remote LLM, TTS, APNs/FCM and SMTP credentials, then live checks for each.
4. **Ingestion load and soak tests** with a larger corpus; tune consumer concurrency and pool sizes.
5. **Reviewed evaluation data** for Moldovan RO/RU cases.
6. **Deployment manifests** (Kubernetes or similar) and secret management for staging and production.

## Real sources

```bash
# the worker does the fetching, so live crawling must be enabled for it too
sed -i 's/^CRAWL_LIVE_ENABLED=.*/CRAWL_LIVE_ENABLED=true/' .env && docker compose up -d api worker
docker compose exec -e NEWSCTL_QUIET=1 worker newsctl verify-sources \
  -legal-basis "permission from <publisher>, <date>" -manifest config/sources/moldova.json
```

The command records the stated legal basis on each publisher's permission, tests every endpoint,
enables and crawls only the ones that pass, waits for the pipeline and writes
`var/reports/sources-<time>.md` (inside the container unless `-out` points elsewhere). The report covers extraction, languages, categories, events with
several sources, cross-language events, fact-check outcomes and AI executions. Permissions in the
manifest start `pending`: confirm the rights with each publisher before relying on them.
