# Local operations

Development and evaluation only; production deployment is out of scope.

## Stack

`docker compose up -d` starts:

| Service | Port | Used for |
|---|---|---|
| PostgreSQL 16 | 5432 | Source of truth (user/password `newsintel`) |
| Kafka 3.8 (KRaft) | 9092 | `BUS_DRIVER=kafka` |
| OpenSearch 2.17 | 9200 | Lexical search (security disabled locally) |
| Qdrant 1.12 | 6333 | Vectors |
| Redis 7 | 6379 | Rate limits, feed cache |
| MinIO | 9000 (console 9001) | `STORAGE_DRIVER=s3` |
| Mailpit | 1025 (UI 8025) | `MAIL_DRIVER=smtp` |

Everything except PostgreSQL is optional: without them the system runs with the PostgreSQL bus,
PostgreSQL full-text search and in-memory vectors (reported as `degraded`), in-process rate limits,
filesystem storage and the dev mail sink.

OpenSearch on a small disk may block index creation (disk watermark). The compose file disables
the threshold; on an existing node run:
`curl -XPUT localhost:9200/_cluster/settings -H 'Content-Type: application/json' -d '{"persistent":{"cluster.routing.allocation.disk.threshold_enabled":false,"cluster.blocks.create_index":null}}'`

## Configuration

Copy `.env.example` to `.env`, set `SECRETS_ENCRYPTION_KEY`, `AUTH_CODE_HMAC_SECRET` and
`AUTH_SIGNING_KEYS` (`openssl rand -base64 32`; signing keys are `kid:base64seed`), then
`set -a; source .env; set +a`. API and worker must share the same secrets. Without them, development
mode generates per-process random values, so tokens, stored provider keys and device tokens do not
survive restarts or cross processes. `APP_ENV=production` refuses to start without them and rejects
unsafe settings (private-network crawling, fixture time anchor, dev mail sink).

## Day-to-day

```bash
go run ./cmd/migrate && go run ./cmd/newsctl seed
go run ./cmd/newsctl ingest-fixtures        # synthetic sources; drains the pipeline (postgres bus)
go run ./cmd/api                            # :8080, metrics :9090
go run ./cmd/worker                         # metrics :9091
go run ./cmd/newsctl bootstrap-admin -email you@example.com
scripts/demo.sh                             # consumer walkthrough
```

With `BUS_DRIVER=kafka` the worker relays the outbox and runs consumer groups; topics and `.dlq`
topics are created at start. `newsctl drain` only applies to the PostgreSQL bus.

## Operator tasks

| Task | How |
|---|---|
| Rebuild search indices | `newsctl reindex [-types event,article,claim]` or `POST /v1/admin/search/reindex` |
| Change embedding model | Register the model, point the `embeddings` route at it, `POST /v1/admin/search/embedding-migration`, wait for backfill jobs, cut over per doc type (rollback available) |
| Replay failed messages | `GET /v1/admin/dead-letters`, `POST /v1/admin/dead-letters/{id}/replay` or `newsctl replay-dead-letter -all` |
| Retry or cancel jobs | `/v1/admin/jobs`, `/{id}/retry`, `/{id}/cancel` |
| Reclassify after taxonomy changes | `POST /v1/admin/categories/reclassify` with `dry_run` first |
| Enable a remote model | Set its API key env var, enable provider and model, switch the route, run `newsctl eval` |
| Enable live crawling | `CRAWL_LIVE_ENABLED=true` and feature flag `live_crawling` |
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
