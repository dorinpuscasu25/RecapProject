# Handoff

## State

The backend described in the master prompt is implemented as a Go monorepo. Phases A to G are
done and verified by tests; phase H is partially done (see
[implementation-plan.md](implementation-plan.md)). The last full run produced 70 passing top-level
tests, 0 failures and 0 skips, including the Kafka, Qdrant and OpenSearch integration test.

Branch: `claude/admiring-fermat-vdqpa4`. All work is committed locally. **Pushing to GitHub failed
with 403** because the Claude GitHub App has no access to `dorinpuscasu25/RecapProject`. Connect
GitHub at https://claude.ai/connect-github (and install the app on the repository) and push the
branch.

## Run it

```bash
cp .env.example .env    # set SECRETS_ENCRYPTION_KEY, AUTH_CODE_HMAC_SECRET, AUTH_SIGNING_KEYS
docker compose up -d && set -a && source .env && set +a
go run ./cmd/migrate && go run ./cmd/newsctl seed && go run ./cmd/newsctl ingest-fixtures
go run ./cmd/api & go run ./cmd/worker &
scripts/demo.sh         # verified end to end against the real binaries
scripts/test-report.sh  # suite + integration matrix
```

## Keys and credentials

| Capability | Needs | Without it |
|---|---|---|
| Remote LLMs | `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or `GEMINI_API_KEY`; enable provider and model, switch routes via `/v1/admin/ai/*` | Local deterministic rules (all features work, lower quality) |
| Narration audio | A TTS-capable provider (e.g. OpenAI-compatible `/audio/speech`) and a `tts` route | Audio requests end `provider_unavailable` |
| Push | `PUSH_DRIVER=apns,fcm`, APNs key/team/topic/PEM, FCM project and service-account JSON | Deliveries recorded `provider_unavailable` |
| Email | SMTP settings (`MAIL_DRIVER=smtp`) | Dev mail sink (development only; refused in production) |
| Production | The three secrets above, `QDRANT_URL`, non-devsink mail | Startup refuses |

## Where to look

- Architecture and decisions: [architecture.md](architecture.md), [decision-log.md](decision-log.md)
- Messaging, jobs and event intelligence: [event-processing.md](event-processing.md)
- AI routing and providers: [ai-providers.md](ai-providers.md)
- Fact-checking rules: [fact-checking-policy.md](fact-checking-policy.md)
- API and examples: [api.md](api.md), `api/openapi.yaml`
- Tests and scenario mapping: [testing.md](testing.md)
- Running locally: [operations-local.md](operations-local.md)

## Known limitations

1. **Quality of the local rules.** Without remote models, summaries are extractive and in the
   source language when no translation exists (the response says so via `language_fallback`), chat
   answers quote sources rather than reason over them, and English paraphrase matching is weak
   (one evaluation case fails by design). Route a remote model and compare with `newsctl eval`.
2. **Evaluation data** is small and synthetic. Add reviewed Moldovan cases per suite before relying
   on the gates for model selection.
3. **No live verification** of remote LLMs, TTS, APNs/FCM, SMTP, S3/MinIO presigning, Redis-backed
   limits, ClickHouse or Langfuse in this environment; each has contract or substitute tests.
4. **Sources**: `official_api` endpoints and JavaScript-rendered sites are not supported (rejected at
   validation). Live crawling is off by default.
5. **Temporal** is not used; long workflows run as PostgreSQL jobs (ADR-006).
6. **Performance**: no load tests yet; concurrency correctness is covered functionally.
7. **Fixtures** are synthetic; no real publisher content is included or crawled.

## Suggested next steps

1. Push the branch once GitHub access is fixed, and add CI running `scripts/test-report.sh` with
   the compose stack.
2. Configure a remote chat model and an embeddings model; run the embedding migration; compare
   evaluation results; keep the local rules as fallback.
3. Onboard real sources with signed permissions (start with official sources: government, ANRE,
   ministries), using `html_list` where no feed exists.
4. Grow evaluation datasets with editor-reviewed cases; add an event-grouping suite.
5. Add load tests for feed, search and ingestion throughput; tune pool sizes and consumer
   concurrency.
6. Wire real TTS and push credentials in a staging environment and verify end to end.
