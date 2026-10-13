# API guide

The contract is [`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1, 173 operations), also
served at `GET /openapi.yaml`. It is generated from the Go request/response types
(`go run ./cmd/openapi-gen`), and tests fail if it drifts from the code, if a mounted route is
undocumented, or if a documented permission differs from the one enforced.

## Conventions

- **Base path** `/v1`. JSON in and out; unknown request fields are rejected (`400`).
- **Authentication**: `Authorization: Bearer <access token>` (EdDSA JWT, 15 min). Machine
  identities use `X-API-Key` (service accounts). Only registration, verification, login, refresh,
  password reset, JWKS, health and the contract are public.
- **Accounts**: every consumer endpoint requires a verified, active account (`news:read`).
  Self-service endpoints require `me:manage`.
- **Permissions** for staff endpoints are listed per operation (`x-required-permissions`).
  A `403` carries `details.required_permission`.
- **Errors** are RFC 9457 `application/problem+json` with a stable `code` and `request_id`:
  `400` malformed, `401` unauthenticated, `403` forbidden/unverified/suspended, `404` (also for
  resources owned by someone else), `409` version or state conflict, `422` validation with field
  `errors[]`, `429` with `Retry-After`, `503` dependency or AI unavailable.
- **Concurrency**: mutable admin resources carry `version`; send it back on update to get `409`
  instead of a lost update.
- **Pagination**: list responses are `{"items": [...], "next_cursor": "…"}`; pass `cursor` and
  `limit`. Feed cursors are stable across new arrivals.
- **Async work** returns `202` with `job_id` and `status_url` (`/v1/admin/jobs/{id}`), or a
  resource with `status` (briefings, audio).
- **Time**: RFC 3339, stored in UTC. Schedules use IANA zones (`Europe/Chisinau`).
- **Language**: `?lang=ro|ru|en`, otherwise the user's summary language. Responses say when they
  fall back to another language (`language_fallback`).

## Identity flow

```bash
BASE=http://localhost:8080
curl -s -X POST $BASE/v1/auth/register -H 'Content-Type: application/json' -d '{
  "first_name":"Ana","last_name":"Rusu","email":"ana@example.com","phone":"069123456",
  "password":"Str0ng-Passw0rd!","accept_terms":true,"accept_privacy":true,"locale":"ro"}'
# 202 {"status":"verification_required","email":"ana@example.com","code_expires_in":600,
#      "message":"If this address can be used, a verification code has been sent to it."}

# development only: read the code from the dev mail sink (or Mailpit with MAIL_DRIVER=smtp)
curl -s "$BASE/v1/dev/mailbox?to=ana@example.com" | jq -r '.items[0].body'

curl -s -X POST $BASE/v1/auth/verify-email -H 'Content-Type: application/json' \
  -d '{"email":"ana@example.com","code":"123456"}'
# 200 {"access_token":"…","refresh_token":"rt_…","token_type":"Bearer","expires_in":900,"user":{…,"status":"active"}}

curl -s -X POST $BASE/v1/auth/refresh -H 'Content-Type: application/json' -d '{"refresh_token":"rt_…"}'
# reusing an already-rotated refresh token → 401 auth.refresh_token_reused (the whole family is revoked)
```

Wrong codes are counted (5 attempts), resends are rate limited, and responses never reveal whether
an address is registered. Login is refused until the email is verified.

## Reading news

```bash
TOKEN=…   # access token
H="Authorization: Bearer $TOKEN"

curl -s -H "$H" "$BASE/v1/categories/tree?lang=ru"
curl -s -H "$H" -X PUT $BASE/v1/users/me/categories -H 'Content-Type: application/json' -d '{"category_ids":["<energy id>"]}'
curl -s -H "$H" "$BASE/v1/feed?limit=20"                 # {"items":[{"event":{…},"reasons":["category"],…}],"next_cursor":"…"}
curl -s -H "$H" "$BASE/v1/events/$EVENT?lang=ro"         # summary with [ref] citations, counts, citation_coverage, version
curl -s -H "$H" "$BASE/v1/events/$EVENT/sources"         # publisher, url, relation (origin/same_story/follow_up)
curl -s -H "$H" "$BASE/v1/events/$EVENT/timeline"        # approved material updates
curl -s -H "$H" "$BASE/v1/catch-up"                      # since last acknowledgement (or 24 h)
curl -s -H "$H" -X POST $BASE/v1/catch-up/acknowledge -H 'Content-Type: application/json' -d '{}'
curl -s -H "$H" "$BASE/v1/search?q=tarif%20energie&mode=hybrid&types=event,claim"
# {"items":[…],"degraded":["lexical:postgres_fts"]}   ← degraded components are always reported
```

## Fact-checks

```bash
curl -s -H "$H" "$BASE/v1/fact-checks?limit=10"     # claim, verdict, review_status, headline, explanation
curl -s -H "$H" "$BASE/v1/fact-checks/$CLAIM"        # + evidence, limitations, history
curl -s -H "$H" "$BASE/v1/events/$EVENT/claims"
curl -s -H "$H" "$BASE/v1/misinformation/trends?days=30"
```

A proposed `refuted` verdict awaiting review is returned as `"verdict":"unverified",
"review_status":"pending_review"`.

## Briefings, audio, chat

```bash
curl -s -H "$H" -X POST $BASE/v1/briefings -H 'Content-Type: application/json' -d '{"duration_minutes":5,"language":"ro"}'
# 202 {"id":"…","status":"queued",…}; poll GET /v1/briefings/{id} until "ready": ordered items with citations
curl -s -H "$H" -X POST $BASE/v1/briefings/$B/audio      # 202; without a TTS route → status "provider_unavailable"
curl -s -H "$H" $BASE/v1/briefings/$B/audio              # status, short-lived url when ready

