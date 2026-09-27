-- +goose Up

-- Delegating an issue to Claude Code is a write like any other: proposed,
-- approved by a person, then performed. Only the source is new.
ALTER TABLE agent_actions DROP CONSTRAINT agent_actions_source_check;
ALTER TABLE agent_actions ADD CONSTRAINT agent_actions_source_check
    CHECK (source IN ('gmail', 'jira', 'notion', 'claude_code'));

-- A runner is a process on its owner's machine that performs delegations.
-- The row is a heartbeat: the delegate tool is offered only while it is fresh.
-- repos holds names only; the paths stay on the machine.
CREATE TABLE code_runners (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name           text        NOT NULL,
    repos          text[]      NOT NULL,
    claude_version text,
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

-- +goose Down
DROP TABLE IF EXISTS code_runners;
-- Rows with the new source would violate the old check; they go first.
DELETE FROM agent_actions WHERE source = 'claude_code';
ALTER TABLE agent_actions DROP CONSTRAINT agent_actions_source_check;
ALTER TABLE agent_actions ADD CONSTRAINT agent_actions_source_check
    CHECK (source IN ('gmail', 'jira', 'notion'));
