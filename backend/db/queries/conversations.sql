-- name: CreateConversation :one
INSERT INTO conversations (user_id, title)
VALUES ($1, $2)
RETURNING *;

-- name: GetConversation :one
SELECT * FROM conversations
WHERE id = $1;

-- name: ListConversationsByUser :many
SELECT * FROM conversations
WHERE user_id = $1
ORDER BY updated_at DESC;

-- name: TouchConversation :exec
UPDATE conversations
SET updated_at = now()
WHERE id = $1;
