-- name: InsertAgentRun :one
INSERT INTO agent_runs (conversation_id, query, status, model)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CompleteAgentRun :one
UPDATE agent_runs
SET status        = 'completed',
    answer        = $2,
    latency_ms    = $3,
    input_tokens  = $4,
    output_tokens = $5,
    finished_at   = now()
WHERE id = $1
RETURNING *;

-- name: FailAgentRun :one
UPDATE agent_runs
SET status      = 'failed',
    error       = $2,
    latency_ms  = $3,
    finished_at = now()
WHERE id = $1
RETURNING *;
