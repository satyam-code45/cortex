-- name: UpsertUser :one
-- Idempotent by email: returns the existing row when the user already exists.
INSERT INTO users (email)
VALUES ($1)
ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
RETURNING *;
