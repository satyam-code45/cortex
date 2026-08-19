-- name: InsertRunEvent :one
INSERT INTO run_events (agent_run_id, seq, type, payload)
VALUES ($1, $2, $3, $4)
RETURNING *;
