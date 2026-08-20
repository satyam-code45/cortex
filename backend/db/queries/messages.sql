-- name: InsertMessage :one
INSERT INTO messages (conversation_id, role, content)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListMessagesByConversation :many
SELECT * FROM messages
WHERE conversation_id = $1
ORDER BY created_at, id;
