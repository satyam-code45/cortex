package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/actions"
	"cortex/internal/store"
)

// The execution job: the only code in the system that can cause a side effect
// in somebody else's Jira, Notion or mailbox.
//
// Everything here is about the number of deliveries. The queue retries jobs —
// that is what makes it reliable — so the same job body runs more than once by
// design, and the property that has to hold is that the second run sends
// nothing. The claim is a compare-and-set that happens BEFORE the upstream
// call, so a retry finds nothing to claim.

// Running the execution job twice delivers once. The second attempt finds the
// row already settled and returns without touching the upstream system.
func TestExecutingAnApprovedWriteTwiceDeliversOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	decider := fixture.userID
	if _, err := querier(pool).ApproveAgentAction(ctx, store.ApproveAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		FinalPayload: []byte(sendPayload),
		DecidedBy:    &decider,
		ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	writer := &countingWriter{action: "gmail.send"}
	worker := newWriteWorker(t, pool, writer)

	for attempt := 1; attempt <= 3; attempt++ {
		if err := executeWrite(t, worker, fixture.actionID, 1); err != nil {
			t.Fatalf("execution job run %d: %v", attempt, err)
		}
	}

	if got := writer.deliveries(); got != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 — three runs of the job must send one email", got)
	}

	row := loadAction(t, pool, fixture.actionID)
	if row.status != actions.StatusExecuted {
		t.Errorf("status = %q, want executed", row.status)
	}
	if row.executedAt == nil {
		t.Error("executed_at was not recorded")
	}
	var result map[string]any
	if err := json.Unmarshal(row.result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	detail, _ := result["detail"].(map[string]any)
	if detail == nil || detail["message_id"] != "msg-0001" {
		t.Errorf("result = %v, want what the upstream system returned", result)
	}

	// The run's timeline records the write once too — a second event would say
	// two things happened.
	types := runEventTypes(t, pool, fixture.runID)
	executed := 0
	for _, eventType := range types {
		if eventType == "action_executed" {
			executed++
		}
	}
	if executed != 1 {
		t.Errorf("action_executed events = %d, want 1 (events: %v)", executed, types)
	}
}

// A worker killed mid-attempt leaves the row 'executing'. The retry must not
// send again: whether the first attempt reached the upstream system is
// genuinely unknown, and a second delivery is worse than an honest failure.
func TestARetryAfterACrashMidExecutionDoesNotDeliverAgain(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	decider := fixture.userID
	if _, err := querier(pool).ApproveAgentAction(ctx, store.ApproveAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		FinalPayload: []byte(sendPayload),
		DecidedBy:    &decider,
		ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// The state a killed worker leaves behind: claimed, outcome unrecorded.
	if _, err := querier(pool).BeginExecutingAgentAction(ctx, fixture.actionID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	writer := &countingWriter{action: "gmail.send"}
	worker := newWriteWorker(t, pool, writer)
	if err := executeWrite(t, worker, fixture.actionID, 2); err != nil {
		t.Fatalf("retry after a crash: %v", err)
	}

	if got := writer.deliveries(); got != 0 {
		t.Errorf("deliveries on the retry = %d, want 0 — the write may already have landed", got)
	}
	row := loadAction(t, pool, fixture.actionID)
	if row.status != actions.StatusFailed {
		t.Errorf("status = %q, want failed so the person who approved it is told", row.status)
	}
	if row.errText == nil || !strings.Contains(*row.errText, "interrupted") {
		t.Errorf("error = %v, want it to say the attempt was interrupted and the outcome unknown", row.errText)
	}
	if !contains(runEventTypes(t, pool, fixture.runID), "action_failed") {
		t.Error("the run's timeline has no action_failed event for the interrupted write")
	}
}

// The edited payload is what executes, not the proposal. Both are kept.
func TestTheEditedPayloadIsWhatExecutes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const proposed = `{"from":"me@example.com","to":["ines@example.com"],"subject":"Sandbox notice","body":"We got it."}`
	const edited = `{"from":"me@example.com","to":["ines@example.com"],"subject":"Sandbox notice","body":"Thanks Ines — we have received the sandbox notice and are on it."}`

	fixture := seedProposedAction(t, pool, "gmail.send", proposed)
	decider := fixture.userID
	if _, err := querier(pool).ApproveAgentAction(ctx, store.ApproveAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		FinalPayload: []byte(edited),
		DecidedBy:    &decider,
		ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
	}); err != nil {
		t.Fatalf("approve with an edit: %v", err)
	}

	writer := &countingWriter{action: "gmail.send"}
	worker := newWriteWorker(t, pool, writer)
	if err := executeWrite(t, worker, fixture.actionID, 1); err != nil {
		t.Fatalf("execution job: %v", err)
	}

	if got := writer.deliveries(); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
	sent := writer.delivered(t, 0)
	if body, _ := sent["body"].(string); !strings.Contains(body, "Thanks Ines") {
		t.Errorf("the delivered body was %q, want the edited version", body)
	}
	if body, _ := sent["body"].(string); body == "We got it." {
		t.Error("the proposed body was delivered; the human's edit was ignored")
	}

	// The proposal survives beside the approved version: the audit answer to
	// "did a person change this before it went out?" depends on having both.
	var proposedStored, finalStored []byte
	if err := pool.QueryRow(ctx,
		`SELECT proposed_payload, final_payload FROM agent_actions WHERE id = $1`,
		fixture.actionID).Scan(&proposedStored, &finalStored); err != nil {
		t.Fatalf("read both payloads: %v", err)
	}
	if !strings.Contains(string(proposedStored), "We got it.") {
		t.Errorf("proposed_payload = %s, want the agent's original request", proposedStored)
	}
	if !strings.Contains(string(finalStored), "Thanks Ines") {
		t.Errorf("final_payload = %s, want the edited version", finalStored)
	}
}

