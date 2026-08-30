package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Day 7 integration requirement: two users, one Postgres — user B must not be
// able to list user A's conversations or read A's runs. Ownership is part of
// every lookup, so a foreign resource is indistinguishable from a missing one.

func TestTwoUsersCannotSeeEachOthersData(t *testing.T) {
	pool := testPool(t)

	// User A owns a conversation with a completed run.
	answer := "ATLAS-1 is blocked on the payments sandbox."
	conversationA, runA := seedRunForUser(t, pool, "alice@example.com", "completed", &answer, nil)
	cookieA, _ := createSessionForEmail(t, pool, "alice@example.com")
	cookieB, _ := createSessionForEmail(t, pool, "bob@example.com")

	h := newChatRouter(pool, &stubEnqueuer{})

	t.Run("A sees their own conversation", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sessionRequest(http.MethodGet, "/api/conversations", nil, cookieA))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		var conversations []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &conversations); err != nil {
			t.Fatalf("decode conversations %q: %v", rec.Body.String(), err)
		}
		if len(conversations) != 1 || conversations[0].ID != conversationA.String() {
			t.Errorf("A's conversations = %+v, want exactly their own %s", conversations, conversationA)
		}
	})

	t.Run("B's conversation list does not contain A's", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sessionRequest(http.MethodGet, "/api/conversations", nil, cookieB))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		var conversations []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &conversations); err != nil {
			t.Fatalf("decode conversations %q: %v", rec.Body.String(), err)
		}
		if len(conversations) != 0 {
			t.Errorf("B's conversations = %+v, want none", conversations)
		}
	})

	t.Run("B cannot read A's run", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sessionRequest(http.MethodGet, "/api/runs/"+runA.String(), nil, cookieB))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (a foreign run must look nonexistent; body %q)",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("B cannot read A's run trace", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sessionRequest(http.MethodGet, "/api/runs/"+runA.String()+"/trace", nil, cookieB))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("B cannot read A's messages", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sessionRequest(http.MethodGet, "/api/conversations/"+conversationA.String()+"/messages", nil, cookieB))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("A still reads their own run", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sessionRequest(http.MethodGet, "/api/runs/"+runA.String(), nil, cookieA))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})
}
