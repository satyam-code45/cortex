-- name: UpsertUserLLMKey :one
-- One key per user: replacing the key or switching provider is the same write.
INSERT INTO user_llm_keys (user_id, provider, key_ciphertext, key_last4)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE
    SET provider       = excluded.provider,
        key_ciphertext = excluded.key_ciphertext,
        key_last4      = excluded.key_last4,
        updated_at     = now()
RETURNING *;

-- name: GetUserLLMKey :one
SELECT * FROM user_llm_keys WHERE user_id = $1;

-- name: DeleteUserLLMKey :execrows
DELETE FROM user_llm_keys WHERE user_id = $1;
