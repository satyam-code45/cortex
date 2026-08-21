-- name: InsertRunEvent :one
-- The sequence is computed inside the statement rather than tracked by the
-- caller: a resumed or retried run has no in-process memory of how far the
-- transcript already got, and guessing would collide with the
-- unique (agent_run_id, seq) constraint and abort the whole transaction.
INSERT INTO run_events (agent_run_id, seq, type, payload)
SELECT $1, coalesce(max(seq), 0) + 1, $2, $3
FROM run_events
WHERE agent_run_id = $1
RETURNING *;

-- name: ListRunEventsByRun :many
-- Ordered by seq, not created_at: two events written inside one transaction can
-- share a timestamp, and the transcript's order is the thing being replayed.
SELECT * FROM run_events
WHERE agent_run_id = $1
ORDER BY seq;
