-- name: InsertMessage :one
-- agent_run_id is null for user messages; the orchestrator sets it on the
-- assistant answer so the frontend can reach the run's trace from the message.
INSERT INTO messages (conversation_id, role, content, agent_run_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListMessagesByConversation :many
SELECT * FROM messages
WHERE conversation_id = $1
ORDER BY created_at, id;
