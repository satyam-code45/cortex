package connections_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/connections"
	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
)

// Per-source write enablement.
//
// The claim under test is structural, not behavioural: a run whose owner has not
// explicitly enabled writes for a source has NO write tool in its registry at
// all. Not a disabled one, not one that refuses — absent, so the model cannot
// name it. The same flag gates the executors, because revoking permission has to
// mean revoking it for an approval already granted.
//
// Demo mode gets the guarantee by a second, independent route: the demo registry
// is assembled from read tools only, and the demo workspace is a real Jira site
// and a real mailbox belonging to a real person.

// jiraWriteToolNames is the full Jira write set.
var jiraWriteToolNames = []string{
	"jira_add_comment", "jira_create_issue", "jira_transition_issue", "jira_update_issue",
}

// notionWriteToolNames is the full Notion write set.
var notionWriteToolNames = []string{"notion_append_to_page", "notion_create_page"}

// gmailWriteToolNames is the full Gmail write set.
var gmailWriteToolNames = []string{"gmail_send_email"}

// fakeJiraWriteSite is an httptest Jira answering the two reads a write-enabled
// connection needs: the project list (registry construction) and mypermissions
// (write readiness). Every write verb is recorded and refused, because nothing
// in this file may perform one.
type fakeJiraWriteSite struct {
	server *httptest.Server

	mu sync.Mutex
	// held is the permission set the site reports; nil means all of them.
	held        map[string]bool
	permQueries []string
	mutations   []string
}

func newFakeJiraWriteSite(t *testing.T) *fakeJiraWriteSite {
	t.Helper()
	f := &fakeJiraWriteSite{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != http.MethodGet {
			f.mu.Lock()
			f.mutations = append(f.mutations, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errorMessages":["fakeJira: no write may happen here"]}`))
			return
		}

		switch r.URL.Path {
		case "/rest/api/3/project/search":
			_, _ = w.Write([]byte(`{"values":[{"id":"10001","key":"SATY","name":"Satyam Project"}],"isLast":true}`))
		case "/rest/api/3/mypermissions":
			f.mu.Lock()
			f.permQueries = append(f.permQueries, r.URL.Query().Get("permissions"))
			held := f.held
			f.mu.Unlock()
			var b strings.Builder
			b.WriteString(`{"permissions":{`)
			for i, key := range []string{"CREATE_ISSUES", "EDIT_ISSUES", "ADD_COMMENTS", "TRANSITION_ISSUES"} {
				have := true
				if held != nil {
					have = held[key]
				}
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`"` + key + `":{"havePermission":`)
				if have {
					b.WriteString("true}")
				} else {
					b.WriteString("false}")
				}
			}
			b.WriteString(`}}`)
			_, _ = w.Write([]byte(b.String()))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessages":["fakeJira: no route for ` + r.URL.Path + `"]}`))
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeJiraWriteSite) grant(held map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = held
}

func (f *fakeJiraWriteSite) permissionsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.permQueries)
}

func (f *fakeJiraWriteSite) mutationsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.mutations)
}

// writeToolNames returns the registry's proposing tools.
func writeToolNames(r *tools.Registry) []string { return r.WriteNames() }

// enableWrites flips the per-source flag directly, standing in for the endpoint.
func enableWrites(t *testing.T, svc *connections.Service, userID uuid.UUID, source string) {
	t.Helper()
	if err := svc.SetWritesEnabled(context.Background(), userID, source, true); err != nil {
		t.Fatalf("enable writes for %s: %v", source, err)
	}
}

// saveNotion stores a Notion connection.
func saveNotion(t *testing.T, svc *connections.Service, userID uuid.UUID, token string) {
	t.Helper()
	err := svc.Save(context.Background(), userID, connections.SourceNotion,
		connections.NotionCredentials{Token: token},
		map[string]string{"workspace_name": "Acme"})
	if err != nil {
		t.Fatalf("save notion connection: %v", err)
	}
}

