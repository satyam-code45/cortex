package store_test

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// migrationsDir is db/migrations relative to this package.
const migrationsDir = "../../db/migrations"

// TEST-1.4: `goose up` is idempotent — running it twice leaves the schema at the
// same version and `goose status` reports nothing pending. The test provisions a
// throwaway database so the migrations are exercised from scratch.
func TestMigrationsAreIdempotent(t *testing.T) {
	goosePath, dsn := requireGoose(t)
	dir, err := filepath.Abs(migrationsDir)
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}

	scratchDSN := createScratchDatabase(t, dsn)

	firstOut := runGoose(t, goosePath, dir, scratchDSN, "up")
	firstVersion := gooseVersion(t, scratchDSN)
	if firstVersion == "" {
		t.Fatalf("no migration version after first `goose up` (output: %s)", firstOut)
	}

	// Second run must be a no-op, not an error and not a re-apply.
	runGoose(t, goosePath, dir, scratchDSN, "up")
	secondVersion := gooseVersion(t, scratchDSN)
	if secondVersion != firstVersion {
		t.Errorf("version after second `goose up` = %q, want unchanged %q", secondVersion, firstVersion)
	}

	status := runGoose(t, goosePath, dir, scratchDSN, "status")
	if strings.Contains(status, "Pending") {
		t.Errorf("`goose status` reports pending migrations after up:\n%s", status)
	}

	// Applied migrations are recorded exactly once.
	conn := connect(t, scratchDSN)
	var applied int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM goose_db_version WHERE version_id > 0 AND is_applied`).Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("list migration files: %v", err)
	}
	if applied != len(files) {
		t.Errorf("goose_db_version has %d applied rows, want %d (one per migration file)", applied, len(files))
	}
}

// REQ-1.3: migration 001 creates the core tables, the required extensions, the
// status/role CHECK constraints, and the unique (agent_run_id, seq) on run_events.
func TestMigrationSchema(t *testing.T) {
	goosePath, dsn := requireGoose(t)
	dir, err := filepath.Abs(migrationsDir)
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}

	scratchDSN := createScratchDatabase(t, dsn)
	runGoose(t, goosePath, dir, scratchDSN, "up")
	conn := connect(t, scratchDSN)
	ctx := context.Background()

	t.Run("extensions", func(t *testing.T) {
		for _, ext := range []string{"pgcrypto", "vector"} {
			var exists bool
			if err := conn.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, ext).Scan(&exists); err != nil {
				t.Fatalf("check extension %s: %v", ext, err)
			}
			if !exists {
				t.Errorf("extension %q is not enabled", ext)
			}
		}
	})

	tables := map[string][]string{
		"users":         {"id", "email", "created_at"},
		"conversations": {"id", "user_id", "title", "created_at", "updated_at"},
		"messages":      {"id", "conversation_id", "role", "content", "created_at"},
		"agent_runs": {"id", "conversation_id", "query", "status", "model", "answer", "error",
			"latency_ms", "input_tokens", "output_tokens", "created_at", "finished_at"},
		"llm_calls":  {"id", "agent_run_id", "purpose", "model", "input_tokens", "output_tokens", "latency_ms", "created_at"},
		"run_events": {"id", "agent_run_id", "seq", "type", "payload", "created_at"},
	}
	for table, columns := range tables {
		t.Run(table, func(t *testing.T) {
			for _, column := range columns {
				var exists bool
				if err := conn.QueryRow(ctx,
					`SELECT EXISTS (SELECT 1 FROM information_schema.columns
					 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
					table, column).Scan(&exists); err != nil {
					t.Fatalf("check column %s.%s: %v", table, column, err)
				}
				if !exists {
					t.Errorf("column %s.%s is missing", table, column)
				}
			}
		})
	}

	t.Run("users email is unique", func(t *testing.T) {
		if _, err := conn.Exec(ctx, `INSERT INTO users (email) VALUES ('dup@example.com')`); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO users (email) VALUES ('dup@example.com')`); err == nil {
			t.Error("duplicate email was accepted, want a unique violation")
		}
	})

	t.Run("agent_runs status defaults to pending and is constrained", func(t *testing.T) {
		userID := insertUser(t, conn, "runs@example.com")
		var conversationID string
		if err := conn.QueryRow(ctx,
			`INSERT INTO conversations (user_id, title) VALUES ($1, 't') RETURNING id`, userID).Scan(&conversationID); err != nil {
			t.Fatalf("insert conversation: %v", err)
		}
		var status string
		if err := conn.QueryRow(ctx,
			`INSERT INTO agent_runs (conversation_id, query) VALUES ($1, 'q') RETURNING status`, conversationID).Scan(&status); err != nil {
			t.Fatalf("insert agent_run: %v", err)
		}
		if status != "pending" {
			t.Errorf("default status = %q, want %q", status, "pending")
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO agent_runs (conversation_id, query, status) VALUES ($1, 'q', 'bogus')`, conversationID); err == nil {
			t.Error("status 'bogus' was accepted, want a CHECK violation")
		}
		for _, valid := range []string{"pending", "running", "awaiting_approval", "completed", "failed"} {
			if _, err := conn.Exec(ctx,
				`INSERT INTO agent_runs (conversation_id, query, status) VALUES ($1, 'q', $2)`, conversationID, valid); err != nil {
				t.Errorf("status %q was rejected: %v", valid, err)
			}
		}
	})

	t.Run("messages role is constrained", func(t *testing.T) {
		userID := insertUser(t, conn, "msgs@example.com")
		var conversationID string
		if err := conn.QueryRow(ctx,
			`INSERT INTO conversations (user_id, title) VALUES ($1, 't') RETURNING id`, userID).Scan(&conversationID); err != nil {
			t.Fatalf("insert conversation: %v", err)
		}
		for _, role := range []string{"user", "assistant"} {
			if _, err := conn.Exec(ctx,
				`INSERT INTO messages (conversation_id, role, content) VALUES ($1, $2, 'c')`, conversationID, role); err != nil {
				t.Errorf("role %q was rejected: %v", role, err)
			}
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'system', 'c')`, conversationID); err == nil {
			t.Error("role 'system' was accepted, want a CHECK violation")
		}
	})

	t.Run("run_events seq is unique per run", func(t *testing.T) {
		userID := insertUser(t, conn, "events@example.com")
		var conversationID, runID string
		if err := conn.QueryRow(ctx,
			`INSERT INTO conversations (user_id, title) VALUES ($1, 't') RETURNING id`, userID).Scan(&conversationID); err != nil {
			t.Fatalf("insert conversation: %v", err)
		}
		if err := conn.QueryRow(ctx,
			`INSERT INTO agent_runs (conversation_id, query) VALUES ($1, 'q') RETURNING id`, conversationID).Scan(&runID); err != nil {
			t.Fatalf("insert agent_run: %v", err)
		}
		var payload []byte
		if err := conn.QueryRow(ctx,
			`INSERT INTO run_events (agent_run_id, seq, type) VALUES ($1, 1, 'run_started') RETURNING payload`, runID).Scan(&payload); err != nil {
			t.Fatalf("insert run_event: %v", err)
		}
		if string(payload) != "{}" {
			t.Errorf("payload default = %q, want %q", payload, "{}")
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO run_events (agent_run_id, seq, type) VALUES ($1, 1, 'run_completed')`, runID); err == nil {
			t.Error("duplicate (agent_run_id, seq) was accepted, want a unique violation")
		}
	})
}

