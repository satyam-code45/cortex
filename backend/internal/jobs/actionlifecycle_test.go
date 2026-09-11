package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/actions"
	"cortex/internal/store"
)

// The lifecycle as the database enforces it.
//
// Every status change is a conditional UPDATE guarded on the status the caller
// expected to find, so the database — not any Go code — decides who wins a race.
// These tests assert that from the outside: an illegal move matches no row, two
// simultaneous approvals resolve to exactly one, and a proposal past its time
// limit can no longer be approved.

const sendPayload = `{"from":"me@example.com","to":["ines@example.com"],"subject":"Sandbox notice","body":"Received."}`

// transitionCase is one guarded query, named by the move it performs.
type transitionCase struct {
	name string
	to   string
	call func(ctx context.Context, q store.Querier, id uuid.UUID) error
}

// transitionCases is every status-changing query on an action row.
func transitionCases(decider uuid.UUID) []transitionCase {
	return []transitionCase{
		{
			name: "approve",
			to:   actions.StatusApproved,
			call: func(ctx context.Context, q store.Querier, id uuid.UUID) error {
				_, err := q.ApproveAgentAction(ctx, store.ApproveAgentActionParams{
					ID:           id,
					UserID:       decider,
					FinalPayload: []byte(sendPayload),
					DecidedBy:    &decider,
					ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
				})
				return err
			},
		},
		{
			name: "reject",
			to:   actions.StatusRejected,
			call: func(ctx context.Context, q store.Querier, id uuid.UUID) error {
				reason := "already handled"
				_, err := q.RejectAgentAction(ctx, store.RejectAgentActionParams{
					ID:           id,
					UserID:       decider,
					RejectReason: &reason,
					DecidedBy:    &decider,
				})
				return err
			},
		},
		{
			name: "expire",
			to:   actions.StatusExpired,
			call: func(ctx context.Context, q store.Querier, id uuid.UUID) error {
				_, err := q.ExpireAgentAction(ctx, id)
				return err
			},
		},
		{
			name: "begin executing",
			to:   actions.StatusExecuting,
			call: func(ctx context.Context, q store.Querier, id uuid.UUID) error {
				_, err := q.BeginExecutingAgentAction(ctx, id)
				return err
			},
		},
		{
			name: "finish",
			to:   actions.StatusExecuted,
			call: func(ctx context.Context, q store.Querier, id uuid.UUID) error {
				_, err := q.FinishAgentAction(ctx, store.FinishAgentActionParams{
					ID:     id,
					Result: []byte(`{"summary":"sent"}`),
				})
				return err
			},
		},
		{
			name: "fail",
			to:   actions.StatusFailed,
			call: func(ctx context.Context, q store.Querier, id uuid.UUID) error {
				message := "upstream refused"
				_, err := q.FailAgentAction(ctx, store.FailAgentActionParams{ID: id, Error: &message})
				return err
			},
		},
	}
}

// Every move the lifecycle does not allow must match no row, from every status.
// The decider is the database: a Go-side check would be one more place the two
// halves of the gate could disagree.
func TestGuardedTransitionsRefuseEveryIllegalMove(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	q := querier(pool)

	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	for _, from := range allActionStatuses() {
		for _, tc := range transitionCases(fixture.userID) {
			t.Run(from+" to "+tc.name, func(t *testing.T) {
				id := insertAction(t, pool, fixture.userID, fixture.runID, "gmail.send", sendPayload, time.Time{})
				setStatus(t, pool, id, from)

				err := tc.call(ctx, q, id)
				legal := actions.CanTransition(from, tc.to)

				switch {
				case legal && err != nil:
					t.Fatalf("the legal move %s -> %s was refused: %v", from, tc.to, err)
				case !legal && err == nil:
					t.Fatalf("the illegal move %s -> %s was performed", from, tc.to)
				case !legal && !errors.Is(err, pgx.ErrNoRows):
					t.Fatalf("the illegal move %s -> %s failed with %v, want no rows matched", from, tc.to, err)
				}

				want := from
				if legal {
					want = tc.to
				}
				if got := loadAction(t, pool, id).status; got != want {
					t.Errorf("status after %s -> %s = %q, want %q", from, tc.to, got, want)
				}
			})
		}
	}
}

