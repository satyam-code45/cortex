-- name: InsertEvidence :one
-- Per-run evidence numbering plus per-run dedupe in one statement.
--
-- The conflict clause is a deliberate no-op update rather than DO NOTHING: the
-- caller needs the existing row's seq back so a second tool call that surfaces
-- the same document reuses its citation number, and DO NOTHING returns no row at
-- all. Assigning tool_call_id to itself keeps "the call that first surfaced this"
-- intact.
INSERT INTO evidence (agent_run_id, tool_call_id, seq, source, external_id,
                      title, url, snippet, source_timestamp)
SELECT sqlc.arg(agent_run_id)::uuid,
       sqlc.narg(tool_call_id)::uuid,
       coalesce(max(seq), 0) + 1,
       sqlc.arg(source)::text,
       sqlc.arg(external_id)::text,
       sqlc.narg(title)::text,
       sqlc.narg(url)::text,
       sqlc.narg(snippet)::text,
       sqlc.narg(source_timestamp)::timestamptz
FROM evidence
WHERE agent_run_id = sqlc.arg(agent_run_id)::uuid
ON CONFLICT (agent_run_id, source, external_id)
    DO UPDATE SET tool_call_id = evidence.tool_call_id
RETURNING *;

-- name: ListEvidenceByRun :many
SELECT * FROM evidence
WHERE agent_run_id = $1
ORDER BY seq;
