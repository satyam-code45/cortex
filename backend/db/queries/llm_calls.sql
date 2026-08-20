-- name: InsertLLMCall :one
INSERT INTO llm_calls (agent_run_id, purpose, model, input_tokens, output_tokens, latency_ms)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;
