package config_test

import (
	"slices"
	"strings"
	"testing"

	"cortex/internal/config"
)

// The demo workspace is optional, one source at a time.
//
// Cortex is a bring-your-own-sources product: every user connects their own
// Jira, Notion and Gmail, and the demo workspace is a showcase the operator may
// or may not configure. So:
//
//   - no demo credentials at all is a supported deployment — Load succeeds and
//     HasDemoWorkspace() is false;
//   - each source is independently present or absent;
//   - a source is configured whole or not at all: half of one fails at startup
//     naming the missing field and the source it belongs to;
//   - GMAIL_QUERY_SCOPE and JIRA_PROJECTS are demanded only alongside the
//     source they pin. Their rationale is unchanged — they confine the demo to
//     a slice of a real mailbox and a real site — so the check moved, it did
//     not weaken.
//
// Every case here sets GMAIL_TOKEN_PATH explicitly, because demo Gmail is keyed
// on a readable token file and the default path resolves against the repository
// root, where the developer running these tests probably has a real one.

// demolessEnv is the minimum environment of a deployment with no demo
// workspace: the database, sign-in, the operator token and the key-encryption
// secret. No Jira, no Notion, no Gmail, and deliberately no OPENAI_API_KEY.
func demolessEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":               testDatabaseURL,
		"GOOGLE_OAUTH_CLIENT_ID":     testGoogleClientID,
		"GOOGLE_OAUTH_CLIENT_SECRET": testGoogleSecret,
		"AUTH_API_TOKEN":             testAuthAPIToken,
		"LLM_KEY_ENCRYPTION_SECRET":  testEncryptionSecret,
		"GMAIL_TOKEN_PATH":           testGmailTokenAbsentPath,
		// Pinned for the same reason as the token path. GMAIL_CREDENTIALS_JSON
		// has a default that resolves to the repository root, where a developer
		// running these tests probably has a real OAuth client — so a case that
		// left it unset would assert demo Gmail's wholeness against a file this
		// suite does not own, and pass here while failing on a fresh clone.
		"GMAIL_CREDENTIALS_JSON": testGmailCredsPath,
	}
}

