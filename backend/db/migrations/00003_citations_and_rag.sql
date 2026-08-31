-- +goose Up

-- Numbered 003, not 002: River owns migration 002 (its own queue schema, applied
-- by cmd/rivermigrate). goose tracks its own sequence, so the gap is cosmetic —
-- it keeps the two systems' version numbers from reading as the same series.

-- One row per tool invocation the agent made. The full arguments and the
-- observation also live in run_events, which is the replay log; this table is
-- the queryable projection of the same facts ("which tools fail most?",
-- "what is p95 Jira latency?"), and it is what the trace endpoint reads for its
-- normalized tool list.
CREATE TABLE tool_calls (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id   uuid        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    seq            int         NOT NULL,
    tool_name      text        NOT NULL,
    arguments      jsonb       NOT NULL DEFAULT '{}',
    result_summary text,
    evidence_count int         NOT NULL DEFAULT 0,
    latency_ms     int,
    status         text        NOT NULL CHECK (status IN ('ok', 'error')),
    error          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_run_id, seq)
);

CREATE INDEX tool_calls_agent_run_id_seq_idx ON tool_calls (agent_run_id, seq);

-- Every citable source a run touched.
--
-- seq is the citation number the model is shown and cites by: evidence 1..N per
-- run, assigned in the order the run discovered them. It has to be stored rather
-- than derived from row order, because the primary key is a uuid and "the third
-- row" is not a stable concept.
--
-- The (agent_run_id, source, external_id) constraint is the per-run dedupe: two
-- tool calls that both surface ATLAS-145 must produce one citation number, not
-- two the model then uses interchangeably. tool_call_id therefore records the
-- call that FIRST surfaced the item.
CREATE TABLE evidence (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id     uuid        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    tool_call_id     uuid        REFERENCES tool_calls (id) ON DELETE SET NULL,
    seq              int         NOT NULL,
    source           text        NOT NULL,
    external_id      text        NOT NULL,
    title            text,
    url              text,
    snippet          text,
    source_timestamp timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_run_id, seq),
    UNIQUE (agent_run_id, source, external_id)
);

-- One row per inline [n] marker in the stored answer. claim_text is the sentence
-- the marker backs, so a reader can see WHAT the source was cited for rather
-- than only that it was cited somewhere.
CREATE TABLE citations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id uuid        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    evidence_id  uuid        NOT NULL REFERENCES evidence (id) ON DELETE CASCADE,
    marker       text        NOT NULL,
    claim_text   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_run_id, marker)
);

-- The indexed half of retrieval: live tools answer "what is the
-- status now", this answers "what was written about it".
--
-- content_hash is the whole idempotence story. Re-indexing is meant to be run
-- often and cost nothing when nothing changed, so an unchanged hash skips
-- chunking and — the part that actually costs money — embedding.
CREATE TABLE documents (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source           text        NOT NULL,
    external_id      text        NOT NULL,
    title            text        NOT NULL DEFAULT '',
    url              text        NOT NULL DEFAULT '',
    content          text        NOT NULL,
    metadata         jsonb       NOT NULL DEFAULT '{}',
    content_hash     text        NOT NULL,
    source_timestamp timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, external_id)
);

CREATE INDEX documents_source_idx ON documents (source);

CREATE TABLE document_chunks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id uuid          NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    chunk_index int           NOT NULL,
    content     text          NOT NULL,
    embedding   vector(1536)  NOT NULL,
    created_at  timestamptz   NOT NULL DEFAULT now(),
    UNIQUE (document_id, chunk_index)
);

-- HNSW rather than ivfflat. ivfflat builds its list structure from the rows
-- present when the index is created, and this one is created on an empty table
-- by a migration — so it would need a rebuild after the first index run to be
-- worth anything. HNSW is incremental and needs no training pass.
--
-- vector_cosine_ops matches the query operator (<=>). An index built for a
-- different distance is silently ignored by the planner, which reads as "pgvector
-- is slow" rather than as a mistake.
CREATE INDEX document_chunks_embedding_idx
    ON document_chunks USING hnsw (embedding vector_cosine_ops);

-- +goose Down
DROP TABLE IF EXISTS document_chunks;
DROP TABLE IF EXISTS documents;
DROP TABLE IF EXISTS citations;
DROP TABLE IF EXISTS evidence;
DROP TABLE IF EXISTS tool_calls;
