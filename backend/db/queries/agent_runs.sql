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

-- name: GetAgentRunForUser :one
-- Ownership is enforced in the query, not in Go: GET /api/runs/{id} takes a
-- caller-supplied UUID, and joining through conversations is what stops it
-- being an IDOR the moment a second user exists.
SELECT r.* FROM agent_runs r
         JOIN conversations c ON c.id = r.conversation_id
WHERE r.id = $1 AND c.user_id = $2;

-- name: GetAgentRunOwner :one
-- Runs don't carry a user_id; ownership lives on the conversation. The worker
-- resolves the owner to build the run's LLM provider from their stored key.
SELECT c.user_id FROM agent_runs r
         JOIN conversations c ON c.id = r.conversation_id
WHERE r.id = $1;

-- name: CountUserRunsSince :one
-- Per-user rate limit (REQ-7.3): the shared cost of a run is Satyam's upstream
-- API quotas even when the LLM spend is the user's. $2 is a timestamp rather
-- than a hardcoded interval so tests can pin the window.
SELECT count(*) FROM agent_runs r
         JOIN conversations c ON c.id = r.conversation_id
WHERE c.user_id = $1 AND r.created_at > $2;

-- name: StartAgentRun :one
-- Claims a queued run. The status guard makes the transition idempotent for a
-- River job that is retried after a worker crash (still 'running'), while
-- refusing to restart a run that already reached a terminal state — returning
-- no rows is the signal to skip.
UPDATE agent_runs
SET status = 'running'
WHERE id = $1
  AND status IN ('pending', 'running')
RETURNING *;
