package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
)

// REQ-2.4's polling endpoint: GET /api/runs/{id} → status, answer, error.
//
// This is the other half of the async switch. Answer and Error are pointers in
// the response so that "still working" is null rather than an empty string: a
// client cannot otherwise tell a run in flight from one that finished with
// nothing to say.

// runBody is the GET /api/runs/{id} response shape.
type runBody struct {
	RunID          string  `json:"run_id"`
	ConversationID string  `json:"conversation_id"`
	Status         string  `json:"status"`
	Answer         *string `json:"answer"`
	Error          *string `json:"error"`
}

func getRun(t *testing.T, h http.Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodGet, "/api/runs/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeRun(t *testing.T, rec *httptest.ResponseRecorder) runBody {
	t.Helper()
	var got runBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return got
}

// seedRunForUser inserts a run in the given state, owned by email.
func seedRunForUser(t *testing.T, pool *pgxpool.Pool, email, status string, answer, runErr *string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var userID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1)
		 ON CONFLICT (email) DO UPDATE SET email = excluded.email
		 RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var conversationID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations (user_id, title) VALUES ($1, 'q') RETURNING id`, userID).
		Scan(&conversationID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	var runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agent_runs (conversation_id, query, status, model, answer, error)
		 VALUES ($1, 'q', $2, $3, $4, $5) RETURNING id`,
		conversationID, status, testModel, answer, runErr).Scan(&runID); err != nil {
		t.Fatalf("insert agent_run: %v", err)
	}
	return conversationID, runID
}

func TestGetRunReportsState(t *testing.T) {
	answer := "ATLAS-1 is blocked on the payments sandbox."
	failure := "llm request failed"

	tests := []struct {
		name       string
		status     string
		answer     *string
		runErr     *string
		wantAnswer *string
		wantError  *string
	}{
		{name: "pending", status: "pending"},
		{name: "running", status: "running"},
		{name: "completed", status: "completed", answer: &answer, wantAnswer: &answer},
		{name: "failed", status: "failed", runErr: &failure, wantError: &failure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			conversationID, runID := seedRunForUser(t, pool, devUserEmail, tt.status, tt.answer, tt.runErr)
			h := newChatRouter(pool, &stubEnqueuer{})

			rec := getRun(t, h, runID.String())
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			got := decodeRun(t, rec)

			if got.RunID != runID.String() {
				t.Errorf("run_id = %q, want %q", got.RunID, runID)
			}
			if got.ConversationID != conversationID.String() {
				t.Errorf("conversation_id = %q, want %q", got.ConversationID, conversationID)
			}
			if got.Status != tt.status {
				t.Errorf("status = %q, want %q", got.Status, tt.status)
			}
			switch {
			case tt.wantAnswer == nil && got.Answer != nil:
				t.Errorf("answer = %q, want null while there is no answer", *got.Answer)
			case tt.wantAnswer != nil && (got.Answer == nil || *got.Answer != *tt.wantAnswer):
				t.Errorf("answer = %v, want %q", got.Answer, *tt.wantAnswer)
			}
			switch {
			case tt.wantError == nil && got.Error != nil:
				t.Errorf("error = %q, want null on a run that did not fail", *got.Error)
			case tt.wantError != nil && (got.Error == nil || *got.Error != *tt.wantError):
				t.Errorf("error = %v, want %q", got.Error, *tt.wantError)
			}
		})
	}
}

// A malformed id is a client error, and an unknown one is a 404 — neither is a
// 500, and neither reveals whether the run exists for someone else.
func TestGetRunRejectsBadIDs(t *testing.T) {
	t.Parallel()

	h := api.NewRouter(api.Deps{
		DB:           &stubDB{},
		Enqueuer:     &stubEnqueuer{},
		Model:        testModel,
		DevUserEmail: devUserEmail,
		Logger:       discardLogger(),
	})

	rec := getRun(t, h, "not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestGetRunUnknownIsNotFound(t *testing.T) {
	pool := testPool(t)
	h := newChatRouter(pool, &stubEnqueuer{})

	rec := getRun(t, h, uuid.NewString())
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
}

// A run belonging to another user must be indistinguishable from one that does
// not exist: the id comes straight from the caller, so ownership is part of the
// lookup, not a check someone can forget.
func TestGetRunIsScopedToTheOwner(t *testing.T) {
	pool := testPool(t)
	_, otherRunID := seedRunForUser(t, pool, "someone-else@cortex.test", "completed", nil, nil)

	h := newChatRouter(pool, &stubEnqueuer{})
	rec := getRun(t, h, otherRunID.String())
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for another user's run (body %q)", rec.Code, rec.Body.String())
	}
}
