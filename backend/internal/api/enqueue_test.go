package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
	"cortex/internal/jobs"
)

// TEST-2.4 — enqueue transactionality, end to end through the handler.
//
// The chat_test cases drive the handler with a stub enqueuer, which proves the
// handler's own rollback behaviour but not that a *real* River insert joins the
// same transaction. This file closes that gap: the handler is wired to the
// actual jobs.Queue, so a 202 must leave exactly one river_job row naming the
// run in the response body — the property REQ-2.4 is really asking for.

// truncateRiverJobs clears the queue table. testPool only truncates the
// application tables, and River owns its own schema (REQ-2.4 amendment).
func truncateRiverJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "TRUNCATE river_job"); err != nil {
		t.Fatalf("truncate river_job (has `make migrate-test` run?): %v", err)
	}
}

func TestChatEnqueuesThroughRiver(t *testing.T) {
	pool := testPool(t)
	truncateRiverJobs(t, pool)
	ctx := context.Background()

	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("build queue: %v", err)
	}

	h := api.NewRouter(api.Deps{
		DB:           pool,
		Enqueuer:     queue,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})

	rec := postChat(t, h, `{"message":"which Atlas issues are blocked?"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeChat(t, rec)
	runID, err := uuid.Parse(got.RunID)
	if err != nil {
		t.Fatalf("run_id %q is not a uuid: %v", got.RunID, err)
	}

	// Exactly one job, on the agent_runs queue, for this run.
	var kind, queueName, jobRunID string
	if err := pool.QueryRow(ctx,
		`SELECT kind, queue, args->>'run_id' FROM river_job`).Scan(&kind, &queueName, &jobRunID); err != nil {
		t.Fatalf("load river_job: %v", err)
	}
	if want := (jobs.AgentRunArgs{}).Kind(); kind != want {
		t.Errorf("job kind = %q, want %q", kind, want)
	}
	if queueName != jobs.AgentRunQueue {
		t.Errorf("job queue = %q, want %q", queueName, jobs.AgentRunQueue)
	}
	if jobRunID != runID.String() {
		t.Errorf("job run_id = %q, want the run in the response %q", jobRunID, runID)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM river_job`); n != 1 {
		t.Errorf("river_job = %d, want exactly 1", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs WHERE id = $1 AND status = 'pending'`, runID); n != 1 {
		t.Errorf("pending agent_runs for %s = %d, want 1", runID, n)
	}
}

// A rejected request must leave the queue untouched, not just the tables.
func TestChatRejectedRequestEnqueuesNothingInRiver(t *testing.T) {
	pool := testPool(t)
	truncateRiverJobs(t, pool)

	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("build queue: %v", err)
	}
	h := api.NewRouter(api.Deps{
		DB:           pool,
		Enqueuer:     queue,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})

	rec := postChat(t, h, `{"conversation_id":"`+uuid.NewString()+`","message":"hello"}`)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want a 4xx (body %q)", rec.Code, rec.Body.String())
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM river_job`); n != 0 {
		t.Errorf("river_job = %d after a rejected request, want 0", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != 0 {
		t.Errorf("agent_runs = %d after a rejected request, want 0", n)
	}
}
