package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/actions"
	"cortex/internal/llm"
	"cortex/internal/store"
	"cortex/internal/tools"
)

// Proposing a write, and pausing on it.
//
// The loop never performs a write. A write tool returns a proposal; this file
// records it, tells the model plainly that nothing has happened, and stops the
// run. A separate job performs the write after a human approves, and another
// resumes the loop. Nothing in the agent package can send an email.
//
// That is a structural claim, not a stylistic one, and it is worth being able to
// state precisely: the orchestrator is handed a *tools.Registry, whose members
// can only ever return a Proposal, and it never holds a tools.Writer. There is
// no code path from an agent iteration to an upstream mutation, so no amount of
// prompt injection can produce one.

// RunStatusAwaitingApproval is the agent_runs status of a run that has stopped
// to wait for a human.
//
// Exported because it is not only the loop's business: the event stream closes
// on it (nothing more will happen until somebody decides, which can take hours),
// and the chat UI renders a paused run differently from a running one. The value
// has been in the status CHECK constraint since the first migration, unused until
// the approval gate needed it.
const RunStatusAwaitingApproval = "awaiting_approval"

// proposalOutcome is what recording one proposal produced.
type proposalOutcome struct {
	// actionID identifies the row, when one exists.
	actionID uuid.UUID
	// waiting is true when a fresh pending row was created, so the run must
	// stop and wait for a decision on it.
	waiting bool
	// refused explains why no row was created at all — a rate limit, today. It
	// becomes the observation, and the model can act on it: say so in the
	// answer, or carry on without the write.
	refused string
	// settled describes a decision the identical proposal already received.
	// This is the re-proposal case: the run was retried, or the model asked
	// again after a resume, and the row it would create already exists in a
	// terminal state. Pausing again would wait forever for a decision that has
	// already been made.
	settled string
}

