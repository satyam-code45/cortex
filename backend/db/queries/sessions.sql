-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionUserByTokenHash :one
-- The middleware's single lookup: session validity and the user in one query.
-- Expiry is enforced here, not in Go, so a revoked-or-expired session and an
-- unknown token are indistinguishable to the caller (both are no-rows).
SELECT s.id AS session_id, s.last_seen_at, s.expires_at,
       u.id AS user_id, u.email, u.name, u.avatar_url
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE id = $1;

-- name: DeleteSessionByTokenHash :execrows
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteExpiredSessions :execrows
-- Opportunistic housekeeping, called on login; there is no background sweeper.
DELETE FROM sessions WHERE expires_at <= now();
