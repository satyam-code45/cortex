package jobs

import (
	"time"

	"github.com/google/uuid"
)

// The approval-gate jobs.
//
// Three of them, and the split is the design rather than an accident of
// packaging:
//
//   - ExecuteWriteArgs performs one approved write. It exists as a job, rather
//     than as work done inside the approve request, because a write must happen
//     exactly once even though the thing that triggers it can be retried. A job
//     row plus a compare-and-set on the action's status gives that; an HTTP
//     handler doing the send inline gives a duplicate email every time somebody
//     double-clicks.
//   - ResumeRunArgs continues a paused agent run. Separate from AgentRunArgs so
//     that River's retry accounting, the claim guard, and the queue's
//     concurrency stay unambiguous — a resume is a different operation on the
//     same run, not a second attempt at the first one.
//   - ExpireActionsArgs sweeps proposals nobody decided in time. Periodic,
//     because the alternative is a run waiting forever on an answer that is
//     never coming.

// WriteActionQueue is the River queue write executions and expiry sweeps run on.
//
// Separate from the agent-run queue on purpose. An agent run occupies its worker
// for tens of seconds and a dozen paid LLM calls; a write is one HTTP request.
// Sharing a queue would leave an approved email queued behind somebody else's
// investigation, which is the one place in this system where latency is felt by
// a person who is watching.
const WriteActionQueue = "write_actions"

// ExecuteWriteArgs identifies the approved action to perform.
//
// Just an id, like AgentRunArgs: the payload the human approved is a column on
// the row, and copying it into the job would create a second version of the one
// thing that must not have two versions.
type ExecuteWriteArgs struct {
	ActionID uuid.UUID `json:"action_id"`
}

// Kind is River's stable identifier for this job type. Persisted in river_job
// rows, so renaming it would orphan every queued write.
func (ExecuteWriteArgs) Kind() string { return "execute_write" }

// ResumeRunArgs identifies the paused run to continue.
type ResumeRunArgs struct {
	RunID uuid.UUID `json:"run_id"`
}

// Kind is River's stable identifier for this job type.
func (ResumeRunArgs) Kind() string { return "resume_run" }

// ExpireActionsArgs carries nothing: the sweep's input is the table.
type ExpireActionsArgs struct{}

// Kind is River's stable identifier for this job type.
func (ExpireActionsArgs) Kind() string { return "expire_actions" }

// expireInterval is how often the expiry sweep runs.
//
// Hourly, against a time limit measured in hours. The sweep is not what makes an
// expired proposal unexecutable — the approve path refuses a stale row on its
// own, checked in the same predicate as the status — so this exists only to
// unstick runs and to stop the pending set growing without bound. Running it
// every minute would buy nothing but wake-ups.
const expireInterval = time.Hour

// expireBatch caps one sweep. A backlog is worked through over successive hours
// rather than in one transaction holding thousands of rows.
const expireBatch = 200
