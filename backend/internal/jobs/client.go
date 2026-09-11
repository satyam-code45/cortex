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
	// WriteWorker performs approved writes. Nil means this process cannot
	// execute a write at all, which is the correct configuration for anything
	// that only submits work.
	WriteWorker *ExecuteWriteWorker
	// ResumeWorker continues runs that paused for approval. It shares the
	// agent-run queue, because a resume IS an agent run and should compete for
	// the same concurrency budget.
	ResumeWorker *ResumeRunWorker
	// ExpireWorker sweeps undecided proposals. Registering it also registers
	// the hourly periodic job that triggers it; without it, nothing expires on
	// a schedule and stale proposals are only refused on approval.
	ExpireWorker *ExpireActionsWorker
	// MaxWorkers is the concurrency on the agent_runs queue.
	MaxWorkers int
	// IndexWorkers is the concurrency on the index_source queue. One is
	// usually right: the crawls are rate-limited by the upstream APIs, not by
	// local CPU, and running three at once mostly buys three sets of 429s.
	IndexWorkers int
	// WriteWorkers is the concurrency on the write_actions queue. Two is a
	// sensible default: a write is one HTTP request, and the queue also carries
	// the expiry sweep, which must not be stuck behind a slow send.
	WriteWorkers int
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

	processing := cfg.Worker != nil || cfg.IndexWorker != nil ||
		cfg.WriteWorker != nil || cfg.ResumeWorker != nil || cfg.ExpireWorker != nil
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
		if cfg.ResumeWorker != nil {
			if err := river.AddWorkerSafely(workers, cfg.ResumeWorker); err != nil {
				return nil, fmt.Errorf("jobs: register run resume worker: %w", err)
			}
			// Shares the agent-run queue rather than claiming its own: a resume
			// is an agent run, and giving it separate concurrency would let a
			// burst of approvals run more loops at once than MAX_ITERATIONS
			// budgeting assumes.
			queues[AgentRunQueue] = river.QueueConfig{MaxWorkers: positive(cfg.MaxWorkers, 1)}
		}
		if cfg.WriteWorker != nil {
			if err := river.AddWorkerSafely(workers, cfg.WriteWorker); err != nil {
				return nil, fmt.Errorf("jobs: register write execution worker: %w", err)
			}
			queues[WriteActionQueue] = river.QueueConfig{MaxWorkers: positive(cfg.WriteWorkers, 1)}
		}
		if cfg.ExpireWorker != nil {
			if err := river.AddWorkerSafely(workers, cfg.ExpireWorker); err != nil {
				return nil, fmt.Errorf("jobs: register action expiry worker: %w", err)
			}
			queues[WriteActionQueue] = river.QueueConfig{MaxWorkers: positive(cfg.WriteWorkers, 1)}
			// The sweep is scheduled by River rather than by a ticker in our
			// own process, so it survives a restart, does not double up when
			// two instances are running, and is visible in the queue's history
			// like every other job. RunOnStart catches up a deployment that was
			// down over an expiry boundary.
			riverConfig.PeriodicJobs = append(riverConfig.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(expireInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return ExpireActionsArgs{}, &river.InsertOpts{Queue: WriteActionQueue}
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}

		riverConfig.Workers = workers
		riverConfig.Queues = queues
	}

	client, err := river.NewClient(riverpgxv5.New(cfg.Pool), riverConfig)
	if err != nil {
		return nil, fmt.Errorf("jobs: build river client: %w", err)
	}
	queue := &Queue{client: client, logger: logger, processing: processing}

	// Two workers need to enqueue onto the very queue they are registered with:
	// finishing a write, or expiring a proposal, can leave a paused run ready to
	// continue. That is a construction cycle, and this is where it is broken —
	// after the client exists, before Start is called, so no worker can be
	// running when the field is written and there is nothing to race with.
	// Injecting a setter rather than exposing one keeps the cycle invisible to
	// callers, who would otherwise have to remember to close it.
	if cfg.WriteWorker != nil {
		cfg.WriteWorker.queue = queue
	}
	if cfg.ExpireWorker != nil {
		cfg.ExpireWorker.queue = queue
	}
	return queue, nil
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
		// Two attempts, not River's default of 25. The second attempt exists
		// solely for a worker killed mid-run (StartAgentRun re-claims a
		// 'running' row); every ordinary failure is already recorded as
		// status=failed and returns nil to River, so a bigger budget could
		// only re-spend LLM money on the rare could-not-record path. Index
		// jobs keep the default: a crawl is idempotent and cheap to retry.
		MaxAttempts: 2,
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

// NewestFinalizedIndexJob reports when the most recent index_source job
// reached a finalized state (completed, cancelled, or discarded), and whether
// any has. It is the Sources refresh cooldown's source of truth: River's job
// rows record every crawl, where documents.updated_at freezes whenever a
// refresh changes nothing (the content-hash short-circuit writes no rows).
// Going through River's JobList API rather than querying river_job keeps the
// no-hand-rolled-SQL rule intact — the table is River's, not ours.
func (q *Queue) NewestFinalizedIndexJob(ctx context.Context) (finishedAt time.Time, ok bool, err error) {
	result, err := q.client.JobList(ctx, river.NewJobListParams().
		Kinds((IndexSourceArgs{}).Kind()).
		States(rivertype.JobStateCompleted, rivertype.JobStateCancelled, rivertype.JobStateDiscarded).
		OrderBy(river.JobListOrderByFinalizedAt, river.SortOrderDesc).
		First(1))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("jobs: list finalized index jobs: %w", err)
	}
	if len(result.Jobs) == 0 || result.Jobs[0].FinalizedAt == nil {
		return time.Time{}, false, nil
	}
	return *result.Jobs[0].FinalizedAt, true, nil
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

// EnqueueExecuteWrite inserts a write execution job inside the caller's
// transaction.
//
// Transactional for the same reason the agent-run enqueue is, and the stakes are
// higher here. The job becomes visible to workers only when the approval commits,
// so there is no window in which a worker could send an email whose approval was
// then rolled back — and none in which an approval commits with no job to carry
// it out.
func (q *Queue) EnqueueExecuteWrite(ctx context.Context, tx pgx.Tx, actionID uuid.UUID) error {
	if _, err := q.client.InsertTx(ctx, tx, ExecuteWriteArgs{ActionID: actionID}, &river.InsertOpts{
		Queue: WriteActionQueue,
		// Three attempts. More than the agent run's two, because the failure
		// modes here are genuinely transient — a 503 from Jira, a network blip —
		// and unlike an agent run a retry costs nothing but one HTTP request.
		// The compare-and-set on the action row is what makes extra attempts
		// safe: only one of them can ever reach the upstream call.
		MaxAttempts: 3,
		// One job per action, ever. A double-clicked approve button, or two
		// requests racing, must not produce two jobs — the status guard would
		// stop the second sending anything, but not queueing a job is cleaner
		// and leaves no confusing row in the queue's history.
		UniqueOpts: river.UniqueOpts{ByArgs: true},
	}); err != nil {
		return fmt.Errorf("enqueue write execution %s: %w", actionID, err)
	}
	return nil
}

// EnqueueResumeRun queues the continuation of a paused run, inside the caller's
// transaction.
//
// Transactional, and this one is subtle enough to be worth spelling out. Every
// caller decides whether to resume by asking "is anything on this run still
// outstanding?" from inside the transaction that just settled the last
// outstanding thing — so it reads its own uncommitted write and correctly sees
// zero. Enqueue outside that transaction and the job can start before the commit
// lands, read the pre-commit state, conclude that work IS still outstanding, and
// return having done nothing. Nothing would ever enqueue another resume, and the
// run would wait for a continuation that was already thrown away.
//
// InsertTx makes the job visible only when the decision it is based on is
// visible, which removes the window entirely.
func (q *Queue) EnqueueResumeRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	if _, err := q.client.InsertTx(ctx, tx, ResumeRunArgs{RunID: runID}, &river.InsertOpts{
		Queue:       AgentRunQueue,
		MaxAttempts: 2,
		// One in-flight resume per run. Two decisions committing at the same
		// instant could each conclude that nothing is outstanding; without this
		// they would both queue a resume and two loops would drive one run,
		// duplicating its events and its LLM spend.
		//
		// The required states must all be present or River rejects the insert
		// outright rather than defaulting — the same trap the index enqueue
		// documents. 'retryable' is added on top: a resume waiting to retry is
		// still in flight.
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
		return fmt.Errorf("enqueue resume of run %s: %w", runID, err)
	}
	return nil
}
