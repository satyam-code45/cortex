-- +goose Up
-- Links an assistant message to the run that produced it, so a re-opened
-- conversation can resolve its [n] citation markers through
-- GET /api/runs/{id}/trace. Nullable: user messages have no run, and rows from
-- before this migration stay null (their markers render as plain text).
-- ON DELETE SET NULL rather than CASCADE: deleting a run's bookkeeping must not
-- delete the conversation's visible history.
ALTER TABLE messages
    ADD COLUMN agent_run_id uuid REFERENCES agent_runs (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE messages
    DROP COLUMN agent_run_id;
