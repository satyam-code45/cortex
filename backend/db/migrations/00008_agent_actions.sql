-- +goose Up

-- Every write Cortex can perform is a row here before it is an effect anywhere
-- else. The agent proposes; a human decides; a job executes. This table is the
-- only thing that connects those three, and it is also the audit trail — which
-- is why the proposal and the decision are stored as separate columns rather
-- than one payload being overwritten. Comparing proposed_payload with
-- final_payload is how anyone later answers "did a human change this before it
-- went out?", and an overwrite would destroy exactly that answer.
--
-- status is a one-way lifecycle:
--     pending -> approved -> executing -> executed | failed
--     pending -> rejected | expired
-- Every transition is a conditional UPDATE guarded on the expected current
-- status, so two concurrent approvals cannot both win: the second matches no
-- row. That compare-and-set is the whole idempotency story — River retries
-- jobs, and a retried execution must not send a second email.
CREATE TABLE agent_actions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id     uuid        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    -- Denormalized from the run's conversation on purpose. Ownership of a run
    -- lives on conversations.user_id, but this row outlives the question that
    -- produced it as an audit record, and every ownership check on it (the
    -- approve endpoint, the per-user rate limit, the audit list) would
    -- otherwise be a two-join reach through agent_runs. It is also the column
    -- that makes "who could have sent this?" answerable directly.
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source           text        NOT NULL CHECK (source IN ('gmail', 'jira', 'notion')),
    -- The specific capability, e.g. 'gmail.send', 'jira.create_issue'. Not
    -- constrained by a CHECK: the set grows with every write tool, and a
    -- migration per tool buys nothing here — the executor registry in Go is
    -- what actually decides whether an action name can be performed, and an
    -- unknown name there is a failed action, not a corrupted row.
    action           text        NOT NULL,
    -- Exactly what the agent asked for, verbatim. The approval UI renders this
    -- field by field rather than summarizing it: a human approving a summary of
    -- an email is not approving the email.
    proposed_payload jsonb       NOT NULL,
    -- What the human approved. NULL until then, and may differ from the
    -- proposal — editing before approving is a first-class outcome, and this
    -- column is what executes.
    final_payload    jsonb,
    status           text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'executing', 'executed', 'failed', 'expired')),
    -- Derived, not random: a hash over the run, the action and the canonical
    -- payload. If the agent-run job is retried after a crash mid-loop, the
    -- re-proposal hashes to the same key and the insert collides instead of
    -- queueing a second identical email for approval.
    idempotency_key  text        NOT NULL UNIQUE,
    reject_reason    text,
    -- What the upstream system returned: a message id, an issue key, a page
    -- id. It is what the agent is told on resume and what the audit view shows.
    result           jsonb,
    error            text,
    proposed_at      timestamptz NOT NULL DEFAULT now(),
    decided_at       timestamptz,
    executed_at      timestamptz,
    decided_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    -- Redundant beside the primary key, and kept anyway: it is the constraint a
    -- later composite foreign key would need, and it documents that an action
    -- belongs to exactly one run.
    UNIQUE (agent_run_id, id)
);

-- The run's pending set, read on every resume decision ("is anything still
-- awaiting a human?") and by the chat UI when it reopens a paused run.
CREATE INDEX agent_actions_run_status_idx ON agent_actions (agent_run_id, status);

-- Serves both per-user reads: the audit list (newest first) and the executed
-- count behind the hourly write limit.
CREATE INDEX agent_actions_user_proposed_at_idx ON agent_actions (user_id, proposed_at DESC);

-- The expiry sweep: pending rows old enough to have timed out. Partial, because
-- pending is a transient state and the terminal rows it excludes are the vast
-- majority of the table over time.
CREATE INDEX agent_actions_pending_proposed_at_idx
    ON agent_actions (proposed_at) WHERE status = 'pending';

-- Writes are off until a user turns them on for a specific source, and the
-- default is what makes that meaningful: an existing read connection cannot
-- start proposing writes because a later migration ran. A run's registry omits
-- the write tools entirely unless this is true, so the model cannot even name
-- one.
ALTER TABLE user_connections
    ADD COLUMN writes_enabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE user_connections
    DROP COLUMN IF EXISTS writes_enabled;
DROP TABLE IF EXISTS agent_actions;
