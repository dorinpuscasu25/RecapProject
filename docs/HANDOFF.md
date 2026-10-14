# Handoff

## State

The backend is complete for local and staging use: Go API and worker, a Python ML service, and the
full infrastructure in one Docker Compose stack. Every requirement's status, with evidence, is in
[audit.md](audit.md). The latest verification run in the development container:

- 79 Go tests passed, 0 failed, 0 skipped (with Kafka, Qdrant, OpenSearch and the ML service)
- ML service pytest: 12 passed, 2 skipped (real-model tests are opt-in)
- `scripts/e2e.sh` against the compose stack: 23 passed, 0 failed
- Load: 373 req/s, p95 268 ms, 0 errors (32 workers, 4 vCPU)
- gosec: 0 findings

Branch: `claude/admiring-fermat-vdqpa4`.

## Run it

```bash
./scripts/setup.sh          # .env with generated secrets and an admin (admin@example.com, password in .env)
docker compose up -d        # builds and starts everything; init seeds and processes the fixtures
scripts/e2e.sh              # verifies the running stack
```

No API keys are needed to run: AI tasks fall back to deterministic local rules, and embeddings and
reranking come from the local ML service. Where HuggingFace is unreachable, set
`ML_EMBEDDING_BACKEND=hash` and `ML_RERANK_BACKEND=embedding` before the first start.

## Keys and credentials

| Capability | Needs | Without it |
|---|---|---|
| Remote LLMs | `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or `GEMINI_API_KEY` in `.env`; enable provider and model and switch routes via `/v1/admin/ai/*` (no restart) | Local deterministic rules (all features work, lower quality) |
| Real embeddings and reranking | Network access to huggingface.co on the first start (or a pre-filled `mlmodels` volume); `HF_TOKEN` only for gated models | `hash` backend (test quality) |
| Narration audio | A TTS-capable provider (e.g. OpenAI-compatible `/audio/speech`) and a `tts` route | Audio requests end `provider_unavailable` |
| Push | `PUSH_DRIVER=apns,fcm`, APNs key/team/topic/PEM, FCM project and service-account JSON | Deliveries recorded `provider_unavailable` |
| Email | SMTP relay settings (`SMTP_*`) | Mailpit (development); production refuses the dev sink |
| Real sources | `CRAWL_LIVE_ENABLED=true`, the publishers' permission, then `newsctl verify-sources` | Synthetic fixtures only |
| Production | The three secrets, `QDRANT_URL`, a real SMTP relay, Argon2 memory ≥ 19 MiB | Startup refuses |

## Where to look

- Status and evidence: [audit.md](audit.md); plan: [implementation-plan.md](implementation-plan.md)
- Architecture and decisions: [architecture.md](architecture.md), [decision-log.md](decision-log.md)
- Messaging, jobs and event intelligence: [event-processing.md](event-processing.md)
- AI routing, providers and the ML service: [ai-providers.md](ai-providers.md), [services/ml](../services/ml)
- Fact-checking rules: [fact-checking-policy.md](fact-checking-policy.md)
- API and examples: [api.md](api.md), `api/openapi.yaml`
- Tests, load and security: [testing.md](testing.md)
- Running locally: [operations-local.md](operations-local.md)

## Known limitations

1. **Not verified live from this environment:** real Moldovan sources (DNS blocked), BGE-M3 and
   reranker weights (HuggingFace blocked), remote LLMs, TTS, APNs/FCM, a real SMTP relay, and
   govulncheck (vulnerability database blocked). Each has a contract test, a substitute test or a
   ready-to-run command.
2. **Quality of the local rules.** Without remote models, summaries are extractive, chat answers
   quote sources rather than reason over them, and English paraphrase matching is weak.
3. **Evaluation data** is small and synthetic.
4. **Sources:** `official_api` endpoints and JavaScript-rendered sites are rejected at validation.
   The crawler dials directly and ignores `HTTPS_PROXY`.
5. **Performance:** the read path is load-tested; ingestion throughput is not yet.
6. **Long jobs** are not checkpointed: a reclaimed backfill restarts (idempotent, but repeated work).

## Suggested next steps

1. Watch the first CI run on GitHub (`.github/workflows/ci.yml`) and fix anything environment-specific.
2. Start the stack where HuggingFace is reachable; compare `newsctl eval -suite retrieval` for `hash`
   and BGE-M3; run the embedding migration.
3. Get publisher permissions and run `newsctl verify-sources` against the Moldovan manifest; tune
   extraction profiles from the report.
4. Add a remote chat model, compare `newsctl eval`, keep local rules as fallback.
5. Wire TTS and push credentials in staging and verify end to end.
6. Grow the evaluation datasets with editor-reviewed Moldovan cases; add ingestion load tests;
   write deployment manifests for staging and production.
