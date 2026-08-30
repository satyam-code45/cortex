package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"cortex/internal/api"
	"cortex/internal/connections"
	"cortex/internal/keys"
)

// TEST-8.2 — Jira + Notion paste flows (REQ-8.2), against httptest fakes.
//
// The invariants are the spec's: a pasted credential is validated with one
// live provider call before anything is stored, what lands in the database is
// ciphertext, the identity captured is the provider's display facts, a bad
// credential is a 422 carrying the provider's own reason with nothing stored,
// and credentials appear in no response body, ever. The GET/DELETE/mode
// endpoints get the REQ-8.2 mode semantics: user the moment anything is
// connected (errored included), demo only when nothing remains or the toggle
// is on.

const (
	fakeJiraRejection   = "Basic auth with password is not allowed on this instance."
	fakeNotionRejection = "API token is invalid."
	testJiraToken       = "jira-api-token-users-own-0042"
	testNotionToken     = "ntn_users_own_integration_token_0042"
)

// fakeJira serves GET /rest/api/3/myself: display facts for the one valid
// email+token pair, an Atlassian-shaped 401 for everything else.
type fakeJira struct {
	server     *httptest.Server
	validEmail string
	validToken string
	calls      atomic.Int64
}

func newFakeJira(t *testing.T, validEmail, validToken string) *fakeJira {
	t.Helper()
	f := &fakeJira{validEmail: validEmail, validToken: validToken}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/rest/api/3/myself" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessages":["no route"]}`))
			return
		}
		email, token, ok := r.BasicAuth()
		if !ok || email != f.validEmail || token != f.validToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errorMessages":["` + fakeJiraRejection + `"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"accountId":"acc-42","displayName":"Satyam Jha","emailAddress":"` + f.validEmail + `"}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// fakeNotion serves GET /v1/users/me: the bot identity for one valid token, a
// Notion-shaped 401 otherwise.
type fakeNotion struct {
	server     *httptest.Server
	validToken string
	calls      atomic.Int64
}

func newFakeNotion(t *testing.T, validToken string) *fakeNotion {
	t.Helper()
	f := &fakeNotion{validToken: validToken}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/users/me" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"object":"error","status":404,"code":"object_not_found","message":"no route"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.validToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"object":"error","status":401,"code":"unauthorized","message":"` + fakeNotionRejection + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"user","name":"Cortex Bot","type":"bot","bot":{"workspace_name":"Acme Workspace"}}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// newConnectionsService builds the connections.Service the router Deps carry,
// on the same cipher the settings tests use.
func newConnectionsService(t *testing.T, db api.DB) *connections.Service {
	t.Helper()
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	return connections.NewService(db, cipher)
}

// newConnectionsRouter wires a router whose Notion validation hits the fake.
// (Jira needs no base-URL override: the user pastes their site URL, so tests
// paste the fake server's.)
func newConnectionsRouter(t *testing.T, db api.DB, notionBaseURL string) http.Handler {
	t.Helper()
	return api.NewRouter(withTestAuth(api.Deps{
		DB:            db,
		Enqueuer:      &stubEnqueuer{},
		Model:         testModel,
		Logger:        discardLogger(),
		Keys:          newTestKeysService(t, db),
		Connections:   newConnectionsService(t, db),
		NotionBaseURL: notionBaseURL,
	}))
}

func putJSON(t *testing.T, h http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodPut, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getConnections(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, connectionsOverview) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/connections", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/connections = %d (body %q)", rec.Code, rec.Body.String())
	}
	var overview connectionsOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode connections overview %q: %v", rec.Body.String(), err)
	}
	return rec, overview
}

// connectionsOverview mirrors the GET /api/connections contract from the spec.
type connectionsOverview struct {
	Mode             string `json:"mode"`
	UseDemoWorkspace bool   `json:"use_demo_workspace"`
	Sources          map[string]struct {
		Status    string            `json:"status"`
		Identity  map[string]string `json:"identity"`
		UpdatedAt *string           `json:"updated_at"`
		LastError string            `json:"last_error"`
	} `json:"sources"`
}

