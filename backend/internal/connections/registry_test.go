package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/agent"
	"cortex/internal/connections"
)

// TEST-8.4 — per-run tool construction (REQ-8.4), plus the TEST-8.3 clause
// about a Gmail refresh failure: status=error and gmail absent from the user's
// registry, never replaced by demo data. All provider traffic goes to httptest
// fakes; the fake Jira records the Basic auth it received, which is how the
// tests prove a user's tool set was built from THEIR stored credentials.

// jiraToolNames is the complete Jira tool set a connected site provides.
var jiraToolNames = []string{
	"jira_get_comments", "jira_get_issue", "jira_get_issue_history",
	"jira_list_projects", "jira_search_issues",
}

// notionToolNames is the complete Notion tool set.
var notionToolNames = []string{"notion_get_page", "notion_search"}

// gmailToolNames is the complete Gmail tool set.
var gmailToolNames = []string{"gmail_get_message", "gmail_search"}

// fakeJiraSite is an httptest Jira that records the Basic auth of every
// request to /rest/api/3/project/search and answers with one project.
type fakeJiraSite struct {
	server *httptest.Server

	mu    sync.Mutex
	users []string // "email:token" per request, so credential provenance is assertable
}

func newFakeJiraSite(t *testing.T) *fakeJiraSite {
	t.Helper()
	f := &fakeJiraSite{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, token, _ := r.BasicAuth()
		f.mu.Lock()
		f.users = append(f.users, email+":"+token)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/rest/api/3/project/search" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessages":["fakeJira: no route for ` + r.URL.Path + `"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"values":[{"id":"10001","key":"SATY","name":"Satyam Project"}],"isLast":true}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeJiraSite) authSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.users)
}

// fakeGoogleToken is an httptest Google token endpoint whose behaviour each
// test scripts: a granted refresh, a permanent invalid_grant, or a 500.
type fakeGoogleToken struct {
	server *httptest.Server

	mu     sync.Mutex
	status int
	body   string
	forms  []string
}

