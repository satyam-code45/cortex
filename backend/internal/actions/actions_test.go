package actions_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"cortex/internal/actions"
)

// The write lifecycle, as pure logic.
//
// The status graph is the specification the conditional UPDATEs implement, so
// it is asserted exhaustively rather than by example: every ordered pair of
// statuses is checked, and only the six legal moves are allowed. The rest of
// this file covers the other two facts the gate rests on — that an identical
// proposal derives an identical idempotency key, and that a proposal past its
// time limit is unusable.

// allStatuses is every value the status column may hold.
var allStatuses = []string{
	actions.StatusPending,
	actions.StatusApproved,
	actions.StatusRejected,
	actions.StatusExecuting,
	actions.StatusExecuted,
	actions.StatusFailed,
	actions.StatusExpired,
}

// legalMoves is the whole lifecycle:
//
//	pending -> approved -> executing -> executed | failed
//	pending -> rejected | expired
//
// Anything not in here must be refused, including every move back towards
// 'pending' and every terminal status changing at all.
var legalMoves = map[string]map[string]bool{
	actions.StatusPending:   {actions.StatusApproved: true, actions.StatusRejected: true, actions.StatusExpired: true},
	actions.StatusApproved:  {actions.StatusExecuting: true},
	actions.StatusExecuting: {actions.StatusExecuted: true, actions.StatusFailed: true},
}

func TestOnlyTheOneWayLifecycleMovesAreLegal(t *testing.T) {
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := legalMoves[from][to]
			if got := actions.CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}

	// A status the table has never heard of moves nowhere, rather than
	// defaulting to permissive.
	for _, to := range allStatuses {
		if actions.CanTransition("banana", to) {
			t.Errorf("CanTransition from an unknown status reached %q", to)
		}
	}
}

func TestExecutionCannotBeReachedWithoutAHumanDecision(t *testing.T) {
	// The single most important edge in the graph: nothing goes straight from
	// "the agent asked" to "it is being carried out". Every route to a side
	// effect passes through 'approved', which only a person can set.
	if actions.CanTransition(actions.StatusPending, actions.StatusExecuting) {
		t.Error("a pending proposal can move straight to executing; the approval gate has a bypass")
	}
	if actions.CanTransition(actions.StatusPending, actions.StatusExecuted) {
		t.Error("a pending proposal can move straight to executed; the approval gate has a bypass")
	}
	if actions.CanTransition(actions.StatusExpired, actions.StatusApproved) ||
		actions.CanTransition(actions.StatusExpired, actions.StatusExecuting) {
		t.Error("an expired proposal can still be revived into an execution")
	}
	if actions.CanTransition(actions.StatusRejected, actions.StatusApproved) {
		t.Error("a rejected proposal can be silently retried")
	}
	// A failed write is never returned to 'approved' for another attempt: the
	// upstream call may already have landed.
	if actions.CanTransition(actions.StatusFailed, actions.StatusApproved) ||
		actions.CanTransition(actions.StatusFailed, actions.StatusExecuting) {
		t.Error("a failed write can be re-attempted, which risks a second delivery")
	}
}

