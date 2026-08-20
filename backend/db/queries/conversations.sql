-- name: CreateConversation :one
INSERT INTO conversations (user_id, title)
VALUES ($1, $2)
RETURNING *;

-- name: GetConversation :one
-- Scoped by user_id on purpose: keeping the ownership predicate in the query
-- means a future handler cannot forget the Go-side check and open an IDOR.
SELECT * FROM conversations
WHERE id = $1 AND user_id = $2;

-- name: ListConversationsByUser :many
SELECT * FROM conversations
WHERE user_id = $1
ORDER BY updated_at DESC;

-- name: TouchConversation :exec
UPDATE conversations
SET updated_at = now()
WHERE id = $1;
