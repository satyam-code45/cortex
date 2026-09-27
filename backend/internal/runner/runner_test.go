package runner_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/actions"
	"cortex/internal/jobs"
	"cortex/internal/runner"
	"cortex/internal/store"
	"cortex/internal/tools/claudecode"
)

// Same lock id as every other database-backed package, so truncating tests
// never interleave across `go test ./...`.
const testDatabaseLockID = int64(0x636F72746578)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database-backed test")
	}
	if !strings.Contains(dsn, "_test") {
		t.Fatal("TEST_DATABASE_URL must name a *_test database; tests truncate it")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", testDatabaseLockID); err != nil {
		conn.Release()
		t.Fatalf("lock test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", testDatabaseLockID)
		conn.Release()
	})
	if _, err := pool.Exec(ctx, "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE river_job"); err != nil {
		t.Fatalf("truncate river_job: %v", err)
	}
	return pool
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func queryInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

const delegation = `{"repo":"cortex","issue_key":"PAY-142","base_branch":"main","prompt":"Fix the bug."}`

// seedRun creates a user with one paused run.
func seedRun(t *testing.T, pool *pgxpool.Pool) (userID, runID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("runner-%s@cortex.test", uuid.NewString())).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`WITH c AS (INSERT INTO conversations (user_id, title) VALUES ($1, 'r') RETURNING id)
		 INSERT INTO agent_runs (conversation_id, query, status, model)
		 SELECT id, 'fix PAY-142', 'awaiting_approval', 'gpt-test' FROM c RETURNING id`, userID).Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return userID, runID
}

// seedAction inserts an action in a given status, stamped the way the guarded
// transitions stamp it.
func seedAction(t *testing.T, pool *pgxpool.Pool, userID, runID uuid.UUID, action, payload, status string) uuid.UUID {
	t.Helper()
	source, _, _ := strings.Cut(action, ".")
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO agent_actions (agent_run_id, user_id, source, action, proposed_payload, final_payload,
		                            idempotency_key, status, decided_at, decided_by)
		 VALUES ($1, $2, $3, $4, $5, CASE WHEN $6 <> 'pending' THEN $5::jsonb END, $7, $6,
		         CASE WHEN $6 <> 'pending' THEN now() END, CASE WHEN $6 <> 'pending' THEN $2::uuid END)
		 RETURNING id`,
		runID, userID, source, action, payload, status, "fixture-"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("insert %s action (%s): %v", action, status, err)
	}
	return id
}

type call struct {
	id      uuid.UUID
	attempt int
}

type recordingPerformer struct {
	mu    sync.Mutex
	calls []call
}

func (p *recordingPerformer) Perform(_ context.Context, id uuid.UUID, attempt int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call{id, attempt})
	return nil
}

func (p *recordingPerformer) got() []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]call(nil), p.calls...)
}

func newRunner(t *testing.T, pool *pgxpool.Pool, perf runner.Performer, userID uuid.UUID) *runner.Runner {
	t.Helper()
	r, err := runner.New(runner.Config{
		Queries:      store.New(pool),
		Performer:    perf,
		UserID:       userID,
		Name:         "laptop",
		Repos:        []string{"cortex"},
		PollInterval: 20 * time.Millisecond,
		Logger:       discardLogger(),
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// The runner only ever touches its owner's claude_code.delegate rows: approved
// ones at attempt 1 on a poll, executing ones at attempt 2 on recovery.
func TestRunnerPerformsOnlyItsOwnersDelegations(t *testing.T) {
	pool := testPool(t)
	owner, run := seedRun(t, pool)
	other, otherRun := seedRun(t, pool)

	approved := seedAction(t, pool, owner, run, "claude_code.delegate", delegation, "approved")
	executing := seedAction(t, pool, owner, run, "claude_code.delegate", delegation, "executing")
	seedAction(t, pool, owner, run, "claude_code.delegate", delegation, "pending")
	seedAction(t, pool, owner, run, "claude_code.delegate", delegation, "rejected")
	seedAction(t, pool, owner, run, "gmail.send", `{"to":["a@b.c"]}`, "approved")
	seedAction(t, pool, owner, run, "gmail.send", `{"to":["a@b.c"]}`, "executing")
	seedAction(t, pool, other, otherRun, "claude_code.delegate", delegation, "approved")
	seedAction(t, pool, other, otherRun, "claude_code.delegate", delegation, "executing")

	cases := []struct {
		name string
		do   func(*runner.Runner) error
		want []call
	}{
		{"PollOnce", func(r *runner.Runner) error { return r.PollOnce(context.Background()) },
			[]call{{approved, 1}}},
		{"RecoverInterrupted", func(r *runner.Runner) error { return r.RecoverInterrupted(context.Background()) },
			[]call{{executing, 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perf := &recordingPerformer{}
			if err := tc.do(newRunner(t, pool, perf, owner)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := perf.got()
			if len(got) != len(tc.want) {
				t.Fatalf("Perform calls = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Perform call %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Run heartbeats the runner online, performs approved work, and takes the row
// down when it stops, so the tool stops being offered.
func TestRunnerRunHeartbeatsPerformsAndGoesOffline(t *testing.T) {
	pool := testPool(t)
	owner, run := seedRun(t, pool)
	approved := seedAction(t, pool, owner, run, "claude_code.delegate", delegation, "approved")

	perf := &recordingPerformer{}
	r := newRunner(t, pool, perf, owner)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(perf.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := perf.got(); len(got) == 0 || got[0] != (call{approved, 1}) {
		t.Errorf("Perform calls = %v, want [%v 1] first", got, approved)
	}
	if n := queryInt(t, pool,
		`SELECT count(*) FROM code_runners WHERE user_id = $1 AND name = 'laptop' AND repos = '{cortex}'
		   AND last_seen_at > now() - interval '1 minute'`, owner); n != 1 {
		t.Errorf("fresh code_runners rows while running = %d, want 1", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM code_runners WHERE user_id = $1`, owner); n != 0 {
		t.Errorf("code_runners rows after shutdown = %d, want 0", n)
	}
}

