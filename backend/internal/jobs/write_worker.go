package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"cortex/internal/actions"
	"cortex/internal/agent"
	"cortex/internal/store"
	"cortex/internal/tools"
)

// ExecuteWriteWorker performs one approved write.
//
// This is the only code in the system that can cause a side effect in Jira,
// Notion or a mailbox on the agent's behalf, and it runs nowhere near the agent
// loop. Everything it does is downstream of a human having read a payload and
// approved it.
//
// The exactly-once property comes from one line: the compare-and-set that moves
// the row from 'approved' to 'executing' happens BEFORE the upstream call. River
// retries jobs — that is what makes it reliable — so a retry must find the row
// already claimed and send nothing. Claiming after the call, or not claiming at
// all, would make every retry a second email.
type ExecuteWriteWorker struct {
	river.WorkerDefaults[ExecuteWriteArgs]

	db store.Beginner
	// writersForUser builds the executors for one user from their own
	// connections. A per-user factory rather than a shared registry because a
	// Writer holds that user's credentials: the write goes out as them, from
	// their mailbox, under their Jira account.
	writersForUser func(ctx context.Context, userID uuid.UUID) (*actions.Registry, error)
	queue          *Queue
	logger         *slog.Logger
}

// NewExecuteWriteWorker builds the worker.
func NewExecuteWriteWorker(
	db store.Beginner,
	writersForUser func(ctx context.Context, userID uuid.UUID) (*actions.Registry, error),
	queue *Queue,
	logger *slog.Logger,
) (*ExecuteWriteWorker, error) {
	if db == nil {
		return nil, errors.New("jobs: db is required")
	}
	if writersForUser == nil {
		return nil, errors.New("jobs: writersForUser is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ExecuteWriteWorker{db: db, writersForUser: writersForUser, queue: queue, logger: logger}, nil
}

// Work performs the job's action.
//
// It returns an error only when the outcome could not be RECORDED. An upstream
// failure is not a job failure: it is written to the row as 'failed', reported to
// the agent on resume, and shown in the audit view. Retrying it would risk
// sending something twice to fix a problem that a person needs to see.
func (w *ExecuteWriteWorker) Work(ctx context.Context, job *river.Job[ExecuteWriteArgs]) error {
	actionID := job.Args.ActionID
	w.logger.Info("write execution job started",
		"action_id", actionID, "job_id", job.ID, "attempt", job.Attempt)

	// The owner and the writer are resolved BEFORE the row is claimed, and the
	// order is the whole point. Claiming first means a transient credential
	// failure — a saturated pool, a momentary decryption fault — leaves the row
	// in 'executing' with nothing sent; the River retry then finds
	// status='executing' with attempt > 1, which is the signature of a crash
	// mid-send, and settles the action as "interrupted, whether it took effect
	// is unknown". That spends an approval and files a misleading audit record
	// for a write that provably never left. Resolving first means such a
	// failure happens while the row is still 'approved', so the retry re-claims
	// cleanly and the approval survives.
	owner, err := w.actionOwner(ctx, actionID)
	if err != nil {
		return err
	}
	if owner == nil {
		// Nothing to perform: the action is gone, already done, or was never
		// approved. Nothing is sent, which is the entire point.
		return nil
	}

	registry, err := w.writersForUser(ctx, *owner)
	if err != nil {
		// Nothing has been claimed yet, so this costs the approval nothing: the
		// row is still 'approved' and the retry starts over.
		return fmt.Errorf("build writers for user %s: %w", *owner, err)
	}

	claimed, err := w.claim(ctx, actionID, job.Attempt)
	if err != nil {
		return err
	}
	if claimed == nil {
		return nil
	}

	writer, found := registry.Get(claimed.Action)
	if !found {
		return w.settle(ctx, claimed, nil, fmt.Errorf(
			"this account can no longer perform %q — the connection it needs was removed or its "+
				"writes were switched off after the approval", claimed.Action))
	}

	payload := claimed.FinalPayload
	if len(payload) == 0 {
		// Approval stores the final payload, so this is belt and braces. The
		// proposal is the only other thing anybody ever saw, so it is the only
		// safe fallback.
		payload = claimed.ProposedPayload
	}

	outcome, execErr := writer.Execute(ctx, payload)
	if execErr != nil {
		w.logger.Error("write execution failed",
			"action_id", actionID, "action", claimed.Action, "error", execErr)
		return w.settle(ctx, claimed, nil, execErr)
	}

	w.logger.Info("write executed", "action_id", actionID, "action", claimed.Action)
	return w.settle(ctx, claimed, &outcome, nil)
}

// actionOwner reads the action's owner without claiming the row.
//
// Split out so the writer registry — the expensive, failure-prone part — can be
// built before the claim. Returns nil when there is nothing worth claiming, so
// the caller can stop before touching any credential.
func (w *ExecuteWriteWorker) actionOwner(ctx context.Context, actionID uuid.UUID) (*uuid.UUID, error) {
	var owner *uuid.UUID
	err := w.inTx(ctx, func(_ pgx.Tx, q store.Querier) error {
		row, err := q.GetAgentAction(ctx, actionID)
		if errors.Is(err, pgx.ErrNoRows) {
			w.logger.Warn("write execution job for an action that no longer exists",
				"action_id", actionID)
			return nil
		}
		if err != nil {
			return fmt.Errorf("load action %s: %w", actionID, err)
		}
		// Only 'approved' can be claimed, and an 'executing' row still needs
		// claim's interrupted-attempt accounting. Anything else is already
		// handled. claim remains the authority: this is a cheap pre-filter that
		// avoids building credentials for nothing, not the guard.
		if row.Status != actions.StatusApproved && row.Status != actions.StatusExecuting {
			w.logger.Info("write execution job skipped, action is not awaiting execution",
				"action_id", actionID, "status", row.Status)
			return nil
		}
		id := row.UserID
		owner = &id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return owner, nil
}

// claim moves the action from 'approved' to 'executing' and returns it, or nil
// when there is nothing to perform.
func (w *ExecuteWriteWorker) claim(ctx context.Context, actionID uuid.UUID, attempt int) (*store.AgentAction, error) {
	var claimed *store.AgentAction

	err := w.inTx(ctx, func(tx pgx.Tx, q store.Querier) error {
		row, err := q.BeginExecutingAgentAction(ctx, actionID)
		if err == nil {
			claimed = &row
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("claim action %s: %w", actionID, err)
		}

		// Nothing was 'approved'. Find out what state it is actually in, because
		// one of them needs handling.
		existing, err := q.GetAgentAction(ctx, actionID)
		if errors.Is(err, pgx.ErrNoRows) {
			w.logger.Warn("write execution job for an action that no longer exists", "action_id", actionID)
			return nil
		}
		if err != nil {
			return fmt.Errorf("load action %s: %w", actionID, err)
		}

		if existing.Status == actions.StatusExecuting && attempt > 1 {
			// A previous attempt claimed this row and then died — the retry is
			// the evidence. Whether the write reached the upstream system is
			// genuinely unknown, so it is never re-sent; it is marked failed
			// with that stated plainly, which is the honest thing to tell both
			// the agent and the person who approved it.
			w.logger.Error("write execution was interrupted mid-attempt; not retrying the send",
				"action_id", actionID, "action", existing.Action, "attempt", attempt)
			return w.recordInterrupted(ctx, tx, q, existing)
		}

		w.logger.Info("write execution job skipped, action is not awaiting execution",
			"action_id", actionID, "status", existing.Status)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// recordInterrupted marks a half-performed write failed and unblocks its run.
func (w *ExecuteWriteWorker) recordInterrupted(
	ctx context.Context,
	tx pgx.Tx,
	q store.Querier,
	row store.AgentAction,
) error {
	const message = "interrupted while being carried out, so whether it took effect is unknown; " +
		"it was not retried, because retrying a write that may already have landed is worse than " +
		"reporting the uncertainty"

	failed, err := q.FailInterruptedAgentAction(ctx, store.FailInterruptedAgentActionParams{
		ID:    row.ID,
		Error: strPtr(message),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // somebody else settled it first
		}
		return fmt.Errorf("record interrupted action %s: %w", row.ID, err)
	}
	if err := agent.RecordActionFailed(ctx, q, failed.AgentRunID, failed.ID, failed.Action, message); err != nil {
		return err
	}
	return w.resumeIfSettled(ctx, tx, q, failed.AgentRunID)
}

// settle records the outcome of an attempted write and unblocks its run.
//
// One transaction for the row, the event and the resume decision, because they
// are three views of a single fact. A row marked executed whose event was lost
// would leave a gap in the run's timeline; a run resumed before the row was
// updated would be told the wrong outcome.
func (w *ExecuteWriteWorker) settle(
	ctx context.Context,
	row *store.AgentAction,
	outcome *tools.WriteOutcome,
	execErr error,
) error {
	// Detached from the job context: the upstream call may already have
	// happened, and failing to record that because a worker is shutting down
	// would leave a sent email looking unsent — the one bookkeeping error that
	// could cause a person to send it again.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()

	return w.inTx(persistCtx, func(tx pgx.Tx, q store.Querier) error {
		if execErr != nil {
			message := execErr.Error()
			failed, err := q.FailAgentAction(persistCtx, store.FailAgentActionParams{
				ID:    row.ID,
				Error: strPtr(message),
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return fmt.Errorf("record failed action %s: %w", row.ID, err)
			}
			if err := agent.RecordActionFailed(persistCtx, q, failed.AgentRunID, failed.ID,
				failed.Action, message); err != nil {
				return err
			}
			return w.resumeIfSettled(persistCtx, tx, q, failed.AgentRunID)
		}

		detail, err := json.Marshal(resultRecord{Summary: outcome.Summary, Detail: outcome.Detail})
		if err != nil {
			return fmt.Errorf("encode action result: %w", err)
		}
		done, err := q.FinishAgentAction(persistCtx, store.FinishAgentActionParams{
			ID:     row.ID,
			Result: detail,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("record executed action %s: %w", row.ID, err)
		}
		if err := agent.RecordActionExecuted(persistCtx, q, done.AgentRunID, done.ID, done.Action,
			outcome.Summary, outcome.Detail); err != nil {
			return err
		}
		return w.resumeIfSettled(persistCtx, tx, q, done.AgentRunID)
	})
}

// resumeIfSettled enqueues a resume once nothing on the run is outstanding.
//
// Checked here rather than at the start of the resume job, and the difference
// matters. A run that proposed two writes gets a decision on each; enqueueing a
// resume after the first would queue a job whose only job is to discover it has
// nothing to do — and, worse, River's per-run job uniqueness could then dedupe
// away the resume that actually mattered. Asking "is everything finished?" before
// enqueueing means at most one resume is ever queued per pause.
func (w *ExecuteWriteWorker) resumeIfSettled(
	ctx context.Context,
	tx pgx.Tx,
	q store.Querier,
	runID uuid.UUID,
) error {
	if w.queue == nil {
		// Only reachable through a wiring mistake: jobs.New back-injects the
		// queue. Logged rather than ignored, because the symptom otherwise is a
		// run that pauses and never answers, with nothing to explain why.
		w.logger.Error("cannot enqueue a resume: the worker has no queue",
			"run_id", runID)
		return nil
	}
	// Serialized per run before counting. Two actions on one run settling at the
	// same time would otherwise each see the other as outstanding and neither
	// would enqueue a resume, stranding the run in 'awaiting_approval'.
	if _, err := q.LockAgentRunForSettlement(ctx, runID); err != nil {
		return fmt.Errorf("lock run %s for settlement: %w", runID, err)
	}
	// Counted through q, which is this transaction: the row just settled above is
	// not visible to anybody else yet, and it must be included or the count would
	// say the run is still waiting on the thing that just finished.
	unsettled, err := q.CountUnsettledActionsByRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("count unsettled actions for run %s: %w", runID, err)
	}
	if unsettled > 0 {
		return nil
	}
	// Enqueued through the same transaction, so the job cannot start before the
	// settlement it is based on is visible to it.
	if err := w.queue.EnqueueResumeRun(ctx, tx, runID); err != nil {
		return fmt.Errorf("enqueue resume of run %s: %w", runID, err)
	}
	return nil
}

// inTx runs fn in a transaction, handing it both the pgx.Tx and the
// transaction-scoped queries.
//
// store.WithTx would do, except that a transactional job enqueue needs the
// pgx.Tx itself — River writes the job row into it — and WithTx deliberately
// exposes only the Querier. Same rollback contract: the deferred Rollback uses
// an uncancellable context, because pgx destroys the connection outright when it
// cannot send the ROLLBACK.
func (w *ExecuteWriteWorker) inTx(ctx context.Context, fn func(tx pgx.Tx, q store.Querier) error) error {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	if err := fn(tx, store.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// resultRecord is the shape stored in agent_actions.result.
//
// Summary is kept beside the raw detail so the audit view and the agent's resume
// message read the same sentence, and neither has to reconstruct it from a
// message id.
type resultRecord struct {
	Summary string         `json:"summary"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// settleTimeout bounds recording an outcome, on a context detached from the job.
const settleTimeout = 10 * time.Second

// strPtr is the pointer-to-string the generated nullable params take.
func strPtr(s string) *string { return &s }
