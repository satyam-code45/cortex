package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cortex/internal/config"
)

// envKeys is every variable internal/config reads. Each test case starts from a
// clean slate: every key is explicitly cleared, then the case's values applied.
var envKeys = []string{
	"HOST",
	"FRONTEND_ORIGIN",
	"API_PUBLIC_URL",
	"DATABASE_URL",
	"OPENAI_API_KEY",
	"OPENAI_BASE_URL",
	"PORT",
	"LLM_MODEL",
	"LLM_UTILITY_MODEL",
	"EMBEDDING_MODEL",
	"MAX_ITERATIONS",
	"AGENT_RUN_WORKERS",
	"CONTEXT_TOKEN_BUDGET",
	"JIRA_PROJECTS",
	"INDEX_MAX_DOCUMENTS",
	"INDEX_WORKERS",
	"JIRA_BASE_URL",
	"JIRA_EMAIL",
	"JIRA_API_TOKEN",
	"NOTION_TOKEN",
	"NOTION_PARENT_PAGE_ID",
	"GMAIL_CREDENTIALS_JSON",
	"GMAIL_TOKEN_PATH",
	"GMAIL_TOKEN_JSON",
	"GMAIL_QUERY_SCOPE",
	"GMAIL_SEND_ALLOWED_DOMAINS",
	"GOOGLE_OAUTH_CLIENT_ID",
	"GOOGLE_OAUTH_CLIENT_SECRET",
	"AUTH_ALLOWED_EMAILS",
	"AUTH_API_TOKEN",
	"ADMIN_EMAILS",
	"DEV_USER_EMAIL",
	"LLM_KEY_ENCRYPTION_SECRET",
	"RUNS_PER_USER_PER_HOUR",
	"INDEX_REFRESH_COOLDOWN",
	"ACTION_TTL",
	"WRITES_PER_USER_PER_HOUR",
	"WRITE_ACTION_WORKERS",
}

