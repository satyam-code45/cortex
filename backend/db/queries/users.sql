-- name: UpsertUser :one
-- Idempotent by email: returns the existing row when the user already exists.
INSERT INTO users (email)
VALUES ($1)
ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
RETURNING *;

-- name: UpsertGoogleUser :one
-- Conflict on email, not google_sub: a pre-existing row (the dev user, say)
-- gains its Google identity on first login instead of duplicating the account.
INSERT INTO users (email, google_sub, name, avatar_url)
VALUES ($1, $2, $3, $4)
ON CONFLICT (email) DO UPDATE
    SET google_sub = EXCLUDED.google_sub,
        name       = EXCLUDED.name,
        avatar_url = EXCLUDED.avatar_url
RETURNING *;
