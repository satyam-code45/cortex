package gmail_test

import (
	"os"
	"path/filepath"
	"testing"

	"cortex/internal/tools/gmail"
)

// A file that exists but holds no usable token falls back to the environment.
//
// The realistic cause is a botched secret-file upload: a truncated or empty
// file at the mounted path. Refusing to boot over it while a good token sits in
// GMAIL_TOKEN_JSON is exactly the failure the fallback exists to prevent, so
// "unreadable" covers unparseable as well as absent.
func TestLoadTokenFallsBackWhenTheFileIsUnparseable(t *testing.T) {
	t.Parallel()

	const envToken = `{"refresh_token":"1//from-the-environment"}`

	tests := []struct {
		name     string
		contents string
	}{
		{"an empty file", ""},
		{"a truncated file", `{"refresh_token":`},
		{"valid JSON with no refresh token", `{"access_token":"only-an-access-token"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), ".gmail-token.json")
			if err := os.WriteFile(path, []byte(tt.contents), 0o600); err != nil {
				t.Fatalf("write the broken token file: %v", err)
			}

			token, err := gmail.LoadToken(path, envToken)
			if err != nil {
				t.Fatalf("LoadToken over a broken file = %v, want the environment value", err)
			}
			if token.RefreshToken != "1//from-the-environment" {
				t.Errorf("refresh token = %q, want the one from GMAIL_TOKEN_JSON", token.RefreshToken)
			}
		})
	}
}

// With no fallback configured, a broken file still reports its own problem
// rather than a misleading one about the environment.
func TestLoadTokenWithNoFallbackReportsTheFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".gmail-token.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write the broken token file: %v", err)
	}

	if _, err := gmail.LoadToken(path, ""); err == nil {
		t.Fatal("LoadToken() error = nil, want a parse failure naming the file")
	}
}
