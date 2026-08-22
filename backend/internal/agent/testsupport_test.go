package agent_test

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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/llm"
	"cortex/internal/store"
	"cortex/internal/tools"
)

// Test support for the agent loop.
//
// TEST-2.1 names the scripted fake Provider as "the key test asset": every loop
// behaviour worth asserting (an answer, a tool call, the iteration cap, a bad
// argument, a failing tool, an oversized result) is a property of what the model
// returns, so the model is the thing that has to be scriptable. Everything here
// is hand-written — the locked stack (CLAUDE.md) carries no test dependencies.
//
// The orchestrator writes its whole transcript through store.Querier, so these
// tests need a real Postgres and skip when TEST_DATABASE_URL is unset, exactly
// like the Day 1 API integration tests.

// ---------------------------------------------------------------------------
// scripted provider
// ---------------------------------------------------------------------------

// callKind distinguishes the two provider entry points the loop uses:
// GenerateWithTools for a reasoning turn, Generate for the utility-model
// summary and for the forced final answer.
type callKind int

const (
	anyCall callKind = iota
	toolsCall
	plainCall
)

func (k callKind) String() string {
	switch k {
	case toolsCall:
		return "GenerateWithTools"
	case plainCall:
		return "Generate"
	default:
		return "any"
	}
}

// providerStep is one queued response.
type providerStep struct {
	// kind, when not anyCall, asserts which provider method the loop reached.
	kind callKind
	// assert runs extra checks on the request that triggered this step. TEST-2.2
	// uses it to assert the summarization call's shape from inside the fake.
	assert func(t *testing.T, call recordedCall)

	text         string
	toolCalls    []llm.ToolCall
	inputTokens  int
	outputTokens int
	err          error
}

// recordedCall is one observed provider invocation.
type recordedCall struct {
	withTools bool
	model     string
	system    string
	messages  []llm.Message
	tools     []llm.ToolDef
}

// fakeProvider replays a queued sequence of responses and records every request
// it was asked to answer.
type fakeProvider struct {
	t *testing.T

	mu    sync.Mutex
	steps []providerStep
	next  int
	calls []recordedCall
}

var _ llm.Provider = (*fakeProvider)(nil)

func newFakeProvider(t *testing.T, steps ...providerStep) *fakeProvider {
	t.Helper()
	return &fakeProvider{t: t, steps: steps}
}

func (p *fakeProvider) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	return p.respond(recordedCall{
		withTools: false,
		model:     req.Model,
		system:    req.System,
		messages:  cloneMessages(req.Messages),
	})
}

func (p *fakeProvider) GenerateWithTools(_ context.Context, req llm.Request, defs []llm.ToolDef) (llm.Response, error) {
	return p.respond(recordedCall{
		withTools: true,
		model:     req.Model,
		system:    req.System,
		messages:  cloneMessages(req.Messages),
		tools:     defs,
	})
}

func (p *fakeProvider) Embed(_ context.Context, _ []string) ([][]float32, error) {
	p.t.Error("fakeProvider: Embed called; the agent loop must not embed")
	return nil, errors.New("fakeProvider: Embed is not scripted")
}

// respond hands back the next scripted step.
//
// An exhausted script is a test failure rather than a silent zero response: it
// means the loop made a call the test did not predict, which is precisely the
// kind of drift these tests exist to catch.
func (p *fakeProvider) respond(call recordedCall) (llm.Response, error) {
	p.mu.Lock()
	index := p.next
	p.next++
	p.calls = append(p.calls, call)
	var step providerStep
	exhausted := index >= len(p.steps)
	if !exhausted {
		step = p.steps[index]
	}
	p.mu.Unlock()

	// The loop asks a model to re-read its own draft answer against the question
	// before accepting it, so every scripted run that ends in an answer takes one
	// more call than its script has steps for. A script that runs out at exactly
	// that call is satisfied here by repeating the draft — which is the real
	// "nothing was missing" path — so a test about dedupe or truncation does not
	// have to carry a step about answer-checking. A script that runs out anywhere
	// else is still the drift these tests exist to catch.
	if exhausted {
		if draft, ok := answerUnderReview(call); ok {
			return llm.Response{Text: draft}, nil
		}
		p.t.Errorf("fakeProvider: unscripted call #%d (%s, %d messages)",
			index+1, kindOf(call), len(call.messages))
		return llm.Response{}, fmt.Errorf("fakeProvider: no scripted response for call %d", index+1)
	}
	if step.kind != anyCall && step.kind != kindOf(call) {
		p.t.Errorf("call #%d reached %s, want %s", index+1, kindOf(call), step.kind)
	}
	if step.assert != nil {
		step.assert(p.t, call)
	}
	if step.err != nil {
		return llm.Response{}, step.err
	}
	return llm.Response{
		Text:         step.text,
		ToolCalls:    step.toolCalls,
		InputTokens:  step.inputTokens,
		OutputTokens: step.outputTokens,
	}, nil
}

