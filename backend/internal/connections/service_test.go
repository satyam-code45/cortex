package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"cortex/internal/agent"
	"cortex/internal/connections"
)

// Connections storage.
//
// The invariants under test: what the database holds for a
// connection is ciphertext (round-trippable under the configured secret, never
// the plaintext), identity carries display facts only, (user_id, source) is
// unique, delete removes the row, and a user's mode returns to demo only when
// no connections remain (or the demo toggle is on) — an errored connection
// still counts.

func TestConnectionCredentialsRoundTripAsCiphertext(t *testing.T) {
	const apiToken = "atlassian-api-token-super-secret-0042"

	pool := testPool(t)
	svc := newTestService(t, pool)
	userID := insertUser(t, pool)
	ctx := context.Background()

	creds := connections.JiraCredentials{
		BaseURL:  "https://satyam.atlassian.net",
		Email:    "satyam@example.com",
		APIToken: apiToken,
	}
	identity := map[string]string{"site_url": creds.BaseURL, "account_name": "Satyam Jha"}
	if err := svc.Save(ctx, userID, connections.SourceJira, creds, identity); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The stored bytes are ciphertext: they differ from the plaintext and do
	// not contain the token.
	var ciphertext []byte
	if err := pool.QueryRow(ctx,
		`SELECT credentials_ciphertext FROM user_connections WHERE user_id = $1 AND source = 'jira'`,
		userID).Scan(&ciphertext); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	if strings.Contains(string(ciphertext), apiToken) {
		t.Fatal("the database holds the plaintext API token")
	}

	// And they decrypt back to exactly what was stored.
	var loaded connections.JiraCredentials
	if err := svc.Load(ctx, userID, connections.SourceJira, &loaded); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != creds {
		t.Errorf("Load = %+v, want the saved credentials %+v", loaded, creds)
	}
}

func TestConnectionIdentityCarriesNoSecrets(t *testing.T) {
	const apiToken = "jira-token-must-not-leak-7777"

	pool := testPool(t)
	svc := newTestService(t, pool)
	userID := insertUser(t, pool)
	ctx := context.Background()

	creds := connections.JiraCredentials{
		BaseURL:  "https://satyam.atlassian.net",
		Email:    "satyam@example.com",
		APIToken: apiToken,
	}
	identity := map[string]string{"site_url": creds.BaseURL, "account_name": "Satyam Jha"}
	if err := svc.Save(ctx, userID, connections.SourceJira, creds, identity); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The raw identity column: display facts, never the token.
	var identityJSON string
	if err := pool.QueryRow(ctx,
		`SELECT identity::text FROM user_connections WHERE user_id = $1 AND source = 'jira'`,
		userID).Scan(&identityJSON); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if strings.Contains(identityJSON, apiToken) {
		t.Errorf("identity %q contains the API token", identityJSON)
	}

	// List — what the API layer shows — reports identity and status only.
	infos, err := svc.List(ctx, userID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List = %d connections, want 1", len(infos))
	}
	info := infos[0]
	if info.Source != connections.SourceJira || info.Status != "active" {
		t.Errorf("List[0] = {source %q, status %q}, want {jira, active}", info.Source, info.Status)
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal Info: %v", err)
	}
	if strings.Contains(string(encoded), apiToken) {
		t.Errorf("connection Info %q carries the API token", encoded)
	}
	var got map[string]string
	if err := json.Unmarshal(info.Identity, &got); err != nil {
		t.Fatalf("decode identity %q: %v", info.Identity, err)
	}
	if got["account_name"] != "Satyam Jha" || got["site_url"] != creds.BaseURL {
		t.Errorf("identity = %v, want the display facts saved with the connection", got)
	}
}

