package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cortex/internal/api"
)

// Multi-user safety rails.
//
// The startup fail-fast for the source pins (GMAIL_QUERY_SCOPE, JIRA_PROJECTS)
// is asserted in internal/config/config_test.go ("missing source pins are
// reported with their rationale"); this file covers the two rails that live in
// the HTTP layer: the per-user run rate limit and the admin gate.

// The N+1th run inside an hour is a 429, and nothing is created for it.
func TestChatRateLimitsTheNPlusFirstRun(t *testing.T) {
	const limit = 2

	pool := testPool(t)
	giveLLMKey(t, pool, devUserEmail)
	enqueuer := &stubEnqueuer{}
	h := api.NewRouter(withTestAuth(api.Deps{
		DB:                 pool,
		Enqueuer:           enqueuer,
		Model:              testModel,
		Logger:             discardLogger(),
		RunsPerUserPerHour: limit,
	}))

	for i := 1; i <= limit; i++ {
		rec := postChat(t, h, `{"message":"question number `+strings.Repeat("i", i)+`"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("run %d status = %d, want 202 (body %q)", i, rec.Code, rec.Body.String())
		}
	}

	rec := postChat(t, h, `{"message":"one over the limit"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("run %d status = %d, want 429 (body %q)", limit+1, rec.Code, rec.Body.String())
	}
	if enqueuer.callCount() != limit {
		t.Errorf("enqueued runs = %d, want %d (the rejected run must not queue)", enqueuer.callCount(), limit)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != limit {
		t.Errorf("agent_runs = %d, want %d", n, limit)
	}

	// The limit is per user: another user's first run still goes through.
	otherCookie, _ := createSessionForEmail(t, pool, "other@example.com")
	giveLLMKey(t, pool, "other@example.com")
	req := sessionRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"hello"}`), otherCookie)
	req.Header.Set("Content-Type", "application/json")
	other := httptest.NewRecorder()
	h.ServeHTTP(other, req)
	if other.Code != http.StatusAccepted {
		t.Errorf("another user's run = %d, want 202 (the limit must be per user; body %q)",
			other.Code, other.Body.String())
	}
}

// POST /api/admin/index: a session not in ADMIN_EMAILS is 403 and queues
// nothing; an admin session and the bearer token both succeed.
func TestAdminIndexGatesOnAdminIdentity(t *testing.T) {
	pool := testPool(t)

	newAdminRouter := func(enqueuer api.Enqueuer) http.Handler {
		return api.NewRouter(withTestAuth(api.Deps{
			DB:           pool,
			Enqueuer:     enqueuer,
			Model:        testModel,
			IndexSources: indexSources,
			Logger:       discardLogger(),
			AdminEmails:  []string{"admin@example.com"},
		}))
	}

	adminIndex := func(h http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder {
		var req *http.Request
		if cookie != nil {
			req = sessionRequest(http.MethodPost, "/api/admin/index", strings.NewReader(`{}`), cookie)
		} else {
			req = localRequest(http.MethodPost, "/api/admin/index", strings.NewReader(`{}`))
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("a non-admin session is 403", func(t *testing.T) {
		enqueuer := &stubEnqueuer{}
		h := newAdminRouter(enqueuer)
		cookie, _ := createSessionForEmail(t, pool, "user@example.com")

		rec := adminIndex(h, cookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
		}
		if len(enqueuer.indexed) != 0 {
			t.Errorf("a 403'd request queued %v, want nothing", enqueuer.indexed)
		}
	})

	t.Run("an admin session is accepted", func(t *testing.T) {
		enqueuer := &stubEnqueuer{}
		h := newAdminRouter(enqueuer)
		cookie, _ := createSessionForEmail(t, pool, "admin@example.com")

		rec := adminIndex(h, cookie)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
		}
		if !equalStringSlices(enqueuer.indexed, indexSources) {
			t.Errorf("queued %v, want %v", enqueuer.indexed, indexSources)
		}
	})

	t.Run("the bearer token is accepted", func(t *testing.T) {
		enqueuer := &stubEnqueuer{}
		h := newAdminRouter(enqueuer)

		rec := adminIndex(h, nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
		}
		if !equalStringSlices(enqueuer.indexed, indexSources) {
			t.Errorf("queued %v, want %v", enqueuer.indexed, indexSources)
		}
	})
}
