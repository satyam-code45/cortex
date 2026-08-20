-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email      text        NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE conversations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title      text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX conversations_user_id_updated_at_idx
    ON conversations (user_id, updated_at DESC);

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    role            text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content         text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX messages_conversation_id_created_at_idx
    ON messages (conversation_id, created_at);

CREATE TABLE agent_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    query           text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'awaiting_approval', 'completed', 'failed')),
    model           text,
    answer          text,
    error           text,
    latency_ms      int,
    input_tokens    int,
    output_tokens   int,
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);

CREATE INDEX agent_runs_conversation_id_created_at_idx
    ON agent_runs (conversation_id, created_at DESC);

CREATE TABLE llm_calls (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id  uuid        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    purpose       text        NOT NULL,
    model         text        NOT NULL,
    input_tokens  int,
    output_tokens int,
    latency_ms    int,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX llm_calls_agent_run_id_created_at_idx
    ON llm_calls (agent_run_id, created_at);

-- Append-only event log for an agent run. Drives the SSE stream and the trace
-- panel, so (agent_run_id, seq) must be gap-free and unique per run.
CREATE TABLE run_events (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id uuid        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    seq          int         NOT NULL,
    type         text        NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_run_id, seq)
);

-- +goose Down
DROP TABLE IF EXISTS run_events;
DROP TABLE IF EXISTS llm_calls;
DROP TABLE IF EXISTS agent_runs;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversations;
DROP TABLE IF EXISTS users;