func insertUser(t *testing.T, conn *pgx.Conn, email string) string {
	t.Helper()
	var id string
	if err := conn.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	return id
}

// requireGoose skips unless both the goose CLI and TEST_DATABASE_URL are
// available (TEST-1.3/TEST-1.4: skip, never fail, without a test database).
func requireGoose(t *testing.T) (goosePath, dsn string) {
	t.Helper()
	dsn = os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping migration test")
	}
	goosePath, err := exec.LookPath("goose")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("goose CLI not on PATH; TEST-1.4 cannot run in CI")
		}
		t.Skip("goose CLI not on PATH; skipping migration test")
	}
	return goosePath, dsn
}

func runGoose(t *testing.T, goosePath, dir, dsn string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-dir", dir, "postgres", dsn}, args...)
	cmd := exec.Command(goosePath, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("goose %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// gooseVersion returns the current schema version recorded in the database.
func gooseVersion(t *testing.T, dsn string) string {
	t.Helper()
	conn := connect(t, dsn)
	var version int64
	if err := conn.QueryRow(context.Background(),
		`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version == 0 {
		return ""
	}
	return fmt.Sprint(version)
}

// createScratchDatabase provisions an empty database next to the one in
// TEST_DATABASE_URL so migrations run from zero, and drops it afterwards.
func createScratchDatabase(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL is not a valid URL: %v", err)
	}

	name := fmt.Sprintf("cortex_mig_%d_%d", time.Now().UnixNano()%1e9, rand.Intn(1000))
	admin := connect(t, dsn)
	ctx := context.Background()
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		conn := connect(t, dsn)
		if _, err := conn.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
			t.Logf("drop scratch database %s: %v", name, err)
		}
	})

	scratch := *u
	scratch.Path = "/" + name
	return scratch.String()
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		// Never print the DSN itself — it carries the password, and this text
		// ends up in CI build logs.
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close(context.Background())
	})
	return conn
}