// saveGmailWithScopes stores a Gmail connection carrying an explicit scope set,
// which is what decides whether the send tool may be registered.
func saveGmailWithScopes(t *testing.T, svc *connections.Service, userID uuid.UUID, scopes string) {
	t.Helper()
	err := svc.Save(context.Background(), userID, connections.SourceGmail,
		connections.GmailCredentials{RefreshToken: "1//fake-refresh", Scopes: scopes},
		map[string]string{"email": "owner@gmail.test"})
	if err != nil {
		t.Fatalf("save gmail connection: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the tools are absent unless writes were enabled
// ---------------------------------------------------------------------------

// With writes_enabled=false — the default for every connection — the
// registry holds the read tools and nothing else. A run cannot even name a write
// tool, so no prompt produces a proposal.
func TestWriteToolsAreAbsentUntilWritesAreEnabled(t *testing.T) {
	pool := testPool(t)
	site := newFakeJiraWriteSite(t)
	builder, svc := newBuilder(t, pool, nil)
	userID := insertUser(t, pool)
	saveJira(t, svc, userID, site.server.URL, "satyam@example.com", "their-jira-token-0042")

	registry, _, err := builder.ForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if got := writeToolNames(registry); len(got) != 0 {
		t.Errorf("write tools = %v on a connection with writes disabled, want none", got)
	}
	if got := registry.Names(); !slices.Equal(got, jiraToolNames) {
		t.Errorf("registry names = %v, want exactly the read set %v", got, jiraToolNames)
	}

	// And no executor exists either: an approval could not be carried out.
	writers, err := builder.WritersForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("WritersForUser: %v", err)
	}
	if writers.Len() != 0 {
		t.Errorf("writers = %d with writes disabled, want 0", writers.Len())
	}
	if got := site.mutationsSeen(); len(got) != 0 {
		t.Errorf("the fake site saw write requests %v; building a registry must perform none", got)
	}
}

// Enabling writes for one source registers that source's
// write tools and nobody else's.
func TestEnablingWritesRegistersOnlyThatSourcesWriteTools(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		wantWrites []string
	}{
		{name: "jira", source: connections.SourceJira, wantWrites: jiraWriteToolNames},
		{name: "notion", source: connections.SourceNotion, wantWrites: notionWriteToolNames},
		{name: "gmail", source: connections.SourceGmail, wantWrites: gmailWriteToolNames},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			site := newFakeJiraWriteSite(t)
			google := newFakeGoogleToken(t)
			builder, svc := newBuilder(t, pool, google)
			userID := insertUser(t, pool)

			// All three sources connected, so "only that source" is assertable.
			saveJira(t, svc, userID, site.server.URL, "satyam@example.com", "their-jira-token-0042")
			saveNotion(t, svc, userID, "ntn_users_own_integration_token_0042")
			saveGmailWithScopes(t, svc, userID, gmail.ScopeReadonly+" "+gmail.ScopeSend)
			enableWrites(t, svc, userID, tt.source)

			registry, _, err := builder.ForUser(context.Background(), userID)
			if err != nil {
				t.Fatalf("ForUser: %v", err)
			}
			if got := writeToolNames(registry); !slices.Equal(got, tt.wantWrites) {
				t.Errorf("write tools = %v, want exactly %v", got, tt.wantWrites)
			}
			// Every write tool is marked as proposing and resolvable.
			for _, name := range tt.wantWrites {
				tool, ok := registry.Get(name)
				if !ok {
					t.Fatalf("%s is not in the registry", name)
				}
				if _, ok := tool.(tools.Proposing); !ok {
					t.Errorf("%s is not marked as proposing; the prompt would be wrong about it", name)
				}
			}

			// The executors match the tools, one per action, and only for the
			// enabled source.
			writers, err := builder.WritersForUser(context.Background(), userID)
			if err != nil {
				t.Fatalf("WritersForUser: %v", err)
			}
			if writers.Len() != len(tt.wantWrites) {
				t.Errorf("writers = %d, want %d (one per write tool)", writers.Len(), len(tt.wantWrites))
			}
			if got := site.mutationsSeen(); len(got) != 0 {
				t.Errorf("the fake site saw write requests %v; registration must perform none", got)
			}
		})
	}
}

// Gmail writes need the send scope on the token itself. A connection
// made before writes were enabled carries a readonly-only token, so the send
// tool stays out of the registry even with the flag on — the alternative is a
// proposal a person approves and Google then refuses.
func TestGmailWritesNeedTheSendScopeOnTheStoredToken(t *testing.T) {
	tests := []struct {
		name       string
		scopes     string
		wantWrites []string
	}{
		{name: "readonly only", scopes: gmail.ScopeReadonly, wantWrites: nil},
		{name: "no scopes recorded", scopes: "", wantWrites: nil},
		{name: "readonly and send", scopes: gmail.ScopeReadonly + " " + gmail.ScopeSend, wantWrites: gmailWriteToolNames},
		{name: "send first", scopes: gmail.ScopeSend + " " + gmail.ScopeReadonly, wantWrites: gmailWriteToolNames},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			google := newFakeGoogleToken(t)
			builder, svc := newBuilder(t, pool, google)
			userID := insertUser(t, pool)
			saveGmailWithScopes(t, svc, userID, tt.scopes)
			enableWrites(t, svc, userID, connections.SourceGmail)

			registry, _, err := builder.ForUser(context.Background(), userID)
			if err != nil {
				t.Fatalf("ForUser: %v", err)
			}
			got := writeToolNames(registry)
			if len(tt.wantWrites) == 0 {
				if len(got) != 0 {
					t.Errorf("write tools = %v on a token without the send scope, want none", got)
				}
			} else if !slices.Equal(got, tt.wantWrites) {
				t.Errorf("write tools = %v, want %v", got, tt.wantWrites)
			}

			// The read tools are unaffected either way: enabling writes must
			// never cost a user their read access.
			for _, name := range gmailToolNames {
				if _, ok := registry.Get(name); !ok {
					t.Errorf("%s is missing; the read path must be untouched", name)
				}
			}
		})
	}
}

