# Moldova News Intelligence — backend

A Go backend, with a Python ML service, that turns news from configured sources into **events**: one record per real-world
development, with a cited multilingual summary, a timeline and source links. Around that it
provides claim-level, evidence-based **fact-checking** with editorial review gates, hybrid
**search**, a **personalized feed** with catch-up, **briefings** with optional audio, **grounded
chat** and **timezone-aware notifications**. Every consumer endpoint requires a verified account.

The system never presents something as done when it isn't. A missing TTS or push credential
shows up as `provider_unavailable`, a degraded search component is listed in `degraded`, and a
claim without evidence is `unverified`, never `false`.

> Backend only: no mobile apps or UI yet, no production deployment manifests. All fixture content
> is **synthetic**. Status of every requirement, with evidence: [docs/audit.md](docs/audit.md).

## What is in the box

| Area | Highlights |
|---|---|
| Identity | Email + password, 6-digit email OTP (HMAC-stored, 10-minute expiry, 5 attempts), Argon2id, EdDSA JWT access tokens, rotating refresh tokens with replay detection, RBAC, export and deletion |
| Sources | Publishers, content-rights permissions, RSS/Atom/JSON Feed/sitemap/HTML endpoints, extraction profiles, robots.txt, SSRF-safe fetching, health and anomalies, all managed through the API |
| Taxonomy | Database-managed categories (ro/ru/en), hierarchy, merges with redirects, versioned, reclassification jobs with dry run |
| AI platform | Provider-agnostic gateway (OpenAI-compatible, Anthropic, Gemini, deterministic local rules), task routes with fallbacks, prompt versions, schema validation, budgets, shadow mode, cost and latency accounting |
| Events | Cross-language grouping, novelty detection, versions, timelines, contradiction holds, reversible merge and split |
| Fact-checking | Evidence retrieval with source-independence accounting, deterministic verdict rules, review queue, separate publish permission, corrections with history, misinformation trends |
| Search | OpenSearch (lexical) + Qdrant (vectors), RRF fusion, cross-encoder reranking, versioned embedding migrations; PostgreSQL FTS fallback reported as degraded |
| ML service | Python FastAPI: multilingual embeddings (BGE-M3, ONNX), multilingual reranker, classification and NER experiments, retrieval evaluation; OpenAI/Cohere-compatible wire formats |
| Experience | Feed and catch-up, briefings and narration audio, grounded chat, notification schedules and APNs/FCM push |
| Operations | Transactional outbox, Kafka or PostgreSQL bus, durable jobs, dead letters with replay, audit log, Prometheus, OpenTelemetry, evaluation harness |

## Quick start

Prerequisites: Docker with Compose v2. Go 1.26+ and `jq` only for the scripts and local development.

```bash
git clone https://github.com/dorinpuscasu25/recapproject && cd recapproject
./scripts/setup.sh        # creates .env: generated secrets and a first admin (admin@example.com)
docker compose up -d      # everything: infrastructure, ML service, init, API, worker
```

The first start builds the images, runs migrations, seeds policies, the AI registry and the
taxonomy, creates the admin, and processes the synthetic fixtures through the whole pipeline
(Kafka → enrichment → events → fact-checks → OpenSearch/Qdrant). The ML service downloads BGE-M3
and the reranker on first use (a few GB, cached in the `mlmodels` volume; the worker waits for it); the API is ready
when `curl localhost:8080/readyz` answers.

| URL | What |
|---|---|
| http://localhost:8080 | API (`/openapi.yaml`, `/healthz`, `/readyz`) |
| http://localhost:8025 | Mailpit: verification codes and all outgoing email |
| http://localhost:8000/docs | ML service (embeddings, rerank) |
| http://localhost:9090/metrics, :9091/metrics | API and worker Prometheus metrics |

```bash
scripts/demo.sh           # register → verify (code from Mailpit) → feed → event → search → chat → briefing
scripts/e2e.sh            # 23 end-to-end checks against the running stack, including a short load test
```

**Keys.** Nothing else is required: without API keys every AI task runs on deterministic local
rules and embeddings come from the local ML service. To use remote models, put
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or `GEMINI_API_KEY` in `.env`, run
`docker compose up -d`, then enable the provider and switch routes through the admin API (no
restart; see [AI providers](docs/ai-providers.md)). TTS, push (APNs/FCM) and a real SMTP relay
are configured the same way ([handoff](docs/HANDOFF.md#keys-and-credentials)).

**Offline or no HuggingFace access:** set `ML_EMBEDDING_BACKEND=hash` and `ML_RERANK_BACKEND=embedding`
in `.env` (deterministic, test-quality vectors) before the first start.

**Local development** without the app containers: `docker compose up -d postgres kafka opensearch qdrant redis s3 mailpit ml`,
then `set -a; . ./.env; set +a` and `go run ./cmd/migrate`, `go run ./cmd/newsctl seed`,
`go run ./cmd/api`, `go run ./cmd/worker` (see [local operations](docs/operations-local.md)).

## Repository layout

```
cmd/            api, worker, migrate, newsctl (operator CLI), openapi-gen, loadtest
api/            openapi.yaml (generated from Go types; do not edit by hand)
internal/
  platform/     config, db, bus (outbox, Kafka, Postgres), httpx, crypto, ssrf, storage, telemetry …
  identity/     registration, OTP, sessions, tokens, RBAC
  sources/ ingest/ taxonomy/ enrich/ nlp/      ingestion, normalization, enrichment
  ai/           gateway, providers, prompts, schemas, admin
  events/ search/ factcheck/                   intelligence core
  feed/ briefings/ chat/ notifications/        user experience
  eval/ analytics/ policy/ audit/ jobs/ mail/ seed/ apispec/
services/ml/    Python ML service (FastAPI): embeddings (BGE-M3), reranking, experiments, evaluation
migrations/     goose SQL migrations (embedded)
fixtures/       synthetic feeds, pages, eval datasets, source manifest
config/sources/ real source manifests (Moldovan RO/RU), used by `newsctl verify-sources`
deploy/         configuration for compose services (SeaweedFS S3 identities)
scripts/        setup, init, demo, e2e, dynamic-config check, test report
docs/           architecture, decisions, data model, policies, API, testing, operations, handoff
```

## Tests

```bash
go test ./...                 # unit + integration (needs PostgreSQL at TEST_DATABASE_ADMIN_URL or the compose default)
scripts/test-report.sh        # everything, plus Kafka/Qdrant/OpenSearch/ML integration when reachable
scripts/e2e.sh                # the running compose stack end to end
go run ./cmd/loadtest -c 32 -d 60s
go run ./cmd/newsctl eval     # quality gates for the currently routed models
(cd services/ml && pytest)    # ML service
```

[docs/testing.md](docs/testing.md) maps each acceptance scenario to its test and lists the
environment-dependent integrations.

## Documentation

- [Architecture](docs/architecture.md) and [decision log](docs/decision-log.md)
- [Implementation audit](docs/audit.md) and [plan and status](docs/implementation-plan.md)
- [Data model](docs/data-model.md) · [Event processing and messaging](docs/event-processing.md)
- [AI providers and routing](docs/ai-providers.md) · [Sources and categories](docs/sources-and-categories.md)
- [Fact-checking policy](docs/fact-checking-policy.md)
- [API guide with cURL examples](docs/api.md) · [OpenAPI contract](api/openapi.yaml)
- [Testing](docs/testing.md) · [Local operations](docs/operations-local.md) · [Handoff](docs/HANDOFF.md)
