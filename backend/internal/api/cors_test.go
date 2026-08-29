package api_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"cortex/internal/api"
)

// TEST-5.2 — CORS admits exactly the one configured frontend origin (A3).
//
// The policy under test, from the spec: Access-Control-Allow-Origin is the
// configured origin and appears on plain GETs too (EventSource never
// preflights but enforces CORS on the response), preflight OPTIONS is answered
// 204 with Allow-Methods "GET, POST, OPTIONS" and Allow-Headers
// "Content-Type", every other origin gets no allow headers, and the middleware
// is mounted on /api/* only.

const testFrontendOrigin = "http://localhost:3000"

// newCORSRouter builds a router with CORS configured for testFrontendOrigin.
// The stub DB is enough: the paths used below are rejected before any query,
// and the CORS headers are set by middleware before the handler runs.
func newCORSRouter() http.Handler {
	return api.NewRouter(api.Deps{
		DB:             &stubDB{},
		Enqueuer:       &stubEnqueuer{},
		Model:          testModel,
		DevUserEmail:   devUserEmail,
		FrontendOrigin: testFrontendOrigin,
		Logger:         discardLogger(),
	})
}

func TestCORSAdmitsOnlyTheConfiguredOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		origin     string
		wantStatus int
		wantAllow  bool // Access-Control-Allow-* headers present
		wantVary   bool // Vary: Origin present
	}{
		{
			name:       "preflight from the configured origin is answered 204 with allow headers",
			method:     http.MethodOptions,
			path:       "/api/chat",
			origin:     testFrontendOrigin,
			wantStatus: http.StatusNoContent,
			wantAllow:  true,
			wantVary:   true,
		},
		{
			name:       "preflight from another origin is answered 204 without allow headers",
			method:     http.MethodOptions,
			path:       "/api/chat",
			origin:     "https://evil.example.com",
			wantStatus: http.StatusNoContent,
			wantAllow:  false,
			wantVary:   true,
		},
		{
			// EventSource does not preflight: the plain GET response itself
			// must carry the header, on the SSE route like any other.
			name:      "plain GET from the configured origin carries the allow header",
			method:    http.MethodGet,
			path:      "/api/runs/not-a-uuid/events",
			origin:    testFrontendOrigin,
			wantAllow: true,
			wantVary:  true,
		},
		{
			name:      "plain GET from another origin carries no allow header",
			method:    http.MethodGet,
			path:      "/api/runs/not-a-uuid",
			origin:    "http://localhost:3001",
			wantAllow: false,
			wantVary:  true,
		},
		{
			// A same-origin or non-browser request sends no Origin at all.
			name:      "request without an Origin header gets no allow header",
			method:    http.MethodGet,
			path:      "/api/runs/not-a-uuid",
			origin:    "",
			wantAllow: false,
			wantVary:  true,
		},
		{
			name:      "the middleware is scoped to /api: healthz gets no CORS headers",
			method:    http.MethodGet,
			path:      "/healthz",
			origin:    testFrontendOrigin,
			wantAllow: false,
			wantVary:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newCORSRouter()

			req := localRequest(tt.method, tt.path, nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.method == http.MethodOptions {
				// What a real browser preflight carries.
				req.Header.Set("Access-Control-Request-Method", http.MethodPost)
				req.Header.Set("Access-Control-Request-Headers", "Content-Type")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if tt.wantStatus != 0 && rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			allowOrigin := rec.Header().Get("Access-Control-Allow-Origin")
			if tt.wantAllow {
				if allowOrigin != testFrontendOrigin {
					t.Errorf("Access-Control-Allow-Origin = %q, want %q", allowOrigin, testFrontendOrigin)
				}
				if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
					t.Errorf("Access-Control-Allow-Methods = %q, want %q", got, "GET, POST, OPTIONS")
				}
				// Last-Event-ID is required for cross-origin SSE resume:
				// EventSource's reconnect preflights it (only the first
				// connect is preflight-free).
				const wantAllowHeaders = "Content-Type, Last-Event-ID"
				if got := rec.Header().Get("Access-Control-Allow-Headers"); got != wantAllowHeaders {
					t.Errorf("Access-Control-Allow-Headers = %q, want %q", got, wantAllowHeaders)
				}
			} else if allowOrigin != "" {
				t.Errorf("Access-Control-Allow-Origin = %q, want none", allowOrigin)
			}

			// Vary: Origin must be present whenever CORS applies — with or
			// without the allow headers — so a cache never serves one
			// origin's response to another.
			hasVary := slices.Contains(rec.Header().Values("Vary"), "Origin")
			if hasVary != tt.wantVary {
				t.Errorf("Vary contains Origin = %v, want %v (Vary = %q)",
					hasVary, tt.wantVary, rec.Header().Values("Vary"))
			}
		})
	}
}

// The allow header value is the exact configured origin — never a wildcard and
// never a reflection of the request's Origin.
func TestCORSNeverReflectsTheRequestOrigin(t *testing.T) {
	t.Parallel()
	h := newCORSRouter()

	req := localRequest(http.MethodGet, "/api/runs/not-a-uuid", nil)
	req.Header.Set("Origin", "http://localhost:3000.evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for a prefix-spoofed origin, want none", got)
	}
}
