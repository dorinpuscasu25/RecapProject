# Developer shortcuts. Load configuration first: set -a; source .env; set +a
SHELL := /bin/bash



.PHONY: up down migrate seed fixtures api worker test test-infra lint openapi eval build

up:            ## start local infrastructure
	docker compose up -d
down:
	docker compose down
build:
	go build ./...
migrate:
	go run ./cmd/migrate
seed:
	go run ./cmd/newsctl seed
fixtures:      ## register synthetic sources and run the pipeline (postgres bus)
	go run ./cmd/newsctl ingest-fixtures
api:
	go run ./cmd/api
worker:
	go run ./cmd/worker
lint:
	gofmt -l . | (! grep .) && go vet ./...
test:          ## unit + integration tests (needs PostgreSQL; see docs/testing.md)
	go test ./...
test-infra:    ## Kafka/Qdrant/OpenSearch integration test
	IT_KAFKA_BROKERS=localhost:9092 IT_QDRANT_URL=http://localhost:6333 IT_OPENSEARCH_URL=http://localhost:9200 \
	  go test ./internal/app/ -run Infra -v
openapi:       ## regenerate api/openapi.yaml
	go run ./cmd/openapi-gen
eval:          ## run evaluation suites against the currently routed models
	go run ./cmd/newsctl eval
