package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/llm"
)

const devUserEmail = "dev@cortex.local"

// stubProvider is a hand-written llm.Provider: no mocking framework, the locked
// stack has no test dependencies. It records every request so tests can assert
// that exactly ONE Generate call is made per chat turn (REQ-1.6).
type stubProvider struct {
	mu       sync.Mutex
	resp     llm.Response
	err      error
	requests []llm.Request
}

var _ llm.Provider = (*stubProvider)(nil)

func (s *stubProvider) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.err != nil {
		return llm.Response{}, s.err
	}
	return s.resp, nil
}

func (s *stubProvider) GenerateWithTools(_ context.Context, _ llm.Request, _ []llm.ToolDef) (llm.Response, error) {
	return llm.Response{}, errors.New("stubProvider: GenerateWithTools must not be called on Day 1")
}

func (s *stubProvider) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("stubProvider: Embed must not be called on Day 1")
}

func (s *stubProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *stubProvider) lastRequest(t *testing.T) llm.Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("provider was never called")
	}
	return s.requests[len(s.requests)-1]
}

// stubDB satisfies api.DB without a real Postgres, for the handler paths that
// return before touching the database and for the health check.
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

// discardLogger keeps test output readable; handlers log errors on purpose.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testPool connects to TEST_DATABASE_URL and truncates the Day 1 tables.
// TEST-1.3: integration tests SKIP (never fail) when the variable is unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database-backed test")
	}
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
