package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"cortex/internal/config"
)

// The seeder's Gmail client must be UNSCOPED, and that is a correctness
// requirement rather than a convenience.
//
// Its duplicate check asks "is this fixture already in the mailbox?" before
// inserting. A client pinned to the fixture label can only see messages that
// already carry the label, so a fixture seeded before the label existed looks
// absent and gets inserted a second time. Whole-mailbox visibility is what
// makes re-running `make seed` idempotent.
//
// Two ways this has broken, both covered below: the constructor rejected the
// unscoped client outright because the caller never opted in (it failed the
// moment a real seed ran), and — the quieter failure — a caller "fixing" that
// by passing a QueryScope instead, which constructs happily and silently
// reintroduces duplicate inserts.
func TestBuildGmailClientIsUnscoped(t *testing.T) {
	dir := t.TempDir()

	credentialsPath := filepath.Join(dir, "credentials.json")
	writeJSONFile(t, credentialsPath, map[string]any{
		"installed": map[string]string{
			"client_id":     "test-client-id",
			"client_secret": "test-client-secret",
		},
	})

	tokenPath := filepath.Join(dir, "token.json")
	writeJSONFile(t, tokenPath, map[string]string{
		"refresh_token": "test-refresh-token",
	})

	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{
			name: "with no query scope configured",
			cfg: &config.Config{
				GmailCredentialsPath: credentialsPath,
				GmailTokenPath:       tokenPath,
			},
		},
		{
			// The agent's scope pin must not leak into the seeder: even with
			// GMAIL_QUERY_SCOPE set (it is required for the server to start,
			// so it is always set in practice), the seeder stays unscoped.
			name: "even when a query scope is configured",
			cfg: &config.Config{
				GmailCredentialsPath: credentialsPath,
				GmailTokenPath:       tokenPath,
				GmailQueryScope:      "label:vantage-labs",
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, err := buildGmailClient(tc.cfg, logger)
			if err != nil {
				t.Fatalf("buildGmailClient: %v", err)
			}
			if client == nil {
				t.Fatal("buildGmailClient returned a nil client without an error")
			}
			if got := client.QueryScope(); got != "" {
				t.Errorf("seeder client query scope = %q, want empty: a scoped "+
					"client cannot see fixtures seeded before the label existed, "+
					"so the duplicate check would insert them again", got)
			}
		})
	}
}

// writeJSONFile writes v as JSON to path, failing the test on any error.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