func TestPutJiraConnectionValidatesStoresEncryptedAndCapturesIdentity(t *testing.T) {
	const email = "satyam@example.com"

	pool := testPool(t)
	jira := newFakeJira(t, email, testJiraToken)
	h := newConnectionsRouter(t, pool, "")

	rec := putJSON(t, h, "/api/connections/jira",
		`{"base_url":"`+jira.server.URL+`","email":"`+email+`","api_token":"`+testJiraToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if jira.calls.Load() != 1 {
		t.Errorf("validation calls = %d, want exactly 1 live /myself check", jira.calls.Load())
	}
	if strings.Contains(rec.Body.String(), testJiraToken) {
		t.Errorf("PUT response echoes the API token: %q", rec.Body.String())
	}

	var saved struct {
		Status   string            `json:"status"`
		Identity map[string]string `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode PUT response %q: %v", rec.Body.String(), err)
	}
	if saved.Status != "connected" {
		t.Errorf("status = %q, want connected", saved.Status)
	}
	if saved.Identity["site_url"] != jira.server.URL || saved.Identity["account_name"] != "Satyam Jha" {
		t.Errorf("identity = %v, want the /myself display facts (site_url, account_name)", saved.Identity)
	}

	// The database holds ciphertext that decrypts back to the pasted token —
	// never the plaintext.
	var ciphertext []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT credentials_ciphertext FROM user_connections WHERE source = 'jira'`).Scan(&ciphertext); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	if strings.Contains(string(ciphertext), testJiraToken) {
		t.Fatal("the database holds the plaintext API token")
	}
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	plain, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt stored ciphertext: %v", err)
	}
	var creds struct {
		BaseURL  string `json:"base_url"`
		Email    string `json:"email"`
		APIToken string `json:"api_token"`
	}
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		t.Fatalf("decode decrypted credentials: %v", err)
	}
	if creds.APIToken != testJiraToken || creds.Email != email || creds.BaseURL != jira.server.URL {
		t.Errorf("decrypted credentials = %+v, want exactly what was pasted", creds)
	}

	// GET reports mode=user with the jira card connected — and no credential
	// anywhere in the body.
	getRec, overview := getConnections(t, h)
	if strings.Contains(getRec.Body.String(), testJiraToken) {
		t.Errorf("GET /api/connections echoes the API token: %q", getRec.Body.String())
	}
	if overview.Mode != "user" {
		t.Errorf("mode = %q, want user once a source is connected", overview.Mode)
	}
	if overview.Sources["jira"].Status != "connected" {
		t.Errorf("jira status = %q, want connected", overview.Sources["jira"].Status)
	}
	if overview.Sources["jira"].Identity["account_name"] != "Satyam Jha" {
		t.Errorf("jira identity = %v, want the captured display facts", overview.Sources["jira"].Identity)
	}
	if overview.Sources["notion"].Status != "absent" || overview.Sources["gmail"].Status != "absent" {
		t.Errorf("unconnected sources = %v, want absent", overview.Sources)
	}
}

func TestPutJiraConnectionRejectsAnInvalidTokenWith422(t *testing.T) {
	pool := testPool(t)
	jira := newFakeJira(t, "satyam@example.com", "the-only-valid-token")
	h := newConnectionsRouter(t, pool, "")

	rec := putJSON(t, h, "/api/connections/jira",
		`{"base_url":"`+jira.server.URL+`","email":"satyam@example.com","api_token":"sk-wrong"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fakeJiraRejection) {
		t.Errorf("422 body %q does not carry Atlassian's own reason %q", rec.Body.String(), fakeJiraRejection)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections`); n != 0 {
		t.Errorf("stored connections = %d after a rejected token, want 0", n)
	}
	// The user stays in demo mode: nothing was connected.
	_, overview := getConnections(t, h)
	if overview.Mode != "demo" {
		t.Errorf("mode after a rejected paste = %q, want demo", overview.Mode)
	}
}

