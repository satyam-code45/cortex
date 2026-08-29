package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TEST-5.4 — the conversation endpoints (REQ-5.6, amendments A1 + A2).
//
// GET /api/conversations lists the dev user's conversations newest-updated
// first; GET /api/conversations/{id}/messages returns the thread in order with
// each message's agent_run_id (null for user messages and pre-migration rows);
// unknown and foreign conversations 404 identically.

// conversationBody mirrors one row of the GET /api/conversations response.
type conversationBody struct {
	ID        string  `json:"id"`
	Title     *string `json:"title"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// messageBody mirrors one row of GET /api/conversations/{id}/messages.
type messageBody struct {
	ID         string  `json:"id"`
	Role       string  `json:"role"`
	Content    string  `json:"content"`
	AgentRunID *string `json:"agent_run_id"`
	CreatedAt  string  `json:"created_at"`
}

// seedUserID inserts (or reuses) a user row.
func seedUserID(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1)
		 ON CONFLICT (email) DO UPDATE SET email = excluded.email
		 RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	return id
}

// seedConversationAt inserts a conversation with a controlled updated_at, so
// ordering assertions do not depend on insert timing.
func seedConversationAt(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, title string, updatedAt time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations (user_id, title, updated_at) VALUES ($1, $2, $3) RETURNING id`,
		userID, title, updatedAt).Scan(&id); err != nil {
		t.Fatalf("insert conversation %q: %v", title, err)
	}
	return id
}

// seedMessageAt inserts one message with a controlled created_at and an
// optional run link.
func seedMessageAt(t *testing.T, pool *pgxpool.Pool, conversationID uuid.UUID, role, content string, agentRunID *uuid.UUID, createdAt time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO messages (conversation_id, role, content, agent_run_id, created_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		conversationID, role, content, agentRunID, createdAt).Scan(&id); err != nil {
		t.Fatalf("insert %s message: %v", role, err)
	}
	return id
}

// seedAgentRun inserts a completed run in the conversation, for messages to
// link to.
func seedAgentRun(t *testing.T, pool *pgxpool.Pool, conversationID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs (conversation_id, query, status, model)
		 VALUES ($1, 'q', 'completed', $2) RETURNING id`,
		conversationID, testModel).Scan(&id); err != nil {
		t.Fatalf("insert agent_run: %v", err)
	}
	return id
}

func getPath(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The list is the dev user's conversations only, newest updated_at first —
// updated_at, not created_at, so the conversation touched by the latest
// message sorts to the top.
func TestListConversationsOrdersByUpdatedAtDesc(t *testing.T) {
	pool := testPool(t)
	userID := seedUserID(t, pool, devUserEmail)

	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	oldest := seedConversationAt(t, pool, userID, "oldest", base)
	newest := seedConversationAt(t, pool, userID, "newest", base.Add(2*time.Hour))
	middle := seedConversationAt(t, pool, userID, "middle", base.Add(time.Hour))

	// Another user's conversation, newer than all of them: it must not appear.
	otherID := seedUserID(t, pool, "someone-else@cortex.test")
	seedConversationAt(t, pool, otherID, "foreign", base.Add(3*time.Hour))

	h := newChatRouter(pool, &stubEnqueuer{})
	rec := getPath(t, h, "/api/conversations")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var got []conversationBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}

	wantOrder := []uuid.UUID{newest, middle, oldest}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d conversations, want %d (body %q)", len(got), len(wantOrder), rec.Body.String())
	}
	for i, want := range wantOrder {
		if got[i].ID != want.String() {
			t.Errorf("conversations[%d].id = %s, want %s (order must be updated_at desc)", i, got[i].ID, want)
		}
	}
	for i, c := range got {
		if c.Title == nil {
			t.Errorf("conversations[%d].title = null, want a string", i)
		}
		if c.CreatedAt == "" || c.UpdatedAt == "" {
			t.Errorf("conversations[%d] timestamps = (%q, %q), want both set", i, c.CreatedAt, c.UpdatedAt)
		}
	}
}

// No conversations is an empty JSON array, never null: the frontend iterates
// the body directly.
func TestListConversationsEmptyIsAnArray(t *testing.T) {
	pool := testPool(t)
	h := newChatRouter(pool, &stubEnqueuer{})

	rec := getPath(t, h, "/api/conversations")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

// The thread comes back in chronological order, and each assistant message
// carries the agent_run_id that produced it (A2) — that link is how a
// re-opened conversation resolves its citation chips. User messages and
// pre-migration assistant rows carry null.
func TestListConversationMessagesCarriesAgentRunID(t *testing.T) {
	pool := testPool(t)
	userID := seedUserID(t, pool, devUserEmail)
	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	conversationID := seedConversationAt(t, pool, userID, "q", base)
	runID := seedAgentRun(t, pool, conversationID)

	userMsg := seedMessageAt(t, pool, conversationID, "user", "why is ATLAS-1 blocked?", nil, base)
	assistantMsg := seedMessageAt(t, pool, conversationID, "assistant", "It is blocked [1].", &runID, base.Add(time.Minute))
	legacyMsg := seedMessageAt(t, pool, conversationID, "assistant", "old answer [1]", nil, base.Add(2*time.Minute))

	h := newChatRouter(pool, &stubEnqueuer{})
	rec := getPath(t, h, "/api/conversations/"+conversationID.String()+"/messages")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var got []messageBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3 (body %q)", len(got), rec.Body.String())
	}

	wantOrder := []uuid.UUID{userMsg, assistantMsg, legacyMsg}
	for i, want := range wantOrder {
		if got[i].ID != want.String() {
			t.Errorf("messages[%d].id = %s, want %s (order must be chronological)", i, got[i].ID, want)
		}
	}

	if got[0].Role != "user" || got[0].AgentRunID != nil {
		t.Errorf("user message = {role:%q agent_run_id:%v}, want {user null}", got[0].Role, got[0].AgentRunID)
	}
	if got[1].Role != "assistant" {
		t.Errorf("messages[1].role = %q, want assistant", got[1].Role)
	}
	if got[1].AgentRunID == nil || *got[1].AgentRunID != runID.String() {
		t.Errorf("assistant agent_run_id = %v, want %s", got[1].AgentRunID, runID)
	}
	if got[2].AgentRunID != nil {
		t.Errorf("legacy assistant agent_run_id = %q, want null", *got[2].AgentRunID)
	}
	if got[1].Content != "It is blocked [1]." {
		t.Errorf("assistant content = %q, want the stored answer verbatim", got[1].Content)
	}
}

// Unknown and foreign conversations are the same 404 — the endpoint must not
// confirm which ids exist for other users. A malformed id is a 400.
func TestListConversationMessagesRejectsBadUnknownAndForeign(t *testing.T) {
	pool := testPool(t)

	otherID := seedUserID(t, pool, "someone-else@cortex.test")
	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	foreignConv := seedConversationAt(t, pool, otherID, "theirs", base)
	seedMessageAt(t, pool, foreignConv, "user", "their secret", nil, base)

	h := newChatRouter(pool, &stubEnqueuer{})

	tests := []struct {
		name       string
		id         string
		wantStatus int
	}{
		{name: "malformed id", id: "not-a-uuid", wantStatus: http.StatusBadRequest},
		{name: "unknown conversation", id: uuid.NewString(), wantStatus: http.StatusNotFound},
		{name: "another user's conversation", id: foreignConv.String(), wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := getPath(t, h, "/api/conversations/"+tt.id+"/messages")
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "their secret") {
				t.Error("response leaked another user's message content")
			}
		})
	}
}
