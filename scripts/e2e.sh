#!/usr/bin/env bash
# End-to-end check of the running compose stack: health of every service, the real integrations
# (Kafka, OpenSearch, Qdrant, Redis, S3, Mailpit, ML service) as used by the app, the consumer
# walkthrough, live dynamic configuration, and a short load test.
# Usage: docker compose up -d --wait && scripts/e2e.sh        (LOAD_SECONDS=0 skips the load test)
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env; set +a
BASE=${BASE:-http://localhost:8080}
PSQL=(docker compose exec -T postgres psql -U newsintel -d newsintel -tAc)
pass=0; fail=0
ok()   { echo "  ok    $1"; pass=$((pass+1)); }
bad()  { echo "  FAIL  $1"; fail=$((fail+1)); }
check() { local name=$1; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi; }
q() { "${PSQL[@]}" "$1" | tr -d '[:space:]'; }
gt0() { [[ $(q "$1") -gt 0 ]]; }

echo "== service health"
check "api /healthz"            curl -fsS "$BASE/healthz"
check "api /readyz"             curl -fsS "$BASE/readyz"
check "worker metrics"          curl -fsS localhost:9091/metrics
check "ml /readyz"              curl -fsS localhost:8000/readyz
check "opensearch cluster"      curl -fsS localhost:9200/_cluster/health
check "qdrant"                  curl -fsS localhost:6333/collections
check "mailpit"                 curl -fsS localhost:8025/api/v1/info
check "s3 (SeaweedFS)"          curl -sS -o /dev/null localhost:8333
check "redis"                   docker compose exec -T redis redis-cli ping
check "kafka topics"            docker compose exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list

echo "== pipeline results (seeded fixtures)"
check "articles enriched"       gt0 "SELECT count(*) FROM articles WHERE status='enriched'"
check "events created"          gt0 "SELECT count(*) FROM events"
check "claims with verdicts"    gt0 "SELECT count(*) FROM claim_verdicts"
check "no dead letters"         test "$(q "SELECT count(*) FROM dead_letters WHERE replayed_at IS NULL")" = 0
check "embeddings via ML service" gt0 "SELECT count(*) FROM ai_executions WHERE provider_slug='local-ml' AND status='succeeded'"
check "qdrant collection (bge-m3)" bash -c "curl -fsS localhost:6333/collections | grep -q bge_m3"
check "opensearch event index"  bash -c "curl -fsS 'localhost:9200/_cat/indices?h=index' | grep -q event"
check "kafka consumer groups"   bash -c "docker compose exec -T kafka /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --list | grep -q ."

echo "== consumer walkthrough (scripts/demo.sh)"
if scripts/demo.sh > /tmp/e2e-demo.log 2>&1; then ok "demo.sh"; else bad "demo.sh (see /tmp/e2e-demo.log)"; fi
TOKEN=$(curl -s -X POST "$BASE/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -r .access_token)
check "search not degraded"     bash -c "curl -fsS '$BASE/v1/search?q=energie' -H 'Authorization: Bearer $TOKEN' | jq -e '.degraded == []'"

echo "== object storage round trip (data export job -> S3 -> presigned download)"
REQ=$(curl -fsS -X POST "$BASE/v1/users/me/data-export" -H "Authorization: Bearer $TOKEN" | jq -r .id)
url=""
for _ in $(seq 1 60); do
  url=$(curl -fsS "$BASE/v1/users/me/data-requests" -H "Authorization: Bearer $TOKEN" | jq -r --arg id "$REQ" '.items[] | select(.id==$id) | .download_url // empty')
  [[ -n $url ]] && break; sleep 1
done
if [[ $url == http* ]] && curl -fsS "$url" | jq -e '.format_version == 1' >/dev/null; then ok "export stored in S3 and downloaded via presigned URL"
else bad "export download (download_url=${url:-none})"; fi

echo "== dynamic configuration without restart (scripts/verify-dynamic-config.sh)"
if scripts/verify-dynamic-config.sh > /tmp/e2e-dyn.log 2>&1; then ok "verify-dynamic-config.sh"; else bad "verify-dynamic-config.sh (see /tmp/e2e-dyn.log)"; fi

if [[ ${LOAD_SECONDS:-15} != 0 ]]; then
  echo "== load (${LOAD_SECONDS:-15}s, 16 workers)"
  if go run ./cmd/loadtest -base "$BASE" -c 16 -d "${LOAD_SECONDS:-15}s" -max-p95 "${LOAD_MAX_P95:-2s}" > /tmp/e2e-load.md; then
    ok "loadtest ($(grep '^\*\*Total' /tmp/e2e-load.md | sed 's/\*\*Total:\*\* //'))"
  else bad "loadtest (see /tmp/e2e-load.md)"; fi
fi

echo
echo "== e2e: $pass passed, $fail failed"
(( fail == 0 ))