// completenessCheckMarker is a distinctive phrase from the injected
// completeness-check instruction (see completenessCheckInstruction in prompt.go).
const completenessCheckMarker = "Before that answer is accepted"

// answerUnderReview reports whether this call is the completeness check, and if
// so returns the draft answer it is reviewing — the assistant turn immediately
// before the injected instruction.
func answerUnderReview(call recordedCall) (string, bool) {
	n := len(call.messages)
	if n < 2 {
		return "", false
	}
	last := call.messages[n-1]
	// Matched by content, not by identity: this file is an external test package
	// and cannot see the unexported instruction. The marker must stay in sync with
	// completenessCheckInstruction in prompt.go — if it drifts, these tests fail
	// loudly with "unscripted call" rather than quietly passing.
	if last.Role != llm.RoleUser || !strings.Contains(last.Content, completenessCheckMarker) {
		return "", false
	}
	draft := call.messages[n-2]
	if draft.Role != llm.RoleAssistant {
		return "", false
	}
	return draft.Content, true
}

func kindOf(call recordedCall) callKind {
	if call.withTools {
		return toolsCall
	}
	return plainCall
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// call returns the i-th recorded call, 0-indexed.
func (p *fakeProvider) call(t *testing.T, i int) recordedCall {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.calls) {
		t.Fatalf("provider call #%d was never made (%d calls total)", i+1, len(p.calls))
	}
	return p.calls[i]
}

func (p *fakeProvider) lastCall(t *testing.T) recordedCall {
	t.Helper()
	p.mu.Lock()
	n := len(p.calls)
	p.mu.Unlock()
	return p.call(t, n-1)
}

func cloneMessages(in []llm.Message) []llm.Message {
	out := make([]llm.Message, len(in))
	copy(out, in)
	return out
}

// ---------------------------------------------------------------------------
// scripted tool
// ---------------------------------------------------------------------------

// fakeTool is a Tool whose behaviour the test dictates. It counts executions,
// which is how dedupe (executed once for two identical calls) and the retry
// policy (executed twice for one transient failure) are observed.
type fakeTool struct {
	name        string
	description string
	schema      json.RawMessage
	// run is called for every execution, with the 1-based attempt number.
	run func(attempt int, args json.RawMessage) (tools.Result, error)

	mu       sync.Mutex
	calls    int
	lastArgs json.RawMessage
}

var _ tools.Tool = (*fakeTool)(nil)

func (f *fakeTool) Name() string { return f.name }

func (f *fakeTool) Description() string {
	if f.description == "" {
		return "a scripted tool for tests"
	}
	return f.description
}

func (f *fakeTool) Schema() json.RawMessage {
	if f.schema == nil {
		return json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	}
	return f.schema
}

func (f *fakeTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	f.mu.Lock()
	f.calls++
	attempt := f.calls
	f.lastArgs = args
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	if f.run == nil {
		return tools.Result{
			Content:  "ok",
			Evidence: []tools.EvidenceItem{{Source: "fake", ExternalID: "FAKE-1"}},
		}, nil
	}
	return f.run(attempt, args)
}

