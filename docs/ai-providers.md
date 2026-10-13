# AI providers, models and routing

All model use goes through `internal/ai.Gateway`. Core code asks for a **task**
(`category_classification`, `claim_extraction`, `event_synthesis`, …) with a typed input; the
gateway resolves the **route** from the database, renders the active **prompt version**, calls the
**model chain**, validates the structured output and records an **execution**. No vendor is
hardcoded in domain code.

## Registry (admin API, permission `ai:configure`)

| Object | Endpoint | Notes |
|---|---|---|
| Provider | `/v1/admin/ai/providers` | kind `openai_compatible`, `anthropic`, `gemini` or `heuristic`; base URL; API key by env-var name (`api_key_env`) or write-only `api_key` (AES-GCM encrypted at rest, never returned); rate limit, concurrency, timeout, extra headers |
| Model | `/v1/admin/ai/models` | kind chat/embedding/rerank/tts/stt; structured-output and JSON-mode support; languages; context and output limits; embedding dimensions (immutable); prices per million tokens or characters |
| Route | `/v1/admin/ai/routes` | per task and mode (`production`, `shadow`): primary model, ordered fallbacks, prompt version, temperature, max tokens, timeout, attempts, quality threshold, daily budget (USD), shadow sample rate, `on_unavailable` (`defer` or `fail`) |
| Prompt | `/v1/admin/ai/prompts` | versioned system/user templates per task; routes pin a version |
| Executions | `/v1/admin/ai/executions`, `/v1/admin/ai/usage` | tokens, characters, cost, latency, status, error category, fallback flag |
| Evaluations | `/v1/admin/ai/evaluations` (`ai:evaluate`) | runs and per-case results from `newsctl eval` |

The registry is cached and invalidated on admin writes. Every change is audited.

### Seeded state

`newsctl seed` registers:

- `heuristic` provider (enabled) with `local-rules` (chat; deterministic rules) and
  `local-hash-embed-384` (embeddings; feature hashing, 384 dimensions). Every task routes here by
  default, so the system works without any API key and tests are deterministic.
- `anthropic` (model `claude-opus-5-5`), `openai`, `gemini` and `local-vllm`
  (OpenAI-compatible, e.g. vLLM or Ollama): **seeded disabled**. Enable them and switch routes from
  the admin API when keys are available.

No TTS route is seeded: audio requests end `provider_unavailable` until an administrator configures
one (for example an OpenAI-compatible `/audio/speech` model).

## Adapters

| Kind | Chat / structured output | Embeddings | TTS | Notes |
|---|---|---|---|---|
| `openai_compatible` | `/chat/completions`; `response_format: json_schema` (strict) when the model supports it, `json_object` + schema in the system prompt for JSON-mode servers, prompt-only otherwise | `/embeddings` (index-ordered, dimension-checked) | `/audio/speech` | Works with OpenAI, vLLM, Ollama, LiteLLM and other compatible gateways |
| `anthropic` | Official Go SDK, Messages API with `output_config.format` JSON schema; `refusal` and `max_tokens` stop reasons mapped to errors | — | — | SDK retries disabled; the gateway owns retries and fallbacks |
| `gemini` | `generateContent` with `responseMimeType: application/json` and an OpenAPI-subset schema | `batchEmbedContents` with `outputDimensionality` | — | Safety blocks and truncation are explicit errors |
| `heuristic` | Deterministic rules for every task | Feature hashing | — | Baseline and test double; it refuses tasks it cannot do honestly (e.g. synthesis without sources in the target language) |

Contract tests (`internal/ai/providers_contract_test.go`) check each adapter's wire format and
error mapping against local HTTP servers.

## Reliability semantics

Errors are classified: `transient` (429, 5xx, timeouts: retried, then fall back), `permanent`
(4xx configuration or auth: fall back, no retry), `invalid_output` (schema or semantic validation
failed: bounded retry with a repair hint), `unsupported`, `budget`, `not_configured`.

When no model in the chain succeeds the gateway returns `ErrDeferred` (route `on_unavailable:
defer`): callers retry later through the bus or jobs and **never fabricate output**. With `fail`,
the error surfaces to the caller (e.g. chat returns `503 ai_unavailable`).

Structured outputs are validated twice: against the JSON schema, and semantically by the task type,
for example claim quotes must be exact spans of the source text, citations must reference supplied
documents, and assigned categories must exist in the active taxonomy.

## Prompt-injection boundary

Crawled text is untrusted. Prompts place it inside delimited data blocks with an explicit
instruction to ignore instructions found there; outputs are schema-constrained; citations are mapped
server-side to retrieved document refs only (an injected "cite z9" is dropped); evaluation case
`ans-injection` exercises this.

## Shadow mode, budgets and accounting

A `shadow` route for a task runs asynchronously on a sample of production calls; its results are
only recorded in `ai_executions` for comparison. A route's daily budget skips its models once the
day's recorded cost reaches the limit (falling back or deferring). Every call records tokens or
characters, cost from model prices, latency and outcome; `GET /v1/admin/ai/usage` aggregates them.

## Switching a model (example)

```bash
# 1. register and enable a provider with its key held in the environment
curl -X POST $BASE/v1/admin/ai/providers -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"slug":"anthropic-prod","kind":"anthropic","display_name":"Claude","api_key_env":"ANTHROPIC_API_KEY","enabled":true}'
# 2. register the model
curl -X POST $BASE/v1/admin/ai/models … -d '{"provider_id":"…","slug":"claude-opus-5-5","model_name":"claude-opus-5-5","kind":"chat","supports_structured":true,"enabled":true}'
# 3. point the route at it, keeping the local rules as fallback
curl -X PATCH $BASE/v1/admin/ai/routes/$ROUTE … -d '{"primary_model_id":"…","fallback_model_ids":["<local-rules id>"],"version":3}'
# 4. measure before and after
go run ./cmd/newsctl eval -suite classification
```

## Embedding migrations

Changing the embeddings route never breaks search. `POST /v1/admin/search/embedding-migration`
creates `building` collections for the new model and enqueues backfill jobs; queries keep using the
`active` collections. Cutover (`/collections/{docType}/cutover`) requires the collection's backfill
job to have succeeded and all backfilled vectors to be present; rollback reactivates the previous
collection.

## Observability

Prometheus counters and histograms per task and model (`ai_executions_total`,
`ai_execution_seconds`). With `LANGFUSE_HOST` and keys set, execution metadata (task, model,
latency, tokens, cost, status) is exported to Langfuse; prompt and completion text are not.
