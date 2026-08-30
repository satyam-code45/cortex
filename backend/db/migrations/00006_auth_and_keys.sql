-- +goose Up

-- Google identity lands on the existing users table: email stays the natural
-- key (UpsertGoogleUser conflicts on it), google_sub links the Google account.
-- NULL google_sub is legitimate — the dev, eval, and bearer users never log in
-- through Google — so the uniqueness guarantee is a partial index.
ALTER TABLE users
    ADD COLUMN name       text,
    ADD COLUMN avatar_url text,
    ADD COLUMN google_sub text;

CREATE UNIQUE INDEX users_google_sub_key
    ON users (google_sub) WHERE google_sub IS NOT NULL;

-- Server-side sessions: revocable, restart-proof, one database. The cookie
-- carries a random 256-bit token; only its SHA-256 is stored, so a database
-- leak does not mint valid cookies.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Bring-your-own-key: one LLM key per user, AES-256-GCM ciphertext only.
-- 'gemini' is admitted by the schema so adding the provider later is a new
-- implementation, not a migration; the API rejects it until one lands.
CREATE TABLE user_llm_keys (
    user_id        uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    provider       text        NOT NULL CHECK (provider IN ('openai', 'gemini')),
    key_ciphertext bytea       NOT NULL,
    key_last4      text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS user_llm_keys;
DROP TABLE IF EXISTS sessions;
DROP INDEX IF EXISTS users_google_sub_key;
ALTER TABLE users
    DROP COLUMN IF EXISTS google_sub,
    DROP COLUMN IF EXISTS avatar_url,
    DROP COLUMN IF EXISTS name;
