-include .env
export

# PORT has a default here as well as in internal/config: `make index` curls the
# running server, and an unset PORT would otherwise build the URL "localhost:/...".
PORT ?= 8080
DATABASE_URL ?= postgres://cortex:cortex@localhost:5432/cortex?sslmode=disable
TEST_DATABASE_URL ?= postgres://cortex:cortex@localhost:5432/cortex_test?sslmode=disable

.PHONY: dev web migrate migrate-test river-migrate river-migrate-test sqlc test check check-web \
	seed seed-plan seed-jira seed-notion seed-notion-replace seed-gmail gmail-auth eval index

dev: ## run the API server (includes queue workers)
	cd backend && go run ./cmd/server

# PORT is overridden explicitly: this Makefile exports .env, where PORT is the
# BACKEND's port (8080) — and `next dev` also honours PORT, so without the
# override the frontend tries to bind the backend's address and dies with
# EADDRINUSE.
WEB_PORT ?= 3000
web: ## run the frontend dev server
	cd frontend && PORT=$(WEB_PORT) npm run dev

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

check: migrate-test check-web ## full gate: build + vet + test (backend + frontend)
	cd backend && test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	cd backend && go build ./... && go vet ./... && go test ./...

check-web: ## frontend gate: typecheck + tests
	cd frontend && npx tsc --noEmit && npx vitest run

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
# Dev-only now that users connect their own Gmail: this authorizes the DEMO
# workspace mailbox. Users connect theirs with the button on the Connections page.
gmail-auth: ## authorize the demo-workspace Gmail once and cache the refresh token (dev-only)
	cd backend && go run ./cmd/gmail-auth

# EVAL_FLAGS passes through to the runner, e.g.
#   make eval EVAL_FLAGS='-case atlas-blocked-issues'
#   make eval EVAL_FLAGS='-concurrency 2'
eval: ## run the evaluation suite
	cd backend && go run ./cmd/eval $(EVAL_FLAGS)

# The Content-Type header is required, not decorative: the endpoint rejects a
# request without it, which is what forces a browser to preflight it and stops
# any page you happen to be visiting from queueing crawls at your local server.
index: ## trigger reindexing of all sources into the vector store
	curl -sS -f -X POST -H 'Content-Type: application/json' -H "Authorization: Bearer $(AUTH_API_TOKEN)" -d '{}' localhost:$(PORT)/api/admin/index
