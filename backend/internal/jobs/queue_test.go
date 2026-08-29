package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/jobs"
)

// TEST-2.4 — enqueue transactionality.
//
// REQ-2.4 requires the chat insert and the enqueue to commit or roll back
// together. That is not a property of our code so much as a property of River's
// InsertTx writing the job row in *our* transaction, so it has to be tested
// against a real Postgres with River's schema applied (`make migrate-test`),
// not against a fake.
//
// The two failure modes being excluded are asymmetric and both bad: a committed
// agent_run with no job is a question that is silently never answered, and a
// committed job for a rolled-back run is a worker looking up a row that does not
// exist.

// testDatabaseLockID namespaces the advisory lock that serializes database-backed
// tests across packages. Any stable constant works; this one spells "cortex".
const testDatabaseLockID = int64(0x636F72746578)

// testPool connects to TEST_DATABASE_URL and truncates. Integration tests SKIP
// (never fail) when the variable is unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database-backed test")
	}
	assertNotDevDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping TEST_DATABASE_URL: %v", err)
	}
	// `go test ./...` runs packages in parallel, and every database-backed test
	// in this repository truncates. A session-level advisory lock serializes
	// them across packages: without it, another package's TRUNCATE lands in the
	// middle of this test and the failures look random.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test database connection: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", testDatabaseLockID); err != nil {
		conn.Release()
		t.Fatalf("lock test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", testDatabaseLockID); err != nil {
			t.Errorf("unlock test database: %v", err)
		}
		conn.Release()
	})

	if _, err := pool.Exec(ctx, "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("truncate application tables: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE river_job"); err != nil {
		t.Fatalf("truncate river_job (has `make migrate-test` run?): %v", err)
	}
	return pool
}

func assertNotDevDatabase(t *testing.T, dsn string) {
	t.Helper()
	name, err := databaseName(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if devDSN := os.Getenv("DATABASE_URL"); devDSN != "" {
		if devName, err := databaseName(devDSN); err == nil && devName == name {
			t.Fatalf("TEST_DATABASE_URL points at the development database %q; tests truncate it", name)
		}
	}
	if !strings.HasSuffix(name, "_test") {
		t.Fatalf("TEST_DATABASE_URL database %q must end in _test; tests truncate it", name)
	}
}

// databaseName extracts the database name without ever echoing the DSN, which
// carries the password.
func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// Deliberately not %w: url.Parse returns *url.Error, whose Error()
		// formats the raw URL — password included — and the caller t.Fatalf's
		// this into captured test output.
		return "", errors.New("invalid DSN")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return "", errors.New("DSN has no database name")
	}
	return name, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func queryInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// TestEnqueueAgentRunCommitsWithCallerWork is the commit half: the job becomes
