package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
	"cortex/internal/llm"
)

const testModel = "gpt-4o-test"

// chatBody is the POST /api/chat success shape from REQ-1.6.
type chatBody struct {
	ConversationID string `json:"conversation_id"`
	Answer         string `json:"answer"`
	RunID          string `json:"run_id"`
}

func postChat(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeChat(t *testing.T, rec *httptest.ResponseRecorder) chatBody {
	t.Helper()
	var got chatBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return got
}

// TEST-1.3 (validation half): a bad request is rejected with 400 before any
// database work or LLM call happens, so it needs no Postgres.
func TestChatRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty message", body: `{"message":""}`},
		{name: "whitespace-only message", body: `{"message":"   \n\t "}`},
		{name: "message field absent", body: `{}`},
		{name: "message absent with conversation_id", body: `{"conversation_id":"` + uuid.NewString() + `"}`},
		{name: "malformed JSON", body: `{"message":`},
		{name: "conversation_id is not a uuid", body: `{"conversation_id":"not-a-uuid","message":"hi"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &stubProvider{resp: llm.Response{Text: "should not be used"}}
			db := &stubDB{}
			h := api.NewRouter(api.Deps{
				DB:           db,
				Provider:     provider,
				Model:        testModel,
				DevUserEmail: devUserEmail,
				Logger:       discardLogger(),
			})

			rec := postChat(t, h, tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}
			if provider.callCount() != 0 {
				t.Errorf("provider called %d times, want 0", provider.callCount())
			}
			if db.begins != 0 {
				t.Errorf("database transaction started %d times, want 0", db.begins)
			}
		})
	}
}

// TEST-1.3 (happy path): one Generate call, and the turn is fully persisted —
// 2 messages, an agent_run in 'completed', and an llm_call row (REQ-1.6).
func TestChatHappyPathPersistsEverything(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	provider := &stubProvider{resp: llm.Response{Text: "Hello from Cortex.", InputTokens: 17, OutputTokens: 5}}
	h := api.NewRouter(api.Deps{
		DB:           pool,
		Provider:     provider,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})

	rec := postChat(t, h, `{"message":"say hello"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeChat(t, rec)
	if got.Answer != "Hello from Cortex." {
		t.Errorf("answer = %q, want %q", got.Answer, "Hello from Cortex.")
	}
	conversationID, err := uuid.Parse(got.ConversationID)
	if err != nil {
		t.Fatalf("conversation_id %q is not a uuid: %v", got.ConversationID, err)
	}
	runID, err := uuid.Parse(got.RunID)
	if err != nil {
		t.Fatalf("run_id %q is not a uuid: %v", got.RunID, err)
	}

	// Exactly one LLM call, carrying the user's message.
	if provider.callCount() != 1 {
		t.Fatalf("provider called %d times, want exactly 1", provider.callCount())
	}
	req := provider.lastRequest(t)
	if req.Model != testModel {
		t.Errorf("llm request model = %q, want %q", req.Model, testModel)
	}
	if req.System == "" {
		t.Error("llm request has no system prompt")
	}
	if len(req.Messages) != 1 {
		t.Fatalf("llm request had %d messages, want 1: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != llm.RoleUser || req.Messages[0].Content != "say hello" {
		t.Errorf("llm request message = %+v, want user/\"say hello\"", req.Messages[0])
	}

	// One conversation, owned by the dev user.
	var ownerEmail string
	if err := pool.QueryRow(ctx,
		`SELECT u.email FROM conversations c JOIN users u ON u.id = c.user_id WHERE c.id = $1`,
		conversationID).Scan(&ownerEmail); err != nil {
		t.Fatalf("load conversation owner: %v", err)
	}
	if ownerEmail != devUserEmail {
		t.Errorf("conversation owner = %q, want %q", ownerEmail, devUserEmail)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM conversations`); n != 1 {
		t.Errorf("conversations = %d, want 1", n)
	}

	// Two messages: the user's, then the assistant's.
	rows, err := pool.Query(ctx,
		`SELECT role, content FROM messages WHERE conversation_id = $1 ORDER BY created_at, id`, conversationID)
	if err != nil {
		t.Fatalf("load messages: %v", err)
	}
	defer rows.Close()
	type msg struct{ role, content string }
	var messages []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.role, &m.content); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate messages: %v", err)
	}
	want := []msg{{"user", "say hello"}, {"assistant", "Hello from Cortex."}}
	if len(messages) != len(want) {
		t.Fatalf("stored %d messages, want 2: %+v", len(messages), messages)
	}
	for i := range want {
		if messages[i] != want[i] {
			t.Errorf("message %d = %+v, want %+v", i, messages[i], want[i])
		}
	}

	// One agent_run, terminal state 'completed', metrics recorded.
	var (
		status       string
		query        string
		answer       *string
		model        *string
		latencyMs    *int32
		inputTokens  *int32
		outputTokens *int32
		finished     bool
		runErr       *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT status, query, answer, model, latency_ms, input_tokens, output_tokens, finished_at IS NOT NULL, error
		 FROM agent_runs WHERE id = $1`, runID).
		Scan(&status, &query, &answer, &model, &latencyMs, &inputTokens, &outputTokens, &finished, &runErr); err != nil {
		t.Fatalf("load agent_run: %v", err)
	}
	if status != "completed" {
		t.Errorf("agent_run status = %q, want %q", status, "completed")
	}
	if query != "say hello" {
		t.Errorf("agent_run query = %q, want %q", query, "say hello")
	}
	if answer == nil || *answer != "Hello from Cortex." {
		t.Errorf("agent_run answer = %v, want %q", answer, "Hello from Cortex.")
	}
	if model == nil || *model != testModel {
		t.Errorf("agent_run model = %v, want %q", model, testModel)
	}
	if inputTokens == nil || *inputTokens != 17 {
		t.Errorf("agent_run input_tokens = %v, want 17", inputTokens)
	}
	if outputTokens == nil || *outputTokens != 5 {
		t.Errorf("agent_run output_tokens = %v, want 5", outputTokens)
	}
	if latencyMs == nil {
		t.Error("agent_run latency_ms is null, want it recorded")
	}
	if !finished {
		t.Error("agent_run finished_at is null on a completed run")
	}
	if runErr != nil {
		t.Errorf("agent_run error = %q on a completed run, want null", *runErr)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != 1 {
		t.Errorf("agent_runs = %d, want 1", n)
	}

	// One llm_call row attached to the run.
	var (
		purpose       string
		callModel     string
		callIn        *int32
		callOut       *int32
		callLatencyMs *int32
	)
	if err := pool.QueryRow(ctx,
		`SELECT purpose, model, input_tokens, output_tokens, latency_ms FROM llm_calls WHERE agent_run_id = $1`, runID).
		Scan(&purpose, &callModel, &callIn, &callOut, &callLatencyMs); err != nil {
		t.Fatalf("load llm_call: %v", err)
	}
	if purpose == "" {
		t.Error("llm_call purpose is empty")
	}
	if callModel != testModel {
		t.Errorf("llm_call model = %q, want %q", callModel, testModel)
	}
	if callIn == nil || *callIn != 17 || callOut == nil || *callOut != 5 {
		t.Errorf("llm_call tokens = (%v, %v), want (17, 5)", callIn, callOut)
	}
	if callLatencyMs == nil {
		t.Error("llm_call latency_ms is null, want it recorded")
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM llm_calls`); n != 1 {
		t.Errorf("llm_calls = %d, want 1", n)
	}

	assertRunEvents(t, pool, runID, []string{"run_started", "llm_call_completed", "run_completed"})
}

// TEST-1.3: passing conversation_id continues the existing conversation and
// replays its history to the model instead of starting a new one (REQ-1.6).
func TestChatContinuesExistingConversation(t *testing.T) {
	pool := testPool(t)

	provider := &stubProvider{resp: llm.Response{Text: "first answer", InputTokens: 3, OutputTokens: 2}}
	h := api.NewRouter(api.Deps{
		DB:           pool,
		Provider:     provider,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})

	first := decodeChat(t, postChat(t, h, `{"message":"first question"}`))
	provider.resp = llm.Response{Text: "second answer", InputTokens: 9, OutputTokens: 4}

	rec := postChat(t, h, `{"conversation_id":"`+first.ConversationID+`","message":"second question"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	second := decodeChat(t, rec)

	if second.ConversationID != first.ConversationID {
		t.Errorf("conversation_id = %q, want the original %q", second.ConversationID, first.ConversationID)
	}
	if second.RunID == first.RunID {
		t.Error("second turn reused the first run_id, want a new run")
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM conversations`); n != 1 {
		t.Errorf("conversations = %d, want 1", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("users = %d, want 1 (dev user is upserted, not duplicated)", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM messages`); n != 4 {
		t.Errorf("messages = %d, want 4", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs WHERE status = 'completed'`); n != 2 {
		t.Errorf("completed agent_runs = %d, want 2", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM llm_calls`); n != 2 {
		t.Errorf("llm_calls = %d, want 2", n)
	}

	if provider.callCount() != 2 {
		t.Fatalf("provider called %d times, want 2", provider.callCount())
	}
	req := provider.lastRequest(t)
	wantHistory := []llm.Message{
		{Role: llm.RoleUser, Content: "first question"},
		{Role: llm.RoleAssistant, Content: "first answer"},
		{Role: llm.RoleUser, Content: "second question"},
	}
	if len(req.Messages) != len(wantHistory) {
		t.Fatalf("second llm request had %d messages, want %d: %+v", len(req.Messages), len(wantHistory), req.Messages)
	}
	for i, want := range wantHistory {
		if req.Messages[i].Role != want.Role || req.Messages[i].Content != want.Content {
			t.Errorf("history[%d] = %+v, want %+v", i, req.Messages[i], want)
		}
	}
}

// TEST-1.3 (error half): an LLM failure is a 502 and the run is stored as
// 'failed' rather than lost (REQ-1.6).
func TestChatProviderErrorStoresFailedRun(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	provider := &stubProvider{err: errors.New("upstream exploded")}
	h := api.NewRouter(api.Deps{
		DB:           pool,
		Provider:     provider,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})

	rec := postChat(t, h, `{"message":"say hello"}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("error response %v has no \"error\" field", body)
	}

	var (
		runID    uuid.UUID
		status   string
		runErr   *string
		finished bool
		answer   *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT id, status, error, finished_at IS NOT NULL, answer FROM agent_runs`).
		Scan(&runID, &status, &runErr, &finished, &answer); err != nil {
		t.Fatalf("load agent_run: %v", err)
	}
	if status != "failed" {
		t.Errorf("agent_run status = %q, want %q", status, "failed")
	}
	if runErr == nil || *runErr == "" {
		t.Error("agent_run error is empty on a failed run")
	}
	if !finished {
		t.Error("agent_run finished_at is null on a failed run")
	}
	if answer != nil {
		t.Errorf("agent_run answer = %q on a failed run, want null", *answer)
	}

	if n := queryInt(t, pool, `SELECT count(*) FROM messages WHERE role = 'assistant'`); n != 0 {
		t.Errorf("assistant messages = %d, want 0 after a failed call", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM messages WHERE role = 'user'`); n != 1 {
		t.Errorf("user messages = %d, want 1", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM llm_calls`); n != 0 {
		t.Errorf("llm_calls = %d, want 0 after a failed call", n)
	}

	assertRunEvents(t, pool, runID, []string{"run_started", "run_failed"})

	// The stored reason must be the classified summary, never the raw provider
	// error: that text embeds the request URL and the upstream response body.
	if runErr != nil && strings.Contains(*runErr, "http") {
		t.Errorf("agent_run error = %q; must not contain the provider URL", *runErr)
	}
}

// TEST-1.3: an unknown conversation_id must not create a run; it is a client
// error, not a 200 and not a 500.
func TestChatUnknownConversationIsClientError(t *testing.T) {
	pool := testPool(t)

	provider := &stubProvider{resp: llm.Response{Text: "unused"}}
	h := api.NewRouter(api.Deps{
		DB:           pool,
		Provider:     provider,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})

	rec := postChat(t, h, `{"conversation_id":"`+uuid.NewString()+`","message":"hello"}`)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("status = %d, want a 4xx (body %q)", rec.Code, rec.Body.String())
	}
	if provider.callCount() != 0 {
		t.Errorf("provider called %d times, want 0", provider.callCount())
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != 0 {
		t.Errorf("agent_runs = %d, want 0", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM messages`); n != 0 {
		t.Errorf("messages = %d, want 0", n)
	}
}

// run_events is the replayable transcript (CLAUDE.md). The exact event types
// are asserted, not just their ordering: an assertion that only checks
// monotonicity passes vacuously when the handler writes no events at all.
func assertRunEvents(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, want []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT seq, type FROM run_events WHERE agent_run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("load run_events: %v", err)
	}
	defer rows.Close()

	var (
		seqs  []int32
		types []string
	)
	for rows.Next() {
		var (
			seq       int32
			eventType string
		)
		if err := rows.Scan(&seq, &eventType); err != nil {
			t.Fatalf("scan run_event: %v", err)
		}
		seqs = append(seqs, seq)
		types = append(types, eventType)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate run_events: %v", err)
	}

	if !reflect.DeepEqual(types, want) {
		t.Errorf("run_events types = %v, want %v", types, want)
	}
	for i, seq := range seqs {
		if seq != int32(i+1) {
			t.Errorf("run_events seq[%d] = %d, want %d (seq must be gap-free from 1)", i, seq, i+1)
		}
	}
}

// A JSON content type is required so the endpoint is not a CORS-"simple"
// request that any origin can POST without a preflight.
func TestChatRequiresJSONContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{name: "json", contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "json with charset", contentType: "application/json; charset=utf-8", wantStatus: http.StatusBadRequest},
		{name: "text plain is CORS-safelisted", contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "form is CORS-safelisted", contentType: "application/x-www-form-urlencoded", wantStatus: http.StatusUnsupportedMediaType},
		{name: "absent", contentType: "", wantStatus: http.StatusUnsupportedMediaType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := &stubProvider{}
			db := &stubDB{}
			h := api.NewRouter(api.Deps{
				DB: db, Provider: provider, Model: "gpt-4o",
				DevUserEmail: devUserEmail, Logger: discardLogger(),
			})

			// An empty message keeps every case off the database: a request
			// that clears the content-type check still stops at validation.
			req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":""}`))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if provider.callCount() != 0 {
				t.Errorf("provider called %d times, want 0", provider.callCount())
			}
		})
	}
}

// An oversized body must be rejected before it is buffered into memory, and an
// oversized message before it is persisted and replayed on every later turn.
func TestChatRejectsOversizedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "body over the reader cap",
			body:       `{"message":"` + strings.Repeat("a", 128<<10) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name:       "message over the rune cap",
			body:       `{"message":"` + strings.Repeat("b", 8001) + `"}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := &stubProvider{}
			db := &stubDB{}
			h := api.NewRouter(api.Deps{
				DB: db, Provider: provider, Model: "gpt-4o",
				DevUserEmail: devUserEmail, Logger: discardLogger(),
			})

			rec := postChat(t, h, tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if provider.callCount() != 0 {
				t.Errorf("provider called %d times, want 0", provider.callCount())
			}
			if db.begins != 0 {
				t.Errorf("database transactions started = %d, want 0", db.begins)
			}
		})
	}
}
