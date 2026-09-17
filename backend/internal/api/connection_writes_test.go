package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
	"cortex/internal/connections"
)

// PUT /api/connections/{source}/writes — granting the ability to write.
//
// The sequence is the point: connecting a source grants reading and nothing
// else. The ability to send mail as somebody, or change their Jira, is granted
// by this call and only by this call, one source at a time, by the person whose
// account it is. So the tests are about refusals: demo mode, a credential that
// cannot actually write, and a deployment with no readiness check at all.
//
// Disabling is unconditional, because revoking permission must never depend on
// an upstream system being reachable.

// recordingReadiness is a stub WriteReadiness: it records what it was asked and
// answers with a scripted error.
type recordingReadiness struct {
	mu      sync.Mutex
	err     error
	sources []string
}

func (r *recordingReadiness) check(_ context.Context, _ uuid.UUID, source string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = append(r.sources, source)
	return r.err
}

func (r *recordingReadiness) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sources...)
}

// newWritesRouter wires a router whose readiness check is the stub. A nil stub
// stands for a deployment that cannot enable writes at all.
func newWritesRouter(t *testing.T, pool *pgxpool.Pool, readiness *recordingReadiness, demoSources ...string) http.Handler {
	t.Helper()
	deps := api.Deps{
		DB:          pool,
		Enqueuer:    &stubEnqueuer{},
		Model:       testModel,
		Logger:      discardLogger(),
		Keys:        newTestKeysService(t, pool),
		Connections: newConnectionsService(t, pool, demoSources...),
	}
	if readiness != nil {
		deps.WriteReadiness = readiness.check
	}
	return api.NewRouter(withTestAuth(deps))
}

// giveJiraConnection stores a Jira connection for the bearer user, bypassing
// the paste endpoint (which has its own tests and would need a live fake).
func giveJiraConnection(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	userID := insertActionUser(t, pool, devUserEmail)
	svc := newConnectionsService(t, pool)
	err := svc.Save(context.Background(), userID, connections.SourceJira,
		connections.JiraCredentials{
			BaseURL:  "https://example.atlassian.net",
			Email:    "satyam@example.com",
			APIToken: "their-jira-token-0042",
		},
		map[string]string{"site_url": "https://example.atlassian.net"})
	if err != nil {
		t.Fatalf("save jira connection: %v", err)
	}
	return userID
}

// writesEnabledInDB reads the flag straight from the table.
func writesEnabledInDB(t *testing.T, pool *pgxpool.Pool, source string) bool {
	t.Helper()
	var enabled bool
	if err := pool.QueryRow(context.Background(),
		`SELECT writes_enabled FROM user_connections WHERE source = $1`, source).Scan(&enabled); err != nil {
		t.Fatalf("read writes_enabled for %s: %v", source, err)
	}
	return enabled
}

// Enabling writes is a deliberate, per-source grant that
// goes through the readiness check and is reported back in the overview.
func TestEnablingWritesChecksReadinessAndRecordsTheFlag(t *testing.T) {
	pool := testPool(t)
	readiness := &recordingReadiness{}
	h := newWritesRouter(t, pool, readiness)
	giveJiraConnection(t, pool)

	if writesEnabledInDB(t, pool, "jira") {
		t.Fatal("a fresh connection has writes enabled; reading must never imply writing")
	}

	rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := readiness.asked(); len(got) != 1 || got[0] != "jira" {
		t.Errorf("readiness asked about %v, want exactly [jira]", got)
	}
	if !writesEnabledInDB(t, pool, "jira") {
		t.Error("writes_enabled is still false after a successful enable")
	}

	// The response is the connections overview, so the UI does not need a second
	// request to render the new state.
	var overview struct {
		Sources map[string]struct {
			WritesEnabled bool `json:"writes_enabled"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode overview %q: %v", rec.Body.String(), err)
	}
	if !overview.Sources["jira"].WritesEnabled {
		t.Errorf("overview reports jira writes_enabled=false after enabling (body %q)", rec.Body.String())
	}
}

// A credential that cannot perform writes is refused with the sentence
// written for the person reading it — never silently enabled so the failure
// lands after somebody has approved something.
func TestEnablingWritesIsRefusedWhenTheCredentialCannot(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantPart string
	}{
		{
			name: "missing jira permissions",
			err: fmt.Errorf("%w: this Jira account is missing the permissions Cortex would need "+
				"(EDIT_ISSUES). Ask a site admin to grant them, then try again",
				connections.ErrWritesUnavailable),
			wantCode: http.StatusConflict,
			wantPart: "EDIT_ISSUES",
		},
		{
			name:     "no connection",
			err:      connections.ErrNoConnection,
			wantCode: http.StatusNotFound,
			wantPart: "no connection",
		},
		{
			name:     "upstream unreachable",
			err:      errors.New("check jira write permissions: dial tcp: connection refused"),
			wantCode: http.StatusBadGateway,
			wantPart: "try again",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			readiness := &recordingReadiness{err: tt.err}
			h := newWritesRouter(t, pool, readiness)
			giveJiraConnection(t, pool)

			rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":true}`)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantPart) {
				t.Errorf("body %q does not mention %q", rec.Body.String(), tt.wantPart)
			}
			if writesEnabledInDB(t, pool, "jira") {
				t.Error("writes were enabled despite the readiness check refusing")
			}
			// The refusal must not leak the credential.
			if strings.Contains(rec.Body.String(), "their-jira-token-0042") {
				t.Error("the refusal echoes the API token")
			}
		})
	}
}