func (f *fakeTool) executions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newRegistry(t *testing.T, list ...tools.Tool) *tools.Registry {
	t.Helper()
	r, err := tools.NewRegistry(list...)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// database fixtures
// ---------------------------------------------------------------------------

// fixedNow pins the clock so the system prompt (which embeds today's date) is
// stable across runs.
var fixedNow = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

// historyMessage is a prior conversation turn to seed.
type historyMessage struct {
	role    string
	content string
}

// seededRun is the fixture one queued run needs.
type seededRun struct {
	userID         uuid.UUID
	conversationID uuid.UUID
	runID          uuid.UUID
	query          string
}

// seedRun inserts a user, a conversation, the prior turns, the user's question
// and a pending agent_run — the exact state POST /api/chat commits.
//
// Written as raw SQL rather than through sqlc so the fixture does not depend on
// the same generated code the orchestrator is being tested on.
func seedRun(t *testing.T, pool *pgxpool.Pool, query string, history ...historyMessage) seededRun {
	t.Helper()
	ctx := context.Background()

	var userID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("agent-%s@cortex.test", uuid.NewString())).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var conversationID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations (user_id, title) VALUES ($1, $2) RETURNING id`,
		userID, query).Scan(&conversationID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	for _, m := range history {
		if _, err := pool.Exec(ctx,
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, $2, $3)`,
			conversationID, m.role, m.content); err != nil {
			t.Fatalf("insert history message: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'user', $2)`,
		conversationID, query); err != nil {
		t.Fatalf("insert question: %v", err)
	}

	var runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agent_runs (conversation_id, query, status, model)
		 VALUES ($1, $2, 'pending', $3) RETURNING id`,
		conversationID, query, testModel).Scan(&runID); err != nil {
		t.Fatalf("insert agent_run: %v", err)
	}

	return seededRun{userID: userID, conversationID: conversationID, runID: runID, query: query}
}

// runRow is the terminal state of an agent_run.
type runRow struct {
	status       string
	answer       *string
	errText      *string
	latencyMS    *int32
	inputTokens  *int32
	outputTokens *int32
	finished     bool
}

func loadRun(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) runRow {
	t.Helper()
	var row runRow
	if err := pool.QueryRow(context.Background(),
		`SELECT status, answer, error, latency_ms, input_tokens, output_tokens, finished_at IS NOT NULL
		 FROM agent_runs WHERE id = $1`, runID).
		Scan(&row.status, &row.answer, &row.errText, &row.latencyMS,
			&row.inputTokens, &row.outputTokens, &row.finished); err != nil {
		t.Fatalf("load agent_run %s: %v", runID, err)
	}
	return row
}

// loadEvents reads the run's transcript in seq order.
func loadEvents(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []store.RunEvent {
	t.Helper()
	events, err := store.New(pool).ListRunEventsByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list run events: %v", err)
	}
	return events
}

// eventTypes lists the event types in order, which is the Day 5 SSE contract.
func eventTypes(events []store.RunEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// decodePayload decodes one event payload into a generic map so assertions read
// the stored JSON, not a Go struct the production code could have changed in
// lockstep with the test.
func decodePayload(t *testing.T, event store.RunEvent) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s payload: %v", event.Type, err)
	}
	return payload
}

// eventsOfType returns every event of the given type, in seq order.
func eventsOfType(events []store.RunEvent, eventType string) []store.RunEvent {
	var out []store.RunEvent
	for _, e := range events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// llmCallRow is one row of llm_calls.
type llmCallRow struct {
	purpose string
	model   string
}

func loadLLMCalls(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []llmCallRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT purpose, model FROM llm_calls WHERE agent_run_id = $1 ORDER BY created_at, id`, runID)
	if err != nil {
		t.Fatalf("query llm_calls: %v", err)
	}
	defer rows.Close()

	var out []llmCallRow
	for rows.Next() {
		var row llmCallRow
		if err := rows.Scan(&row.purpose, &row.model); err != nil {
			t.Fatalf("scan llm_calls: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate llm_calls: %v", err)
	}
	return out
}

// messagesOf returns the conversation's messages in insertion order.
func messagesOf(t *testing.T, pool *pgxpool.Pool, conversationID uuid.UUID) []historyMessage {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT role, content FROM messages WHERE conversation_id = $1 ORDER BY created_at, id`,
		conversationID)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()

	var out []historyMessage
	for rows.Next() {
		var m historyMessage
		if err := rows.Scan(&m.role, &m.content); err != nil {
			t.Fatalf("scan messages: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate messages: %v", err)
	}
	return out
}
