-- +goose Up

-- Per-user source connections: each row is one user's credential for
-- one source, AES-256-GCM ciphertext only — same envelope as user_llm_keys.
-- identity holds display facts (site URL, account name, mailbox address),
-- never secrets: it is what the connections page may show back.
-- status='error' means the stored credential stopped working (e.g. a revoked
-- Gmail refresh token); the row is kept so the UI can say "reconnect" and the
-- user's runs stay in user mode — an errored source is dropped from the run's
-- tool registry, never silently replaced by demo data.
CREATE TABLE user_connections (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source                 text        NOT NULL CHECK (source IN ('jira', 'notion', 'gmail')),
    credentials_ciphertext bytea       NOT NULL,
    identity               jsonb       NOT NULL DEFAULT '{}',
    status                 text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'error')),
    last_error             text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, source)
);

-- No separate user_id index: UNIQUE (user_id, source) already provides a
-- composite index led by user_id, which serves both the per-user list
-- (including its ORDER BY source) and the single-row lookup.

-- The "Use demo workspace" toggle: with it on (or with zero connections) the
-- user's runs use the full demo registry; demo and user sources are never
-- mixed in one run.
ALTER TABLE users
    ADD COLUMN use_demo_workspace boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users
    DROP COLUMN IF EXISTS use_demo_workspace;
DROP TABLE IF EXISTS user_connections;
