-include .env
export

DATABASE_URL ?= postgres://cortex:cortex@localhost:5432/cortex?sslmode=disable
TEST_DATABASE_URL ?= postgres://cortex:cortex@localhost:5432/cortex_test?sslmode=disable

.PHONY: dev migrate migrate-test river-migrate river-migrate-test sqlc test check seed seed-plan eval index

dev: ## run the API server (includes queue workers)
	cd backend && go run ./cmd/server

# Two migration systems by design: goose owns application schema, River owns its
# own (its DDL is not safely vendorable into goose -- see cmd/rivermigrate).
migrate: river-migrate ## apply database migrations (application + queue)
	cd backend && goose -dir db/migrations postgres "$(DATABASE_URL)" up

migrate-test: river-migrate-test ## apply migrations to the integration-test database
	cd backend && goose -dir db/migrations postgres "$(TEST_DATABASE_URL)" up

river-migrate: ## apply River's queue schema
	cd backend && go run ./cmd/rivermigrate --database-url "$(DATABASE_URL)"

river-migrate-test: ## apply River's queue schema to the integration-test database
	cd backend && go run ./cmd/rivermigrate --database-url "$(TEST_DATABASE_URL)"

sqlc: ## regenerate type-safe query code
	cd backend && sqlc generate

test: migrate-test ## run all backend tests
	cd backend && go test ./...

check: migrate-test ## full gate: build + vet + test
	cd backend && test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	cd backend && go build ./... && go vet ./... && go test ./...

seed: ## load synthetic company data into Jira (writes ~370 issues/comments/transitions)
	cd backend && go run ./cmd/seed --confirm

seed-plan: ## show what `make seed` would write, without writing it
	cd backend && go run ./cmd/seed

eval: ## run the evaluation suite
	cd backend && go run ./cmd/eval

index: ## trigger reindexing of all sources into the vector store
	curl -s -X POST localhost:$(PORT)/api/admin/index