// visible only when the caller's transaction commits, and it names the run that
// committed with it.
func TestEnqueueAgentRunCommitsWithCallerWork(t *testing.T) {
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
		`INSERT INTO users (email) VALUES ('enqueue@cortex.test') RETURNING id`).Scan(&userID); err != nil {
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

	// Nothing is visible outside the transaction yet — neither half.
	if n := queryInt(t, pool, `SELECT count(*) FROM river_job`); n != 0 {
		t.Errorf("river_job = %d before commit, want 0", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != 0 {
		t.Errorf("agent_runs = %d before commit, want 0", n)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Both halves landed, and the job points at the run.
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs WHERE id = $1`, runID); n != 1 {
		t.Errorf("agent_runs = %d after commit, want 1", n)
	}

	var kind, queueName, state, jobRunID string
	if err := pool.QueryRow(ctx,
		`SELECT kind, queue, state::text, args->>'run_id' FROM river_job`).
		Scan(&kind, &queueName, &state, &jobRunID); err != nil {
		t.Fatalf("load river_job: %v", err)
	}
	if want := (jobs.AgentRunArgs{}).Kind(); kind != want {
		t.Errorf("job kind = %q, want %q", kind, want)
	}
	if queueName != jobs.AgentRunQueue {
		t.Errorf("job queue = %q, want %q", queueName, jobs.AgentRunQueue)
	}
	if state != "available" {
		t.Errorf("job state = %q, want available", state)
	}
	if jobRunID != runID.String() {
		t.Errorf("job args run_id = %q, want %q (a job for a different run looks up a missing row)",
			jobRunID, runID)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM river_job`); n != 1 {
		t.Errorf("river_job = %d after commit, want exactly 1", n)
	}
}

// TestEnqueueAgentRunRollsBackWithCallerWork is the rollback half: if the
// caller's transaction goes away, so does the job. Otherwise a worker would pick
// up a run row that was never committed.
func TestEnqueueAgentRunRollsBackWithCallerWork(t *testing.T) {
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

	var userID, conversationID, runID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ('rollback@cortex.test') RETURNING id`).Scan(&userID); err != nil {
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

	// Something later in the handler fails.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	for _, table := range []string{"users", "conversations", "agent_runs", "river_job"} {
		if n := queryInt(t, pool, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s = %d after rollback, want 0", table, n)
		}
	}
}

// The job payload is deliberately just the run id: the query, conversation and
// history are already rows, and duplicating them would create two sources of
// truth that can disagree after a retry.
func TestAgentRunArgsPayload(t *testing.T) {
	if got := (jobs.AgentRunArgs{}).Kind(); got != "agent_run" {
		t.Errorf("Kind() = %q, want %q (renaming it orphans every queued job)", got, "agent_run")
	}
	if jobs.AgentRunQueue != "agent_runs" {
		t.Errorf("AgentRunQueue = %q, want %q", jobs.AgentRunQueue, "agent_runs")
	}
}

// An insert-only queue (no workers) is what a CLI needs, and Start/Stop on one
// must be a no-op rather than an error.
func TestInsertOnlyQueueStartStop(t *testing.T) {
	pool := testPool(t)
	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("build queue: %v", err)
	}
	if err := queue.Start(context.Background()); err != nil {
		t.Errorf("Start on an insert-only queue: %v", err)
	}
	if err := queue.Stop(context.Background()); err != nil {
		t.Errorf("Stop on an insert-only queue: %v", err)
	}
}

// A queue with no pool is a wiring mistake, and it must fail at construction
// rather than on the first enqueue.
func TestNewRequiresPool(t *testing.T) {
	if _, err := jobs.New(jobs.Config{Logger: discardLogger()}); err == nil {
		t.Error("jobs.New accepted a nil pool, want an error")
	}
}

// EnqueueIndexSource must actually reach River.
//
// This exists because it did not. The first version passed a custom
// UniqueOpts.ByState that omitted 'pending', and River rejects a partial state
// list outright (insert_opts.go: requiredV3states) — so every call returned an
// error, POST /api/admin/index always answered 500, and nothing was ever
// indexed. Nothing caught it: the admin handler's tests drive a stub enqueuer,
// and this file had no coverage of the indexing path at all.
//
// The lesson generalizes past this one bug, which is why the test asserts the
// insert rather than the options: UniqueOpts is validated by River at insert
// time, not by the compiler, so the only way to know the configuration is legal
// is to insert with it.
func TestEnqueueIndexSourceInsertsAJob(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("build queue: %v", err)
	}

	if err := queue.EnqueueIndexSource(ctx, "notion"); err != nil {
		t.Fatalf("EnqueueIndexSource: %v", err)
	}

	var kind, queueName, state string
	var args []byte
	if err := pool.QueryRow(ctx,
		`SELECT kind, queue, state, args FROM river_job ORDER BY id DESC LIMIT 1`).
		Scan(&kind, &queueName, &state, &args); err != nil {
		t.Fatalf("read river_job: %v", err)
	}
	if kind != "index_source" {
		t.Errorf("kind = %q, want index_source", kind)
	}
	if queueName != jobs.IndexSourceQueue {
		t.Errorf("queue = %q, want %q", queueName, jobs.IndexSourceQueue)
	}
	// Decoded rather than string-matched: jsonb is stored normalized, so
	// Postgres renders it as `{"source": "notion"}` with a space and a raw
	// substring check fails on formatting rather than on content.
	var decoded jobs.IndexSourceArgs
	if err := json.Unmarshal(args, &decoded); err != nil {
		t.Fatalf("decode river_job.args: %v", err)
	}
	if decoded.Source != "notion" {
		t.Errorf("args.source = %q, want notion", decoded.Source)
	}

	// The uniqueness guard: a second request for the same source while the first
	// is still pending must collapse into it rather than queue a second crawl.
	if err := queue.EnqueueIndexSource(ctx, "notion"); err != nil {
		t.Fatalf("second EnqueueIndexSource: %v", err)
	}
	var notionJobs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'index_source' AND args->>'source' = 'notion'`).
		Scan(&notionJobs); err != nil {
		t.Fatalf("count river_job: %v", err)
	}
	if notionJobs != 1 {
		t.Errorf("notion index jobs = %d, want 1 (the duplicate must collapse)", notionJobs)
	}

	// A different source is different work and must queue separately.
	if err := queue.EnqueueIndexSource(ctx, "jira"); err != nil {
		t.Fatalf("EnqueueIndexSource(jira): %v", err)
	}
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'index_source'`).Scan(&total); err != nil {
		t.Fatalf("count river_job: %v", err)
	}
	if total != 2 {
		t.Errorf("index jobs = %d, want 2 (one per source)", total)
	}
}