// A write nobody approved is never carried out, however the job is triggered.
// There is no path from a pending proposal to a delivery.
func TestAPendingProposalIsNeverExecuted(t *testing.T) {
	pool := testPool(t)
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	writer := &countingWriter{action: "gmail.send"}
	worker := newWriteWorker(t, pool, writer)

	// Every attempt number, including a retry: none of them may claim a row
	// that no person has approved.
	for attempt := 1; attempt <= 3; attempt++ {
		if err := executeWrite(t, worker, fixture.actionID, attempt); err != nil {
			t.Fatalf("execution job on a pending proposal (attempt %d): %v", attempt, err)
		}
	}
	if got := writer.deliveries(); got != 0 {
		t.Fatalf("deliveries = %d, want 0 — a proposal nobody approved was carried out", got)
	}
	if got := loadAction(t, pool, fixture.actionID).status; got != actions.StatusPending {
		t.Errorf("status = %q, want it to stay pending", got)
	}
}

// A rejected proposal is terminal and is never silently retried.
func TestARejectedProposalIsNeverExecuted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	reason := "we already emailed her yesterday"
	decider := fixture.userID
	if _, err := querier(pool).RejectAgentAction(ctx, store.RejectAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		RejectReason: &reason,
		DecidedBy:    &decider,
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	writer := &countingWriter{action: "gmail.send"}
	worker := newWriteWorker(t, pool, writer)
	if err := executeWrite(t, worker, fixture.actionID, 1); err != nil {
		t.Fatalf("execution job on a rejected proposal: %v", err)
	}
	if got := writer.deliveries(); got != 0 {
		t.Errorf("deliveries = %d, want 0 — a declined write went out anyway", got)
	}
	if got := loadAction(t, pool, fixture.actionID).status; got != actions.StatusRejected {
		t.Errorf("status = %q, want rejected", got)
	}
}

// An upstream refusal is recorded on the row and reported, not retried into a
// second delivery attempt.
func TestAnUpstreamFailureIsRecordedRatherThanRetried(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "notion.append_to_page",
		`{"page_id":"3c34fab12b9b80d68cfbc64d5b8cebe0","markdown":"## Notes"}`)

	decider := fixture.userID
	if _, err := querier(pool).ApproveAgentAction(ctx, store.ApproveAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		FinalPayload: []byte(`{"page_id":"3c34fab12b9b80d68cfbc64d5b8cebe0","markdown":"## Notes"}`),
		DecidedBy:    &decider,
		ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	writer := &countingWriter{
		action: "notion.append_to_page",
		fail:   errors.New(`the integration lacks the "Insert content" capability`),
	}
	worker := newWriteWorker(t, pool, writer)
	if err := executeWrite(t, worker, fixture.actionID, 1); err != nil {
		t.Fatalf("execution job: %v", err)
	}
	if got := writer.deliveries(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}

	row := loadAction(t, pool, fixture.actionID)
	if row.status != actions.StatusFailed {
		t.Errorf("status = %q, want failed", row.status)
	}
	if row.errText == nil || !strings.Contains(*row.errText, "Insert content") {
		t.Errorf("error = %v, want the upstream reason a person can act on", row.errText)
	}
	if !contains(runEventTypes(t, pool, fixture.runID), "action_failed") {
		t.Error("the run's timeline has no action_failed event")
	}

	// And a retry of the job does not attempt the write a second time.
	if err := executeWrite(t, worker, fixture.actionID, 2); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := writer.deliveries(); got != 1 {
		t.Errorf("attempts after a retry = %d, want 1 — a failed write is not re-attempted", got)
	}
}

// Switching writes off after an approval revokes it: the executor is simply not
// there, and the action fails with a reason rather than going out.
func TestAnApprovalWhoseCapabilityWasRemovedFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	decider := fixture.userID
	if _, err := querier(pool).ApproveAgentAction(ctx, store.ApproveAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		FinalPayload: []byte(sendPayload),
		DecidedBy:    &decider,
		ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// An empty registry is what a user who switched their Gmail writes off
	// between approving and execution produces.
	worker := newWriteWorker(t, pool)
	if err := executeWrite(t, worker, fixture.actionID, 1); err != nil {
		t.Fatalf("execution job: %v", err)
	}
	row := loadAction(t, pool, fixture.actionID)
	if row.status != actions.StatusFailed {
		t.Errorf("status = %q, want failed", row.status)
	}
	if row.errText == nil || !strings.Contains(*row.errText, "gmail.send") {
		t.Errorf("error = %v, want it to name the capability that is gone", row.errText)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