// recordProposal persists a proposed write and reports what the loop should do
// about it.
//
// It runs in its own transaction, alongside the action_proposed event, so the
// row and the event that announces it land together. An action row with no
// event would be invisible in the run's timeline; an event with no row would
// point at nothing.
func (o *Orchestrator) recordProposal(
	ctx context.Context,
	state *runState,
	iteration int,
	call llm.ToolCall,
	proposal *tools.Proposal,
) (proposalOutcome, error) {
	key, err := actions.IdempotencyKey(state.runID, proposal.Action, proposal.Payload)
	if err != nil {
		return proposalOutcome{}, err
	}

	var out proposalOutcome
	err = o.withTx(ctx, func(q store.Querier) error {
		if refusal, err := o.checkWriteLimit(ctx, q, state); err != nil {
			return err
		} else if refusal != "" {
			out.refused = refusal
			return nil
		}

		row, err := q.InsertAgentAction(ctx, store.InsertAgentActionParams{
			AgentRunID:      state.runID,
			UserID:          state.ownerID,
			Source:          proposal.Source,
			Action:          proposal.Action,
			ProposedPayload: proposal.Payload,
			IdempotencyKey:  key,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// The key collided: this exact write was already proposed for this
			// run. Reuse that row rather than creating a second approval for the
			// same thing.
			existing, getErr := q.GetAgentActionByIdempotencyKey(ctx, key)
			if getErr != nil {
				return fmt.Errorf("load existing action for %s: %w", proposal.Action, getErr)
			}
			out.actionID = existing.ID
			if existing.Status == actions.StatusPending {
				// Still awaiting a decision. The run waits on it exactly as if
				// it had just been created — which is what happens when a
				// crashed run is retried and re-proposes before anybody
				// decided.
				out.waiting = true
				return nil
			}
			out.settled = settledNote(existing)
			return nil
		case err != nil:
			return fmt.Errorf("record proposed action %s: %w", proposal.Action, err)
		}

		out.actionID = row.ID
		out.waiting = true
		return appendEvent(ctx, q, state.runID, EventActionProposed, actionProposedPayload{
			Iteration:  iteration,
			ActionID:   row.ID,
			ToolCallID: call.ID,
			Tool:       call.Name,
			Source:     proposal.Source,
			Action:     proposal.Action,
			Summary:    proposal.Summary,
			Payload:    proposal.Payload,
		})
	})
	if err != nil {
		return proposalOutcome{}, err
	}
	return out, nil
}

// checkWriteLimit reports the refusal text when the owner has reached their
// hourly write ceiling, or empty when they have not.
//
// Enforced here rather than at execution on purpose. A limit that fires after a
// person has read a payload and clicked approve has wasted the one expensive
// part of this whole mechanism — their attention — and told them nothing they
// could have acted on. Firing at proposal time makes it an observation the model
// receives and can explain in its answer.
func (o *Orchestrator) checkWriteLimit(ctx context.Context, q store.Querier, state *runState) (string, error) {
	if o.writesPerUserPerHour <= 0 {
		return "", nil
	}
	since := o.now().Add(-time.Hour).UTC()
	count, err := q.CountExecutedActionsSince(ctx, store.CountExecutedActionsSinceParams{
		UserID: state.ownerID,
		Since:  pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return "", fmt.Errorf("count recent writes: %w", err)
	}
	if count < int64(o.writesPerUserPerHour) {
		return "", nil
	}
	return fmt.Sprintf("This account has already performed %d writes in the last hour, which is the "+
		"limit (%d), so no further write can be proposed right now. Nothing was recorded. Answer the "+
		"question with what you have, and say plainly that the write could not be proposed because "+
		"the hourly limit was reached.", count, o.writesPerUserPerHour), nil
}

// settledNote describes a decision an identical earlier proposal received.
func settledNote(row store.AgentAction) string {
	var b strings.Builder
	b.WriteString("ALREADY DECIDED: this exact request was proposed earlier in this run and is no " +
		"longer waiting.\n")
	switch row.Status {
	case actions.StatusExecuted:
		fmt.Fprintf(&b, "It was approved and carried out. %s\n", resultSentence(row.Result))
		b.WriteString("It has therefore already happened once. Do not propose it again.")
	case actions.StatusRejected:
		reason := "no reason was given"
		if row.RejectReason != nil && strings.TrimSpace(*row.RejectReason) != "" {
			reason = strings.TrimSpace(*row.RejectReason)
		}
		fmt.Fprintf(&b, "A person rejected it, with this reason: %s\n", reason)
		b.WriteString("Do not propose it again. Either answer without it, or propose something " +
			"materially different that addresses the reason.")
	case actions.StatusFailed:
		detail := "no error was recorded"
		if row.Error != nil && strings.TrimSpace(*row.Error) != "" {
			detail = strings.TrimSpace(*row.Error)
		}
		fmt.Fprintf(&b, "It was approved but failed: %s\n", detail)
		b.WriteString("Do not retry it automatically — say in your answer that it was attempted " +
			"and failed, and what the error was.")
	case actions.StatusExpired:
		b.WriteString("Nobody decided in time, so it expired and can no longer be carried out.\n")
		b.WriteString("Answer without it and say that it was never approved.")
	default:
		fmt.Fprintf(&b, "Its status is %q.\n", row.Status)
		b.WriteString("Do not propose it again.")
	}
	return b.String()
}

// resultSentence renders an executed action's result for the model, tolerating a
// result column that is absent or not an object.
func resultSentence(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		return ""
	}
	if summary, ok := detail["summary"].(string); ok && strings.TrimSpace(summary) != "" {
		// Composed by this system, but it quotes the payload's subject line back
		// — which came from whoever wrote the message being replied to.
		return untrustedInline(summary)
	}
	parts := make([]string, 0, 3)
	for _, key := range []string{"issue_key", "message_id", "page_id"} {
		if value, ok := detail[key].(string); ok && value != "" {
			parts = append(parts, key+" "+value)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "The result was: " + strings.Join(parts, ", ") + "."
}

// pause stops the run to wait for a human and releases the worker.
//
// It records the pause and returns nil, so River treats the job as done. That is
// the whole trick behind waiting without cost: there is no held connection, no
// sleeping worker, and no iteration budget being consumed. The run's state lives
// entirely in Postgres, and a decision — arriving in a minute or in six hours —
// enqueues a fresh job that rebuilds the loop from the event log.
func (o *Orchestrator) pause(ctx context.Context, state *runState) error {
	inputTokens := int32(state.inputTokens)
	outputTokens := int32(state.outputTokens)

	// Detached from the job context for the same reason the terminal write-back
	// is: a worker being shut down must still record the pause, or the run is
	// left 'running' with nobody driving it and no way to tell.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()

	waiting := state.pending
	err := o.withTx(persistCtx, func(q store.Querier) error {
		if err := appendEvent(persistCtx, q, state.runID, EventRunPaused, runPausedPayload{
			Iteration:      state.iterations,
			Waiting:        waiting,
			IterationsUsed: state.iterations,
			InputTokens:    state.inputTokens,
			OutputTokens:   state.outputTokens,
		}); err != nil {
			return err
		}
		if _, err := q.PauseAgentRun(persistCtx, store.PauseAgentRunParams{
			ID:           state.runID,
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The run left 'running' underneath us — a concurrent failure
				// path, say. The proposals stand and will expire; there is
				// nothing to pause.
				o.logger.Warn("agent: run was not running when pausing", "run_id", state.runID)
				return nil
			}
			return fmt.Errorf("pause agent run: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("pause run %s: %w", state.runID, err)
	}
	o.logger.Info("agent: run paused for approval",
		"run_id", state.runID, "iteration", state.iterations, "actions", len(waiting))
	return nil
}

// finishProposal turns a write tool's result into an observation and, when a
// fresh proposal was recorded, adds the run to the set waiting on it.
//
// Deliberately bypasses fitToContext, which truncates or summarizes an oversized
// observation. A proposal observation is a few hundred characters of this
// system's own instruction — "nothing has happened, a person is deciding, do not
// call this again" — and summarizing that away is the one truncation that could
// change what the model does next. It also skips the result cache: the answer to
// "what happened to this proposal?" changes when a human decides, so a cached
// copy of the pre-decision reply is exactly the wrong thing to serve.
func (o *Orchestrator) finishProposal(
	ctx context.Context,
	state *runState,
	iteration int,
	call llm.ToolCall,
	result tools.Result,
	finish func(observation, status, errText string, opts toolOutcome) (string, error),
) (string, error) {
	proposal := result.Proposal

	out, err := o.recordProposal(ctx, state, iteration, call, proposal)
	if err != nil {
		return "", err
	}

	switch {
	case out.refused != "":
		// No row was created. Reported as an error status so the trace shows the
		// call did not achieve anything, and as an observation the model can act
		// on rather than a failure that ends the run.
		return finish(fenceSystem(call.Name, out.refused), "error", "write limit reached", toolOutcome{
			rawLength: len(out.refused),
		})

	case out.settled != "":
		return finish(fenceSystem(call.Name, out.settled), "ok", "", toolOutcome{
			rawLength: len(out.settled),
		})

	case out.waiting:
		state.pending = append(state.pending, pausedAction{
			ActionID: out.actionID,
			Source:   proposal.Source,
			Action:   proposal.Action,
			Summary:  proposal.Summary,
		})
		observation := fenceSystem(call.Name, result.Content)
		return finish(observation, "ok", "", toolOutcome{rawLength: len(result.Content)})

	default:
		// Every outcome recordProposal can produce is handled above. Previously
		// this was the `default` arm, which meant a future fifth outcome would
		// silently pause the run and wait for a decision on a proposal that may
		// never have been written. Failing loudly is the safer default for a
		// branch whose mistake is an investigation that never answers.
		return "", fmt.Errorf("agent: recording the %s proposal produced no outcome", call.Name)
	}
}
