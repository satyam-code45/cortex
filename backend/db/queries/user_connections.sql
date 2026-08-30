-- name: UpsertUserConnection :one
-- One connection per (user, source): reconnecting replaces the credential and
-- clears any error state in the same write.
INSERT INTO user_connections (user_id, source, credentials_ciphertext, identity)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, source) DO UPDATE
    SET credentials_ciphertext = excluded.credentials_ciphertext,
        identity               = excluded.identity,
        status                 = 'active',
        last_error             = NULL,
        updated_at             = now()
RETURNING *;

-- name: GetUserConnection :one
SELECT * FROM user_connections WHERE user_id = $1 AND source = $2;

-- name: ListUserConnections :many
SELECT * FROM user_connections WHERE user_id = $1 ORDER BY source;

-- name: DeleteUserConnection :execrows
DELETE FROM user_connections WHERE user_id = $1 AND source = $2;

-- name: SetUserConnectionError :exec
UPDATE user_connections
SET status = 'error', last_error = $3, updated_at = now()
WHERE user_id = $1 AND source = $2;

-- name: SetUserDemoWorkspace :exec
UPDATE users SET use_demo_workspace = $2 WHERE id = $1;

-- name: GetUserDemoWorkspace :one
SELECT use_demo_workspace FROM users WHERE id = $1;