S=$(curl -s -H "$H" -X POST $BASE/v1/chat/sessions -H 'Content-Type: application/json' \
  -d "{\"scope_type\":\"event\",\"scope_id\":\"$EVENT\",\"language\":\"en\"}" | jq -r .id)
curl -s -H "$H" -X POST $BASE/v1/chat/sessions/$S/messages -H 'Content-Type: application/json' \
  -d '{"content":"Why is this important for a Moldova business?"}'
# {"content":"… [e1]","citations":[{"ref":"e1","type":"event","sources":[{"url":"https://…"}]}],
#  "abstained":false,"uncertainty":"…"}
curl -s -H "$H" "$BASE/v1/events/$EVENT/explain?level=background"
```

Answers cite only retrieved documents (refs are mapped server-side) and abstain when the sources do
not cover the question.

## Notifications

```bash
curl -s -H "$H" -X POST $BASE/v1/devices -H 'Content-Type: application/json' \
  -d '{"platform":"ios","token":"<APNs token>","app_version":"1.0.0"}'       # token never echoed
curl -s -H "$H" -X PUT $BASE/v1/notifications/schedules -H 'Content-Type: application/json' \
  -d '{"kind":"daily","local_times":["08:00"],"timezone":"Europe/Chisinau","quiet_start":"22:00","quiet_end":"07:00"}'
# {"name":"default","next_run_at":"2026-10-14T05:00:00Z",…}  (named schedules: PUT /v1/notifications/schedules/{name})
curl -s -H "$H" $BASE/v1/notifications/deliveries     # sent / skipped (with reason) / provider_unavailable / invalid_token
```

## Administration (examples)

```bash
A="Authorization: Bearer $ADMIN"
# publisher → permission → endpoint → test → enable → crawl → health
curl -s -H "$A" -X POST $BASE/v1/admin/publishers -H 'Content-Type: application/json' \
  -d '{"name":"Știri Exemplu","canonical_domain":"stiri-exemplu.md","publisher_type":"newsroom","primary_languages":["ro"]}'
curl -s -H "$A" -X PUT $BASE/v1/admin/publishers/$P/permissions -H 'Content-Type: application/json' \
  -d '{"legal_basis":"written agreement","permission_status":"granted","allow_crawl":true,"allow_store_full_text":true,
       "allow_summarize":true,"allow_embed":true,"allow_snippets":true,"robots_review_status":"compliant",
       "terms_review_status":"compliant","attribution_text":"Sursa: Știri Exemplu"}'
curl -s -H "$A" -X PATCH $BASE/v1/admin/publishers/$P -H 'Content-Type: application/json' -d '{"status":"active"}'
curl -s -H "$A" -X POST $BASE/v1/admin/sources -H 'Content-Type: application/json' \
  -d '{"publisher_id":"'$P'","name":"Știri RSS","endpoint_type":"rss","entry_url":"https://stiri-exemplu.md/rss","poll_interval_seconds":900}'
curl -s -H "$A" -X POST $BASE/v1/admin/sources/$E/test
curl -s -H "$A" -X POST $BASE/v1/admin/sources/$E/enable
curl -s -H "$A" -X POST $BASE/v1/admin/sources/$E/crawl
curl -s -H "$A" $BASE/v1/admin/sources/$E/health

# taxonomy
curl -s -H "$A" -X POST $BASE/v1/admin/categories -H 'Content-Type: application/json' \
  -d '{"slug":"electricity-prices","parent_id":"<energy id>","names":{"ro":"Prețuri la energia electrică","ru":"Цены на электроэнергию","en":"Electricity prices"},
       "classification_hints":["tarif energie electrică","kilowatt-oră"],"status":"active"}'
curl -s -H "$A" -X POST $BASE/v1/admin/categories/reclassify -H 'Content-Type: application/json' -d '{"dry_run":true}'

# AI routing, evaluation, operations
curl -s -H "$A" $BASE/v1/admin/ai/routes
curl -s -H "$A" "$BASE/v1/admin/ai/executions?task=event_synthesis"
curl -s -H "$A" $BASE/v1/admin/ai/evaluations
curl -s -H "$A" "$BASE/v1/admin/fact-checks/queue"
curl -s -H "$A" "$BASE/v1/admin/jobs?status=failed"
curl -s -H "$A" "$BASE/v1/admin/audit-logs?resource_type=source_endpoint"
```

`scripts/demo.sh` runs the consumer flow end to end against a local API and worker.
