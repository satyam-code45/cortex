package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	"cortex/internal/agent"
)

// AgentRunWorker executes one queued agent run.
//
// It is intentionally almost empty. All the interesting behaviour lives in
// agent.Orchestrator, which knows nothing about River — so the loop can be
// tested with a scripted fake provider and no queue, no worker, and no job
// table. A worker that contained logic would drag River into every one of those
// tests.
//
// Embedding river.WorkerDefaults supplies the optional hooks (Timeout,
// NextRetry, Middleware) so this type only declares Work.
type AgentRunWorker struct {
	river.WorkerDefaults[AgentRunArgs]

	orchestrator *agent.Orchestrator
	logger       *slog.Logger
}

// NewAgentRunWorker builds the worker.
func NewAgentRunWorker(orchestrator *agent.Orchestrator, logger *slog.Logger) (*AgentRunWorker, error) {
	if orchestrator == nil {
		return nil, errors.New("jobs: orchestrator is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentRunWorker{orchestrator: orchestrator, logger: logger}, nil
}

// Work runs the agent loop for the job's run.
//
// Returning an error tells River to retry. The orchestrator only does that when
// it could not *record* an outcome — an ordinary failure (the provider is down,
// the model produced nothing) is already written to agent_runs as 'failed' and
// returns nil, because a retry would spend money again to reach the same place.
func (w *AgentRunWorker) Work(ctx context.Context, job *river.Job[AgentRunArgs]) error {
	w.logger.Info("agent run job started",
		"run_id", job.Args.RunID, "job_id", job.ID, "attempt", job.Attempt)

	if err := w.orchestrator.Run(ctx, job.Args.RunID); err != nil {
		return fmt.Errorf("run agent %s: %w", job.Args.RunID, err)
	}

	w.logger.Info("agent run job finished", "run_id", job.Args.RunID, "job_id", job.ID)
	return nil
}
