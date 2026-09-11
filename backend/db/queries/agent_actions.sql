-- name: InsertAgentAction :one
-- Records a proposal. ON CONFLICT DO NOTHING on the idempotency key rather than
-- an upsert: a colliding insert means this exact action was already proposed for
-- this run — an agent-run job retried after a crash mid-loop re-proposes
-- identically — and the existing row, which may already have been decided, must
-- win. No rows returned is the caller's signal to fetch and reuse it.
INSERT INTO agent_actions (agent_run_id, user_id, source, action, proposed_payload, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING *;

-- name: GetAgentActionByIdempotencyKey :one
SELECT * FROM agent_actions WHERE idempotency_key = $1;

-- name: GetAgentActionForUser :one
-- Ownership is enforced in the query, not in Go: the approve and reject
-- endpoints take a caller-supplied UUID, and this predicate is what stops one
-- user approving another's pending email.
SELECT * FROM agent_actions WHERE id = $1 AND user_id = $2;

-- name: ApproveAgentAction :one
-- pending -> approved, storing the payload that will actually execute.
--
-- The user_id predicate is the ownership guard. The handler also reads the row
-- for the caller first, so this is belt and braces today — but the guarantee
-- "one person cannot decide another person's write" belongs in the statement
-- that performs the decision, not only in the handler that happens to call it.
-- A future caller that forgets the pre-read is then a no-op, not a breach.
--
-- The status guard is the concurrency control: two simultaneous approvals both
-- read a pending row, both issue this UPDATE, and exactly one matches. The
-- loser gets no rows, which the handler reports as "already decided" instead of
-- enqueueing a second execution.
--
-- The TTL guard sits in the same predicate rather than in a prior SELECT for the
-- same reason: checked separately, a row could expire between the check and the
-- write. $4 is the cutoff timestamp — passed in rather than computed from an
-- interval here so the configured TTL stays in one place and tests can pin it.
UPDATE agent_actions
SET status       = 'approved',
    final_payload = $2,
    decided_by   = $3,
    decided_at   = now()
WHERE id = $1
  AND user_id = sqlc.arg(user_id)
  AND status = 'pending'
  AND proposed_at > $4
RETURNING *;

-- name: RejectAgentAction :one
-- pending -> rejected. Terminal for the action; the run continues and the agent
-- is told the reason.
UPDATE agent_actions
SET status        = 'rejected',
    reject_reason = $2,
    decided_by    = $3,
    decided_at    = now()
WHERE id = $1
  AND user_id = sqlc.arg(user_id)
  AND status = 'pending'
RETURNING *;

-- name: BeginExecutingAgentAction :one
-- approved -> executing. This is the compare-and-set that makes execution
-- exactly-once: the execution job claims the row before it touches the upstream
-- API, so a retry of that job — or a duplicate enqueue — finds the row no longer
-- 'approved', matches nothing, and returns without sending anything.
UPDATE agent_actions
SET status = 'executing'
WHERE id = $1
  AND status = 'approved'
RETURNING *;

-- name: FinishAgentAction :one
-- executing -> executed, recording what the upstream system returned.
UPDATE agent_actions
SET status      = 'executed',
    result      = $2,
    executed_at = now()
WHERE id = $1
  AND status = 'executing'
RETURNING *;

-- name: FailAgentAction :one
-- executing -> failed. Terminal: the row is not returned to 'approved' for
-- another attempt, because a failed write may or may not have landed upstream
-- and re-sending on a guess is the one outcome worse than reporting the failure.
UPDATE agent_actions
SET status      = 'failed',
    error       = $2,
    executed_at = now()
WHERE id = $1
  AND status = 'executing'
RETURNING *;

-- name: ExpireAgentAction :one
-- pending -> expired. An expired proposal can never execute.
UPDATE agent_actions
SET status     = 'expired',
    decided_at = now()
WHERE id = $1
  AND status = 'pending'
RETURNING *;

-- name: ListAgentActionsByRun :many
SELECT * FROM agent_actions WHERE agent_run_id = $1 ORDER BY proposed_at, id;

-- name: CountUnsettledActionsByRun :one
-- How many of the run's actions have not reached a terminal state.
--
-- 'approved' and 'executing' count alongside 'pending', and that is the whole
-- point of the query. A run resumes only when every proposal is genuinely
-- finished — counting only 'pending' would let a run resume the instant the last
-- decision was made, while an approved email was still being sent, and the agent
-- would report "approved, outcome unknown" instead of "sent, here is the message
-- id". Waiting the extra second buys a truthful answer.
SELECT count(*) FROM agent_actions
WHERE agent_run_id = $1
  AND status IN ('pending', 'approved', 'executing');

-- name: GetAgentAction :one
-- By id alone, for the execution job: it runs on behalf of the row's own owner
-- rather than a request, so there is no caller identity to check it against.
-- Every endpoint a user can reach uses GetAgentActionForUser instead.
SELECT * FROM agent_actions WHERE id = $1;

-- name: FailInterruptedAgentAction :one
-- executing -> failed, for a write whose worker died mid-attempt.
--
-- Distinct from FailAgentAction only in intent, and worth its own name: this one
-- records that the outcome is UNKNOWN rather than that the call returned an
-- error. The write may well have landed upstream, so it must never be retried,
-- and the person who approved it needs to be told exactly that.
UPDATE agent_actions
SET status      = 'failed',
    error       = $2,
    executed_at = now()
WHERE id = $1
  AND status = 'executing'
RETURNING *;

-- name: ListAgentActionsForUser :many
-- The audit view: every action across every run, newest first.
SELECT * FROM agent_actions
WHERE user_id = $1
ORDER BY proposed_at DESC, id
LIMIT $2;

-- name: CountExecutedActionsSince :one
-- The per-user hourly write limit. Counted on rows that reached (or are
-- reaching) the upstream system — 'executing' is included deliberately, so a
-- burst of in-flight writes cannot slip past a count that only sees finished
-- ones.
--
-- The window is anchored on when the write went OUT, not on when it was
-- proposed. A proposal may sit pending for the whole ACTION_TTL (24h by
-- default), so counting by proposed_at would leave the ceiling bypassable:
-- approve a day's worth of aged proposals and every one of them executes within
-- a minute while counting as zero against the hour. What the limit exists to
-- bound is how much mail actually leaves the account, so that is what it counts.
--
-- An 'executing' row has no executed_at yet and falls back to decided_at:
-- approval is the moment execution is set in motion, which keeps in-flight
-- writes inside the window without letting a row stuck mid-execution hold the
-- ceiling down forever.
--
-- sqlc.arg(since) is a timestamp rather than a hardcoded interval so tests can
-- pin the window, matching the run limit's query.
SELECT count(*) FROM agent_actions
WHERE user_id = $1
  AND COALESCE(executed_at, decided_at) > sqlc.arg(since)
  AND status IN ('executing', 'executed');

-- name: ListExpiredPendingActions :many
-- The expiry sweep's input: pending rows past the TTL cutoff ($1).
SELECT * FROM agent_actions
WHERE status = 'pending' AND proposed_at <= $1
ORDER BY proposed_at
LIMIT $2;

-- name: SetUserConnectionWrites :execrows
-- Enabling or disabling writes for one source. execrows, not exec: zero rows
-- means the connection does not exist, which the handler answers with a 404
-- rather than a silent success.
UPDATE user_connections
SET writes_enabled = $3, updated_at = now()
WHERE user_id = $1 AND source = $2;