// allActionStatuses is every value a row may hold, as the lifecycle tests walk
// through them.
func allActionStatuses() []string {
	return []string{
		actions.StatusPending, actions.StatusApproved, actions.StatusRejected,
		actions.StatusExecuting, actions.StatusExecuted, actions.StatusFailed,
		actions.StatusExpired,
	}
}

// Two people (or two double-clicks) approving at the same instant must produce
// one approval and one execution, not two emails.
func TestConcurrentApprovalsResolveToOneWinner(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		losers  int
		other   []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decider := fixture.userID
			<-start
			_, err := querier(pool).ApproveAgentAction(ctx, store.ApproveAgentActionParams{
				ID:           fixture.actionID,
				UserID:       decider,
				FinalPayload: []byte(sendPayload),
				DecidedBy:    &decider,
				ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, pgx.ErrNoRows):
				losers++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected approval errors: %v", other)
	}
	if winners != 1 {
		t.Errorf("approvals that matched a row = %d, want exactly 1 — each extra one queues another email", winners)
	}
	if losers != racers-1 {
		t.Errorf("approvals refused = %d, want %d", losers, racers-1)
	}

	// And the same race one step further down: the winner's execution claim is
	// also exactly-once, which is what makes a retried job harmless.
	var claims, refused int
	start = make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := querier(pool).BeginExecutingAgentAction(ctx, fixture.actionID)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				claims++
			} else if errors.Is(err, pgx.ErrNoRows) {
				refused++
			} else {
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected claim errors: %v", other)
	}
	if claims != 1 {
		t.Errorf("execution claims = %d, want exactly 1 — a second claim is a second delivery", claims)
	}
	if refused != racers-1 {
		t.Errorf("execution claims refused = %d, want %d", refused, racers-1)
	}
}

// A proposal older than the time limit can no longer be approved: the age check
// shares one predicate with the status check, so nothing can expire in the gap
// between them.
func TestApprovalRefusesAProposalPastItsTimeLimit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const ttl = 24 * time.Hour
	now := time.Now().UTC()

	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)
	stale := insertAction(t, pool, fixture.userID, fixture.runID, "gmail.send", sendPayload,
		now.Add(-ttl-time.Minute))
	fresh := insertAction(t, pool, fixture.userID, fixture.runID, "gmail.send", sendPayload,
		now.Add(-time.Hour))

	approve := func(id uuid.UUID) error {
		decider := fixture.userID
		_, err := querier(pool).ApproveAgentAction(ctx, store.ApproveAgentActionParams{
			ID:           id,
			UserID:       decider,
			FinalPayload: []byte(sendPayload),
			DecidedBy:    &decider,
			ProposedAt: pgtype.Timestamptz{
				Time:  actions.Cutoff(ttl, now),
				Valid: true,
			},
		})
		return err
	}

	if err := approve(stale); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("approving a proposal past its time limit = %v, want no rows matched", err)
	}
	if got := loadAction(t, pool, stale).status; got == actions.StatusApproved {
		t.Error("a proposal past its time limit was approved, so a stale write could still go out")
	}
	if err := approve(fresh); err != nil {
		t.Errorf("approving a proposal inside its time limit failed: %v", err)
	}
}

// An expired proposal can never execute: it is terminal, so the execution
// claim matches nothing.
func TestAnExpiredProposalCanNeverExecute(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	if _, err := querier(pool).ExpireAgentAction(ctx, fixture.actionID); err != nil {
		t.Fatalf("expire proposal: %v", err)
	}

	writer := &countingWriter{action: "gmail.send"}
	worker := newWriteWorker(t, pool, writer)
	if err := executeWrite(t, worker, fixture.actionID, 1); err != nil {
		t.Fatalf("execution job on an expired proposal returned an error: %v", err)
	}
	if writer.deliveries() != 0 {
		t.Errorf("deliveries = %d, want 0 — an expired proposal must never be carried out", writer.deliveries())
	}
	if got := loadAction(t, pool, fixture.actionID).status; got != actions.StatusExpired {
		t.Errorf("status = %q, want it to stay expired", got)
	}
}

