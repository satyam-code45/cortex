package jobs_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"cortex/internal/jobs"
)

// Agent-run jobs carry River MaxAttempts 2.
//
// River's default is 25 attempts. An agent run is a paid, non-idempotent
// investigation: every ordinary failure is already recorded as status=failed
// and returns nil to River, so the only retry worth having is the one that
// covers a worker killed mid-run. Left at the default, that same crash could
// restart a paid investigation two dozen times.
func TestEnqueueAgentRunCapsRetryAttempts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("build queue: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit

	var userID, conversationID, runID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ('max-attempts@cortex.test') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO conversations (user_id, title) VALUES ($1, 'q') RETURNING id`, userID).
		Scan(&conversationID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO agent_runs (conversation_id, query, status) VALUES ($1, 'q', 'pending') RETURNING id`,
		conversationID).Scan(&runID); err != nil {
		t.Fatalf("insert agent_run: %v", err)
	}
	if err := queue.EnqueueAgentRun(ctx, tx, runID); err != nil {
		t.Fatalf("EnqueueAgentRun: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var maxAttempts int
	if err := pool.QueryRow(ctx,
		`SELECT max_attempts FROM river_job WHERE kind = 'agent_run'`).Scan(&maxAttempts); err != nil {
		t.Fatalf("load river_job.max_attempts: %v", err)
	}
	if maxAttempts != 2 {
		t.Errorf("agent_run job max_attempts = %d, want 2 (A5: no infinite retries of a paid run)", maxAttempts)
	}
}
