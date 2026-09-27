package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Migration 00009 up and down. The source CHECK accepts
// claude_code and still rejects an unknown source; code_runners exists with
// its (user_id, name) uniqueness; down restores the old CHECK and drops the table.
func TestClaudeCodeDelegationMigrationUpAndDown(t *testing.T) {
	goosePath, dsn := requireGoose(t)
	dir, err := filepath.Abs(migrationsDir)
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	scratchDSN := createScratchDatabase(t, dsn)
	runGoose(t, goosePath, dir, scratchDSN, "up")
	conn := connect(t, scratchDSN)
	ctx := context.Background()

	userID := insertUser(t, conn, "delegation-migration@cortex.test")
	var runID string
	if err := conn.QueryRow(ctx,
		`WITH c AS (INSERT INTO conversations (user_id, title) VALUES ($1, 'm') RETURNING id)
		 INSERT INTO agent_runs (conversation_id, query, status, model)
		 SELECT id, 'q', 'awaiting_approval', 'gpt-test' FROM c RETURNING id`, userID).Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	insertAction := func(conn *pgx.Conn, source, key string) error {
		_, err := conn.Exec(ctx,
			`INSERT INTO agent_actions (agent_run_id, user_id, source, action, proposed_payload, idempotency_key)
			 VALUES ($1, $2, $3, $3 || '.do', '{}', $4)`, runID, userID, source, key)
		return err
	}

	for _, tc := range []struct {
		source string
		ok     bool
	}{
		{"claude_code", true},
		{"gmail", true},
		{"jira", true},
		{"notion", true},
		{"slack", false},
		{"", false},
	} {
		err := insertAction(conn, tc.source, "k-up-"+tc.source)
		if tc.ok && err != nil {
			t.Errorf("source %q rejected after up: %v", tc.source, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("source %q accepted after up, want the CHECK to reject it", tc.source)
		}
	}

	if _, err := conn.Exec(ctx,
		`INSERT INTO code_runners (user_id, name, repos, last_seen_at) VALUES ($1, 'laptop', '{cortex}', now())`,
		userID); err != nil {
		t.Fatalf("insert code_runner: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO code_runners (user_id, name, repos, last_seen_at) VALUES ($1, 'laptop', '{other}', now())`,
		userID); err == nil {
		t.Error("a second code_runner with the same (user_id, name) was accepted, want a unique violation")
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO code_runners (user_id, name, last_seen_at) VALUES ($1, 'no-repos', now())`,
		userID); err == nil {
		t.Error("a code_runner with NULL repos was accepted, want NOT NULL")
	}

	runGoose(t, goosePath, dir, scratchDSN, "down-to", "8")
	down := connect(t, scratchDSN)
	var exists bool
	if err := down.QueryRow(ctx, `SELECT to_regclass('public.code_runners') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check code_runners: %v", err)
	}
	if exists {
		t.Error("code_runners still exists after down")
	}
	if err := insertAction(down, "claude_code", "k-down-cc"); err == nil {
		t.Error("source claude_code accepted after down, want the old CHECK back")
	}
	if err := insertAction(down, "gmail", "k-down-gmail"); err != nil {
		t.Errorf("source gmail rejected after down: %v", err)
	}

	// And back up again, cleanly.
	runGoose(t, goosePath, dir, scratchDSN, "up")
	if err := connect(t, scratchDSN).QueryRow(ctx,
		`SELECT to_regclass('public.code_runners') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Errorf("code_runners missing after re-up (err %v)", err)
	}
}
