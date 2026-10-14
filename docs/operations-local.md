# Local operations

Development and evaluation only; production deployment is out of scope.

## Stack

`./scripts/setup.sh && docker compose up -d` starts:

| Service | Port | Used for |
|---|---|---|
| `postgres` (16) | 5432 | Source of truth (user/password `newsintel`) |
| `kafka` (3.8, KRaft) | 9092 (host), 19092 (containers) | Message bus (`BUS_DRIVER=kafka`) |
| `opensearch` (2.17) | 9200 | Lexical search (security disabled locally) |
| `qdrant` (1.12) | 6333 | Vectors |
| `redis` (7) | 6379 | Rate limits, feed cache |
| `s3` (SeaweedFS) | 8333 | S3-compatible object storage: exports, audio, raw snapshots. Credentials in `deploy/seaweedfs/s3.json` (ADR-022) |
| `mailpit` | 1025 SMTP, 8025 UI | All outgoing email, including verification codes |
| `ml` | 8000 | Python ML service: embeddings and reranking ([services/ml](../services/ml)) |
| `init` | — | One-shot: migrate, seed, create the admin, ingest fixtures (`SEED_FIXTURES`) |
| `api` | 8080, metrics 9090 | HTTP API |
| `worker` | metrics 9091 | Consumers, jobs, schedulers, mail |

Startup order is enforced with healthchecks: infrastructure → `init` (completed) → `api`;
`worker` also waits for `ml` to be ready. Rebuild after code changes with
`docker compose up -d --build api worker`.

Without the full stack the Go binaries also run with only PostgreSQL: PostgreSQL bus, PostgreSQL
full-text search and in-memory vectors (reported as `degraded`), in-process rate limits,
filesystem storage and the dev mail sink.

OpenSearch on a small disk may block index creation (disk watermark). The compose file disables
the threshold; on an existing node run:
`curl -XPUT localhost:9200/_cluster/settings -H 'Content-Type: application/json' -d '{"persistent":{"cluster.routing.allocation.disk.threshold_enabled":false,"cluster.blocks.create_index":null}}'`

## Configuration

`scripts/setup.sh` creates `.env` from `.env.example` with generated `SECRETS_ENCRYPTION_KEY`,
`AUTH_CODE_HMAC_SECRET`, `AUTH_SIGNING_KEYS` and a first admin (`ADMIN_EMAIL`, `ADMIN_PASSWORD`,
`ADMIN_PHONE`). It never overwrites an existing `.env`. Compose refuses to start when a required
secret is missing and says to run the script. API, worker and CLI must share the same secrets.
`APP_ENV=production` refuses to start without them and rejects unsafe settings (private-network
crawling, fixture time anchor, dev mail sink, weak Argon2 memory). Numeric settings are
range-checked.

ML service settings (`ML_*` in `.env`): `ML_EMBEDDING_BACKEND` (`fastembed`, `sentence_transformers`
or `hash`), `ML_EMBEDDING_MODEL` (default `BAAI/bge-m3`), `ML_EMBEDDING_DIMS`, `ML_RERANK_BACKEND`
(`fastembed`, `sentence_transformers`, `embedding` or `none`), `ML_RERANK_MODEL`, `ML_API_KEY`
(optional bearer token) and `HF_TOKEN` for gated models. Changing the embedding model or dimensions
is a vector migration (see below), not just a setting.

`S3_PUBLIC_ENDPOINT` is the storage address clients use; presigned URLs are signed for it
(compose: `localhost:8333`, while services use `s3:8333`).

## Day-to-day

```bash
docker compose logs -f worker                       # pipeline progress
docker compose exec worker newsctl reindex          # any newsctl command runs inside the image
docker compose exec worker newsctl eval -suite retrieval
scripts/e2e.sh                                      # end-to-end check of the running stack
scripts/verify-dynamic-config.sh                    # live config changes without restarts
go run ./cmd/loadtest -c 32 -d 60s                  # read-path load test
```

With `BUS_DRIVER=kafka` the worker relays the outbox and runs consumer groups; topics and `.dlq`
topics are created at start. `newsctl drain` only applies to the PostgreSQL bus.

Configuration changes made through the admin API (sources, categories, AI providers, models,
routes, prompts, policies, roles) reach every process immediately through PostgreSQL
`LISTEN/NOTIFY` on channel `newsintel_config`; time-based cache expiry remains as a fallback
(ADR-023).

## Operator tasks

| Task | How |
|---|---|
| Rebuild search indices | `newsctl reindex [-types event,article,claim]` or `POST /v1/admin/search/reindex` |
| Change embedding model | Register the model, point the `embeddings` route at it, `POST /v1/admin/search/embedding-migration`, wait for backfill jobs, cut over per doc type (rollback available) |
| Replay failed messages | `GET /v1/admin/dead-letters`, `POST /v1/admin/dead-letters/{id}/replay` or `newsctl replay-dead-letter -all` |
| Retry or cancel jobs | `/v1/admin/jobs`, `/{id}/retry`, `/{id}/cancel` |
| Reclassify after taxonomy changes | `POST /v1/admin/categories/reclassify` with `dry_run` first |
| Enable a remote model | Set its API key env var, enable provider and model, switch the route, run `newsctl eval` |
| Enable live crawling | `CRAWL_LIVE_ENABLED=true` (API and worker) and feature flag `live_crawling` for scheduled polling |
| Verify real sources | `newsctl verify-sources -legal-basis "…" [-manifest config/sources/moldova.json]`; see [audit](audit.md#real-sources) |
| Create or promote an admin | `newsctl create-admin -email … -password … -phone …` (or `ADMIN_*` env) |
| Switch to real ML models | `ML_EMBEDDING_BACKEND=fastembed` (default) with HuggingFace reachable or the model cache mounted at `/models`; then compare `newsctl eval -suite retrieval` |
| Enable breaking alerts | Feature flag `breaking_notifications` (rollout percentage supported); thresholds in policy `notifications` |
| Push notifications | `PUSH_DRIVER=apns,fcm` with APNs key (`.p8` PEM) and FCM service-account JSON; otherwise deliveries are `provider_unavailable` |
| Audit | `GET /v1/admin/audit-logs?resource_type=…` |

## Editable policies (`PUT /v1/admin/policies/{key}`)

| Key | Controls |
|---|---|
| `event_matching` | Candidate window, thresholds, signal weights, anchor requirement, LLM for ambiguous pairs |
| `factcheck` | Independence thresholds, evidence window, review-required verdicts, confidence threshold, high-risk terms |
| `ranking` | Feed weights (relevance, recency, importance, novelty, diversity), half-life, disputed penalty, per-category cap |
| `notifications` | Daily budget, update severity and cooldown, digest lookback, breaking thresholds (importance, sources, confidence, freshness, no dispute) |
| `chat` | Retention days, history turns, context documents |
| `search` | RRF k, lexical/semantic candidates, rerank depth |
| `briefings` | Events per minute, max events, words per minute |

## Observability

- Logs: JSON (`LOG_LEVEL`), request id on every line; no secrets, tokens or chat content.
- Metrics: Prometheus on the internal listeners (HTTP latency and status, bus processing and
  dead letters, jobs, AI executions and latency, analytics drops).
- Tracing: OpenTelemetry OTLP/HTTP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.
- AI: `/v1/admin/ai/executions` and `/usage`; optional Langfuse export (metadata only).
- Product analytics: `analytics_events` (PostgreSQL) or ClickHouse (`ANALYTICS_DRIVER`).
