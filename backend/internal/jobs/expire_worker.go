package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"cortex/internal/actions"
	"cortex/internal/agent"
	"cortex/internal/store"
)

// ExpireActionsWorker retires proposals nobody decided in time.
//
// It is not what makes an expired proposal unexecutable — the approve query
// checks the age in the same predicate as the status, so a stale row cannot be
// approved even if this never ran. What it is for is the other half of the
// problem: a run that proposed a write and was never answered would otherwise
// sit in 'awaiting_approval' forever, with a pending card in the chat and an
// investigation nobody ever got an answer to.
//
// So the sweep expires the row AND resumes the run, telling the agent the
// proposal was never decided. The run then answers with what it has, which is
// the outcome the person asking would have wanted a day ago.
type ExpireActionsWorker struct {
	river.WorkerDefaults[ExpireActionsArgs]

	db     store.Beginner
	ttl    time.Duration
	queue  *Queue
	logger *slog.Logger
	now    func() time.Time
}

// NewExpireActionsWorker builds the worker. A ttl of zero or less disables
// expiry, which makes the sweep a no-op rather than an error.
func NewExpireActionsWorker(
	db store.Beginner,
	ttl time.Duration,
	queue *Queue,
	logger *slog.Logger,
) (*ExpireActionsWorker, error) {
	if db == nil {
		return nil, errors.New("jobs: db is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ExpireActionsWorker{db: db, ttl: ttl, queue: queue, logger: logger, now: time.Now}, nil
}

// Work expires one batch of overdue proposals.
func (w *ExpireActionsWorker) Work(ctx context.Context, job *river.Job[ExpireActionsArgs]) error {
	if w.ttl <= 0 {
		return nil
	}

	cutoff := actions.Cutoff(w.ttl, w.now().UTC())
	var expired int
	runs := map[uuid.UUID]struct{}{}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	err = func(q store.Querier) error {
		overdue, err := q.ListExpiredPendingActions(ctx, store.ListExpiredPendingActionsParams{
			ProposedAt: pgtype.Timestamptz{Time: cutoff, Valid: true},
			Limit:      expireBatch,
		})
		if err != nil {
			return fmt.Errorf("list expired proposals: %w", err)
		}

		// Every affected run is locked up front, in a deterministic order.
		// Each settling transaction takes the same per-run lock to stop
		// concurrent settlements both concluding the run is still waiting; this
		// one settles a whole batch, so without a fixed order two overlapping
		// batches could take the same locks in opposite orders and deadlock.
		for _, runID := range sortedRunIDs(overdue) {
			if _, err := q.LockAgentRunForSettlement(ctx, runID); err != nil {
				return fmt.Errorf("lock run %s for settlement: %w", runID, err)
			}
		}

		for _, row := range overdue {
			gone, err := q.ExpireAgentAction(ctx, row.ID)
			if errors.Is(err, pgx.ErrNoRows) {
				// Decided in the moment between the list and this update. The
				// person got there first, which is the good outcome.
				continue
			}
			if err != nil {
				return fmt.Errorf("expire proposal %s: %w", row.ID, err)
			}
			if err := agent.RecordActionFailed(ctx, q, gone.AgentRunID, gone.ID, gone.Action,
				actions.ExpiredObservation); err != nil {
				return err
			}
			expired++
			runs[gone.AgentRunID] = struct{}{}
		}

		// Resumes are enqueued from inside the transaction's view of the data
		// but after every row is settled, so the unsettled count each run is
		// tested against reflects the whole batch. A run with two expired
		// proposals must be resumed once, not twice.
		for _, row := range overdue {
			if _, touched := runs[row.AgentRunID]; !touched {
				continue
			}
			delete(runs, row.AgentRunID)
			unsettled, err := q.CountUnsettledActionsByRun(ctx, row.AgentRunID)
			if err != nil {
				return fmt.Errorf("count unsettled actions for run %s: %w", row.AgentRunID, err)
			}
			if unsettled > 0 {
				continue
			}
			if w.queue == nil {
				// Only reachable through a wiring mistake: jobs.New
				// back-injects the queue. Logged rather than skipped in
				// silence, because the symptom is a run that never answers.
				w.logger.Error("cannot enqueue a resume: the sweep has no queue",
					"run_id", row.AgentRunID)
				continue
			}
			// Through the transaction, so the job cannot start before the
			// expiries it is based on are visible to it — otherwise it would
			// read the pre-commit state, find the proposals still pending, and
			// return having done nothing, leaving the run stuck for good.
			if err := w.queue.EnqueueResumeRun(ctx, tx, row.AgentRunID); err != nil {
				return fmt.Errorf("enqueue resume of run %s: %w", row.AgentRunID, err)
			}
		}
		return nil
	}(store.New(tx))
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	if expired > 0 {
		w.logger.Info("expired undecided write proposals",
			"count", expired, "cutoff", cutoff.Format(time.RFC3339), "job_id", job.ID)
	}

	// Separate transaction, deliberately: it takes per-run locks of its own, and
	// keeping it out of the batch above means the two cannot interleave their
	// lock acquisition.
	return w.resumeStalledRuns(ctx, job.ID)
}

// sortedRunIDs returns the distinct run ids of a batch, in a stable order.
func sortedRunIDs(rows []store.AgentAction) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(rows))
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		if _, dup := seen[row.AgentRunID]; dup {
			continue
		}
		seen[row.AgentRunID] = struct{}{}
		out = append(out, row.AgentRunID)
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// resumeStalledRuns releases runs that are paused for a decision already made.
//
// This should never find anything: the per-run lock each settling transaction
// takes is what stops two concurrent settlements from both deciding the other
// is still outstanding. It exists because the failure it recovers from is
// invisible and permanent — a run stuck in 'awaiting_approval' has no pending
// action, so the expiry sweep above cannot see it either, and the person who
// asked the question simply never gets an answer. A cheap query on an hourly
// job is worth removing that outcome from the system.
func (w *ExpireActionsWorker) resumeStalledRuns(ctx context.Context, jobID int64) error {
	if w.queue == nil {
		return nil
	}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	q := store.New(tx)
	stalled, err := q.ListResumableStalledRuns(ctx, expireBatch)
	if err != nil {
		return fmt.Errorf("list stalled runs: %w", err)
	}
	if len(stalled) == 0 {
		return nil
	}

	// Already ordered by id from the query, which is the same deterministic
	// order the batch above locks in.
	for _, runID := range stalled {
		if _, err := q.LockAgentRunForSettlement(ctx, runID); err != nil {
			return fmt.Errorf("lock run %s for settlement: %w", runID, err)
		}
		// Re-checked under the lock: the row was read before it was held, so a
		// settlement could have enqueued the resume in between.
		unsettled, err := q.CountUnsettledActionsByRun(ctx, runID)
		if err != nil {
			return fmt.Errorf("count unsettled actions for run %s: %w", runID, err)
		}
		if unsettled > 0 {
			continue
		}
		if err := w.queue.EnqueueResumeRun(ctx, tx, runID); err != nil {
			return fmt.Errorf("enqueue resume of stalled run %s: %w", runID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	w.logger.Warn("resumed runs that were paused for a decision already made",
		"count", len(stalled), "job_id", jobID)
	return nil
}
