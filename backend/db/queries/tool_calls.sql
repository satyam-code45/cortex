-- name: InsertToolCall :one
-- seq is computed inside the statement, exactly as InsertRunEvent does: a run
-- retried after a worker crash has no in-process memory of how many tool calls
-- it already recorded, and guessing would collide with the unique
-- (agent_run_id, seq) constraint.
INSERT INTO tool_calls (agent_run_id, seq, tool_name, arguments, result_summary,
                        evidence_count, latency_ms, status, error)
SELECT sqlc.arg(agent_run_id)::uuid,
       coalesce(max(seq), 0) + 1,
       sqlc.arg(tool_name)::text,
       sqlc.arg(arguments)::jsonb,
       sqlc.narg(result_summary)::text,
       sqlc.arg(evidence_count)::int,
       sqlc.narg(latency_ms)::int,
       sqlc.arg(status)::text,
       sqlc.narg(error)::text
FROM tool_calls
WHERE agent_run_id = sqlc.arg(agent_run_id)::uuid
RETURNING *;

-- name: ListToolCallsByRun :many
SELECT * FROM tool_calls
WHERE agent_run_id = $1
ORDER BY seq;
