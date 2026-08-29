-- name: InsertEvalRun :one
INSERT INTO eval_runs (git_sha, started_at, metrics)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListEvalRuns :many
SELECT * FROM eval_runs
ORDER BY started_at DESC;