// Writes are never available in demo mode. The demo registry is
// assembled from read tools only, and a demo-mode user has no executors —
// the demo credentials are not theirs to write with.
func TestDemoModeHasNoWriteToolsAndNoWriters(t *testing.T) {
	pool := testPool(t)
	site := newFakeJiraWriteSite(t)
	builder, svc := newBuilder(t, pool, nil)

	// A user with nothing connected is in demo mode.
	fresh := insertUser(t, pool)
	registry, _, err := builder.ForUser(context.Background(), fresh)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if got := writeToolNames(registry); len(got) != 0 {
		t.Errorf("demo registry write tools = %v, want none", got)
	}
	writers, err := builder.WritersForUser(context.Background(), fresh)
	if err != nil {
		t.Fatalf("WritersForUser: %v", err)
	}
	if writers.Len() != 0 {
		t.Errorf("demo writers = %d, want 0", writers.Len())
	}

	// And a user who connected their own Jira, enabled writes, and then turned
	// the demo workspace on is back on somebody else's data: no writes there
	// either, even though their own connection has the flag set.
	toggled := insertUser(t, pool)
	saveJira(t, svc, toggled, site.server.URL, "satyam@example.com", "their-jira-token-0042")
	enableWrites(t, svc, toggled, connections.SourceJira)
	if err := svc.SetUseDemo(context.Background(), toggled, true); err != nil {
		t.Fatalf("set use_demo_workspace: %v", err)
	}

	registry, _, err = builder.ForUser(context.Background(), toggled)
	if err != nil {
		t.Fatalf("ForUser with the demo toggle: %v", err)
	}
	if got := writeToolNames(registry); len(got) != 0 {
		t.Errorf("write tools = %v on a demo-toggled run, want none — the demo workspace is somebody else's", got)
	}
	writers, err = builder.WritersForUser(context.Background(), toggled)
	if err != nil {
		t.Fatalf("WritersForUser with the demo toggle: %v", err)
	}
	if writers.Len() != 0 {
		t.Errorf("writers = %d on a demo-toggled user, want 0", writers.Len())
	}
}

// A write-enabled connection never lends its write tools to another user.
func TestWriteToolsAreNeverBorrowedFromAnotherUser(t *testing.T) {
	pool := testPool(t)
	site := newFakeJiraWriteSite(t)
	builder, svc := newBuilder(t, pool, nil)

	owner := insertUser(t, pool)
	saveJira(t, svc, owner, site.server.URL, "owner@example.com", "owner-token-0042")
	enableWrites(t, svc, owner, connections.SourceJira)

	other := insertUser(t, pool)
	saveJira(t, svc, other, site.server.URL, "other@example.com", "other-token-0099")

	registry, _, err := builder.ForUser(context.Background(), other)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if got := writeToolNames(registry); len(got) != 0 {
		t.Errorf("write tools = %v for a user who enabled none, want none", got)
	}
}

// ---------------------------------------------------------------------------
// write readiness
// ---------------------------------------------------------------------------

