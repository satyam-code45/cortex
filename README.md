# Cortex

An AI analyst for your organization's project data. Cortex connects to live enterprise sources — Jira, Notion, Gmail — and answers complex questions by autonomously investigating across them: planning what it needs to know, calling the right tools, reasoning across the evidence, and returning answers with citations back to the underlying sources. Every run is fully traceable — each tool call, retrieved document, and reasoning step is recorded and inspectable.

**Status: under active development.**

## Stack

- **Backend:** Go — Chi, pgx/sqlc, River (Postgres-backed job queue), openai-go
- **Data:** PostgreSQL 16 + pgvector — one database for relational data, vector search, and the job queue
- **Frontend:** Next.js + TypeScript + Tailwind

## Quickstart

```bash
cp .env.example .env   # fill in your keys
docker compose up -d
make migrate
make dev
```

Full setup and demo instructions coming as the project lands.
