package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
)

const devUserEmail = "dev@cortex.local"

// stubEnqueuer is a hand-written api.Enqueuer: no mocking framework, the locked
// stack has no test dependencies.
//
// It replaces Day 1's stubProvider. Since the chat handler became asynchronous
// it makes no LLM calls at all, and the thing worth asserting is whether a run
// was queued — and, when err is set, that a failed enqueue takes the whole
// transaction down with it.
type stubEnqueuer struct {
	mu     sync.Mutex
	err    error
	runIDs []uuid.UUID
}

var _ api.Enqueuer = (*stubEnqueuer)(nil)

func (s *stubEnqueuer) EnqueueAgentRun(_ context.Context, _ pgx.Tx, runID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runIDs = append(s.runIDs, runID)
	return s.err
}

func (s *stubEnqueuer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runIDs)
}

func (s *stubEnqueuer) lastRunID(t *testing.T) uuid.UUID {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runIDs) == 0 {
		t.Fatal("enqueuer was never called")
	}
	return s.runIDs[len(s.runIDs)-1]
}

// stubDB satisfies api.DB without a real Postgres, for the handler paths that
// return before touching the database and for the health check.
//
// Every query method fails loudly rather than returning empty results: these
// tests assert that a request was rejected *before* any database work, and a
// silently-succeeding stub would let a regression through.
type stubDB struct {
	pingErr  error
	beginErr error
	begins   int
}

func (d *stubDB) Begin(_ context.Context) (pgx.Tx, error) {
	d.begins++
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return nil, errors.New("stubDB: Begin is not supported; this test must not reach the database")
}

func (d *stubDB) Ping(_ context.Context) error { return d.pingErr }

func (d *stubDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errStubDBQuery
}

func (d *stubDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errStubDBQuery
}

func (d *stubDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errRow{}
}

// errStubDBQuery is returned by every stubDB query path.
var errStubDBQuery = errors.New("stubDB: queries are not supported; this test must not reach the database")

// errRow is a pgx.Row that always fails, so a QueryRow reached by mistake
// surfaces as a test failure rather than as a zero value.
type errRow struct{}

func (errRow) Scan(_ ...any) error { return errStubDBQuery }

// localRequest builds a request that carries a Host the server answers to.
//
// httptest.NewRequest defaults Host to "example.com", which the hostCheck
// middleware correctly rejects with 421. Real clients dial localhost, so tests
// have to as well — otherwise every test would be exercising the rebinding
// guard instead of the handler it is about.
func localRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "localhost:8080"
	return req
}

// discardLogger keeps test output readable; handlers log errors on purpose.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testDatabaseLockID namespaces the advisory lock that serializes database-backed
// tests across packages. Any stable constant works; this one spells "cortex".
const testDatabaseLockID = int64(0x636F72746578)

// testPool connects to TEST_DATABASE_URL and truncates the Day 1 tables.
// TEST-1.3: integration tests SKIP (never fail) when the variable is unset.
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
	// users cascades to conversations → messages/agent_runs → llm_calls/run_events.
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
		t.Fatalf("truncate test database: %v", err)
	}
	return pool
}

// queryInt runs a scalar count/aggregate against the test database. Verification
// SQL lives in the test rather than in db/queries so the assertions do not go
// through the same sqlc code the handler is being tested on.
func queryInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// assertNotDevDatabase refuses to run against the development database.
//
// testPool truncates users, which cascades to every table Day 1 writes. The
// natural way to "fix" a missing test database is to repoint
// TEST_DATABASE_URL at DATABASE_URL, and that would silently wipe real data on
// the next test run — so make it a hard failure instead of a comment in
// .env.example.
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

// databaseName extracts the database name from a Postgres DSN. The error never
// includes the DSN itself, which carries the password.
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
		return "", fmt.Errorf("DSN has no database name")
	}
	return name, nil
}