// A delegation found in 'executing' when the runner starts (the
// previous process died mid-run) is failed as interrupted and Claude Code is
// NOT run again — exercised through the runner and the real worker.
func TestRunnerRecoveryNeverReRunsClaudeCode(t *testing.T) {
	pool := testPool(t)
	owner, run := seedRun(t, pool)
	id := seedAction(t, pool, owner, run, "claude_code.delegate", delegation, "executing")

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "T"}, {"config", "user.email", "t@cortex.test"},
		{"commit", "-q", "--allow-empty", "-m", "initial"},
	} {
		if out, err := gitCmd(repo, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	calls := filepath.Join(t.TempDir(), "calls")
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\necho run >> '" + calls + "'\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"duration_ms":1,"total_cost_usd":0,"session_id":"s","result":"done"}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writer, err := claudecode.NewWriter(claudecode.WriterConfig{
		Repos:       map[string]claudecode.Repo{"cortex": {Path: repo}},
		WorktreeDir: filepath.Join(t.TempDir(), "wt"),
		Bin:         bin,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := actions.NewRegistry(writer)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Config{Pool: pool, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := jobs.NewExecuteWriteWorker(pool,
		func(context.Context, uuid.UUID) (*actions.Registry, error) { return registry, nil },
		queue, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	r := newRunner(t, pool, worker, owner)
	if err := r.RecoverInterrupted(context.Background()); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	// And a later poll must not pick it up either.
	if err := r.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if _, err := os.Stat(calls); err == nil {
		t.Error("Claude Code was run again for a delegation that was already executing")
	}
	var status string
	var errText *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, error FROM agent_actions WHERE id = $1`, id).Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	if status != actions.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if errText == nil || !strings.Contains(*errText, "interrupted") {
		t.Errorf("error = %v, want it to say the run was interrupted", errText)
	}
	if n := queryInt(t, pool,
		`SELECT count(*) FROM river_job WHERE kind = 'resume_run' AND args->>'run_id' = $1`, run.String()); n != 1 {
		t.Errorf("resume_run jobs = %d, want 1 so the run is told", n)
	}
}

// gitCmd runs git in dir, ignoring GIT_* variables a surrounding git hook set;
// they would otherwise aim the command at the repository being committed to.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd
}
