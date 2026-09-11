package jobs_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"cortex/internal/actions"
	"cortex/internal/jobs"
)

// The expiry sweep.
//
// Expiry is enforced twice because either half alone is insufficient. The
// approve query refusing a stale row is what stops the write; this sweep is what
// stops the RUN waiting forever on a decision nobody is ever going to make. A
// question asked yesterday should get an answer, even if the write it proposed
// was never approved.

func runExpirySweep(t *testing.T, worker *jobs.ExpireActionsWorker) {
	t.Helper()
	if err := worker.Work(context.Background(), &river.Job[jobs.ExpireActionsArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: 1},
		Args:   jobs.ExpireActionsArgs{},
	}); err != nil {
		t.Fatalf("expiry sweep: %v", err)
	}
}

func TestExpirySweepRetiresUndecidedProposalsAndLeavesFreshOnesAlone(t *testing.T) {
	pool := testPool(t)
	const ttl = 24 * time.Hour
	now := time.Now().UTC()

	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)
	setStatus(t, pool, fixture.actionID, actions.StatusPending)

	stale := insertAction(t, pool, fixture.userID, fixture.runID, "jira.create_issue",
		`{"project_key":"ATLAS","summary":"Track certification"}`, now.Add(-ttl-time.Hour))
	fresh := insertAction(t, pool, fixture.userID, fixture.runID, "jira.add_comment",
		`{"key":"ATLAS-101","body":"Looking into it"}`, now.Add(-time.Hour))
	// A stale row that was already decided must not be touched: the person got
	// there first, which is the good outcome.
	decided := insertAction(t, pool, fixture.userID, fixture.runID, "notion.create_page",
		`{"parent_page_id":"3c34fab12b9b80d68cfbc64d5b8cebe0","title":"Notes","markdown":"x"}`,
		now.Add(-ttl-time.Hour))
	setStatus(t, pool, decided, actions.StatusRejected)

	worker, err := jobs.NewExpireActionsWorker(pool, ttl, nil, discardLogger())
	if err != nil {
		t.Fatalf("build expiry worker: %v", err)
	}
	runExpirySweep(t, worker)

	if got := loadAction(t, pool, stale).status; got != actions.StatusExpired {
		t.Errorf("a proposal past the time limit has status %q, want expired", got)
	}
	if got := loadAction(t, pool, fresh).status; got != actions.StatusPending {
		t.Errorf("a proposal inside the time limit has status %q, want pending", got)
	}
	if got := loadAction(t, pool, decided).status; got != actions.StatusRejected {
		t.Errorf("an already-decided row was changed to %q by the sweep", got)
	}

	// The run's timeline says what happened, so the agent can be told when it
	// resumes rather than the proposal silently vanishing.
	if !contains(runEventTypes(t, pool, fixture.runID), "action_failed") {
		t.Error("the expired proposal produced no event in the run's timeline")
	}
	var errText *string
	if err := pool.QueryRow(context.Background(),
		`SELECT payload->>'error' FROM run_events WHERE agent_run_id = $1 AND type = 'action_failed'`,
		fixture.runID).Scan(&errText); err != nil {
		t.Fatalf("read the expiry event: %v", err)
	}
	if errText == nil || !strings.Contains(*errText, "expired") {
		t.Errorf("the expiry event says %v, want it to state that nobody decided in time", errText)
	}
}

func TestExpirySweepIsANoOpWhenExpiryIsDisabled(t *testing.T) {
	pool := testPool(t)
	now := time.Now().UTC()

	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)
	ancient := insertAction(t, pool, fixture.userID, fixture.runID, "gmail.send", sendPayload,
		now.Add(-365*24*time.Hour))

	worker, err := jobs.NewExpireActionsWorker(pool, 0, nil, discardLogger())
	if err != nil {
		t.Fatalf("build expiry worker: %v", err)
	}
	runExpirySweep(t, worker)

	for _, id := range []uuid.UUID{fixture.actionID, ancient} {
		if got := loadAction(t, pool, id).status; got != actions.StatusPending {
			t.Errorf("status with expiry disabled = %q, want pending", got)
		}
	}
}
