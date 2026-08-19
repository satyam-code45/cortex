-include .env
export

DATABASE_URL ?= postgres://cortex:cortex@localhost:5432/cortex?sslmode=disable

.PHONY: dev migrate sqlc test check seed eval index

dev: ## run the API server (includes queue workers)
	cd backend && go run ./cmd/server

migrate: ## apply database migrations
	cd backend && goose -dir db/migrations postgres "$(DATABASE_URL)" up

sqlc: ## regenerate type-safe query code
	cd backend && sqlc generate

test: ## run all backend tests
	cd backend && go test ./...

check: ## full gate: build + vet + test
	cd backend && go build ./... && go vet ./... && go test ./...

seed: ## load synthetic company data into Jira/Notion/Gmail
	cd backend && go run ./cmd/seed

eval: ## run the evaluation suite
	cd backend && go run ./cmd/eval

index: ## trigger reindexing of all sources into the vector store
	curl -s -X POST localhost:$(PORT)/api/admin/index