// The rate-limit count includes writes that are still in flight, so a burst of
// approvals cannot slip past a count that only sees finished ones.
func TestExecutedWriteCountIncludesInFlightWrites(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)

	byStatus := map[string]int{
		actions.StatusPending:   1,
		actions.StatusApproved:  1,
		actions.StatusRejected:  1,
		actions.StatusExpired:   1,
		actions.StatusFailed:    1,
		actions.StatusExecuting: 2,
		actions.StatusExecuted:  3,
	}
	setStatus(t, pool, fixture.actionID, actions.StatusPending)
	for status, count := range byStatus {
		for i := 0; i < count; i++ {
			id := insertAction(t, pool, fixture.userID, fixture.runID, "gmail.send", sendPayload, time.Time{})
			setStatus(t, pool, id, status)
		}
	}

	count, err := querier(pool).CountExecutedActionsSince(ctx, store.CountExecutedActionsSinceParams{
		UserID: fixture.userID,
		Since:  pgtype.Timestamptz{Time: time.Now().UTC().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("count executed actions: %v", err)
	}
	// Two executing plus three executed. Nothing pending, rejected, expired or
	// failed counts: none of those reached anybody.
	if count != 5 {
		t.Errorf("writes counted against the hourly limit = %d, want 5 (executing + executed)", count)
	}
}

// A run resumes only when nothing on it is outstanding, and "outstanding"
// includes an approved write that has not landed yet.
func TestUnsettledCountWaitsForWritesStillInFlight(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)
	second := insertAction(t, pool, fixture.userID, fixture.runID, "jira.create_issue",
		`{"project_key":"ATLAS","summary":"Track certification"}`, time.Time{})

	unsettled := func() int64 {
		n, err := querier(pool).CountUnsettledActionsByRun(ctx, fixture.runID)
		if err != nil {
			t.Fatalf("count unsettled actions: %v", err)
		}
		return n
	}

	if got := unsettled(); got != 2 {
		t.Fatalf("unsettled with two pending proposals = %d, want 2", got)
	}
	setStatus(t, pool, second, actions.StatusRejected)
	if got := unsettled(); got != 1 {
		t.Errorf("unsettled after one rejection = %d, want 1", got)
	}
	setStatus(t, pool, fixture.actionID, actions.StatusApproved)
	if got := unsettled(); got != 1 {
		t.Errorf("unsettled with an approved write still to go out = %d, want 1", got)
	}
	setStatus(t, pool, fixture.actionID, actions.StatusExecuting)
	if got := unsettled(); got != 1 {
		t.Errorf("unsettled with a write in flight = %d, want 1", got)
	}
	setStatus(t, pool, fixture.actionID, actions.StatusExecuted)
	if got := unsettled(); got != 0 {
		t.Errorf("unsettled once every proposal is finished = %d, want 0", got)
	}
}

// A terminal row is immutable: no query in the set moves it, and its recorded
// facts survive.
func TestTerminalRowsKeepTheirRecord(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedProposedAction(t, pool, "gmail.send", sendPayload)
	q := querier(pool)

	decider := fixture.userID
	if _, err := q.ApproveAgentAction(ctx, store.ApproveAgentActionParams{
		ID:           fixture.actionID,
		UserID:       decider,
		FinalPayload: []byte(sendPayload),
		DecidedBy:    &decider,
		ProposedAt:   pgtype.Timestamptz{Time: time.Time{}, Valid: true},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := q.BeginExecutingAgentAction(ctx, fixture.actionID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := q.FinishAgentAction(ctx, store.FinishAgentActionParams{
		ID:     fixture.actionID,
		Result: []byte(`{"summary":"the email was sent","detail":{"message_id":"msg-1"}}`),
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	before := loadAction(t, pool, fixture.actionID)
	if before.status != actions.StatusExecuted {
		t.Fatalf("status = %q, want executed", before.status)
	}
	if before.executedAt == nil {
		t.Error("executed_at was not recorded")
	}
	if before.decidedBy == nil || *before.decidedBy != fixture.userID {
		t.Errorf("decided_by = %v, want the approving user", before.decidedBy)
	}

	for _, tc := range transitionCases(fixture.userID) {
		if err := tc.call(ctx, q, fixture.actionID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s on an executed row = %v, want no rows matched", tc.name, err)
		}
	}
	after := loadAction(t, pool, fixture.actionID)
	if after.status != before.status || string(after.result) != string(before.result) {
		t.Errorf("an executed row changed: %+v then %+v", before, after)
	}
	var result map[string]any
	if err := json.Unmarshal(after.result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result["summary"] != "the email was sent" {
		t.Errorf("result summary = %v, want the recorded one", result["summary"])
	}
}
