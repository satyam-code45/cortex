package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cortex/internal/api"
)

// newPublicHostRouter is newChatRouter with a deployment's public hostname
// configured, which is what a hosted run passes.
func newPublicHostRouter(db api.DB, enqueuer api.Enqueuer, public string) http.Handler {
	return api.NewRouter(withTestAuth(api.Deps{
		DB:             db,
		Enqueuer:       enqueuer,
		Model:          testModel,
		Logger:         discardLogger(),
		PublicHostname: public,
	}))
}

// The Host check is what closes DNS rebinding.
//
// Loopback binding keeps the API off the network and the JSON content-type
// requirement forces a preflight that never succeeds, but neither stops a page on
// attacker.com whose short-TTL record flips to 127.0.0.1: that page becomes
// same-origin and can read every run and queue unbounded paid work. A browser
// always sends the name it dialled in Host and scripts cannot forge it, so this
// test is the guard on the guard.
func TestRejectsUnexpectedHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		host       string
		wantStatus int
	}{
		{name: "localhost with port", host: "localhost:8080", wantStatus: http.StatusOK},
		{name: "localhost without port", host: "localhost", wantStatus: http.StatusOK},
		{name: "loopback IPv4", host: "127.0.0.1:8080", wantStatus: http.StatusOK},
		{name: "loopback IPv6", host: "[::1]:8080", wantStatus: http.StatusOK},
		{
			name:       "rebinding host is refused",
			host:       "attacker.example.com",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{
			name:       "rebinding host with the right port is still refused",
			host:       "attacker.example.com:8080",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{
			// The classic bypass: a name that merely contains an allowed one.
			name:       "hostname containing localhost is refused",
			host:       "localhost.attacker.example.com",
			wantStatus: http.StatusMisdirectedRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newChatRouter(&stubDB{}, &stubEnqueuer{})

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// A hosted deployment answers to its own public hostname, and to nothing else
// it was not told about.
//
// Without this the check rejects every request a hosted deployment receives —
// including its own health check, which a platform reads as "the service never
// started". The allowlist stays closed, though: configuring one public name
// must not turn the check into a wildcard, or the rebinding defence the rest of
// this file is about would be gone in production and present only locally.
func TestAnswersToTheConfiguredPublicHostname(t *testing.T) {
	t.Parallel()

	const public = "cortex-api.onrender.com"

	tests := []struct {
		name       string
		host       string
		wantStatus int
	}{
		{name: "the configured public name", host: public, wantStatus: http.StatusOK},
		{name: "with a port", host: public + ":443", wantStatus: http.StatusOK},
		{name: "case-insensitive", host: "Cortex-API.OnRender.com", wantStatus: http.StatusOK},
		{name: "loopback still works", host: "localhost:8080", wantStatus: http.StatusOK},
		{
			name:       "another host on the same platform is refused",
			host:       "someone-else.onrender.com",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{
			// The same bypass as above, against the configured name this time.
			name:       "a name merely containing the public one is refused",
			host:       public + ".attacker.example.com",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{name: "an unrelated host is refused", host: "attacker.example.com", wantStatus: http.StatusMisdirectedRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newPublicHostRouter(&stubDB{}, &stubEnqueuer{}, public)

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// A local run must answer to loopback only, whatever a deployment would allow.
func TestWithNoPublicHostnameOnlyLoopbackIsAnswered(t *testing.T) {
	t.Parallel()

	h := newPublicHostRouter(&stubDB{}, &stubEnqueuer{}, "")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "cortex-api.onrender.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want 421 — an unconfigured deployment name is not allowed",
			rec.Code)
	}
}

// A rejected Host must be turned away before the handler runs, so a
// cross-origin page cannot spend money even on the endpoint that costs money.
func TestUnexpectedHostReachesNoHandler(t *testing.T) {
	t.Parallel()

	enqueuer := &stubEnqueuer{}
	db := &stubDB{}
	h := newChatRouter(db, enqueuer)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"message":"spend my money"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "attacker.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMisdirectedRequest)
	}
	if enqueuer.callCount() != 0 {
		t.Errorf("enqueued %d runs, want 0", enqueuer.callCount())
	}
	if db.begins != 0 {
		t.Errorf("database transactions started = %d, want 0", db.begins)
	}
}