// Writes are never available in demo mode. The demo workspace is a
// real Jira site and a real mailbox belonging to somebody else, so a signed-in
// stranger must not be able to enable writes against it — and the readiness
// check is never even reached.
func TestWritesCannotBeEnabledInDemoMode(t *testing.T) {
	// Nothing connected is no longer demo mode, so the refusal is the plainer
	// one: there is no jira connection to enable writes on. The readiness check
	// is still never consulted, which is the property that matters — nothing
	// about a user's credentials is touched before the request is refused.
	t.Run("nothing connected", func(t *testing.T) {
		pool := testPool(t)
		readiness := &recordingReadiness{}
		h := newWritesRouter(t, pool, readiness)
		insertActionUser(t, pool, devUserEmail)

		rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":true}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "no connection") {
			t.Errorf("body %q does not say there is no connection to enable writes on",
				rec.Body.String())
		}
		if got := readiness.asked(); len(got) != 0 {
			t.Errorf("readiness was consulted %v with nothing connected", got)
		}
	})

	t.Run("demo toggle on with a connection of their own", func(t *testing.T) {
		pool := testPool(t)
		readiness := &recordingReadiness{}
		// The demo workspace has to exist for a user to be in demo mode.
		h := newWritesRouter(t, pool, readiness, "jira", "notion")
		giveJiraConnection(t, pool)

		if rec := putJSONAsHuman(t, pool, h, "/api/connections/mode", `{"use_demo_workspace":true}`); rec.Code != http.StatusOK {
			t.Fatalf("switch to the demo workspace = %d (body %q)", rec.Code, rec.Body.String())
		}

		rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":true}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		if writesEnabledInDB(t, pool, "jira") {
			t.Error("writes were enabled while the run would target the demo workspace")
		}
		if got := readiness.asked(); len(got) != 0 {
			t.Errorf("readiness was consulted %v in demo mode", got)
		}
	})
}

// A deployment with no readiness check cannot enable writes, and says so rather
// than flipping a flag it cannot stand behind.
func TestWritesCannotBeEnabledWithoutAReadinessCheck(t *testing.T) {
	pool := testPool(t)
	h := newWritesRouter(t, pool, nil)
	giveJiraConnection(t, pool)

	rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if writesEnabledInDB(t, pool, "jira") {
		t.Error("writes were enabled on a deployment that cannot check them")
	}
}

// Disabling is unconditional: revoking permission must never depend on an
// upstream system being reachable, or on the demo toggle, or on a readiness
// check that now fails.
func TestDisablingWritesNeverConsultsAnything(t *testing.T) {
	pool := testPool(t)
	readiness := &recordingReadiness{}
	h := newWritesRouter(t, pool, readiness)
	giveJiraConnection(t, pool)

	if rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d (body %q)", rec.Code, rec.Body.String())
	}
	// The site has since gone down, and the account lost its permissions.
	readiness.mu.Lock()
	readiness.err = errors.New("check jira write permissions: dial tcp: connection refused")
	readiness.sources = nil
	readiness.mu.Unlock()

	rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if writesEnabledInDB(t, pool, "jira") {
		t.Error("writes are still enabled after being switched off")
	}
	if got := readiness.asked(); len(got) != 0 {
		t.Errorf("readiness was consulted %v while switching writes OFF", got)
	}
}

// The endpoint refuses a request it cannot make sense of before touching
// anything: an unknown source, a missing content type, a malformed body.
func TestWritesToggleRejectsMalformedRequests(t *testing.T) {
	pool := testPool(t)
	readiness := &recordingReadiness{}
	h := newWritesRouter(t, pool, readiness)
	giveJiraConnection(t, pool)

	t.Run("unknown source", func(t *testing.T) {
		rec := putJSONAsHuman(t, pool, h, "/api/connections/slack/writes", `{"enabled":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
		}
	})
	t.Run("no content type", func(t *testing.T) {
		req := humanRequest(t, pool, http.MethodPut, "/api/connections/jira/writes", strings.NewReader(`{"enabled":true}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("status = %d, want 415 (body %q)", rec.Code, rec.Body.String())
		}
	})
	t.Run("malformed body", func(t *testing.T) {
		rec := putJSONAsHuman(t, pool, h, "/api/connections/jira/writes", `{"enabled":`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
		}
	})
	t.Run("anonymous", func(t *testing.T) {
		req := anonymousRequest(http.MethodPut, "/api/connections/jira/writes",
			strings.NewReader(`{"enabled":true}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
		}
	})

	if writesEnabledInDB(t, pool, "jira") {
		t.Error("a malformed request enabled writes")
	}
}

// Enabling writes for a source the user has not connected is a 404, whether the
// readiness check says so or the flag update finds no row.
func TestEnablingWritesOnAnUnconnectedSource(t *testing.T) {
	pool := testPool(t)
	readiness := &recordingReadiness{}
	h := newWritesRouter(t, pool, readiness)
	// Jira is connected (so the user is not in demo mode); Notion is not.
	giveJiraConnection(t, pool)

	rec := putJSONAsHuman(t, pool, h, "/api/connections/notion/writes", `{"enabled":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
}

// Granting write capability is part of the same human-only guarantee as
// approving a write: a token holder who could switch writes on has done most of
// the work of sending mail as the owner.
func TestTheOperatorTokenCannotEnableWrites(t *testing.T) {
	pool := testPool(t)
	readiness := &recordingReadiness{}
	h := newWritesRouter(t, pool, readiness)
	giveJiraConnection(t, pool)

	// localRequest carries the bearer token, which is the point here.
	req := localRequest(http.MethodPut, "/api/connections/jira/writes",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if writesEnabledInDB(t, pool, "jira") {
		t.Error("writes_enabled is true after a refused request")
	}
	if got := readiness.asked(); len(got) != 0 {
		t.Errorf("readiness asked about %v, want nothing — the refusal comes first", got)
	}
}
