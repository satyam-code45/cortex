package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Queue is the enqueue side of the job system.
//
// It wraps *river.Client behind a two-method surface so the HTTP handler can
// enqueue without importing River at all. That indirection earns its keep twice:
// it keeps the API layer independent of the queue implementation, and it lets
// the transactional-enqueue test drive the rollback path with a fake that simply
// returns an error.
type Queue struct {
	client *river.Client[pgx.Tx]
	logger *slog.Logger
	// processing records whether workers were configured. Tracked explicitly
	// rather than inferred from the River client, so Start and Stop do not
	// depend on how River happens to represent an insert-only client.
	processing bool
}

// jobTimeout bounds one agent run.
//
// River's own default is one minute, and it cancels the job's context when it
// elapses — which killed exactly the runs worth having. A multi-hop
// investigation makes a dozen LLM calls (90s allowed each) plus Jira, Notion and
// Gmail round trips, so the deeper the investigation, the more certainly it died
// at 60s with "llm request timed out". Shallow runs finished and looked fine,
// which is what made it easy to miss.
//
// The ceiling that actually bounds a run is MAX_ITERATIONS, not the clock, so
// this is set well above any legitimate run and exists only to stop a wedged job
// from occupying a worker forever. It must stay below River's
// RescueStuckJobsAfter (1h by default), or a slow run would be rescued and
// retried while it is still working.
const jobTimeout = 15 * time.Minute

// Config configures the job queue.
type Config struct {
	// Pool is the database River uses for its own tables.
	Pool *pgxpool.Pool
	// Worker executes agent runs. Nil registers no workers, producing an
	// enqueue-only client — which is what a CLI that wants to submit work
	// without processing it needs.
	Worker *AgentRunWorker
	// MaxWorkers is the concurrency on the agent_runs queue.
	MaxWorkers int
	// Logger receives River's own logs.
	Logger *slog.Logger
}

// New builds the queue.
//
// A client with no workers configured is insert-only: River requires that
// Queues and Workers are either both present or both absent, so the two are
// wired together or not at all.
func New(cfg Config) (*Queue, error) {
	if cfg.Pool == nil {
		return nil, errors.New("jobs: Pool is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	riverConfig := &river.Config{Logger: logger, JobTimeout: jobTimeout}

	processing := cfg.Worker != nil
	if cfg.Worker != nil {
		maxWorkers := cfg.MaxWorkers
		if maxWorkers <= 0 {
			maxWorkers = 1
		}
		workers := river.NewWorkers()
		if err := river.AddWorkerSafely(workers, cfg.Worker); err != nil {
			return nil, fmt.Errorf("jobs: register agent run worker: %w", err)
		}
		riverConfig.Workers = workers
		riverConfig.Queues = map[string]river.QueueConfig{
			AgentRunQueue: {MaxWorkers: maxWorkers},
		}
	}

	client, err := river.NewClient(riverpgxv5.New(cfg.Pool), riverConfig)
	if err != nil {
		return nil, fmt.Errorf("jobs: build river client: %w", err)
	}
	return &Queue{client: client, logger: logger, processing: processing}, nil
}

// EnqueueAgentRun inserts an agent run job inside the caller's transaction.
//
// Taking the transaction as a parameter is the whole point: the job becomes
// visible to workers only when the caller commits, so it cannot reference a run
// row that was rolled back. Conversely, if the enqueue fails the caller's
// message and run inserts roll back with it — the user gets an error instead of
// a run that silently never executes.
func (q *Queue) EnqueueAgentRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	if _, err := q.client.InsertTx(ctx, tx, AgentRunArgs{RunID: runID}, &river.InsertOpts{
		Queue: AgentRunQueue,
	}); err != nil {
		return fmt.Errorf("enqueue agent run %s: %w", runID, err)
	}
	return nil
}

// Start begins processing jobs. It is a no-op on an insert-only queue.
func (q *Queue) Start(ctx context.Context) error {
	if !q.processing {
		return nil
	}
	if err := q.client.Start(ctx); err != nil {
		return fmt.Errorf("jobs: start river client: %w", err)
	}
	return nil
}

// Stop drains in-flight jobs, waiting until they finish or ctx expires.
//
// Draining rather than cancelling matters here: an interrupted agent run has
// already paid for its LLM calls, so letting it finish and record its answer is
// strictly better than killing it and having River retry from the top.
func (q *Queue) Stop(ctx context.Context) error {
	if !q.processing {
		return nil
	}
	if err := q.client.Stop(ctx); err != nil {
		return fmt.Errorf("jobs: stop river client: %w", err)
	}
	return nil
}
