package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"cortex/internal/api"
)

const testModel = "gpt-4o-test"

// chatBody is the POST /api/chat success shape.
//
// Day 2 removed `answer`: the handler queues a run and returns immediately, so
// there is nothing to answer with yet. The outcome is read from
// GET /api/runs/{id}.
type chatBody struct {
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
}

func postChat(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
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

// newChatRouter builds a router with the given collaborators.
func newChatRouter(db api.DB, enqueuer api.Enqueuer) http.Handler {
	return api.NewRouter(api.Deps{
		DB:           db,
		Enqueuer:     enqueuer,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})
}

// A bad request is rejected with 400 before any database work or enqueue
// happens, so it needs no Postgres.
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
		{
			// The zero UUID used to be the "not supplied" sentinel, so an explicit
			// all-zeros id was indistinguishable from an absent field and silently
			// started a new conversation instead of being rejected.
			name: "conversation_id is the zero uuid",
			body: `{"conversation_id":"00000000-0000-0000-0000-000000000000","message":"hi"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enqueuer := &stubEnqueuer{}
			db := &stubDB{}
			h := newChatRouter(db, enqueuer)

			rec := postChat(t, h, tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}
			if enqueuer.callCount() != 0 {
				t.Errorf("enqueued %d runs, want 0", enqueuer.callCount())
			}
			if db.begins != 0 {
				t.Errorf("database transaction started %d times, want 0", db.begins)
			}
		})
	}
}

// The happy path is now an enqueue, not an answer: 202, a run in 'pending', the
// user's message stored, and nothing the worker owns written yet.
func TestChatEnqueuesRun(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	enqueuer := &stubEnqueuer{}
	h := newChatRouter(pool, enqueuer)

	rec := postChat(t, h, `{"message":"which Atlas issues are blocked?"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeChat(t, rec)
	conversationID, err := uuid.Parse(got.ConversationID)
	if err != nil {
		t.Fatalf("conversation_id %q is not a uuid: %v", got.ConversationID, err)
	}
	runID, err := uuid.Parse(got.RunID)
	if err != nil {
		t.Fatalf("run_id %q is not a uuid: %v", got.RunID, err)
	}

	// The queued job must name the run that was just committed; a mismatch is a
	// job that will look up a row that does not exist.
	if enqueuer.callCount() != 1 {
		t.Fatalf("enqueued %d runs, want exactly 1", enqueuer.callCount())
	}
	if queued := enqueuer.lastRunID(t); queued != runID {
		t.Errorf("enqueued run %s, want %s (the run in the response)", queued, runID)
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

	// Only the user's message: the assistant's turn is written by the worker.
	var role, content string
	if err := pool.QueryRow(ctx,
		`SELECT role, content FROM messages WHERE conversation_id = $1`, conversationID).
		Scan(&role, &content); err != nil {
		t.Fatalf("load message: %v", err)
	}
	if role != "user" || content != "which Atlas issues are blocked?" {
		t.Errorf("stored message = (%q, %q), want the user's question", role, content)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM messages`); n != 1 {
		t.Errorf("messages = %d, want 1", n)
	}

	// The run is queued, not started, and carries no outcome yet.
	var (
		status   string
		query    string
		model    *string
		answer   *string
		runErr   *string
		finished bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT status, query, model, answer, error, finished_at IS NOT NULL
		 FROM agent_runs WHERE id = $1`, runID).
		Scan(&status, &query, &model, &answer, &runErr, &finished); err != nil {
		t.Fatalf("load agent_run: %v", err)
	}
	if status != "pending" {
		t.Errorf("agent_run status = %q, want %q", status, "pending")
	}
	if query != "which Atlas issues are blocked?" {
		t.Errorf("agent_run query = %q, want the user's message", query)
	}
	if model == nil || *model != testModel {
		t.Errorf("agent_run model = %v, want %q", model, testModel)
	}
	if answer != nil {
		t.Errorf("agent_run answer = %q on a pending run, want null", *answer)
	}
	if runErr != nil {
		t.Errorf("agent_run error = %q on a pending run, want null", *runErr)
	}
	if finished {
		t.Error("agent_run finished_at is set on a pending run")
	}

	// Everything below belongs to the worker, and must not exist yet. Asserting
	// zero here is what proves the handler stopped doing the loop's job.
	if n := queryInt(t, pool, `SELECT count(*) FROM llm_calls`); n != 0 {
		t.Errorf("llm_calls = %d, want 0 (the worker makes the model calls)", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM run_events`); n != 0 {
		t.Errorf("run_events = %d, want 0 (the worker writes the transcript)", n)
	}
}

// A failed enqueue must take the whole transaction with it. Otherwise a run row
// exists that no worker will ever pick up: a question accepted and then silently
// never answered.
func TestChatFailedEnqueueRollsBackEverything(t *testing.T) {
	pool := testPool(t)

	enqueuer := &stubEnqueuer{err: errStubDBQuery}
	h := newChatRouter(pool, enqueuer)

	rec := postChat(t, h, `{"message":"this must not persist"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if enqueuer.callCount() != 1 {
		t.Errorf("enqueue attempted %d times, want 1", enqueuer.callCount())
	}

	for _, table := range []string{"conversations", "messages", "agent_runs"} {
		if n := queryInt(t, pool, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s = %d after a failed enqueue, want 0", table, n)
		}
	}
}

// Passing conversation_id continues the existing conversation instead of
// starting a new one.
func TestChatContinuesExistingConversation(t *testing.T) {
	pool := testPool(t)

	enqueuer := &stubEnqueuer{}
	h := newChatRouter(pool, enqueuer)

	first := decodeChat(t, postChat(t, h, `{"message":"first question"}`))

	rec := postChat(t, h, `{"conversation_id":"`+first.ConversationID+`","message":"second question"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
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
	if n := queryInt(t, pool, `SELECT count(*) FROM messages`); n != 2 {
		t.Errorf("messages = %d, want 2", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs WHERE status = 'pending'`); n != 2 {
		t.Errorf("pending agent_runs = %d, want 2", n)
	}
	if enqueuer.callCount() != 2 {
		t.Errorf("enqueued %d runs, want 2", enqueuer.callCount())
	}
}

// An unknown conversation_id must not create a run; it is a client error, not a
// 202 and not a 500.
func TestChatUnknownConversationIsClientError(t *testing.T) {
	pool := testPool(t)

	enqueuer := &stubEnqueuer{}
	h := newChatRouter(pool, enqueuer)

	rec := postChat(t, h, `{"conversation_id":"`+uuid.NewString()+`","message":"hello"}`)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("status = %d, want a 4xx (body %q)", rec.Code, rec.Body.String())
	}
	if enqueuer.callCount() != 0 {
		t.Errorf("enqueued %d runs, want 0", enqueuer.callCount())
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != 0 {
		t.Errorf("agent_runs = %d, want 0", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM messages`); n != 0 {
		t.Errorf("messages = %d, want 0", n)
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
			enqueuer := &stubEnqueuer{}
			h := newChatRouter(&stubDB{}, enqueuer)

			// An empty message keeps every case off the database: a request
			// that clears the content-type check still stops at validation.
			req := localRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":""}`))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if enqueuer.callCount() != 0 {
				t.Errorf("enqueued %d runs, want 0", enqueuer.callCount())
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
			enqueuer := &stubEnqueuer{}
			db := &stubDB{}
			h := newChatRouter(db, enqueuer)

			rec := postChat(t, h, tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if enqueuer.callCount() != 0 {
				t.Errorf("enqueued %d runs, want 0", enqueuer.callCount())
			}
			if db.begins != 0 {
				t.Errorf("database transactions started = %d, want 0", db.begins)
			}
		})
	}
}
