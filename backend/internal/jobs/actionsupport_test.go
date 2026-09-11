package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"cortex/internal/actions"
	"cortex/internal/jobs"
	"cortex/internal/store"
	"cortex/internal/tools"
)

// Test support for the approval gate's jobs.
//
// The fixtures are written as raw SQL rather than through sqlc, so the
// assertions do not run through the same generated code the worker under test
// uses. The fake writer counts deliveries, which is how "exactly once" is
// observed: the interesting number in these tests is almost always how many
// times a message left the building.

// actionFixture is a seeded run with one proposed write on it.
type actionFixture struct {
	userID   uuid.UUID
	runID    uuid.UUID
	actionID uuid.UUID
}

// seedProposedAction inserts a user, a conversation, a paused run and one
// pending action row.
func seedProposedAction(t *testing.T, pool *pgxpool.Pool, action string, payload string) actionFixture {
	t.Helper()
	ctx := context.Background()

	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("writes-%s@cortex.test", uuid.NewString())).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var conversationID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations (user_id, title) VALUES ($1, 'writes') RETURNING id`,
		userID).Scan(&conversationID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	var runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agent_runs (conversation_id, query, status, model)
		 VALUES ($1, 'reply to Ines', 'awaiting_approval', 'gpt-test') RETURNING id`,
		conversationID).Scan(&runID); err != nil {
		t.Fatalf("insert agent_run: %v", err)
	}

	return actionFixture{
		userID:   userID,
		runID:    runID,
		actionID: insertAction(t, pool, userID, runID, action, payload, time.Time{}),
	}
}

