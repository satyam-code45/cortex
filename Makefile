-include .env
export

DATABASE_URL ?= postgres://cortex:cortex@localhost:5432/cortex?sslmode=disable
TEST_DATABASE_URL ?= postgres://cortex:cortex@localhost:5432/cortex_test?sslmode=disable

.PHONY: dev migrate migrate-test river-migrate river-migrate-test sqlc test check \
	seed seed-plan seed-jira seed-notion seed-notion-replace seed-gmail gmail-auth eval index

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

seed: ## load every fixture set into Jira, Notion and Gmail
	cd backend && go run ./cmd/seed --confirm

seed-plan: ## show what `make seed` would write, without writing it
	cd backend && go run ./cmd/seed

seed-jira: ## Jira only (~370 issues/comments/transitions)
	cd backend && go run ./cmd/seed --target jira --confirm

seed-notion: ## Notion only (plan, roadmap, retro, meeting notes)
	cd backend && go run ./cmd/seed --target notion --confirm

seed-notion-replace: ## Notion only, rewriting pages that already exist from the fixtures
	cd backend && go run ./cmd/seed --target notion --confirm --replace

seed-gmail: ## Gmail only (~16 backdated fixture emails); needs `make gmail-auth` first
	cd backend && go run ./cmd/seed --target gmail --confirm

# One-time, and interactive: the OAuth consent screen needs a human at a browser.
# It leaves .gmail-token.json behind, which the server and the seeder then read.
gmail-auth: ## authorize Gmail once and cache the refresh token
	cd backend && go run ./cmd/gmail-auth

eval: ## run the evaluation suite
	cd backend && go run ./cmd/eval

index: ## trigger reindexing of all sources into the vector store
	curl -s -X POST localhost:$(PORT)/api/admin/index
