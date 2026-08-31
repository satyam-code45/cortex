package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/connections"
	"cortex/internal/keys"
	"cortex/internal/tools"
)

// Test support for the connections package.
//
// The Service is the one path to user_connections and everything it stores is
// ciphertext, so these tests need a real Postgres. They follow the repository
// pattern: skip when TEST_DATABASE_URL is unset, refuse the dev database, and
// serialize truncation across packages with the shared advisory lock.

// testEncryptionSecret is a valid LLM_KEY_ENCRYPTION_SECRET (64 hex chars) —
// source credentials share the same encryption envelope as LLM keys.
const testEncryptionSecret = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func newTestCipher(t *testing.T) *keys.Cipher {
	t.Helper()
	cipher, err := keys.NewCipher(testEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	return cipher
}

func newTestService(t *testing.T, pool *pgxpool.Pool) *connections.Service {
	t.Helper()
	return connections.NewService(pool, newTestCipher(t))
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// insertUser creates one user row and returns its id.
func insertUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("connections-%s@cortex.test", uuid.NewString())).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// stubTool is a named do-nothing tool for building demo registries.
type stubTool struct{ name string }

var _ tools.Tool = (*stubTool)(nil)

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "a stub tool for connections tests" }
func (s *stubTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
}
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.Result{Content: "stub", Evidence: []tools.EvidenceItem{}}, nil
}

// newDemoRegistry builds a stand-in for the full demo-workspace registry,
// including the knowledge-base tool that must exist ONLY in demo mode.
func newDemoRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	r, err := tools.NewRegistry(
		&stubTool{name: "jira_search_issues"},
		&stubTool{name: "notion_search"},
		&stubTool{name: "gmail_search"},
		&stubTool{name: "search_knowledge_base"},
	)
	if err != nil {
		t.Fatalf("build demo registry: %v", err)
	}
	return r
}

// queryInt runs a scalar aggregate against the test database.
func queryInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// testDatabaseLockID matches the constant every other database-backed test
// package uses, so truncation is serialized across `go test ./...`.
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

	// users cascades to user_connections.
	if _, err := pool.Exec(ctx, "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("truncate test database: %v", err)
	}
	return pool
}

// assertNotDevDatabase refuses to run against the development database, which
// testPool would truncate.
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

// databaseName extracts the database name from a DSN without ever echoing the
// DSN (it carries the password).
func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("invalid DSN")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return "", errors.New("DSN has no database name")
	}
	return name, nil
}
