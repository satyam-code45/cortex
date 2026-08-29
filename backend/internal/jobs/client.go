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
	"github.com/riverqueue/river/rivertype"
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

// jobTimeout bounds one agent run and one indexing crawl.
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
//
// The same ceiling covers an indexing crawl, which is bounded by
// INDEX_MAX_DOCUMENTS rather than by the clock. A crawl that does hit this is
// retried, and the content-hash check makes the retry resume rather than
// restart: everything already written is skipped without being re-embedded.
const jobTimeout = 15 * time.Minute

// Config configures the job queue.
type Config struct {
	// Pool is the database River uses for its own tables.
	Pool *pgxpool.Pool
	// Worker executes agent runs. Nil registers no workers, producing an
	// enqueue-only client — which is what a CLI that wants to submit work
	// without processing it needs.
	Worker *AgentRunWorker
	// IndexWorker executes source reindexing. Nil leaves the indexing queue
	// unserved, which is what an enqueue-only client wants.
	IndexWorker *IndexSourceWorker
	// MaxWorkers is the concurrency on the agent_runs queue.
	MaxWorkers int
	// IndexWorkers is the concurrency on the index_source queue. One is
	// usually right: the crawls are rate-limited by the upstream APIs, not by
	// local CPU, and running three at once mostly buys three sets of 429s.
	IndexWorkers int
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

	processing := cfg.Worker != nil || cfg.IndexWorker != nil
	if processing {
		workers := river.NewWorkers()
		queues := map[string]river.QueueConfig{}

		if cfg.Worker != nil {
			if err := river.AddWorkerSafely(workers, cfg.Worker); err != nil {
				return nil, fmt.Errorf("jobs: register agent run worker: %w", err)
			}
			queues[AgentRunQueue] = river.QueueConfig{MaxWorkers: positive(cfg.MaxWorkers, 1)}
		}
		if cfg.IndexWorker != nil {
			if err := river.AddWorkerSafely(workers, cfg.IndexWorker); err != nil {
				return nil, fmt.Errorf("jobs: register index source worker: %w", err)
			}
			queues[IndexSourceQueue] = river.QueueConfig{MaxWorkers: positive(cfg.IndexWorkers, 1)}
		}

		riverConfig.Workers = workers
		riverConfig.Queues = queues
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

// EnqueueIndexSource queues a reindex of one source.
//
// Not transactional, unlike EnqueueAgentRun: there is no row it has to commit
// with. The admin endpoint that calls it has nothing to roll back, and an
// indexing job is idempotent — running it twice re-reads the sources and writes
// nothing the first run already wrote.
func (q *Queue) EnqueueIndexSource(ctx context.Context, source string) error {
	if _, err := q.client.Insert(ctx, IndexSourceArgs{Source: source}, &river.InsertOpts{
		Queue: IndexSourceQueue,
		// One in-flight job per source. Someone hitting the admin endpoint three
		// times should get one crawl, not three concurrent ones racing to write
		// the same document rows.
		//
		// The state list must contain all four of River's required states —
		// available, pending, running, scheduled (insert_opts.go:
		// requiredV3states) — or River rejects the insert outright rather than
		// falling back to a default. Omitting `pending` made every enqueue here
		// fail, so POST /api/admin/index always answered 500 and nothing was
		// ever indexed; TestEnqueueIndexSourceInsertsAJob exists so that cannot
		// go unnoticed again. `retryable` is added on top of the required four:
		// a crawl that failed and is waiting to retry is still in flight, and
		// queueing a second one beside it is the duplicate work this prevents.
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}); err != nil {
		return fmt.Errorf("enqueue index of %s: %w", source, err)
	}
	return nil
}

// positive returns value when it is positive, and fallback otherwise.
func positive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
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