func newFakeGoogleToken(t *testing.T) *fakeGoogleToken {
	t.Helper()
	f := &fakeGoogleToken{
		status: http.StatusOK,
		body:   `{"access_token":"ya29.fake-access","token_type":"Bearer","expires_in":3600}`,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.forms = append(f.forms, string(raw))
		status, body := f.status, f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGoogleToken) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// newBuilder wires a RegistryBuilder whose Google endpoint is the fake.
func newBuilder(t *testing.T, pool *pgxpool.Pool, google *fakeGoogleToken) (*connections.RegistryBuilder, *connections.Service) {
	t.Helper()
	svc := newTestService(t, pool)
	cfg := connections.RegistryBuilderConfig{
		Service:            svc,
		Demo:               newDemoRegistry(t),
		GoogleClientID:     "web-client-id.apps.googleusercontent.com",
		GoogleClientSecret: "web-client-secret",
		Logger:             discardLogger(),
	}
	if google != nil {
		cfg.GoogleTokenURL = google.server.URL + "/token"
	}
	builder, err := connections.NewRegistryBuilder(cfg)
	if err != nil {
		t.Fatalf("build registry builder: %v", err)
	}
	return builder, svc
}

func saveJira(t *testing.T, svc *connections.Service, userID uuid.UUID, baseURL, email, token string) {
	t.Helper()
	err := svc.Save(context.Background(), userID, connections.SourceJira,
		connections.JiraCredentials{BaseURL: baseURL, Email: email, APIToken: token},
		map[string]string{"site_url": baseURL})
	if err != nil {
		t.Fatalf("save jira connection: %v", err)
	}
}

func saveGmail(t *testing.T, svc *connections.Service, userID uuid.UUID, refreshToken string) {
	t.Helper()
	err := svc.Save(context.Background(), userID, connections.SourceGmail,
		connections.GmailCredentials{RefreshToken: refreshToken},
		map[string]string{"email": "owner@gmail.test"})
	if err != nil {
		t.Fatalf("save gmail connection: %v", err)
	}
}

func TestForUserWithZeroConnectionsGetsTheFullDemoRegistry(t *testing.T) {
	pool := testPool(t)
	builder, _ := newBuilder(t, pool, nil)
	userID := insertUser(t, pool)

	registry, sources, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if sources.Mode != agent.ModeDemo {
		t.Errorf("sources.Mode = %q, want demo", sources.Mode)
	}
	if len(sources.Connected) != 0 {
		t.Errorf("sources.Connected = %v, want empty in demo mode", sources.Connected)
	}
	want := []string{"gmail_search", "jira_search_issues", "notion_search", "search_knowledge_base"}
	if got := registry.Names(); !slices.Equal(got, want) {
		t.Errorf("demo registry names = %v, want the full demo set %v", got, want)
	}
}

func TestForUserJiraOnlyBuildsOnlyJiraToolsFromTheirCredentials(t *testing.T) {
	pool := testPool(t)
	jiraSite := newFakeJiraSite(t)
	builder, svc := newBuilder(t, pool, nil)
	userID := insertUser(t, pool)
	saveJira(t, svc, userID, jiraSite.server.URL, "satyam@example.com", "their-jira-token-0042")

	registry, sources, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if sources.Mode != agent.ModeUser {
		t.Errorf("sources.Mode = %q, want user", sources.Mode)
	}
	if !slices.Equal(sources.Connected, []string{"jira"}) {
		t.Errorf("sources.Connected = %v, want [jira]", sources.Connected)
	}

	// ONLY the Jira tool set: no demo clients, no other sources, and no
	// knowledge-base tool (the corpus IS the demo workspace).
	if got := registry.Names(); !slices.Equal(got, jiraToolNames) {
		t.Errorf("registry names = %v, want exactly the jira tool set %v", got, jiraToolNames)
	}
	for _, name := range registry.Names() {
		if name == "search_knowledge_base" {
			t.Error("a user-mode registry carries search_knowledge_base; the demo corpus must not answer for real data")
		}
	}

	// The tools were built from THEIR decrypted credentials: executing one
	// hits their own site with their own basic auth.
	tool, ok := registry.Get("jira_list_projects")
	if !ok {
		t.Fatal("jira_list_projects missing from the user registry")
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute jira_list_projects: %v", err)
	}
	if !strings.Contains(result.Content, "SATY") {
		t.Errorf("tool result %q does not carry the user's own project", result.Content)
	}
	auth := jiraSite.authSeen()
	if len(auth) == 0 {
		t.Fatal("the user's Jira site was never called")
	}
	for _, a := range auth {
		if a != "satyam@example.com:their-jira-token-0042" {
			t.Errorf("jira request authenticated as %q, want the user's stored email:token", a)
		}
	}
}

func TestForUserDemoToggleRestoresTheFullDemoSet(t *testing.T) {
	pool := testPool(t)
	jiraSite := newFakeJiraSite(t)
	builder, svc := newBuilder(t, pool, nil)
	userID := insertUser(t, pool)
	saveJira(t, svc, userID, jiraSite.server.URL, "satyam@example.com", "tok")
	if err := svc.SetUseDemo(context.Background(), userID, true); err != nil {
		t.Fatalf("SetUseDemo: %v", err)
	}

	registry, sources, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if sources.Mode != agent.ModeDemo {
		t.Errorf("sources.Mode with the toggle on = %q, want demo", sources.Mode)
	}
	want := []string{"gmail_search", "jira_search_issues", "notion_search", "search_knowledge_base"}
	if got := registry.Names(); !slices.Equal(got, want) {
		t.Errorf("registry names with the toggle on = %v, want the full demo set %v", got, want)
	}
	if len(jiraSite.authSeen()) != 0 {
		t.Errorf("demo-toggle run touched the user's Jira site %d time(s), want 0", len(jiraSite.authSeen()))
	}
}

// TEST-8.3 (registry half): a permanently failing refresh marks the gmail
// connection status=error and drops it from THIS user's registry — the run
// keeps their other sources and never gets demo data instead.
func TestGmailRefreshFailureMarksErrorAndDropsGmailNotReplacedByDemo(t *testing.T) {
	const refreshToken = "grt-revoked-refresh-token-9999"

	pool := testPool(t)
	jiraSite := newFakeJiraSite(t)
	google := newFakeGoogleToken(t)
	google.respond(http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	builder, svc := newBuilder(t, pool, google)
	userID := insertUser(t, pool)
	saveJira(t, svc, userID, jiraSite.server.URL, "satyam@example.com", "tok")
	saveGmail(t, svc, userID, refreshToken)

	registry, sources, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if sources.Mode != agent.ModeUser {
		t.Errorf("sources.Mode = %q, want user — a broken gmail credential must not demote to demo", sources.Mode)
	}
	if !slices.Equal(sources.Connected, []string{"jira"}) {
		t.Errorf("sources.Connected = %v, want [jira] (gmail dropped)", sources.Connected)
	}
	if got := registry.Names(); !slices.Equal(got, jiraToolNames) {
		t.Errorf("registry names = %v, want only the jira set %v — gmail absent, no demo substitutes", got, jiraToolNames)
	}

	// The connection is marked so the UI can offer "reconnect", with a safe
	// message that never carries the token.
	var status, lastError string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, coalesce(last_error, '') FROM user_connections WHERE user_id = $1 AND source = 'gmail'`,
		userID).Scan(&status, &lastError); err != nil {
		t.Fatalf("read gmail connection: %v", err)
	}
	if status != "error" {
		t.Errorf("gmail connection status = %q, want error", status)
	}
	if lastError == "" {
		t.Error("gmail connection has no last_error; the UI needs a reason to show")
	}
	if strings.Contains(lastError, refreshToken) {
		t.Errorf("last_error %q carries the refresh token", lastError)
	}
}

// When every connection is errored the builder reports ErrNoUsableSources with
// an honest empty registry and user-mode sources — the run fails rather than
// silently answering from the demo workspace.
func TestForUserWithOnlyErroredConnectionsFailsWithoutDemoFallback(t *testing.T) {
	pool := testPool(t)
	google := newFakeGoogleToken(t)
	google.respond(http.StatusUnauthorized,
		`{"error":"invalid_grant","error_description":"Bad Request"}`)
	builder, svc := newBuilder(t, pool, google)
	userID := insertUser(t, pool)
	saveGmail(t, svc, userID, "grt-gone")

	registry, sources, err := builder.ForUser(context.Background(), userID)
	if !errors.Is(err, agent.ErrNoUsableSources) {
		t.Fatalf("ForUser error = %v, want to wrap agent.ErrNoUsableSources", err)
	}
	if sources.Mode != agent.ModeUser {
		t.Errorf("sources.Mode = %q, want user — demo must never stand in for an errored connection", sources.Mode)
	}
	if registry == nil {
		t.Fatal("ForUser returned a nil registry alongside ErrNoUsableSources; the failed run's trace needs it")
	}
	if registry.Len() != 0 {
		t.Errorf("registry has %d tools (%v), want 0 — especially no demo tools", registry.Len(), registry.Names())
	}
}

// A transient Google failure (5xx) must not mark the connection: River retries
// the run, and the user must not see "reconnect" over a hiccup.
func TestGmailTransientRefreshFailureIsRetriedNotMarked(t *testing.T) {
	pool := testPool(t)
	google := newFakeGoogleToken(t)
	google.respond(http.StatusInternalServerError, `{"error":"internal_failure"}`)
	builder, svc := newBuilder(t, pool, google)
	userID := insertUser(t, pool)
	saveGmail(t, svc, userID, "grt-fine-google-down")

	_, _, err := builder.ForUser(context.Background(), userID)
	if err == nil {
		t.Fatal("ForUser returned nil for a transient Google failure, want an error so River retries")
	}
	if errors.Is(err, agent.ErrNoUsableSources) {
		t.Errorf("transient failure classified permanent: %v", err)
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM user_connections WHERE user_id = $1 AND source = 'gmail'`,
		userID).Scan(&status); err != nil {
		t.Fatalf("read gmail connection: %v", err)
	}
	if status != "active" {
		t.Errorf("gmail connection status after a transient failure = %q, want active (not marked)", status)
	}
}

// User B's registry is built from user B's connections alone.
func TestForUserNeverSeesAnotherUsersConnections(t *testing.T) {
	pool := testPool(t)
	jiraSite := newFakeJiraSite(t)
	builder, svc := newBuilder(t, pool, nil)

	userA := insertUser(t, pool)
	userB := insertUser(t, pool)
	userC := insertUser(t, pool)
	saveJira(t, svc, userA, jiraSite.server.URL, "a@example.com", "a-token")
	if err := svc.Save(context.Background(), userB, connections.SourceNotion,
		connections.NotionCredentials{Token: "ntn-b-token"}, map[string]string{"bot_name": "B Bot"}); err != nil {
		t.Fatalf("save notion for B: %v", err)
	}

	// B gets ONLY their notion tools — not A's jira.
	registryB, sourcesB, err := builder.ForUser(context.Background(), userB)
	if err != nil {
		t.Fatalf("ForUser(B): %v", err)
	}
	if !slices.Equal(sourcesB.Connected, []string{"notion"}) {
		t.Errorf("B's connected sources = %v, want [notion]", sourcesB.Connected)
	}
	if got := registryB.Names(); !slices.Equal(got, notionToolNames) {
		t.Errorf("B's registry = %v, want only the notion set %v (never A's jira)", got, notionToolNames)
	}

	// C, with nothing connected, stays on the demo workspace.
	_, sourcesC, err := builder.ForUser(context.Background(), userC)
	if err != nil {
		t.Fatalf("ForUser(C): %v", err)
	}
	if sourcesC.Mode != agent.ModeDemo {
		t.Errorf("C's mode = %q, want demo", sourcesC.Mode)
	}

	// A's registry really is A's.
	registryA, sourcesA, err := builder.ForUser(context.Background(), userA)
	if err != nil {
		t.Fatalf("ForUser(A): %v", err)
	}
	if !slices.Equal(sourcesA.Connected, []string{"jira"}) {
		t.Errorf("A's connected sources = %v, want [jira]", sourcesA.Connected)
	}
	if got := registryA.Names(); !slices.Equal(got, jiraToolNames) {
		t.Errorf("A's registry = %v, want only the jira set %v", got, jiraToolNames)
	}
}

// A user with gmail connected and a working refresh gets the gmail tool set —
// built from their stored refresh token via the eager credential check.
func TestForUserGmailOnlyBuildsGmailToolsAfterASuccessfulRefresh(t *testing.T) {
	const refreshToken = "grt-alive-refresh-token"

	pool := testPool(t)
	google := newFakeGoogleToken(t)
	builder, svc := newBuilder(t, pool, google)
	userID := insertUser(t, pool)
	saveGmail(t, svc, userID, refreshToken)

	registry, sources, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if !slices.Equal(sources.Connected, []string{"gmail"}) {
		t.Errorf("sources.Connected = %v, want [gmail]", sources.Connected)
	}
	if got := registry.Names(); !slices.Equal(got, gmailToolNames) {
		t.Errorf("registry names = %v, want only the gmail set %v", got, gmailToolNames)
	}
	// The eager refresh used the stored refresh token.
	google.mu.Lock()
	forms := slices.Clone(google.forms)
	google.mu.Unlock()
	if len(forms) != 1 {
		t.Fatalf("token endpoint calls = %d, want exactly 1 eager refresh", len(forms))
	}
	if !strings.Contains(forms[0], "refresh_token="+refreshToken) ||
		!strings.Contains(forms[0], "grant_type=refresh_token") {
		t.Errorf("refresh form %q does not carry the user's stored refresh token", forms[0])
	}
}