// Malformed requests are rejected before any live call or database work.
func TestPutJiraConnectionRejectsUnusableRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "base_url is not a URL",
			body:       `{"base_url":"not a url","email":"a@b.c","api_token":"t"}`,
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "missing email",
			body:       `{"base_url":"https://x.atlassian.net","email":"","api_token":"t"}`,
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "missing token",
			body:       `{"base_url":"https://x.atlassian.net","email":"a@b.c","api_token":"  "}`,
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "malformed JSON",
			body:       `{"base_url":`,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// stubDB: all of these must be rejected before database work.
			h := newConnectionsRouter(t, &stubDB{}, "")
			rec := putJSON(t, h, "/api/connections/jira", tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestPutNotionConnectionValidatesStoresEncryptedAndCapturesIdentity(t *testing.T) {
	pool := testPool(t)
	notion := newFakeNotion(t, testNotionToken)
	h := newConnectionsRouter(t, pool, notion.server.URL)

	rec := putJSON(t, h, "/api/connections/notion", `{"token":"`+testNotionToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if notion.calls.Load() != 1 {
		t.Errorf("validation calls = %d, want exactly 1 live /v1/users/me check", notion.calls.Load())
	}
	if strings.Contains(rec.Body.String(), testNotionToken) {
		t.Errorf("PUT response echoes the integration token: %q", rec.Body.String())
	}
	var saved struct {
		Status   string            `json:"status"`
		Identity map[string]string `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode PUT response %q: %v", rec.Body.String(), err)
	}
	if saved.Status != "connected" {
		t.Errorf("status = %q, want connected", saved.Status)
	}
	if saved.Identity["bot_name"] != "Cortex Bot" || saved.Identity["workspace_name"] != "Acme Workspace" {
		t.Errorf("identity = %v, want the bot/workspace names Notion returned", saved.Identity)
	}

	var ciphertext []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT credentials_ciphertext FROM user_connections WHERE source = 'notion'`).Scan(&ciphertext); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	if strings.Contains(string(ciphertext), testNotionToken) {
		t.Fatal("the database holds the plaintext integration token")
	}
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	plain, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt stored ciphertext: %v", err)
	}
	if !strings.Contains(plain, testNotionToken) {
		t.Errorf("decrypted credentials %q do not round-trip the pasted token", plain)
	}

	getRec, overview := getConnections(t, h)
	if strings.Contains(getRec.Body.String(), testNotionToken) {
		t.Errorf("GET /api/connections echoes the integration token: %q", getRec.Body.String())
	}
	if overview.Sources["notion"].Status != "connected" || overview.Mode != "user" {
		t.Errorf("overview = mode %q notion %q, want user/connected", overview.Mode, overview.Sources["notion"].Status)
	}
}

func TestPutNotionConnectionRejectsAnInvalidTokenWith422(t *testing.T) {
	pool := testPool(t)
	notion := newFakeNotion(t, "the-only-valid-token")
	h := newConnectionsRouter(t, pool, notion.server.URL)

	rec := putJSON(t, h, "/api/connections/notion", `{"token":"ntn_wrong"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fakeNotionRejection) {
		t.Errorf("422 body %q does not carry Notion's own reason %q", rec.Body.String(), fakeNotionRejection)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections`); n != 0 {
		t.Errorf("stored connections = %d after a rejected token, want 0", n)
	}

	// An empty token is refused before any live call.
	empty := putJSON(t, h, "/api/connections/notion", `{"token":"   "}`)
	if empty.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty-token status = %d, want 422", empty.Code)
	}
}