// withDemoless returns demolessEnv with overrides applied, so a case states
// only the demo credentials it is adding.
func withDemoless(overrides map[string]string) map[string]string {
	env := demolessEnv()
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

// demoJiraEnv is a whole demo Jira source plus the pin it requires.
var demoJiraEnv = map[string]string{
	"JIRA_BASE_URL":  testJiraBaseURL,
	"JIRA_EMAIL":     testJiraEmail,
	"JIRA_API_TOKEN": testJiraAPIToken,
	"JIRA_PROJECTS":  testJiraProjectsPin,
	// The server key funds the indexing crawl over whatever demo corpus exists.
	"OPENAI_API_KEY": testAPIKey,
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func TestLoadDemoWorkspaceIsOptionalPerSource(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string

		wantErr         bool
		wantErrContains []string
		wantErrOmits    []string

		wantJira    bool
		wantNotion  bool
		wantGmail   bool
		wantSources []string
	}{
		{
			// The headline of the day: the product boots with none of the
			// operator's personal accounts, and says so rather than pretending
			// a demo exists.
			name:        "no demo credentials at all loads with no demo workspace",
			env:         demolessEnv(),
			wantSources: nil,
		},
		{
			// The pins protect the demo workspace. With no demo workspace they
			// protect nothing, so their presence is merely inert — it must not
			// conjure a demo source into existence either.
			name: "pins without their sources neither fail nor create a demo",
			env: withDemoless(map[string]string{
				"GMAIL_QUERY_SCOPE": testGmailQueryScope,
				"JIRA_PROJECTS":     testJiraProjectsPin,
			}),
			wantSources: nil,
		},
		{
			name:        "demo Jira alone",
			env:         merge(demolessEnv(), demoJiraEnv),
			wantJira:    true,
			wantSources: []string{"jira"},
		},
		{
			// The server key funds one thing: embedding a corpus over the demo
			// workspace. Calling the demo's live Jira, Notion and Gmail tools
			// costs no tokens, and every investigation runs on its owner's own
			// key, so an operator who declines to fund a searchable corpus
			// still has a working demo. Refusing to boot would be demanding a
			// paid credential in order to run the features that do not spend
			// it. app.Build drops the indexer and the knowledge-base tool and
			// logs why.
			name:        "a demo workspace without the server key still loads",
			env:         merge(demolessEnv(), demoJiraEnv, map[string]string{"OPENAI_API_KEY": ""}),
			wantJira:    true,
			wantSources: []string{"jira"},
		},
		{
			name: "demo Notion alone",
			env: withDemoless(map[string]string{
				"NOTION_TOKEN":   testNotionToken,
				"OPENAI_API_KEY": testAPIKey,
			}),
			wantNotion:  true,
			wantSources: []string{"notion"},
		},
		{
			// Demo Gmail is configured when its cached refresh token is
			// readable: GMAIL_CREDENTIALS_JSON has a default and so is never
			// empty, and it is the token that decides whether the demo mailbox
			// can authenticate at all.
			name: "demo Gmail alone, from the token file",
			env: withDemoless(map[string]string{
				"GMAIL_TOKEN_PATH":  testGmailTokenPath,
				"GMAIL_QUERY_SCOPE": testGmailQueryScope,
				"OPENAI_API_KEY":    testAPIKey,
			}),
			wantGmail:   true,
			wantSources: []string{"gmail"},
		},
		{
			// The same token pasted into the environment instead, for a host
			// whose filesystem does not survive a deploy.
			name: "demo Gmail alone, from GMAIL_TOKEN_JSON with no file",
			env: withDemoless(map[string]string{
				"GMAIL_TOKEN_JSON":  `{"refresh_token":"1//env-refresh-token"}`,
				"GMAIL_QUERY_SCOPE": testGmailQueryScope,
				"OPENAI_API_KEY":    testAPIKey,
			}),
			wantGmail:   true,
			wantSources: []string{"gmail"},
		},
		{
			name: "all three demo sources",
			env: merge(demolessEnv(), demoJiraEnv, map[string]string{
				"NOTION_TOKEN":      testNotionToken,
				"GMAIL_TOKEN_PATH":  testGmailTokenPath,
				"GMAIL_QUERY_SCOPE": testGmailQueryScope,
			}),
			wantJira:    true,
			wantNotion:  true,
			wantGmail:   true,
			wantSources: []string{"jira", "notion", "gmail"},
		},
		{
			// Half a source is the one thing that is NOT optional: a site URL
			// with no credentials would build a demo Jira client that fails
			// every call it is asked to make, with nothing at startup to
			// explain why.
			name:            "a demo Jira with only a site URL names both missing fields",
			env:             withDemoless(map[string]string{"JIRA_BASE_URL": testJiraBaseURL}),
			wantErr:         true,
			wantErrContains: []string{"JIRA_EMAIL", "JIRA_API_TOKEN", "Jira", "partly configured"},
			wantErrOmits:    []string{"NOTION_TOKEN", "DATABASE_URL"},
		},
		{
			name: "a demo Jira missing only its token names that token",
			env: withDemoless(map[string]string{
				"JIRA_BASE_URL":  testJiraBaseURL,
				"JIRA_EMAIL":     testJiraEmail,
				"OPENAI_API_KEY": testAPIKey,
				"JIRA_PROJECTS":  testJiraProjectsPin,
			}),
			wantErr:         true,
			wantErrContains: []string{"JIRA_API_TOKEN", "Jira", "partly configured"},
			wantErrOmits:    []string{"NOTION_TOKEN", "DATABASE_URL", "GMAIL_QUERY_SCOPE"},
		},
		{
			name: "a demo Jira missing only its site URL names that URL",
			env: withDemoless(map[string]string{
				"JIRA_EMAIL":     testJiraEmail,
				"JIRA_API_TOKEN": testJiraAPIToken,
				"OPENAI_API_KEY": testAPIKey,
				"JIRA_PROJECTS":  testJiraProjectsPin,
			}),
			wantErr:         true,
			wantErrContains: []string{"JIRA_BASE_URL", "Jira", "partly configured"},
			wantErrOmits:    []string{"NOTION_TOKEN", "DATABASE_URL"},
		},
		{
			// The pin is required BECAUSE the mailbox is real, so it is
			// demanded exactly when the mailbox is configured — and the error
			// must not also demand the Jira pin, which protects a source this
			// deployment does not have.
			name: "demo Gmail without GMAIL_QUERY_SCOPE is rejected",
			env: withDemoless(map[string]string{
				"GMAIL_TOKEN_PATH": testGmailTokenPath,
				"OPENAI_API_KEY":   testAPIKey,
			}),
			wantErr:         true,
			wantErrContains: []string{"GMAIL_QUERY_SCOPE"},
			wantErrOmits:    []string{"JIRA_PROJECTS", "NOTION_TOKEN"},
		},
		{
			// Gmail's other half. A cached token says which mailbox; the OAuth
			// client says who is asking, and a refresh needs both. Caught here
			// rather than deeper in client construction, so the message names
			// the demo workspace instead of only a missing file.
			name: "demo Gmail with a token but no OAuth client is rejected",
			env: withDemoless(map[string]string{
				"GMAIL_TOKEN_PATH":       testGmailTokenPath,
				"GMAIL_QUERY_SCOPE":      testGmailQueryScope,
				"GMAIL_CREDENTIALS_JSON": "/nonexistent/cortex-test/gmail-credentials.json",
				"OPENAI_API_KEY":         testAPIKey,
			}),
			wantErr:         true,
			wantErrContains: []string{"GMAIL_CREDENTIALS_JSON", "partly configured"},
			wantErrOmits:    []string{"JIRA_PROJECTS", "NOTION_TOKEN"},
		},
		{
			name:            "demo Jira without JIRA_PROJECTS is rejected",
			env:             merge(demolessEnv(), demoJiraEnv, map[string]string{"JIRA_PROJECTS": ""}),
			wantErr:         true,
			wantErrContains: []string{"JIRA_PROJECTS"},
			wantErrOmits:    []string{"GMAIL_QUERY_SCOPE", "NOTION_TOKEN"},
		},
		{
			// Notion has one field, so it cannot be half-configured — and its
			// presence must not drag in either pin.
			name: "demo Notion alone requires neither pin",
			env: withDemoless(map[string]string{
				"NOTION_TOKEN":   testNotionToken,
				"OPENAI_API_KEY": testAPIKey,
			}),
			wantNotion:  true,
			wantSources: []string{"notion"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range envKeys {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := config.Load()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() = %v, want an error", cfg)
				}
				for _, want := range tt.wantErrContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				for _, omit := range tt.wantErrOmits {
					if strings.Contains(err.Error(), omit) {
						t.Errorf("error %q should not mention %q", err, omit)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}

			if cfg.DemoJira != tt.wantJira {
				t.Errorf("DemoJira = %t, want %t", cfg.DemoJira, tt.wantJira)
			}
			if cfg.DemoNotion != tt.wantNotion {
				t.Errorf("DemoNotion = %t, want %t", cfg.DemoNotion, tt.wantNotion)
			}
			if cfg.DemoGmail != tt.wantGmail {
				t.Errorf("DemoGmail = %t, want %t", cfg.DemoGmail, tt.wantGmail)
			}
			wantWorkspace := len(tt.wantSources) > 0
			if got := cfg.HasDemoWorkspace(); got != wantWorkspace {
				t.Errorf("HasDemoWorkspace() = %t, want %t", got, wantWorkspace)
			}
			if got := cfg.DemoSources(); !slices.Equal(got, tt.wantSources) {
				t.Errorf("DemoSources() = %v, want %v", got, tt.wantSources)
			}
		})
	}
}
