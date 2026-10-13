# Moldova News Intelligence — backend

A Go backend that turns news from configured sources into **events**: one record per real-world
development, with a cited multilingual summary, a timeline and source links. Around that it
provides claim-level, evidence-based **fact-checking** with editorial review gates, hybrid
**search**, a **personalized feed** with catch-up, **briefings** with optional audio, **grounded
chat** and **timezone-aware notifications**. Every consumer endpoint requires a verified account.

The system never presents something as done when it isn't. A missing TTS or push credential
shows up as `provider_unavailable`, a degraded search component is listed in `degraded`, and a
claim without evidence is `unverified`, never `false`.

> Backend only: no UI, no deployment manifests. All fixture content is **synthetic**.

## What is in the box

| Area | Highlights |
|---|---|
| Identity | Email + password, 6-digit email OTP (HMAC-stored, 10-minute expiry, 5 attempts), Argon2id, EdDSA JWT access tokens, rotating refresh tokens with replay detection, RBAC, export and deletion |
| Sources | Publishers, content-rights permissions, RSS/Atom/JSON Feed/sitemap/HTML endpoints, extraction profiles, robots.txt, SSRF-safe fetching, health and anomalies, all managed through the API |
| Taxonomy | Database-managed categories (ro/ru/en), hierarchy, merges with redirects, versioned, reclassification jobs with dry run |
| AI platform | Provider-agnostic gateway (OpenAI-compatible, Anthropic, Gemini, deterministic local rules), task routes with fallbacks, prompt versions, schema validation, budgets, shadow mode, cost and latency accounting |
| Events | Cross-language grouping, novelty detection, versions, timelines, contradiction holds, reversible merge and split |
| Fact-checking | Evidence retrieval with source-independence accounting, deterministic verdict rules, review queue, separate publish permission, corrections with history, misinformation trends |
| Search | OpenSearch (lexical) + Qdrant (vectors), RRF fusion, versioned embedding migrations; PostgreSQL FTS fallback reported as degraded |
| Experience | Feed and catch-up, briefings and narration audio, grounded chat, notification schedules and APNs/FCM push |
| Operations | Transactional outbox, Kafka or PostgreSQL bus, durable jobs, dead letters with replay, audit log, Prometheus, OpenTelemetry, evaluation harness |

## Quick start

Prerequisites: Go 1.26+, Docker (for the local stack), `jq` for the demo script.

```bash
cp .env.example .env              # then set the three secrets (openssl rand -base64 32)
docker compose up -d              # PostgreSQL, Kafka, OpenSearch, Qdrant, Redis, MinIO, Mailpit
set -a; source .env; set +a

go run ./cmd/migrate              # embedded migrations
go run ./cmd/newsctl seed         # policies, AI registry, starter taxonomy, gazetteer
go run ./cmd/newsctl ingest-fixtures   # synthetic sources → articles → events → claims

go run ./cmd/api &                # :8080 (metrics on :9090)
go run ./cmd/worker &             # bus consumers, jobs, mail, schedulers (metrics on :9091)
scripts/demo.sh                   # register → verify → feed → event → search → chat → briefing
```

Create an administrator: register and verify an account, then run
`go run ./cmd/newsctl bootstrap-admin -email you@example.com`.

## Repository layout

```
cmd/            api, worker, migrate, newsctl (operator CLI), openapi-gen
api/            openapi.yaml (generated from Go types; do not edit by hand)
internal/
  platform/     config, db, bus (outbox, Kafka, Postgres), httpx, crypto, ssrf, storage, telemetry …
  identity/     registration, OTP, sessions, tokens, RBAC
  sources/ ingest/ taxonomy/ enrich/ nlp/      ingestion, normalization, enrichment
  ai/           gateway, providers, prompts, schemas, admin
  events/ search/ factcheck/                   intelligence core
  feed/ briefings/ chat/ notifications/        user experience
  eval/ analytics/ policy/ audit/ jobs/ mail/ seed/ apispec/
migrations/     goose SQL migrations (embedded)
fixtures/       synthetic feeds, pages, eval datasets, source manifest
docs/           architecture, decisions, data model, policies, API, testing, operations, handoff
```

## Tests

```bash
go test ./...      # unit + integration (needs PostgreSQL at TEST_DATABASE_ADMIN_URL or the compose default)
make test-infra    # Kafka redelivery + OpenSearch/Qdrant rebuild against the local stack
go run ./cmd/newsctl eval   # quality gates for the currently routed models
```

[docs/testing.md](docs/testing.md) maps each acceptance scenario to its test and lists the
environment-dependent integrations.

## Documentation

- [Architecture](docs/architecture.md) and [decision log](docs/decision-log.md)
- [Implementation plan and status](docs/implementation-plan.md)
- [Data model](docs/data-model.md) · [Event processing and messaging](docs/event-processing.md)
- [AI providers and routing](docs/ai-providers.md) · [Sources and categories](docs/sources-and-categories.md)
- [Fact-checking policy](docs/fact-checking-policy.md)
- [API guide with cURL examples](docs/api.md) · [OpenAPI contract](api/openapi.yaml)
- [Testing](docs/testing.md) · [Local operations](docs/operations-local.md) · [Handoff](docs/HANDOFF.md)
