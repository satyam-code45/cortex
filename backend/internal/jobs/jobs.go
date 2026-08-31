// Package jobs holds the River job definitions, their workers, and the client
// that runs them.
//
// River is a job queue built on Postgres, which is why it is here at all: an
// agent run takes tens of seconds and a dozen paid LLM calls, so it cannot live
// inside an HTTP request. The usual answer is Redis plus a separate worker
// process; River lets the queue be the database we already have,
// which keeps the deployment to one binary and one datastore.
//
// The property that matters most is transactional enqueue: River's InsertTx
// writes the job row in *our* transaction. So the user's message, the agent_run
// row, and the job that will execute it either all land or none do. There is no
// window in which a run exists with no job to execute it, and none in which a
// job references a run that was never committed.
package jobs

import (
	"github.com/google/uuid"
)

// AgentRunQueue is the River queue agent runs are enqueued on. Giving them a
// named queue rather than the default keeps their concurrency independent of the
// indexing and eval jobs that arrive on later days.
const AgentRunQueue = "agent_runs"

// AgentRunArgs identifies the run a job should execute.
//
// The payload is deliberately just an ID: the run's query, conversation, and
// history are already rows in Postgres, and duplicating them into the job would
// create two sources of truth that can disagree after a retry.
type AgentRunArgs struct {
	RunID uuid.UUID `json:"run_id"`
}

// Kind is River's stable identifier for this job type. It is persisted in
// river_job rows, so renaming it would orphan every queued job.
func (AgentRunArgs) Kind() string { return "agent_run" }