// One connection per (user, source): the schema enforces it, and Save upserts
// so a reconnect replaces rather than duplicates.
func TestOneConnectionPerUserAndSource(t *testing.T) {
	pool := testPool(t)
	svc := newTestService(t, pool)
	userID := insertUser(t, pool)
	ctx := context.Background()

	// The constraint itself: a raw duplicate insert must be a unique violation.
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_connections (user_id, source, credentials_ciphertext) VALUES ($1, 'notion', '\x00'::bytea)`,
		userID); err != nil {
		t.Fatalf("insert first notion row: %v", err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO user_connections (user_id, source, credentials_ciphertext) VALUES ($1, 'notion', '\x01'::bytea)`,
		userID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("duplicate (user, source) insert error = %v, want a unique violation (23505)", err)
	}

	// Save twice for the same source: still one row, second credentials win,
	// and error state is cleared by the reconnect.
	if err := svc.Save(ctx, userID, connections.SourceJira,
		connections.JiraCredentials{BaseURL: "https://a.atlassian.net", Email: "a@x.test", APIToken: "token-one"},
		map[string]string{"site_url": "https://a.atlassian.net"}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := svc.MarkError(ctx, userID, connections.SourceJira, "credential revoked"); err != nil {
		t.Fatalf("MarkError: %v", err)
	}
	if err := svc.Save(ctx, userID, connections.SourceJira,
		connections.JiraCredentials{BaseURL: "https://b.atlassian.net", Email: "b@x.test", APIToken: "token-two"},
		map[string]string{"site_url": "https://b.atlassian.net"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	if n := queryInt(t, pool,
		`SELECT count(*) FROM user_connections WHERE user_id = $1 AND source = 'jira'`, userID); n != 1 {
		t.Errorf("jira rows after two saves = %d, want 1 (upsert, not duplicate)", n)
	}
	var loaded connections.JiraCredentials
	if err := svc.Load(ctx, userID, connections.SourceJira, &loaded); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.APIToken != "token-two" {
		t.Errorf("stored token = %q, want the replacement token-two", loaded.APIToken)
	}
	infos, err := svc.List(ctx, userID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range infos {
		if info.Source == connections.SourceJira && (info.Status != "active" || info.LastError != "") {
			t.Errorf("reconnect left status %q lastError %q, want active with no error", info.Status, info.LastError)
		}
	}
}

func TestDeleteRemovesTheConnection(t *testing.T) {
	pool := testPool(t)
	svc := newTestService(t, pool)
	userID := insertUser(t, pool)
	ctx := context.Background()

	if err := svc.Save(ctx, userID, connections.SourceNotion,
		connections.NotionCredentials{Token: "ntn-secret"}, map[string]string{"bot_name": "Cortex"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := svc.Delete(ctx, userID, connections.SourceNotion); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_connections WHERE user_id = $1`, userID); n != 0 {
		t.Errorf("rows after delete = %d, want 0", n)
	}
	var creds connections.NotionCredentials
	if err := svc.Load(ctx, userID, connections.SourceNotion, &creds); !errors.Is(err, connections.ErrNoConnection) {
		t.Errorf("Load after delete = %v, want ErrNoConnection", err)
	}
	if err := svc.Delete(ctx, userID, connections.SourceNotion); !errors.Is(err, connections.ErrNoConnection) {
		t.Errorf("second Delete = %v, want ErrNoConnection", err)
	}
}

// Mode is all-or-nothing by design: demo only with zero connections or the
// explicit toggle; any connection — including an errored one — keeps the user
// in user mode so demo data never silently stands in for real sources.
func TestModeIsDemoOnlyWithNoConnectionsOrTheToggle(t *testing.T) {
	tests := []struct {
		name            string
		connectionCount int
		useDemo         bool
		demoAvailable   bool
		want            string
	}{
		// Zero connections is NOT demo. It used to be, which made the
		// deployment owner's real Jira and mailbox the default identity of
		// every new account; the demo workspace is now somewhere a user opts
		// into.
		{"zero connections is none", 0, false, true, connections.ModeNone},
		{"one connection is user", 1, false, true, agent.ModeUser},
		{"three connections is user", 3, false, true, agent.ModeUser},
		{"toggle forces demo despite connections", 2, true, true, agent.ModeDemo},
		{"toggle with zero connections is demo", 0, true, true, agent.ModeDemo},

		// A toggle with no demo behind it is inert rather than authoritative.
		// The row outlives the configuration that justified it: an operator who
		// removes the demo credentials leaves every opted-in user with
		// use_demo_workspace still true, and the UI hides the control when
		// there is no demo — so treating the stale flag as demo mode would
		// strand those users with no way to turn it off.
		{"toggle without a demo falls back to their own sources", 2, true, false, agent.ModeUser},
		{"toggle without a demo and nothing connected is none", 0, true, false, connections.ModeNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connections.Mode(tt.connectionCount, tt.useDemo, tt.demoAvailable); got != tt.want {
				t.Errorf("Mode(%d, %v, %v) = %q, want %q",
					tt.connectionCount, tt.useDemo, tt.demoAvailable, got, tt.want)
			}
		})
	}
}

func TestModeReturnsToNoneWhenTheLastConnectionGoes(t *testing.T) {
	pool := testPool(t)
	// A deployment that HAS a demo workspace, so the toggle assertion at the
	// end is testing the toggle rather than the absence of a demo.
	svc := newTestServiceWithDemo(t, pool, "jira", "notion")
	userID := insertUser(t, pool)
	ctx := context.Background()

	mode := func() string {
		t.Helper()
		infos, err := svc.List(ctx, userID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		useDemo, err := svc.UseDemo(ctx, userID)
		if err != nil {
			t.Fatalf("UseDemo: %v", err)
		}
		return connections.Mode(len(infos), useDemo, svc.DemoAvailable())
	}

	if got := mode(); got != connections.ModeNone {
		t.Fatalf("mode with no connections = %q, want none", got)
	}

	if err := svc.Save(ctx, userID, connections.SourceJira,
		connections.JiraCredentials{BaseURL: "https://x.atlassian.net", Email: "x@x.test", APIToken: "t1"},
		map[string]string{}); err != nil {
		t.Fatalf("Save jira: %v", err)
	}
	if err := svc.Save(ctx, userID, connections.SourceGmail,
		connections.GmailCredentials{RefreshToken: "grt-1"}, map[string]string{"email": "x@gmail.test"}); err != nil {
		t.Fatalf("Save gmail: %v", err)
	}
	if got := mode(); got != agent.ModeUser {
		t.Fatalf("mode with two connections = %q, want user", got)
	}

	// An errored connection still counts: no silent demo swap.
	if err := svc.MarkError(ctx, userID, connections.SourceGmail, "refresh token revoked"); err != nil {
		t.Fatalf("MarkError: %v", err)
	}
	if got := mode(); got != agent.ModeUser {
		t.Errorf("mode with an errored connection = %q, want user (an errored source must never demote to demo)", got)
	}

	// Deleting one of two keeps user mode; deleting the last returns to demo.
	if err := svc.Delete(ctx, userID, connections.SourceGmail); err != nil {
		t.Fatalf("Delete gmail: %v", err)
	}
	if got := mode(); got != agent.ModeUser {
		t.Errorf("mode with one connection remaining = %q, want user", got)
	}
	if err := svc.Delete(ctx, userID, connections.SourceJira); err != nil {
		t.Fatalf("Delete jira: %v", err)
	}
	if got := mode(); got != connections.ModeNone {
		t.Errorf("mode after the last delete = %q, want none", got)
	}

	// The explicit toggle also forces demo while connections exist.
	if err := svc.Save(ctx, userID, connections.SourceJira,
		connections.JiraCredentials{BaseURL: "https://x.atlassian.net", Email: "x@x.test", APIToken: "t1"},
		map[string]string{}); err != nil {
		t.Fatalf("re-Save jira: %v", err)
	}
	if err := svc.SetUseDemo(ctx, userID, true); err != nil {
		t.Fatalf("SetUseDemo: %v", err)
	}
	if got := mode(); got != agent.ModeDemo {
		t.Errorf("mode with the demo toggle on = %q, want demo", got)
	}
}
