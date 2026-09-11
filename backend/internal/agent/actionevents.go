package agent

import (
	"context"
	"time"

	"github.com/google/uuid"

	"cortex/internal/store"
)

// The approval-gate events written from outside the loop.
//
// A decision is made by an HTTP handler and a write is performed by a job, but
// both belong in the RUN's timeline — the trace panel shows one story, and a
// pause with no visible decision is a hole in it. So the event payloads stay
// owned by this package, which is also the package that replays them, and these
// three functions are the way in.
//
// Exported for the same reason ReconstructTranscript is: writing to run_events
// is production behaviour shared across packages, not an internal detail of the
// loop. Keeping the payload structs private means the shape cannot drift out of
// step with the replay that has to understand it.

// RecordActionDecided appends the event for a human's approve or reject.
//
// edited says whether the approved payload differs from the proposal. It is
// computed by the caller, which has both versions in hand, and stored rather
// than derived later so the audit answer to "did a person change this?" survives
// even if the payloads are ever compacted.
func RecordActionDecided(
	ctx context.Context,
	q store.Querier,
	runID, actionID uuid.UUID,
	action, status, reason string,
	decidedBy *uuid.UUID,
	decidedAt time.Time,
	edited bool,
) error {
	return appendEvent(ctx, q, runID, EventActionDecided, actionDecidedPayload{
		ActionID:  actionID,
		Action:    action,
		Status:    status,
		Reason:    reason,
		DecidedBy: decidedBy,
		DecidedAt: decidedAt.UTC(),
		Edited:    edited,
	})
}

// RecordActionExecuted appends the event for a write that landed.
func RecordActionExecuted(
	ctx context.Context,
	q store.Querier,
	runID, actionID uuid.UUID,
	action, summary string,
	result map[string]any,
) error {
	return appendEvent(ctx, q, runID, EventActionExecuted, actionExecutedPayload{
		ActionID: actionID,
		Action:   action,
		Summary:  summary,
		Result:   result,
	})
}

// RecordActionFailed appends the event for a write that was approved and did not
// happen.
func RecordActionFailed(
	ctx context.Context,
	q store.Querier,
	runID, actionID uuid.UUID,
	action, errText string,
) error {
	return appendEvent(ctx, q, runID, EventActionFailed, actionFailedPayload{
		ActionID: actionID,
		Action:   action,
		Error:    errText,
	})
}