// DELETE removes a connection; mode returns to demo only when the last one
// goes.
func TestDeleteConnectionAndModeTransitions(t *testing.T) {
	const email = "satyam@example.com"

	pool := testPool(t)
	jira := newFakeJira(t, email, testJiraToken)
	notion := newFakeNotion(t, testNotionToken)
	h := newConnectionsRouter(t, pool, notion.server.URL)

	if rec := putJSON(t, h, "/api/connections/jira",
		`{"base_url":"`+jira.server.URL+`","email":"`+email+`","api_token":"`+testJiraToken+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("connect jira = %d (body %q)", rec.Code, rec.Body.String())
	}
	if rec := putJSON(t, h, "/api/connections/notion", `{"token":"`+testNotionToken+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("connect notion = %d (body %q)", rec.Code, rec.Body.String())
	}
	if _, overview := getConnections(t, h); overview.Mode != "user" {
		t.Fatalf("mode with two connections = %q, want user", overview.Mode)
	}

	del := func(source string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localRequest(http.MethodDelete, "/api/connections/"+source, nil))
		return rec
	}

	if rec := del("jira"); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE jira = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}
	if _, overview := getConnections(t, h); overview.Mode != "user" {
		t.Errorf("mode with notion still connected = %q, want user (demo only when nothing remains)", overview.Mode)
	}

	if rec := del("notion"); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE notion = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}
	_, overview := getConnections(t, h)
	if overview.Mode != "demo" {
		t.Errorf("mode after the last disconnect = %q, want demo", overview.Mode)
	}
	if overview.Sources["jira"].Status != "absent" || overview.Sources["notion"].Status != "absent" {
		t.Errorf("sources after disconnecting = %v, want absent", overview.Sources)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections`); n != 0 {
		t.Errorf("rows after both deletes = %d, want 0", n)
	}

	if rec := del("notion"); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE with nothing stored = %d, want 404", rec.Code)
	}
	if rec := del("slack"); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown source = %d, want 404", rec.Code)
	}
}

// The demo toggle flips mode without touching connections, and an errored
// connection still counts as a connection — mode stays user, never a silent
// demo swap.
func TestModeToggleAndErroredConnectionStillCountsAsUser(t *testing.T) {
	const email = "satyam@example.com"

	pool := testPool(t)
	jira := newFakeJira(t, email, testJiraToken)
	h := newConnectionsRouter(t, pool, "")

	if rec := putJSON(t, h, "/api/connections/jira",
		`{"base_url":"`+jira.server.URL+`","email":"`+email+`","api_token":"`+testJiraToken+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("connect jira = %d (body %q)", rec.Code, rec.Body.String())
	}

	// Toggle on: mode demo, connection untouched.
	rec := putJSON(t, h, "/api/connections/mode", `{"use_demo_workspace":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT mode = %d (body %q)", rec.Code, rec.Body.String())
	}
	var overview connectionsOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode mode response %q: %v", rec.Body.String(), err)
	}
	if overview.Mode != "demo" || !overview.UseDemoWorkspace {
		t.Errorf("after toggling on: mode %q use_demo %v, want demo/true", overview.Mode, overview.UseDemoWorkspace)
	}
	if overview.Sources["jira"].Status != "connected" {
		t.Errorf("toggling demo changed the jira card to %q, want still connected", overview.Sources["jira"].Status)
	}

	// Toggle off: back to user.
	rec = putJSON(t, h, "/api/connections/mode", `{"use_demo_workspace":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT mode = %d (body %q)", rec.Code, rec.Body.String())
	}
	if _, overview := getConnections(t, h); overview.Mode != "user" {
		t.Errorf("mode after toggling off = %q, want user", overview.Mode)
	}

	// Mark the only connection errored: the card says error with a reason and
	// mode STAYS user.
	if _, err := pool.Exec(context.Background(),
		`UPDATE user_connections SET status = 'error', last_error = 'refresh token revoked' WHERE source = 'jira'`); err != nil {
		t.Fatalf("mark jira errored: %v", err)
	}
	getRec, overview := getConnections(t, h)
	if overview.Sources["jira"].Status != "error" {
		t.Errorf("errored jira card status = %q, want error", overview.Sources["jira"].Status)
	}
	if overview.Sources["jira"].LastError == "" {
		t.Error("errored jira card has no last_error for the UI to show")
	}
	if overview.Mode != "user" {
		t.Errorf("mode with an errored connection = %q, want user (never a silent demo swap)", overview.Mode)
	}
	if strings.Contains(getRec.Body.String(), testJiraToken) {
		t.Errorf("overview echoes the API token: %q", getRec.Body.String())
	}
}

// Connections are per user: what A pastes, B's overview does not show.
func TestConnectionsAreIsolatedPerUser(t *testing.T) {
	pool := testPool(t)
	jira := newFakeJira(t, "alice@example.com", testJiraToken)
	h := newConnectionsRouter(t, pool, "")

	cookieA, _ := createSessionForEmail(t, pool, "alice@example.com")
	cookieB, _ := createSessionForEmail(t, pool, "bob@example.com")

	req := sessionRequest(http.MethodPut, "/api/connections/jira", strings.NewReader(
		`{"base_url":"`+jira.server.URL+`","email":"alice@example.com","api_token":"`+testJiraToken+`"}`), cookieA)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("A connects jira = %d (body %q)", rec.Code, rec.Body.String())
	}

	recB := httptest.NewRecorder()
	h.ServeHTTP(recB, sessionRequest(http.MethodGet, "/api/connections", nil, cookieB))
	if recB.Code != http.StatusOK {
		t.Fatalf("B GET /api/connections = %d (body %q)", recB.Code, recB.Body.String())
	}
	var overviewB connectionsOverview
	if err := json.Unmarshal(recB.Body.Bytes(), &overviewB); err != nil {
		t.Fatalf("decode B's overview %q: %v", recB.Body.String(), err)
	}
	if overviewB.Mode != "demo" {
		t.Errorf("B's mode = %q, want demo — A's connection must not leak", overviewB.Mode)
	}
	if overviewB.Sources["jira"].Status != "absent" {
		t.Errorf("B sees A's jira connection as %q, want absent", overviewB.Sources["jira"].Status)
	}

	// And B cannot delete A's connection.
	recDel := httptest.NewRecorder()
	h.ServeHTTP(recDel, sessionRequest(http.MethodDelete, "/api/connections/jira", nil, cookieB))
	if recDel.Code != http.StatusNotFound {
		t.Errorf("B DELETE A's jira = %d, want 404", recDel.Code)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections`); n != 1 {
		t.Errorf("connections after B's delete attempt = %d, want A's 1 intact", n)
	}
}
