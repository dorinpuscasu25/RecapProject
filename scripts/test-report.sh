#!/usr/bin/env bash
# Runs the full test suite and prints a clear pass/fail summary plus the environment-dependent
# integrations that were or were not exercised (acceptance scenario 14).
# Usage: scripts/test-report.sh            (infrastructure tests run if the services are reachable)
#        IT_SKIP_INFRA=1 scripts/test-report.sh
set -uo pipefail
cd "$(dirname "$0")/.."
out=$(mktemp -d)

reach() { (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && exec 3>&- ; }
if [[ -z ${IT_SKIP_INFRA:-} ]]; then
  reach localhost 9092 && export IT_KAFKA_BROKERS=${IT_KAFKA_BROKERS:-localhost:9092}
  reach localhost 6333 && export IT_QDRANT_URL=${IT_QDRANT_URL:-http://localhost:6333}
  reach localhost 9200 && export IT_OPENSEARCH_URL=${IT_OPENSEARCH_URL:-http://localhost:9200}
fi

echo "== go vet"; go vet ./... && echo "vet: ok"
echo "== go test ./... (json → $out/test.json)"
go test -count=1 -json ./... > "$out/test.json" 2> "$out/stderr.txt"
status=$?

jq -r 'select(.Test != null and (.Test | contains("/") | not) and (.Action=="pass" or .Action=="fail" or .Action=="skip"))
       | "\(.Action)\t\(.Package | sub(".*/internal/";""))\t\(.Test)"' "$out/test.json" > "$out/results.tsv" || true   # top-level tests only
pass=$(grep -c "^pass" "$out/results.tsv"); fail=$(grep -c "^fail" "$out/results.tsv"); skip=$(grep -c "^skip" "$out/results.tsv")
echo
echo "== summary: $pass passed, $fail failed, $skip skipped"
if (( fail > 0 )); then echo "-- failed:"; grep "^fail" "$out/results.tsv" | cut -f2,3 | sed 's/^/   /'; fi
if (( skip > 0 )); then echo "-- skipped:"; grep "^skip" "$out/results.tsv" | cut -f2,3 | sed 's/^/   /'; fi

echo
echo "== environment-dependent integrations"
row() { printf "   %-44s %s\n" "$1" "$2"; }
row "PostgreSQL (required for integration tests)" "$(reach localhost 5432 && echo exercised || echo 'NOT reachable')"
row "Kafka redelivery (TestInfra…)" "$([[ -n ${IT_KAFKA_BROKERS:-} ]] && echo exercised || echo 'not run: set IT_KAFKA_BROKERS')"
row "Qdrant vectors (TestInfra…)" "$([[ -n ${IT_QDRANT_URL:-} ]] && echo exercised || echo 'not run: set IT_QDRANT_URL')"
row "OpenSearch lexical (TestInfra…)" "$([[ -n ${IT_OPENSEARCH_URL:-} ]] && echo exercised || echo 'not run: set IT_OPENSEARCH_URL')"
row "Remote LLMs (OpenAI/Anthropic/Gemini)" "wire contracts only (httptest); no live calls"
row "TTS provider" "contract + fake provider; no live synthesis"
row "APNs / FCM push" "contract tests (httptest); no live delivery"
row "SMTP delivery" "dev sink in tests; SMTP via Mailpit manually"
row "S3 / MinIO storage" "filesystem driver in tests"
row "Redis rate limits and cache" "in-memory equivalents in tests"
row "ClickHouse analytics / Langfuse" "contract tests (httptest)"
echo
echo "raw output: $out"
exit $status
