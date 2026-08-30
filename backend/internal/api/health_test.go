package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"cortex/internal/api"
)

// REQ-1.6: GET /healthz returns 200 {"status":"ok"} and checks the DB ping.
func TestHealthz(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
		wantBody   string
	}{
		{name: "database reachable", pingErr: nil, wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "database unreachable", pingErr: errors.New("connection refused"), wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := api.NewRouter(withTestAuth(api.Deps{
				DB:       &stubDB{pingErr: tt.pingErr},
				Enqueuer: &stubEnqueuer{},
				Model:    testModel,
				Logger:   discardLogger(),
			}))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, localRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if tt.wantBody != "" && body["status"] != tt.wantBody {
				t.Errorf("status field = %q, want %q", body["status"], tt.wantBody)
			}
			if tt.wantBody == "" && body["status"] == "ok" {
				t.Error("status field = \"ok\" while the database ping failed")
			}
		})
	}
}

// NewRouter must tolerate a nil logger (REQ-1.6 wiring); it falls back to the
// default slog logger rather than panicking on the first request.
func TestNewRouterWithNilLogger(t *testing.T) {
	h := api.NewRouter(withTestAuth(api.Deps{DB: &stubDB{}, Enqueuer: &stubEnqueuer{}, Model: testModel}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