// Jira write enablement verifies the account's permissions via
// /rest/api/3/mypermissions and refuses without them, so a user is never told
// writes are on for an account Jira will refuse.
func TestJiraWriteReadinessChecksPermissions(t *testing.T) {
	tests := []struct {
		name        string
		held        map[string]bool
		wantErr     bool
		wantMessage string
	}{
		{name: "all granted", held: nil},
		{
			name: "missing edit and comment",
			held: map[string]bool{
				"CREATE_ISSUES": true, "TRANSITION_ISSUES": true,
				"EDIT_ISSUES": false, "ADD_COMMENTS": false,
			},
			wantErr: true, wantMessage: "ADD_COMMENTS",
		},
		{
			name:    "nothing granted",
			held:    map[string]bool{},
			wantErr: true, wantMessage: "EDIT_ISSUES",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			site := newFakeJiraWriteSite(t)
			site.grant(tt.held)
			builder, svc := newBuilder(t, pool, nil)
			userID := insertUser(t, pool)
			saveJira(t, svc, userID, site.server.URL, "satyam@example.com", "their-jira-token-0042")

			err := builder.CheckWriteReadiness(context.Background(), userID, connections.SourceJira)
			if tt.wantErr {
				if !errors.Is(err, connections.ErrWritesUnavailable) {
					t.Fatalf("CheckWriteReadiness error = %v, want ErrWritesUnavailable", err)
				}
				if !strings.Contains(err.Error(), tt.wantMessage) {
					t.Errorf("error %q does not name the missing permission %q", err, tt.wantMessage)
				}
			} else if err != nil {
				t.Fatalf("CheckWriteReadiness: %v", err)
			}

			// The check is a read, and it asks Jira explicitly which permissions
			// it wants (an omitted list is a 400, not "all of them").
			asked := site.permissionsAsked()
			if len(asked) == 0 {
				t.Fatal("mypermissions was never called; the permission check did not happen")
			}
			for _, key := range []string{"CREATE_ISSUES", "EDIT_ISSUES", "ADD_COMMENTS", "TRANSITION_ISSUES"} {
				if !strings.Contains(asked[0], key) {
					t.Errorf("permissions query %q does not ask about %s", asked[0], key)
				}
			}
			if got := site.mutationsSeen(); len(got) != 0 {
				t.Errorf("the readiness check performed writes %v; it must be a read", got)
			}
		})
	}
}

// Gmail readiness is the send scope, and the message points at the
// only thing that can grant it — a fresh consent screen.
func TestGmailWriteReadinessRequiresTheSendScope(t *testing.T) {
	pool := testPool(t)
	builder, svc := newBuilder(t, pool, nil)
	userID := insertUser(t, pool)

	saveGmailWithScopes(t, svc, userID, gmail.ScopeReadonly)
	err := builder.CheckWriteReadiness(context.Background(), userID, connections.SourceGmail)
	if !errors.Is(err, connections.ErrWritesUnavailable) {
		t.Fatalf("readiness with a readonly token = %v, want ErrWritesUnavailable", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "reconnect") {
		t.Errorf("error %q does not tell the user to reconnect Gmail", err)
	}

	saveGmailWithScopes(t, svc, userID, gmail.ScopeReadonly+" "+gmail.ScopeSend)
	if err := builder.CheckWriteReadiness(context.Background(), userID, connections.SourceGmail); err != nil {
		t.Fatalf("readiness with the send scope: %v", err)
	}
}

// Notion has no capability endpoint, so there is nothing to pre-check
// and enablement proceeds. The one alternative — attempting a real write to find
// out — is the exact unattended side effect this day exists to prevent, so the
// readiness check must make no request at all.
func TestNotionWriteReadinessMakesNoRequest(t *testing.T) {
	pool := testPool(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"object":"error","status":403,"code":"restricted_resource",` +
			`"message":"notion: nothing may be attempted here"}`))
	}))
	t.Cleanup(server.Close)

	svc := newTestService(t, pool)
	builder, err := connections.NewRegistryBuilder(connections.RegistryBuilderConfig{
		Service:       svc,
		Demo:          newDemoRegistry(t),
		NotionBaseURL: server.URL,
		Logger:        discardLogger(),
	})
	if err != nil {
		t.Fatalf("build registry builder: %v", err)
	}
	userID := insertUser(t, pool)
	saveNotion(t, svc, userID, "ntn_users_own_integration_token_0042")

	if err := builder.CheckWriteReadiness(context.Background(), userID, connections.SourceNotion); err != nil {
		t.Fatalf("notion readiness = %v, want nil (nothing is checkable)", err)
	}
	if calls != 0 {
		t.Errorf("notion readiness made %d request(s); it must attempt nothing", calls)
	}
}

// Readiness on a source that was never connected is a missing connection, not a
// permission problem — the endpoint answers 404 rather than 409 on it.
func TestWriteReadinessOnAnUnconnectedSource(t *testing.T) {
	pool := testPool(t)
	builder, _ := newBuilder(t, pool, nil)
	userID := insertUser(t, pool)

	for _, source := range []string{
		connections.SourceJira, connections.SourceNotion, connections.SourceGmail,
	} {
		err := builder.CheckWriteReadiness(context.Background(), userID, source)
		if !errors.Is(err, connections.ErrNoConnection) {
			t.Errorf("readiness for an unconnected %s = %v, want ErrNoConnection", source, err)
		}
	}
}

// pooledUser keeps the compiler honest about the helper signature used above.
var _ = func(t *testing.T, pool *pgxpool.Pool) uuid.UUID { return insertUser(t, pool) }
