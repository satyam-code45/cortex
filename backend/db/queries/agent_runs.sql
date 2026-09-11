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
-- Per-user rate limit: the shared cost of a run is Satyam's upstream
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

-- name: PauseAgentRun :one
-- running -> awaiting_approval. The status guard keeps the transition honest for
-- a run that raced to a terminal state; no rows means there is nothing to pause.
--
-- finished_at stays NULL: the run is not finished, it is waiting. Token totals
-- are stored so a paused run's cost is visible before it resumes, and because a
-- resume rebuilds them from the event log rather than from memory.
UPDATE agent_runs
SET status        = 'awaiting_approval',
    input_tokens  = $2,
    output_tokens = $3
WHERE id = $1
  AND status = 'running'
RETURNING *;

-- name: ResumeAgentRun :one
-- awaiting_approval -> running, claiming a paused run for the resume job.
--
-- 'running' is admitted alongside 'awaiting_approval' for the same reason
-- StartAgentRun admits it: a resume job retried after its worker was killed
-- mid-loop must be able to pick the run back up. Anything terminal matches
-- nothing, so a run that already answered cannot be resumed into a second life.
UPDATE agent_runs
SET status = 'running'
WHERE id = $1
  AND status IN ('awaiting_approval', 'running')
RETURNING *;

-- name: GetAgentRun :one
SELECT * FROM agent_runs WHERE id = $1;

-- name: LockAgentRunForSettlement :one
-- Serializes the settle-and-resume decision for one run.
--
-- Every path that settles an action asks, in the same transaction, "is anything
-- on this run still outstanding?" and enqueues the resume only when the answer
-- is no. Under READ COMMITTED that question is unsafe when two actions on one
-- run are settled concurrently: each transaction sees its own settlement plus
-- the OTHER row in its pre-commit state, so both count one outstanding action
-- and neither enqueues a resume. The run then sits in 'awaiting_approval'
-- forever — the expiry sweep only looks at 'pending' rows, so nothing would
-- ever find it again.
--
-- Taking this lock as the first statement of each settling transaction makes
-- those transactions run one at a time per run, so the second one sees the
-- first's committed settlement and enqueues exactly one resume. It locks the
-- run row rather than using an advisory lock so the lock is released by COMMIT
-- with no separate unlock to leak.
SELECT id FROM agent_runs WHERE id = $1 FOR UPDATE;

-- name: ListResumableStalledRuns :many
-- The liveness backstop: runs paused for a decision that has already been made.
--
-- A run reaches this state only through a bug — the lock above is what prevents
-- it — but "the investigation never answers and nothing in the system can find
-- it" is a bad enough outcome to warrant a cheap sweep that cannot be reasoned
-- wrong. Ordered by id so a batch locks runs in a deterministic order.
SELECT r.id FROM agent_runs r
WHERE r.status = 'awaiting_approval'
  AND NOT EXISTS (
    SELECT 1 FROM agent_actions a
    WHERE a.agent_run_id = r.id
      AND a.status IN ('pending', 'approved', 'executing')
  )
ORDER BY r.id
LIMIT $1;
