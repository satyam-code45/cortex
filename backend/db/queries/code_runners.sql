-- name: UpsertCodeRunnerHeartbeat :one
INSERT INTO code_runners (user_id, name, repos, claude_version, last_seen_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id, name) DO UPDATE
SET repos          = EXCLUDED.repos,
    claude_version = EXCLUDED.claude_version,
    last_seen_at   = now()
RETURNING *;

-- name: GetOnlineCodeRunnerForUser :one
-- The freshest runner seen since $2. Several runners per user are not
-- supported; if two exist the most recent one wins.
SELECT * FROM code_runners
WHERE user_id = $1
  AND last_seen_at >= $2
ORDER BY last_seen_at DESC
LIMIT 1;

-- name: DeleteCodeRunner :exec
DELETE FROM code_runners WHERE user_id = $1 AND name = $2;
