package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	"cortex/internal/agent"
)

// ResumeRunWorker continues a run that paused for human approval.
//
// As thin as AgentRunWorker, and for the same reason: rebuilding a paused run's
// transcript from run_events is the orchestrator's business, and it can be
// tested with a scripted provider and no queue at all.
type ResumeRunWorker struct {
	river.WorkerDefaults[ResumeRunArgs]

	orchestrator *agent.Orchestrator
	logger       *slog.Logger
}

// NewResumeRunWorker builds the worker.
func NewResumeRunWorker(orchestrator *agent.Orchestrator, logger *slog.Logger) (*ResumeRunWorker, error) {
	if orchestrator == nil {
		return nil, errors.New("jobs: orchestrator is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ResumeRunWorker{orchestrator: orchestrator, logger: logger}, nil
}

// Work resumes the job's run.
func (w *ResumeRunWorker) Work(ctx context.Context, job *river.Job[ResumeRunArgs]) error {
	w.logger.Info("run resume job started",
		"run_id", job.Args.RunID, "job_id", job.ID, "attempt", job.Attempt)

	if err := w.orchestrator.Resume(ctx, job.Args.RunID); err != nil {
		return fmt.Errorf("resume agent run %s: %w", job.Args.RunID, err)
	}

	w.logger.Info("run resume job finished", "run_id", job.Args.RunID, "job_id", job.ID)
	return nil
}