func TestTerminalStatusesCanNeverChangeAgain(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{actions.StatusPending, false},
		{actions.StatusApproved, false},
		{actions.StatusExecuting, false},
		{actions.StatusExecuted, true},
		{actions.StatusFailed, true},
		{actions.StatusRejected, true},
		{actions.StatusExpired, true},
		{"banana", false},
	}
	for _, tc := range tests {
		if got := actions.Terminal(tc.status); got != tc.want {
			t.Errorf("Terminal(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
	// Cross-check: a terminal status is exactly one with no legal move out.
	for _, status := range allStatuses {
		hasMove := false
		for _, to := range allStatuses {
			if actions.CanTransition(status, to) {
				hasMove = true
			}
		}
		if actions.Terminal(status) == hasMove {
			t.Errorf("Terminal(%q) = %v but it has moves = %v; the two must be opposites",
				status, actions.Terminal(status), hasMove)
		}
	}
}

func TestIdempotencyKeyIdentifiesTheSameProposalOfTheSameRun(t *testing.T) {
	runA := uuid.New()
	runB := uuid.New()
	const action = "gmail.send"

	key := func(t *testing.T, run uuid.UUID, action string, payload string) string {
		t.Helper()
		k, err := actions.IdempotencyKey(run, action, json.RawMessage(payload))
		if err != nil {
			t.Fatalf("IdempotencyKey: %v", err)
		}
		if k == "" {
			t.Fatal("IdempotencyKey returned an empty key")
		}
		return k
	}

	base := key(t, runA, action, `{"to":["ines@example.com"],"subject":"Sandbox"}`)

	// The point of deriving the key from content: a retried run that proposes
	// the identical write collides with the row it already created instead of
	// asking a person to approve the same email twice. Key order and whitespace
	// are serializer noise and must not change the answer.
	reordered := key(t, runA, action, "{\n  \"subject\": \"Sandbox\",\n  \"to\": [\"ines@example.com\"]\n}")
	if reordered != base {
		t.Errorf("a re-serialized identical payload derived a different key\n got %s\nwant %s", reordered, base)
	}

	// Different content, different intent.
	if changed := key(t, runA, action, `{"to":["ines@example.com"],"subject":"Sandbox!"}`); changed == base {
		t.Error("a changed subject derived the same key, so a second proposal would be swallowed")
	}
	// A genuinely later run asking for the same thing deserves its own approval.
	if other := key(t, runB, action, `{"to":["ines@example.com"],"subject":"Sandbox"}`); other == base {
		t.Error("two different runs derived the same key, so the second could never be approved")
	}
	// The action name is part of the identity too.
	if other := key(t, runA, "jira.add_comment", `{"to":["ines@example.com"],"subject":"Sandbox"}`); other == base {
		t.Error("two different actions derived the same key")
	}

	if _, err := actions.IdempotencyKey(runA, action, json.RawMessage(`{not json`)); err == nil {
		t.Error("IdempotencyKey accepted a payload that is not JSON")
	}
}

func TestExpiryMakesAStaleProposalUndecidable(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	const ttl = 24 * time.Hour

	tests := []struct {
		name       string
		proposedAt time.Time
		ttl        time.Duration
		want       bool
	}{
		{name: "just proposed", proposedAt: now, ttl: ttl, want: false},
		{name: "an hour old", proposedAt: now.Add(-time.Hour), ttl: ttl, want: false},
		{name: "a minute short of the limit", proposedAt: now.Add(-ttl + time.Minute), ttl: ttl, want: false},
		{name: "exactly at the limit", proposedAt: now.Add(-ttl), ttl: ttl, want: true},
		{name: "long past the limit", proposedAt: now.Add(-72 * time.Hour), ttl: ttl, want: true},
		{name: "expiry disabled", proposedAt: now.Add(-72 * time.Hour), ttl: 0, want: false},
		{name: "expiry disabled by a negative limit", proposedAt: now.Add(-72 * time.Hour), ttl: -time.Hour, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := actions.Expired(tc.proposedAt, tc.ttl, now); got != tc.want {
				t.Errorf("Expired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCutoffIsTheOldestStillDecidableProposal(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	if got, want := actions.Cutoff(24*time.Hour, now), now.Add(-24*time.Hour); !got.Equal(want) {
		t.Errorf("Cutoff = %s, want %s", got, want)
	}
	// Disabled expiry has to be expressible as a predicate the approve query
	// can still use: the zero time is before every real proposed_at, so
	// "proposed_at > cutoff" stays true for every row.
	if got := actions.Cutoff(0, now); !got.IsZero() {
		t.Errorf("Cutoff with expiry disabled = %s, want the zero time", got)
	}
	if got := actions.Cutoff(-time.Hour, now); !got.IsZero() {
		t.Errorf("Cutoff with a negative limit = %s, want the zero time", got)
	}
}