// insertAction adds one pending action row to an existing run. A zero
// proposedAt means "now".
func insertAction(
	t *testing.T,
	pool *pgxpool.Pool,
	userID, runID uuid.UUID,
	action, payload string,
	proposedAt time.Time,
) uuid.UUID {
	t.Helper()
	source := action[:len(action)-len(actionSuffix(action))]
	// A unique key per fixture row: the real key is derived from the payload,
	// and two fixtures that happen to seed the same write on the same run would
	// collide, which is a property with its own test rather than something the
	// helper should have to think about.
	key := "fixture-" + uuid.NewString()

	var id uuid.UUID
	var err error
	if proposedAt.IsZero() {
		err = pool.QueryRow(context.Background(),
			`INSERT INTO agent_actions (agent_run_id, user_id, source, action, proposed_payload, idempotency_key)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			runID, userID, source, action, payload, key).Scan(&id)
	} else {
		err = pool.QueryRow(context.Background(),
			`INSERT INTO agent_actions (agent_run_id, user_id, source, action, proposed_payload, idempotency_key, proposed_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			runID, userID, source, action, payload, key, proposedAt).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insert agent_action: %v", err)
	}
	return id
}

// actionSuffix returns the part of an action name after the source prefix,
// e.g. ".send" for "gmail.send".
func actionSuffix(action string) string {
	for i := range action {
		if action[i] == '.' {
			return action[i:]
		}
	}
	return ""
}

// actionRow is the subset of a row the assertions care about.
type actionRow struct {
	status       string
	finalPayload []byte
	result       []byte
	errText      *string
	executedAt   *time.Time
	decidedBy    *uuid.UUID
}

func loadAction(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) actionRow {
	t.Helper()
	var row actionRow
	if err := pool.QueryRow(context.Background(),
		`SELECT status, final_payload, result, error, executed_at, decided_by
		 FROM agent_actions WHERE id = $1`, id).
		Scan(&row.status, &row.finalPayload, &row.result, &row.errText, &row.executedAt, &row.decidedBy); err != nil {
		t.Fatalf("load agent_action %s: %v", id, err)
	}
	return row
}

// setStatus forces a row into a state a test needs to start from, bypassing
// the guarded transitions on purpose — it is standing in for a previous
// process, not exercising the lifecycle.
//
// It stamps the decision and execution times the guarded transitions always
// stamp (ApproveAgentAction and ExpireAgentAction set decided_at;
// FinishAgentAction sets executed_at). Status alone would produce a row that
// cannot exist in production, and the hourly write limit counts on
// COALESCE(executed_at, decided_at) — so a decided row left with both NULL
// would silently fall outside every window it is meant to be inside.
func setStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent_actions
		    SET status      = $2,
		        decided_at  = CASE WHEN $2 = 'pending' THEN NULL
		                           ELSE COALESCE(decided_at, now()) END,
		        executed_at  = CASE WHEN $2 = 'executed' THEN COALESCE(executed_at, now())
		                           ELSE executed_at END
		  WHERE id = $1`, id, status); err != nil {
		t.Fatalf("force status %s: %v", status, err)
	}
}

// runEventTypes lists a run's event types in order.
func runEventTypes(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT type FROM run_events WHERE agent_run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("query run_events: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			t.Fatalf("scan run_events: %v", err)
		}
		out = append(out, eventType)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate run_events: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// fake writer
// ---------------------------------------------------------------------------

// countingWriter is a tools.Writer that records every delivery instead of
// performing one. The count is the assertion these tests are built around.
type countingWriter struct {
	action string
	// fail, when set, is returned instead of an outcome — the upstream-refused
	// path, which must be recorded rather than retried.
	fail error

	mu       sync.Mutex
	payloads []json.RawMessage
}

var _ tools.Writer = (*countingWriter)(nil)

func (w *countingWriter) Action() string { return w.action }

func (w *countingWriter) Execute(_ context.Context, payload json.RawMessage) (tools.WriteOutcome, error) {
	w.mu.Lock()
	w.payloads = append(w.payloads, append(json.RawMessage(nil), payload...))
	w.mu.Unlock()
	if w.fail != nil {
		return tools.WriteOutcome{}, w.fail
	}
	return tools.WriteOutcome{
		Summary: "the email was sent",
		Detail:  map[string]any{"message_id": "msg-0001"},
	}, nil
}

// deliveries is how many times this writer was asked to perform its action.
func (w *countingWriter) deliveries() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.payloads)
}

// delivered returns the payload of the i-th delivery.
func (w *countingWriter) delivered(t *testing.T, i int) map[string]any {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if i >= len(w.payloads) {
		t.Fatalf("delivery #%d never happened (%d deliveries)", i+1, len(w.payloads))
	}
	var out map[string]any
	if err := json.Unmarshal(w.payloads[i], &out); err != nil {
		t.Fatalf("decode delivered payload: %v", err)
	}
	return out
}

// newWriteWorker builds the execution worker over a registry holding writers.
// The queue is nil: enqueueing a resume needs River's schema and a client, and
// none of these tests are about the resume being queued.
func newWriteWorker(t *testing.T, pool *pgxpool.Pool, writers ...tools.Writer) *jobs.ExecuteWriteWorker {
	t.Helper()
	registry, err := actions.NewRegistry(writers...)
	if err != nil {
		t.Fatalf("build writer registry: %v", err)
	}
	worker, err := jobs.NewExecuteWriteWorker(pool,
		func(context.Context, uuid.UUID) (*actions.Registry, error) { return registry, nil },
		nil, discardLogger())
	if err != nil {
		t.Fatalf("build execute-write worker: %v", err)
	}
	return worker
}

// executeWrite runs one attempt of the execution job.
func executeWrite(t *testing.T, worker *jobs.ExecuteWriteWorker, actionID uuid.UUID, attempt int) error {
	t.Helper()
	return worker.Work(context.Background(), &river.Job[jobs.ExecuteWriteArgs]{
		JobRow: &rivertype.JobRow{ID: int64(attempt), Attempt: attempt},
		Args:   jobs.ExecuteWriteArgs{ActionID: actionID},
	})
}

// querier is the generated query set over the pool, for the tests that drive
// the guarded transitions directly.
func querier(pool *pgxpool.Pool) store.Querier { return store.New(pool) }