const (
	testDatabaseURL  = "postgres://u:p@localhost:5432/db?sslmode=disable"
	testAPIKey       = "sk-test-key"
	testJiraBaseURL  = "https://test.atlassian.net"
	testJiraEmail    = "dev@example.com"
	testJiraAPIToken = "jira-test-token"

	testNotionToken = "ntn-test-token"
	// testGmailTokenAbsentPath names a file that does not exist, for cases
	// asserting what a deployment without demo Gmail requires.
	testGmailTokenAbsentPath = "/nonexistent/cortex-test/gmail-token.json"

	// Vars required since Google sign-in.
	testGmailQueryScope  = "label:vantage-labs"
	testJiraProjectsPin  = "ATLAS"
	testGoogleClientID   = "test-client.apps.googleusercontent.com"
	testGoogleSecret     = "google-test-secret"
	testAuthAPIToken     = "auth-test-token"
	testEncryptionSecret = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

// testGmailTokenPath and testGmailCredsPath are existing files, created by
// TestMain.
//
// Demo Gmail counts as configured when its token is readable and requires its
// OAuth client alongside, so these tests must own both files rather than
// inherit whatever the developer happens to have in the repository: otherwise
// demo Gmail is configured on one machine and not another, and every
// requirement conditional on it differs with it.
var (
	testGmailTokenPath string
	testGmailCredsPath string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cortex-config-test")
	if err != nil {
		panic("create temp dir for the gmail fixtures: " + err.Error())
	}
	testGmailTokenPath = filepath.Join(dir, "gmail-token.json")
	if err := os.WriteFile(testGmailTokenPath,
		[]byte(`{"refresh_token":"test-refresh-token"}`), 0o600); err != nil {
		panic("write the gmail token fixture: " + err.Error())
	}
	testGmailCredsPath = filepath.Join(dir, "gmail-credentials.json")
	if err := os.WriteFile(testGmailCredsPath,
		[]byte(`{"installed":{"client_id":"test","client_secret":"test"}}`), 0o600); err != nil {
		panic("write the gmail credentials fixture: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// requiredEnv is the minimum set that lets Load succeed, so a case can state
// only what it is actually varying.
func requiredEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":   testDatabaseURL,
		"OPENAI_API_KEY": testAPIKey,
		"JIRA_BASE_URL":  testJiraBaseURL,
		"JIRA_EMAIL":     testJiraEmail,
		"JIRA_API_TOKEN": testJiraAPIToken,
		// Required since the agent started investigating across three
		// sources: a server that cannot reach one of them refuses to start
		// rather than answering multi-hop questions with a third of the
		// evidence missing.
		"NOTION_TOKEN":           testNotionToken,
		"GMAIL_CREDENTIALS_JSON": testGmailCredsPath,
		"GMAIL_TOKEN_PATH":       testGmailTokenPath,

		// Required since Google sign-in opened: its credentials, operator
		// bearer token, key-encryption secret, and the source pins that keep an
		// authenticated stranger inside the demo workspace.
		"GMAIL_QUERY_SCOPE":          testGmailQueryScope,
		"JIRA_PROJECTS":              testJiraProjectsPin,
		"GOOGLE_OAUTH_CLIENT_ID":     testGoogleClientID,
		"GOOGLE_OAUTH_CLIENT_SECRET": testGoogleSecret,
		"AUTH_API_TOKEN":             testAuthAPIToken,
		"LLM_KEY_ENCRYPTION_SECRET":  testEncryptionSecret,
	}
}

// withSignInWant fills the sign-in-era fields a pre-existing want literal leaves
// at their zero value: every case built on requiredEnv() gets the same required
// values and defaults, and only a case that varies one of them states it.
func withSignInWant(want config.Config) config.Config {
	// Every case built on requiredEnv() configures all three demo sources, so
	// all three derived flags are true. They are set unconditionally rather
	// than inferred from the Gmail token path: DemoJira and DemoNotion have
	// nothing to do with that path, and keying them on it meant the first case
	// to vary GMAIL_TOKEN_PATH would silently expect all three false and fail
	// with an unreadable whole-struct diff. A case that configures a different
	// demo shape belongs in demo_test.go, which asserts the flags directly.
	want.DemoJira, want.DemoNotion, want.DemoGmail = true, true, true
	if want.GmailTokenPath == "" {
		want.GmailTokenPath = testGmailTokenPath
	}
	if want.GmailQueryScope == "" {
		want.GmailQueryScope = testGmailQueryScope
	}
	if want.JiraProjects == nil {
		want.JiraProjects = []string{testJiraProjectsPin}
	}
	if want.GoogleOAuthClientID == "" {
		want.GoogleOAuthClientID = testGoogleClientID
	}
	if want.GoogleOAuthClientSecret == "" {
		want.GoogleOAuthClientSecret = testGoogleSecret
	}
	if want.AuthAPIToken == "" {
		want.AuthAPIToken = testAuthAPIToken
	}
	if want.LLMKeyEncryptionSecret == "" {
		want.LLMKeyEncryptionSecret = testEncryptionSecret
	}
	if want.DevUserEmail == "" {
		want.DevUserEmail = config.DefaultDevUserEmail
	}
	if want.AdminEmails == nil {
		want.AdminEmails = []string{want.DevUserEmail}
	}
	if want.RunsPerUserPerHour == 0 {
		want.RunsPerUserPerHour = config.DefaultRunsPerUserPerHour
	}
	if want.IndexRefreshCooldown == 0 {
		want.IndexRefreshCooldown = config.DefaultIndexRefreshCooldown
	}
	if want.ActionTTL == 0 {
		want.ActionTTL = config.DefaultActionTTL
	}
	if want.WritesPerUserPerHour == 0 {
		want.WritesPerUserPerHour = config.DefaultWritesPerUserPerHour
	}
	if want.WriteActionWorkers == 0 {
		want.WriteActionWorkers = config.DefaultWriteActionWorkers
	}
	return want
}

// withEnv returns the required set with overrides applied.
func withEnv(overrides map[string]string) map[string]string {
	env := requiredEnv()
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

// Required variables are enforced, defaults applied.
func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string

		wantErr         bool
		wantErrContains []string
		wantErrOmits    []string
		want            config.Config
	}{
		{
			// The demo sources are absent from this list on purpose: with no
			// environment at all there is no demo workspace to configure, and a
			// bring-your-own-sources deployment is a supported one. Demanding
			// Jira, Notion and Gmail here is what welded the product to one
			// person's accounts.
			name: "every missing required var is reported in one error",
			// GMAIL_TOKEN_PATH is the one variable set, and it is set to a path
			// that does not exist: demo Gmail counts as configured when its
			// token file is readable, so without this the case would find the
			// developer's own .gmail-token.json and demand the pins that go with
			// a demo workspace.
			env:     map[string]string{"GMAIL_TOKEN_PATH": testGmailTokenAbsentPath},
			wantErr: true,
			wantErrContains: []string{
				"DATABASE_URL",
				"GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET",
				"AUTH_API_TOKEN", "LLM_KEY_ENCRYPTION_SECRET",
			},
			wantErrOmits: []string{
				"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN",
				"NOTION_TOKEN", "GMAIL_QUERY_SCOPE", "JIRA_PROJECTS",
				"OPENAI_API_KEY",
			},
		},
		{
			// The pins stopped being optional when sign-in opened; the
			// error must say why, not just name the variable.
			name:            "missing source pins are reported with their rationale",
			env:             withEnv(map[string]string{"GMAIL_QUERY_SCOPE": "", "JIRA_PROJECTS": ""}),
			wantErr:         true,
			wantErrContains: []string{"GMAIL_QUERY_SCOPE", "JIRA_PROJECTS", "pins"},
			wantErrOmits:    []string{"DATABASE_URL", "GOOGLE_OAUTH_CLIENT_ID"},
		},
		{
			// A wrong-length key would otherwise surface as a crypto error on the
			// first key save, far from the .env line that caused it.
			name:            "malformed LLM_KEY_ENCRYPTION_SECRET is rejected at load",
			env:             withEnv(map[string]string{"LLM_KEY_ENCRYPTION_SECRET": "not-hex"}),
			wantErr:         true,
			wantErrContains: []string{"LLM_KEY_ENCRYPTION_SECRET", "64 hex"},
		},
		{
			name:            "missing DATABASE_URL only",
			env:             withEnv(map[string]string{"DATABASE_URL": ""}),
			wantErr:         true,
			wantErrContains: []string{"DATABASE_URL"},
			wantErrOmits:    []string{"OPENAI_API_KEY", "JIRA_BASE_URL"},
		},
		// OPENAI_API_KEY has no case here because it is required in no
		// configuration at all. That it is accepted as empty even alongside a
		// demo workspace — the configuration most likely to want it — is
		// asserted in demo_test.go, next to the rest of the demo contract.
		{
			// A demo source is optional but never half-configured: a site URL
			// and an email with no token would otherwise build a demo Jira
			// client that fails every call, with nothing at startup to say why.
			// The error names the siblings that ARE set, because the fix is
			// either to supply the token or to unset them.
			name:            "a partly configured demo Jira is rejected",
			env:             withEnv(map[string]string{"JIRA_API_TOKEN": ""}),
			wantErr:         true,
			wantErrContains: []string{"JIRA_API_TOKEN", "partly configured"},
			wantErrOmits:    []string{"DATABASE_URL", "OPENAI_API_KEY"},
		},
		{
			name: "defaults applied when optional vars unset",
			env:  requiredEnv(),
			want: config.Config{
				DatabaseURL:        testDatabaseURL,
				Host:               "127.0.0.1",
				Port:               "8080",
				FrontendOrigin:     config.DefaultFrontendOrigin,
				OpenAIAPIKey:       testAPIKey,
				OpenAIBaseURL:      "",
				LLMModel:           "gpt-4o",
				LLMUtilityModel:    "gpt-4o-mini",
				EmbeddingModel:     "text-embedding-3-small",
				MaxIterations:      12,
				ContextTokenBudget: 80000,
				AgentRunWorkers:    4,
				IndexMaxDocuments:  config.DefaultIndexMaxDocuments,
				IndexWorkers:       config.DefaultIndexWorkers,
				JiraBaseURL:        testJiraBaseURL,
				JiraEmail:          testJiraEmail,
				JiraAPIToken:       testJiraAPIToken,
				NotionToken:        testNotionToken,
				// Relative credential paths resolve against the repository root,
				// not the working directory — every make target runs from backend/.
				GmailCredentialsPath: config.RepoPath(testGmailCredsPath),
				GmailTokenPath:       testGmailTokenPath,
			},
		},
		{
			name: "explicit values override every default",
			env: withEnv(map[string]string{
				"OPENAI_BASE_URL":      "http://127.0.0.1:1234/v1/",
				"HOST":                 "0.0.0.0",
				"PORT":                 "9999",
				"LLM_MODEL":            "gpt-4.1",
				"LLM_UTILITY_MODEL":    "gpt-4.1-mini",
				"EMBEDDING_MODEL":      "text-embedding-3-large",
				"MAX_ITERATIONS":       "5",
				"AGENT_RUN_WORKERS":    "2",
				"CONTEXT_TOKEN_BUDGET": "60000",
			}),
			want: config.Config{
				DatabaseURL:        testDatabaseURL,
				Host:               "0.0.0.0",
				Port:               "9999",
				FrontendOrigin:     config.DefaultFrontendOrigin,
				OpenAIAPIKey:       testAPIKey,
				OpenAIBaseURL:      "http://127.0.0.1:1234/v1/",
				LLMModel:           "gpt-4.1",
				LLMUtilityModel:    "gpt-4.1-mini",
				EmbeddingModel:     "text-embedding-3-large",
				MaxIterations:      5,
				ContextTokenBudget: 60000,
				AgentRunWorkers:    2,
				IndexMaxDocuments:  config.DefaultIndexMaxDocuments,
				IndexWorkers:       config.DefaultIndexWorkers,
				JiraBaseURL:        testJiraBaseURL,
				JiraEmail:          testJiraEmail,
				JiraAPIToken:       testJiraAPIToken,
				NotionToken:        testNotionToken,
				// Relative credential paths resolve against the repository root,
				// not the working directory — every make target runs from backend/.
				GmailCredentialsPath: config.RepoPath(testGmailCredsPath),
				GmailTokenPath:       testGmailTokenPath,
			},
		},
		{
			// JIRA_PROJECTS scopes the indexing crawl. Parsing is worth a case
			// because the whitespace and the trailing comma are what a human
			// editing .env actually types.
			name: "JIRA_PROJECTS is parsed into a trimmed list",
			env: withEnv(map[string]string{
				"JIRA_PROJECTS": " ATLAS , BEACON ,, COMET, ",
			}),
			want: config.Config{
				DatabaseURL:        testDatabaseURL,
				Host:               "127.0.0.1",
				Port:               "8080",
				FrontendOrigin:     config.DefaultFrontendOrigin,
				OpenAIAPIKey:       testAPIKey,
				LLMModel:           "gpt-4o",
				LLMUtilityModel:    "gpt-4o-mini",
				EmbeddingModel:     "text-embedding-3-small",
				MaxIterations:      12,
				ContextTokenBudget: 80000,
				AgentRunWorkers:    4,
				IndexMaxDocuments:  config.DefaultIndexMaxDocuments,
				IndexWorkers:       config.DefaultIndexWorkers,
				JiraBaseURL:        testJiraBaseURL,
				JiraEmail:          testJiraEmail,
				JiraAPIToken:       testJiraAPIToken,
				JiraProjects:       []string{"ATLAS", "BEACON", "COMET"},
				NotionToken:        testNotionToken,

				GmailCredentialsPath: config.RepoPath(testGmailCredsPath),
				GmailTokenPath:       testGmailTokenPath,
			},
		},
		{
			name: "empty optional vars fall back to defaults",
			env: withEnv(map[string]string{
				"PORT":              "",
				"LLM_MODEL":         "",
				"LLM_UTILITY_MODEL": "",
				"EMBEDDING_MODEL":   "",
				"MAX_ITERATIONS":    "",
				"AGENT_RUN_WORKERS": "",
			}),
			want: config.Config{
				DatabaseURL:        testDatabaseURL,
				Host:               "127.0.0.1",
				Port:               "8080",
				FrontendOrigin:     config.DefaultFrontendOrigin,
				OpenAIAPIKey:       testAPIKey,
				LLMModel:           "gpt-4o",
				LLMUtilityModel:    "gpt-4o-mini",
				EmbeddingModel:     "text-embedding-3-small",
				MaxIterations:      12,
				ContextTokenBudget: 80000,
				AgentRunWorkers:    4,
				IndexMaxDocuments:  config.DefaultIndexMaxDocuments,
				IndexWorkers:       config.DefaultIndexWorkers,
				JiraBaseURL:        testJiraBaseURL,
				JiraEmail:          testJiraEmail,
				JiraAPIToken:       testJiraAPIToken,
				NotionToken:        testNotionToken,
				// Relative credential paths resolve against the repository root,
				// not the working directory — every make target runs from backend/.
				GmailCredentialsPath: config.RepoPath(testGmailCredsPath),
				GmailTokenPath:       testGmailTokenPath,
			},
		},
		{
			// A tuning knob must never stop the server booting: an unparseable
			// value falls back to the documented default.
			name: "unparseable numeric vars fall back to defaults",
			env: withEnv(map[string]string{
				"MAX_ITERATIONS":    "twelve",
				"AGENT_RUN_WORKERS": "-3",
			}),
			want: config.Config{
				DatabaseURL:        testDatabaseURL,
				Host:               "127.0.0.1",
				Port:               "8080",
				FrontendOrigin:     config.DefaultFrontendOrigin,
				OpenAIAPIKey:       testAPIKey,
				LLMModel:           "gpt-4o",
				LLMUtilityModel:    "gpt-4o-mini",
				EmbeddingModel:     "text-embedding-3-small",
				MaxIterations:      12,
				ContextTokenBudget: 80000,
				AgentRunWorkers:    4,
				IndexMaxDocuments:  config.DefaultIndexMaxDocuments,
				IndexWorkers:       config.DefaultIndexWorkers,
				JiraBaseURL:        testJiraBaseURL,
				JiraEmail:          testJiraEmail,
				JiraAPIToken:       testJiraAPIToken,
				NotionToken:        testNotionToken,
				// Relative credential paths resolve against the repository root,
				// not the working directory — every make target runs from backend/.
				GmailCredentialsPath: config.RepoPath(testGmailCredsPath),
				GmailTokenPath:       testGmailTokenPath,
			},
		},
		{
			// A trailing slash on the site URL would produce "//rest/api/3/..."
			// in every request path.
			name: "trailing slash is trimmed from JIRA_BASE_URL",
			env:  withEnv(map[string]string{"JIRA_BASE_URL": testJiraBaseURL + "/"}),
			want: config.Config{
				DatabaseURL:        testDatabaseURL,
				Host:               "127.0.0.1",
				Port:               "8080",
				FrontendOrigin:     config.DefaultFrontendOrigin,
				OpenAIAPIKey:       testAPIKey,
				LLMModel:           "gpt-4o",
				LLMUtilityModel:    "gpt-4o-mini",
				EmbeddingModel:     "text-embedding-3-small",
				MaxIterations:      12,
				ContextTokenBudget: 80000,
				AgentRunWorkers:    4,
				IndexMaxDocuments:  config.DefaultIndexMaxDocuments,
				IndexWorkers:       config.DefaultIndexWorkers,
				JiraBaseURL:        testJiraBaseURL,
				JiraEmail:          testJiraEmail,
				JiraAPIToken:       testJiraAPIToken,
				NotionToken:        testNotionToken,
				// Relative credential paths resolve against the repository root,
				// not the working directory — every make target runs from backend/.
				GmailCredentialsPath: config.RepoPath(testGmailCredsPath),
				GmailTokenPath:       testGmailTokenPath,
			},
		},
		{
			// Was "unrelated future vars are ignored" until the CORS check started
			// reading FRONTEND_ORIGIN. A trailing slash is trimmed because the
			// value is compared byte-for-byte against the browser's Origin
			// header, which never carries one.
			name: "FRONTEND_ORIGIN is read and its trailing slash trimmed",
			env:  withEnv(map[string]string{"FRONTEND_ORIGIN": "https://cortex.example/"}),
			want: config.Config{
				DatabaseURL:        testDatabaseURL,
				Host:               "127.0.0.1",
				Port:               "8080",
				FrontendOrigin:     "https://cortex.example",
				OpenAIAPIKey:       testAPIKey,
				LLMModel:           "gpt-4o",
				LLMUtilityModel:    "gpt-4o-mini",
				EmbeddingModel:     "text-embedding-3-small",
				MaxIterations:      12,
				ContextTokenBudget: 80000,
				AgentRunWorkers:    4,
				IndexMaxDocuments:  config.DefaultIndexMaxDocuments,
				IndexWorkers:       config.DefaultIndexWorkers,
				JiraBaseURL:        testJiraBaseURL,
				JiraEmail:          testJiraEmail,
				JiraAPIToken:       testJiraAPIToken,
				NotionToken:        testNotionToken,
				// Relative credential paths resolve against the repository root,
				// not the working directory — every make target runs from backend/.
				GmailCredentialsPath: config.RepoPath(testGmailCredsPath),
				GmailTokenPath:       testGmailTokenPath,
			},
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
					t.Fatalf("Load() = %+v, want error", cfg)
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
			if cfg == nil {
				t.Fatal("Load() returned nil config with nil error")
			}
			// DeepEqual rather than !=: Config gained a slice field
			// (JiraProjects) and is no longer comparable.
			tt.want = withSignInWant(tt.want)
			if !reflect.DeepEqual(*cfg, tt.want) {
				// Config implements Stringer with its secrets redacted, so this
				// message cannot leak the API key or the Jira token.
				t.Errorf("Load() = %v, want %v", *cfg, tt.want)
			}
		})
	}
}

// Defaults are exported so callers (and .env.example) stay in sync.
func TestDefaultConstants(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"port", config.DefaultPort, "8080"},
		{"llm model", config.DefaultLLMModel, "gpt-4o"},
		{"llm utility model", config.DefaultLLMUtilityModel, "gpt-4o-mini"},
		{"embedding model", config.DefaultEmbeddingModel, "text-embedding-3-small"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}
