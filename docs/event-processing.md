# Event processing and messaging

## Envelope

Every message is a versioned contract written to `outbox_events` in the same transaction as the
state change it announces:

```json
{
  "id": "01a1…",                         // UUIDv7; the idempotency key for every consumer
  "type": "event.updated.v1",
  "topic": "newsintel.event.v1",
  "key": "<aggregate id>",               // Kafka partition key: per-aggregate ordering
  "aggregate_type": "event",
  "aggregate_id": "…",
  "occurred_at": "2026-10-13T08:00:00Z",
  "payload": { … },
  "headers": { "request_id": "…" }
}
```

Breaking a payload means adding a new `.vN` type; consumers keep accepting the old one until it
is retired.

## Message types

| Type | Producer | Payload (main fields) | Consumers |
|---|---|---|---|
| `source.poll.requested.v1` | scheduler, `POST /admin/sources/{id}/crawl` | crawl_run_id, source_endpoint_id, trigger | `ingest-poller` |
| `article.discovered.v1` | ingest-poller | crawl_run_id, endpoint, publisher, feed item | `ingest-fetcher` |
| `article.extracted.v1` | ingest (persist) | article_id, revision | (informational) |
| `article.enrichment.requested.v1` | ingest, reprocess, reclassify | article_id, revision, reason | `enricher` |
| `article.enriched.v1` | enricher | article_id, revision, language, category_slugs, claim_ids | `search-indexer`, `fact-checker` (primary-evidence re-verification) |
| `event.resolution.requested.v1` | enricher | article_id | `event-resolver` |
| `event.created.v1` / `event.updated.v1` | events | event_id, version, kind, material, severity, article_ids, category_ids, importance | `search-indexer`, `notification-events` |
| `claim.verification.requested.v1` | events, admin reverify | claim_id, event_id, reason | `fact-checker` |
| `claim.verdict.updated.v1` | fact-checker, review publish/correct | claim_id, verdict_id, version, verdict, proposed, review_status | `search-indexer`, `notification-events` |
| `search.index.requested.v1` | editorial operations | doc_type, id, force | `search-indexer` |
| `content.deleted.v1` | withdrawals, quarantine | doc_type, id | `search-indexer`, `fact-checker` (evidence superseded) |
| `briefing.requested.v1` / `briefing.ready.v1` / `audio.requested.v1` | briefings | ids | (informational; work runs as jobs) |
| `notification.requested.v1` | notification scheduler and event consumer | delivery_id | `notification-sender` |

Informational types have no internal consumer; they exist for downstream analytics and
integrations.

## Topics

With `BUS_DRIVER=kafka`, types are grouped by aggregate into a few physical topics (prefix
`newsintel.`): `source.v1`, `article.v1`, `event.v1`, `claim.v1`, `search.v1`, `briefing.v1`
(briefing and audio), `notification.v1`, `content.v1`, each with a `.dlq` sibling. Topics are
created at worker start (6 partitions, replication 1 locally; set appropriately per cluster).
Consumer group = `<prefix>.<consumer name>`. Producers use acks=all.

With `BUS_DRIVER=postgres` (default for local runs and tests), the relay fans each outbox row out
to `message_deliveries` (one row per subscribed consumer) and workers claim rows with
`FOR UPDATE SKIP LOCKED`. Same envelope, same consumers.

## Delivery guarantees

- **At-least-once.** The relay marks an outbox row published only after the broker acknowledges.
- **Idempotent consumers.** Handlers run inside `Delivery.Once`: the consumer's work and a
  `consumer_dedupe(consumer, message_id)` row commit together; a redelivered message is skipped.
  Handlers whose work spans several transactions (event resolution) write the dedupe row in their
  final transaction. The infrastructure test re-relays every message and asserts identical state.
- **Retries.** Errors are retried with exponential backoff (2 s doubling, capped at 10 min) up to
  the consumer's `MaxAttempts` (6–10). Permanent errors (`bus.PermanentErr`) skip retries. In
  Kafka mode retries are bounded inside the consumer to keep partition order.
- **Dead letters.** Exhausted messages go to `dead_letters` (and the `.dlq` topic). Operators list
  them at `GET /v1/admin/dead-letters` and replay with the original id
  (`POST /v1/admin/dead-letters/{id}/replay` or `newsctl replay-dead-letter`).
- **Stale writes.** Search documents are written with external versioning
  (`search_index_state` + OpenSearch `version_type=external_gte`), so out-of-order or replayed
  index requests cannot regress a document.

## Durable jobs

| Kind | Started by | Notes |
|---|---|---|
| `taxonomy.reclassify` | `POST /admin/categories/reclassify` | dry run reports proposed changes; editorial assignments are preserved |
| `articles.reprocess` | admin article/source reprocess | re-enrichment with reason |
| `search.reindex` | admin, `newsctl reindex` | rebuilds OpenSearch and Qdrant documents from PostgreSQL |
| `search.vector_backfill` | embedding migration plan | fills a `building` collection; cutover requires it to succeed |
| `briefing.generate`, `audio.generate` | briefing/audio requests | audio ends `provider_unavailable` without a TTS route |
| `user.export`, `user.deletion` | self-service data requests | |

Jobs have status (`queued`, `processing`, `succeeded`, `failed`, `quarantined`, `cancelled`),
attempts with backoff, idempotency keys, results and last errors (`/v1/admin/jobs`).

## Event intelligence

1. **Candidates.** For an enriched article: vector neighbours (Qdrant), shared entities, shared fact
   keys and a time window (`event_matching.time_window_hours`).
2. **Scoring.** Weighted semantic, entity, fact and time signals (`event_matching.weights`), plus an
   anchor requirement: a shared entity or decision fact, so topical similarity alone never merges
   two decisions. Ambiguous scores can be sent to the `event_equivalence` AI route
   (feature flag `llm_event_equivalence`).
3. **Resolution** is serialized with an advisory lock so concurrent articles about a new event
   create one event, not two.
4. **Novelty.** Facts are normalized across languages (numbers, money, percentages, dates and a
   language-independent decision class). An article that adds no new fact keys is coverage, not an
   update. New facts produce a material update with severity; a conflicting fact is a
   **contradiction**, recorded but held (`requires_approval`) until an editor approves it.
5. **Synthesis.** Multilingual summaries (short/normal/detailed) with `[ref]` citations validated
   against supplied sources; unsupported citations fail validation. Coverage of claims by citations
   is reported (`citation_coverage`).
6. **Versions.** Material changes snapshot the event (`event_versions`) and add a timeline entry.
   Editors can merge (reversible through `event_merge_history`), split, correct, approve held
   updates and refresh; all actions are audited.

## Notifications

The worker's scheduler tick claims due schedules (`SKIP LOCKED`), computes the occurrence key
(`2026-10-14T08:00@Europe/Chisinau`), applies the missed-window (1 h), quiet-hours and daily-budget
rules, picks top events since the last digest, and writes one delivery per active device (unique
per user, schedule, occurrence and device). `notification-sender` decrypts the token only to send,
maps provider answers (sent, invalid token → device deactivated, transient → retry,
no credentials → `provider_unavailable`).

Followers of an event receive material, approved updates at or above the policy severity.
Breaking alerts (flag `breaking_notifications`) are sent once per event to category followers only
when the event is fresh, sufficiently sourced and confident, its checkable claims have been
assessed and at least one is corroborated, and nothing is disputed or awaiting review. The rule is
re-evaluated when verdicts change.
