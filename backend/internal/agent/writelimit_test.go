package agent_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/llm"
)

// The hourly write ceiling.
//
// "Writes are rate-limited per user per hour, counted on executed actions."
// The window is what this file is about: the limit exists so a runaway or
// persuaded agent cannot empty somebody's mailbox into the world, and what
// bounds that is how many writes actually LEFT in the last hour. A proposal can
// sit undecided for up to ACTION_TTL (24h by default), so counting by when a
// write was proposed rather than by when it went out leaves a gap: approve a
// batch of day-old proposals and they all execute now while counting as zero.
//
// The refusal-at-proposal-time and observation wording are covered by
// approval_test.go; this is only about which writes the window includes.

// seedWriteExecutedAgo records a write that was proposed some time ago and
// executed more recently — the shape a proposal approved after a delay takes.
//
// The offsets are relative to fixedNow, the clock the orchestrator under test
// reads, rather than to the database's now(): a fixture aged against a different
// clock than the limit's window would not be testing the window at all.
func seedWriteExecutedAgo(
	t *testing.T,
	pool *pgxpool.Pool,
	userID, runID uuid.UUID,
	key string,
	proposedAgo, executedAgo time.Duration,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agent_actions
		   (agent_run_id, user_id, source, action, proposed_payload, idempotency_key, status,
		    proposed_at, decided_at, executed_at)
		 VALUES ($1, $2, 'gmail', 'gmail.send', '{}'::jsonb, $3, 'executed', $4, $5, $5)`,
		runID, userID, key,
		fixedNow.Add(-proposedAgo), fixedNow.Add(-executedAgo)); err != nil {
		t.Fatalf("seed an executed write: %v", err)
	}
}

// Writes that went out inside the last hour count against the ceiling however
// long ago they were proposed. Otherwise the limit is bypassable by letting
// proposals age: with a 24h TTL, a day's worth of pending approvals can all be
// approved at once and execute inside a minute while counting as zero.
func TestTheHourlyLimitCountsWritesByWhenTheyWentOut(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "reply to Ines confirming we received the sandbox notice")
	sendEmail := newSendEmailTool()

	// Two writes were proposed three hours ago and carried out five minutes
	// ago: two emails left this account within the hour.
	for i := range 2 {
		seedWriteExecutedAgo(t, pool, seeded.userID, seeded.runID,
			fmt.Sprintf("aged-%d", i), 3*time.Hour, 5*time.Minute)
	}

	provider := newFakeProvider(t,
		providerStep{
			kind:      toolsCall,
			toolCalls: []llm.ToolCall{toolCall("call_1", "gmail_send_email", `{"note":"reply"}`)},
		},
		// Reached only if the limit lets the proposal through; scripted so the
		// run can finish either way and the assertion is about the rows.
		providerStep{
			kind: toolsCall,
			text: "I could not propose the email: the hourly write limit was reached.",
		},
	)

	o := newApprovalOrchestrator(t, pool, provider, newRegistry(t, sendEmail),
		approvalOptions{writesPerUserPerHour: 2})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	pending := 0
	for _, row := range loadActions(t, pool, seeded.runID) {
		if row.status == "pending" {
			pending++
		}
	}
	if pending != 0 {
		t.Errorf("pending proposals = %d, want 0 — two writes went out in the last hour, "+
			"which is the ceiling, so the third must be refused before a row is written", pending)
	}
}

// A write that went out more than an hour ago is outside the window and must not
// hold the limit down: the ceiling is per hour, not per day.
func TestTheHourlyLimitReleasesWritesOlderThanTheWindow(t *testing.T) {
	pool := testPool(t)
	seeded := seedRun(t, pool, "reply to Ines confirming we received the sandbox notice")
	sendEmail := newSendEmailTool()

	for i := range 2 {
		seedWriteExecutedAgo(t, pool, seeded.userID, seeded.runID,
			fmt.Sprintf("yesterday-%d", i), 26*time.Hour, 25*time.Hour)
	}

	provider := newFakeProvider(t, providerStep{
		kind:      toolsCall,
		toolCalls: []llm.ToolCall{toolCall("call_1", "gmail_send_email", `{"note":"reply"}`)},
	})

	o := newApprovalOrchestrator(t, pool, provider, newRegistry(t, sendEmail),
		approvalOptions{writesPerUserPerHour: 2})
	if err := o.Run(context.Background(), seeded.runID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	pending := 0
	for _, row := range loadActions(t, pool, seeded.runID) {
		if row.status == "pending" {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("pending proposals = %d, want 1 — yesterday's writes are outside the hourly window",
			pending)
	}
}
