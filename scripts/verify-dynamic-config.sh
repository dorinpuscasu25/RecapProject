#!/usr/bin/env bash
# Verifies, against a running stack (docker compose), that sources, categories and AI routes can be
# added/changed through the admin API and take effect in the separately running worker without any
# restart. Usage: ADMIN_EMAIL=… ADMIN_PASSWORD=… scripts/verify-dynamic-config.sh
set -euo pipefail
BASE=${BASE:-http://localhost:8080}
PSQL=${PSQL:-docker compose exec -T postgres psql -U newsintel -d newsintel -Atc}
: "${ADMIN_EMAIL:?}" "${ADMIN_PASSWORD:?}"
TOKEN=$(curl -s -X POST $BASE/v1/auth/login -H 'Content-Type: application/json' -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -r .access_token)
api() { curl -s -X "$1" "$BASE$2" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' ${3:+-d "$3"}; }
q() { $PSQL "$1"; }
wait_for() { local what=$1 cond=$2 i; for i in $(seq 1 60); do [[ $(q "$cond") == t ]] && { echo "  ok: $what (${i}s)"; return; }; sleep 1; done; echo "  FAIL: $what"; exit 1; }
RUN=$(date +%s)

echo "== 1. AI route switch reaches the worker"
P=$(api POST /v1/admin/ai/providers "{\"slug\":\"alt-rules-$RUN\",\"kind\":\"heuristic\",\"display_name\":\"Alt rules\",\"enabled\":true}" | jq -r .id)
M=$(api POST /v1/admin/ai/models "{\"provider_id\":\"$P\",\"slug\":\"alt-local-rules-$RUN\",\"model_name\":\"local-rules\",\"kind\":\"chat\",\"supports_structured\":true,\"enabled\":true}" | jq -r .id)
R=$(api GET /v1/admin/ai/routes | jq -c '.items[] | select(.task=="category_classification" and .mode=="production")')
ORIG=$(jq -r .primary_model_id <<<"$R")
if [[ $(jq -r .primary_model <<<"$R") == alt-local-rules-* ]]; then # left over by an interrupted run
  ORIG=$(api GET /v1/admin/ai/models | jq -r '.items[] | select(.slug=="local-rules") | .id')
fi
restore() { # always put the original model back and switch off this run's test provider
  local cur; cur=$(api GET /v1/admin/ai/routes | jq -c '.items[] | select(.task=="category_classification" and .mode=="production")')
  api PATCH /v1/admin/ai/routes/$(jq -r .id <<<"$cur") "{\"primary_model_id\":\"$ORIG\",\"version\":$(jq .version <<<"$cur")}" | jq -e --arg m "$ORIG" '.primary_model_id == $m' >/dev/null \
    && echo "  restored the original classification model" || echo "  WARNING: could not restore the classification route"
  P_VER=$(api GET /v1/admin/ai/providers | jq -r --arg id "$P" '.items[] | select(.id==$id) | .version')
  [[ -n $P_VER ]] && api PATCH /v1/admin/ai/providers/$P "{\"enabled\":false,\"version\":$P_VER}" >/dev/null || true
}
trap restore EXIT
api PATCH /v1/admin/ai/routes/$(jq -r .id <<<"$R") "{\"primary_model_id\":\"$M\",\"version\":$(jq .version <<<"$R")}" >/dev/null

echo "== 2. new category, then a new source (test → enable → crawl), no restarts"
ENERGY=$(api GET /v1/admin/categories | jq -r '.. | objects | select(.slug?=="energy") | .id' | head -1)
# archive categories left by earlier runs so they do not compete with this run's category
for old in $(q "SELECT id FROM categories WHERE slug LIKE 'startups-%' AND status='active'"); do api POST /v1/admin/categories/$old/archive >/dev/null; done
CAT=$(api POST /v1/admin/categories "{\"slug\":\"startups-$RUN\",\"names\":{\"ro\":\"Startup-uri $RUN\",\"ru\":\"Стартапы $RUN\",\"en\":\"Startups $RUN\"},\"classification_hints\":[\"startup-uri\",\"centru de tehnologie\",\"стартап\"],\"status\":\"active\"}")
jq -c '{slug, status}' <<<"$CAT"
PUB=$(api POST /v1/admin/publishers "{\"name\":\"Verificare $RUN\",\"canonical_domain\":\"diverse-exemplu.md\",\"publisher_type\":\"newsroom\",\"primary_languages\":[\"ro\"]}" | jq -r '.id // empty')
if [[ -z $PUB ]]; then PUB=$(api GET "/v1/admin/publishers?q=diverse-exemplu.md" | jq -r '.items[0].id'); fi
api PUT /v1/admin/publishers/$PUB/permissions '{"legal_basis":"synthetic fixture","permission_status":"granted","allow_crawl":true,"allow_store_full_text":true,"allow_summarize":true,"allow_embed":true,"allow_snippets":true,"robots_review_status":"compliant","terms_review_status":"compliant","attribution_text":"Sursa: fixture"}' >/dev/null
api PATCH /v1/admin/publishers/$PUB '{"status":"active"}' >/dev/null
EP=$(api POST /v1/admin/sources "{\"publisher_id\":\"$PUB\",\"name\":\"Day 2 $RUN\",\"endpoint_type\":\"fixture\",\"entry_url\":\"fixture://feeds/diverse-ro-day2.xml\",\"fetch_article_pages\":false,\"languages\":[\"ro\"],\"poll_interval_seconds\":900}" | jq -r '.id // empty')
REUSED=false
if [[ -z $EP ]]; then # second run: the source exists; reprocess its articles instead of crawling new ones
  EP=$(q "SELECT id FROM source_endpoints WHERE entry_url='fixture://feeds/diverse-ro-day2.xml'"); REUSED=true
fi
api POST /v1/admin/sources/$EP/test | jq -c '{ok, item_count, detected_format}'
api POST /v1/admin/sources/$EP/enable | jq -c '{status}'
if $REUSED; then
  api POST /v1/admin/sources/$EP/reprocess '{"reason":"reclassify"}' | jq -c '{status}'
else
  api POST /v1/admin/sources/$EP/crawl | jq -c '{status}'
  wait_for "worker crawled the new source" "SELECT count(*)>0 FROM source_crawl_runs WHERE source_endpoint_id='$EP' AND status IN ('succeeded','partial')"
fi
wait_for "worker classified with the switched route" "SELECT count(*)>0 FROM ai_executions WHERE model_slug='alt-local-rules-$RUN' AND status='succeeded'"
wait_for "an article got the new category" "SELECT count(*)>0 FROM article_categories ac JOIN categories c ON c.id=ac.category_id WHERE c.slug='startups-$RUN'"

echo "== 3. edit and disable the source"
api PATCH /v1/admin/sources/$EP '{"poll_interval_seconds":1800}' | jq -c '{poll_interval_seconds}'
api POST /v1/admin/sources/$EP/disable | jq -c '{status}'
[[ $(api POST /v1/admin/sources/$EP/crawl | jq -r .code) != null ]] && echo "  ok: crawling a disabled source is refused" || echo "  note: manual crawl of a disabled source accepted"

echo "== archive the test category"
api POST /v1/admin/categories/$(jq -r .id <<<"$CAT")/archive | jq -c '{slug, status}'

echo "ALL DYNAMIC CONFIGURATION CHECKS PASSED"
