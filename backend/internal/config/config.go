// Package config loads and validates the process configuration from the
// environment. It is the only place that reads os.Getenv, so every other
// package receives its settings as plain struct fields.
package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	// DefaultHost binds to loopback only. Until auth exists, every request is
	// the dev user and every chat call spends real money, so the server must
	// not be reachable from the network by default. Set HOST=0.0.0.0 to expose
	// it deliberately.
	DefaultHost            = "127.0.0.1"
	DefaultPort            = "8080"
	DefaultFrontendOrigin  = "http://localhost:3000"
	DefaultLLMModel        = "gpt-4o"
	DefaultLLMUtilityModel = "gpt-4o-mini"
	DefaultEmbeddingModel  = "text-embedding-3-small"

	// DefaultMaxIterations bounds one agent investigation. It is the ceiling on
	// what a single question can cost, so it is configuration rather than a
	// constant.
	DefaultMaxIterations = 12
	// DefaultAgentRunWorkers is how many agent runs execute concurrently in the
	// server process.
	DefaultAgentRunWorkers = 4

	// DefaultContextTokenBudget caps the estimated size of an agent run's
	// transcript. 80k is roughly two-thirds of a 128k context window, leaving
	// room for the response, the tool definitions, and estimator error. When
	// a transcript approaches it, the oldest tool observations are replaced
	// by utility-model summaries.
	DefaultContextTokenBudget = 80000

	// DefaultIndexMaxDocuments caps how many documents one indexing crawl reads
	// from a single source. It exists because a crawl is otherwise unbounded in
	// both time and OpenAI spend: an unfamiliar mailbox has no natural size.
	DefaultIndexMaxDocuments = 400
	// DefaultIndexWorkers is the concurrency on the indexing queue. One: the
	// crawls are bounded by upstream rate limits, not by local CPU, so running
	// several at once mostly buys several sets of 429s.
	DefaultIndexWorkers = 1

	// DefaultGmailCredentialsPath is where the OAuth Desktop client JSON is
	// expected, relative to the repository root.
	DefaultGmailCredentialsPath = "./gmail-credentials.json"
	// DefaultGmailTokenPath is where cmd/gmail-auth caches the refresh token.
	DefaultGmailTokenPath = "./.gmail-token.json"

	// DefaultDevUserEmail identifies the operator: it is the user the bearer
	// token acts as, and the default admin. It stopped being an implicit chat
	// identity when Google sign-in landed.
	DefaultDevUserEmail = "dev@cortex.local"
	// DefaultRunsPerUserPerHour bounds run creation per user. The LLM spend is
	// the user's own key, but every run also consumes the server's Jira, Notion
	// and Gmail quotas — the limit protects the shared part.
	DefaultRunsPerUserPerHour = 30
	// DefaultIndexRefreshCooldown is the minimum interval between user-triggered
	// Sources refreshes. The upstream APIs are paginated and rate-limited; a
	// 15-minute-fresh local copy beats a live crawl per page view.
	DefaultIndexRefreshCooldown = 15 * time.Minute
	// DefaultActionTTL is how long a proposed write waits for a human before it
	// expires and can no longer be carried out.
	//
	// A day: a working day plus a night, so a proposal made at 5pm is still
	// there in the morning — and short enough that an email drafted from last
	// week's context cannot be approved into today's situation. An approval is a
	// judgement about a moment, and the moment does not last indefinitely.
	DefaultActionTTL = 24 * time.Hour
	// DefaultWritesPerUserPerHour bounds how many writes one user can have
	// carried out per hour.
	//
	// Separate from the run limit and much lower, because it bounds something
	// different. A run costs quota; a write reaches other people, and twenty
	// emails or tickets an hour is already far more than a person can
	// meaningfully approve one at a time. The limit is a backstop against a
	// runaway loop, not a throughput target.
	DefaultWritesPerUserPerHour = 20
	// DefaultWriteActionWorkers is the concurrency on the write queue. Two: a
	// write is one HTTP request, and the queue also carries the expiry sweep,
	// which must not sit behind a slow send.
	DefaultWriteActionWorkers = 2
)

// Config holds every setting the server needs. Fields map 1:1 to the variables
// documented in .env.example.
type Config struct {
	// DatabaseURL is the Postgres connection string (required).
	DatabaseURL string
	// Host is the interface the HTTP server binds to.
	Host string
	// Port is the TCP port the HTTP server listens on.
	Port string
	// FrontendOrigin is the browser origin allowed by CORS. Exactly one: the
	// API serves one first-party frontend, and a list would only invite
	// wildcarding later.
	FrontendOrigin string
	// APIPublicURL is the origin this API is reached at from outside, e.g.
	// https://cortex-api.onrender.com. Empty for local development.
	//
	// One value because two separate things need the same fact, and letting
	// them drift apart is how a deployment breaks in confusing ways. The
	// hostname goes into the Host-header allowlist, without which a hosted
	// deployment answers 421 to everything including its own health check; and
	// the origin builds the OAuth redirect URIs, which Google matches as exact
	// strings and which otherwise point at localhost from a public site.
	APIPublicURL string

	// OpenAIAPIKey authenticates the server's own OpenAI calls: the indexing
	// crawl and the knowledge-base query embedder, and the completions of
	// non-BYOK processes like cmd/eval. Optional — every investigation runs on
	// its owner's stored key, so an empty value costs the indexed corpus and
	// nothing else.
	OpenAIAPIKey string
	// OpenAIBaseURL overrides the OpenAI endpoint. Empty means the SDK default;
	// tests point it at an httptest server.
	OpenAIBaseURL string

	// LLMModel is the main reasoning model used by the agent.
	LLMModel string
	// LLMUtilityModel is the cheaper model used for summarization and grading.
	LLMUtilityModel string
	// EmbeddingModel produces the vectors stored in pgvector.
	EmbeddingModel string

	// MaxIterations caps the agent loop.
	MaxIterations int
	// AgentRunWorkers is the River worker count on the agent_runs queue.
	AgentRunWorkers int
	// ContextTokenBudget caps the estimated transcript size of one agent run
	// before the oldest observations are compacted into summaries.
	ContextTokenBudget int

	// IndexMaxDocuments caps documents read per source per indexing run.
	IndexMaxDocuments int
	// IndexWorkers is the River worker count on the index_source queue.
	IndexWorkers int

	// The Jira, Notion and Gmail credentials below configure the DEMO
	// workspace, and all three are optional.
	//
	// They were required while the demo workspace was the product. Since users
	// connect their own sources, a demo source is a showcase: absent, that
	// source simply has no demo client, and with all three absent there is no
	// demo workspace at all and Cortex is exactly what it claims to be — bring
	// your own sources. What stays enforced is that a source is configured
	// whole or not at all (see requireWholeDemoSource): a half-specified source
	// would degrade every demo run with no explanation, which is the failure
	// the original required-checks existed to prevent.

	// JiraBaseURL is the Atlassian site root, e.g. https://site.atlassian.net.
	JiraBaseURL string
	// JiraEmail is the Atlassian account email used for API token auth.
	JiraEmail string
	// JiraAPIToken authenticates against the Jira REST API.
	JiraAPIToken string
	// JiraProjects restricts the agent and the indexing crawl to these project
	// keys (required since Google sign-in).
	//
	// It was optional while the only user was the operator. With Google sign-in
	// open, the Jira site has real projects and an authenticated stranger must
	// be structurally unable to reach outside the pinned set. The same role
	// GmailQueryScope plays for mail.
	JiraProjects []string

	// NotionToken is the internal integration secret.
	NotionToken string
	// NotionParentPageID is the page the seeder creates fixture pages under.
	// Empty means "discover it", which works when exactly one page is shared
	// with the integration.
	NotionParentPageID string

	// GmailCredentialsPath points at the OAuth Desktop client JSON downloaded
	// from Google Cloud.
	GmailCredentialsPath string
	// GmailTokenPath is where cmd/gmail-auth cached the refresh token.
	GmailTokenPath string
	// GmailTokenJSON carries the same JSON the token file holds, for hosts with
	// an ephemeral filesystem.
	//
	// The demo mailbox authenticates from a file written by the interactive
	// `make gmail-auth`, which cannot be run inside a container — so on a
	// platform that discards the filesystem on deploy, demo Gmail broke at the
	// first redeploy with no way to repair it in place. The operator pastes the
	// token here once instead. The file still wins when both are present, so a
	// developer's local re-auth takes effect without touching the environment.
	GmailTokenJSON string
	// GmailQueryScope is ANDed into every Gmail search (required since Google
	// sign-in).
	//
	// It was optional while the only user was the operator. With Google sign-in
	// open, the Gmail account is a real mailbox and an authenticated stranger
	// must be structurally unable to search outside the pinned scope (e.g.
	// `label:vantage-labs`) — so the server refuses to start without it, and
	// the Gmail client refuses to construct without it.
	GmailQueryScope string

	// GoogleOAuthClientID identifies the OAuth Web application client used for
	// sign-in (required). This is a separate client from the Desktop one used
	// by cmd/gmail-auth: a Desktop client cannot take a server redirect URI.
	GoogleOAuthClientID string
	// GoogleOAuthClientSecret authenticates the code exchange (required).
	GoogleOAuthClientSecret string
	// AuthAllowedEmails, when set, restricts sign-in to these addresses.
	// Empty means any Google account may sign in.
	AuthAllowedEmails []string
	// AuthAPIToken is the static bearer token for non-browser callers — make
	// index and scripts (required). Requests carrying it act as DevUserEmail
	// with admin rights, so it is an operator credential.
	AuthAPIToken string
	// AdminEmails lists the session emails allowed to call admin endpoints.
	// Defaults to [DevUserEmail].
	AdminEmails []string
	// DevUserEmail is the operator identity: the user the bearer token acts as
	// and the default admin email.
	DevUserEmail string
	// LLMKeyEncryptionSecret is the AES-256 key (64 hex chars = 32 bytes) that
	// encrypts users' stored LLM API keys (required). Rotating it invalidates
	// every stored key; users re-add them.
	LLMKeyEncryptionSecret string
	// RunsPerUserPerHour caps run creation per user; beyond it POST /api/chat
	// returns 429.
	RunsPerUserPerHour int
	// IndexRefreshCooldown is the minimum interval between user-triggered
	// Sources refreshes.
	IndexRefreshCooldown time.Duration

	// ActionTTL is how long a proposed write stays decidable.
	ActionTTL time.Duration
	// WritesPerUserPerHour caps executed writes per user; a proposal beyond it
	// is refused at proposal time, so the agent can say so in its answer.
	WritesPerUserPerHour int
	// WriteActionWorkers is the concurrency on the write execution queue.
	WriteActionWorkers int
	// GmailSendAllowedDomains restricts the domains a proposed email may be
	// addressed to. Empty — the default — allows any: a policy nobody
	// configured should not silently disable the feature.
	GmailSendAllowedDomains []string

	// DemoJira, DemoNotion and DemoGmail report whether each demo source is
	// fully configured. They are derived during Load, not read from the
	// environment, so every caller asks the same question the same way instead
	// of re-deriving "is this source present" from a different subset of fields.
	DemoJira   bool
	DemoNotion bool
	DemoGmail  bool
}

// HasDemoWorkspace reports whether any demo source is configured.
//
// False is a supported deployment, not a misconfiguration: every user brings
// their own sources, demo mode is unreachable, and the startup line says so.
func (c *Config) HasDemoWorkspace() bool {
	return c.DemoJira || c.DemoNotion || c.DemoGmail
}

// DemoSources names the configured demo sources, in the order the tool registry
// lists them. Empty when there is no demo workspace.
func (c *Config) DemoSources() []string {
	var sources []string
	if c.DemoJira {
		sources = append(sources, "jira")
	}
	if c.DemoNotion {
		sources = append(sources, "notion")
	}
	if c.DemoGmail {
		sources = append(sources, "gmail")
	}
	return sources
}

// String renders the configuration with its secrets redacted.
//
// Config exists to hold credentials, so the safe rendering is the default one:
// implementing Stringer means %v and %+v cannot print the OpenAI key or the Jira
// token, no matter who formats it — a log line, a debug print, or a test failure
// message. A test that dumped this struct on mismatch is exactly how a real
// token ends up in captured output.
func (c Config) String() string {
	return fmt.Sprintf("Config{DatabaseURL:%s Host:%s Port:%s FrontendOrigin:%s APIPublicURL:%s "+
		"OpenAIAPIKey:%s OpenAIBaseURL:%s LLMModel:%s LLMUtilityModel:%s EmbeddingModel:%s "+
		"MaxIterations:%d AgentRunWorkers:%d ContextTokenBudget:%d IndexMaxDocuments:%d IndexWorkers:%d "+
		"JiraBaseURL:%s JiraEmail:%s JiraAPIToken:%s "+
		"JiraProjects:%s NotionToken:%s NotionParentPageID:%s "+
		"GmailCredentialsPath:%s GmailTokenPath:%s GmailTokenJSON:%s GmailQueryScope:%s "+
		"GoogleOAuthClientID:%s GoogleOAuthClientSecret:%s AuthAllowedEmails:%s "+
		"AuthAPIToken:%s AdminEmails:%s DevUserEmail:%s LLMKeyEncryptionSecret:%s "+
		"RunsPerUserPerHour:%d IndexRefreshCooldown:%s "+
		"ActionTTL:%s WritesPerUserPerHour:%d WriteActionWorkers:%d GmailSendAllowedDomains:%s "+
		"DemoJira:%t DemoNotion:%t DemoGmail:%t}",
		redactDSN(c.DatabaseURL), c.Host, c.Port, c.FrontendOrigin, c.APIPublicURL,
		redact(c.OpenAIAPIKey), c.OpenAIBaseURL, c.LLMModel, c.LLMUtilityModel, c.EmbeddingModel,
		c.MaxIterations, c.AgentRunWorkers, c.ContextTokenBudget, c.IndexMaxDocuments, c.IndexWorkers,
		c.JiraBaseURL, c.JiraEmail, redact(c.JiraAPIToken),
		strings.Join(c.JiraProjects, ","), redact(c.NotionToken), c.NotionParentPageID,
		c.GmailCredentialsPath, c.GmailTokenPath, redact(c.GmailTokenJSON), c.GmailQueryScope,
		c.GoogleOAuthClientID, redact(c.GoogleOAuthClientSecret), strings.Join(c.AuthAllowedEmails, ","),
		redact(c.AuthAPIToken), strings.Join(c.AdminEmails, ","), c.DevUserEmail, redact(c.LLMKeyEncryptionSecret),
		c.RunsPerUserPerHour, c.IndexRefreshCooldown,
		c.ActionTTL, c.WritesPerUserPerHour, c.WriteActionWorkers,
		strings.Join(c.GmailSendAllowedDomains, ","),
		c.DemoJira, c.DemoNotion, c.DemoGmail)
}

// redactDSN strips the password from a Postgres connection string.
//
// The DSN is a credential too: the documented local default is
// postgres://cortex:cortex@... , so printing it in the clear defeats the point of
// this method for the one field people are most likely to paste into an issue.
func redactDSN(dsn string) string {
	if dsn == "" {
		return "<unset>"
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		// Unparseable: say nothing about the contents rather than guess.
		return "<set>"
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.Redacted()
}

// redact reports whether a secret is present without revealing it. The length is
// safe to show and is usually the thing being debugged (a truncated paste).
func redact(secret string) string {
	if secret == "" {
		return "<unset>"
	}
	return fmt.Sprintf("<set:%d chars>", len(secret))
}

// Load reads the configuration from the environment. All missing required
// variables are reported in a single error so they can be fixed in one pass.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Host:            envOr("HOST", DefaultHost),
		Port:            envOr("PORT", DefaultPort),
		FrontendOrigin:  strings.TrimRight(envOr("FRONTEND_ORIGIN", DefaultFrontendOrigin), "/"),
		APIPublicURL:    strings.TrimRight(strings.TrimSpace(os.Getenv("API_PUBLIC_URL")), "/"),
		OpenAIAPIKey:    os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:   os.Getenv("OPENAI_BASE_URL"),
		LLMModel:        envOr("LLM_MODEL", DefaultLLMModel),
		LLMUtilityModel: envOr("LLM_UTILITY_MODEL", DefaultLLMUtilityModel),
		EmbeddingModel:  envOr("EMBEDDING_MODEL", DefaultEmbeddingModel),

		MaxIterations:      envInt("MAX_ITERATIONS", DefaultMaxIterations),
		AgentRunWorkers:    envInt("AGENT_RUN_WORKERS", DefaultAgentRunWorkers),
		ContextTokenBudget: envInt("CONTEXT_TOKEN_BUDGET", DefaultContextTokenBudget),

		IndexMaxDocuments: envInt("INDEX_MAX_DOCUMENTS", DefaultIndexMaxDocuments),
		IndexWorkers:      envInt("INDEX_WORKERS", DefaultIndexWorkers),

		JiraBaseURL:  strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/"),
		JiraEmail:    os.Getenv("JIRA_EMAIL"),
		JiraAPIToken: os.Getenv("JIRA_API_TOKEN"),
		JiraProjects: envList("JIRA_PROJECTS"),

		NotionToken:        os.Getenv("NOTION_TOKEN"),
		NotionParentPageID: strings.TrimSpace(os.Getenv("NOTION_PARENT_PAGE_ID")),

		GmailCredentialsPath: RepoPath(envOr("GMAIL_CREDENTIALS_JSON", DefaultGmailCredentialsPath)),
		GmailTokenPath:       RepoPath(envOr("GMAIL_TOKEN_PATH", DefaultGmailTokenPath)),
		GmailTokenJSON:       strings.TrimSpace(os.Getenv("GMAIL_TOKEN_JSON")),
		GmailQueryScope:      strings.TrimSpace(os.Getenv("GMAIL_QUERY_SCOPE")),

		GoogleOAuthClientID:     strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_ID")),
		GoogleOAuthClientSecret: strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET")),
		AuthAllowedEmails:       envList("AUTH_ALLOWED_EMAILS"),
		AuthAPIToken:            strings.TrimSpace(os.Getenv("AUTH_API_TOKEN")),
		AdminEmails:             envList("ADMIN_EMAILS"),
		DevUserEmail:            envOr("DEV_USER_EMAIL", DefaultDevUserEmail),
		LLMKeyEncryptionSecret:  strings.TrimSpace(os.Getenv("LLM_KEY_ENCRYPTION_SECRET")),
		RunsPerUserPerHour:      envInt("RUNS_PER_USER_PER_HOUR", DefaultRunsPerUserPerHour),
		IndexRefreshCooldown:    envDuration("INDEX_REFRESH_COOLDOWN", DefaultIndexRefreshCooldown),

		ActionTTL:               envDuration("ACTION_TTL", DefaultActionTTL),
		WritesPerUserPerHour:    envInt("WRITES_PER_USER_PER_HOUR", DefaultWritesPerUserPerHour),
		WriteActionWorkers:      envInt("WRITE_ACTION_WORKERS", DefaultWriteActionWorkers),
		GmailSendAllowedDomains: envList("GMAIL_SEND_ALLOWED_DOMAINS"),
	}
	if len(cfg.AdminEmails) == 0 {
		cfg.AdminEmails = []string{cfg.DevUserEmail}
	}

	// Which demo sources exist is resolved before validation, because several
	// checks below are conditional on it.
	//
	// Each demo source is optional but must be whole: a Jira URL with no token
	// would otherwise produce a demo workspace that fails every run it is asked
	// to serve, with nothing at startup to explain why.
	//
	// Gmail's presence is keyed on the cached refresh token rather than on
	// GMAIL_CREDENTIALS_JSON: that path has a default and so is never empty, and
	// it is the token (file or GMAIL_TOKEN_JSON) that decides whether the demo
	// mailbox can actually authenticate.
	var missing []string
	missing = append(missing, requireWholeDemoSource("Jira", []demoField{
		{name: "JIRA_BASE_URL", value: cfg.JiraBaseURL},
		{name: "JIRA_EMAIL", value: cfg.JiraEmail},
		{name: "JIRA_API_TOKEN", value: cfg.JiraAPIToken},
	})...)
	cfg.DemoJira = cfg.JiraBaseURL != "" && cfg.JiraEmail != "" && cfg.JiraAPIToken != ""
	cfg.DemoNotion = cfg.NotionToken != ""
	cfg.DemoGmail = cfg.GmailTokenJSON != "" || fileExists(cfg.GmailTokenPath)

	// Demo Gmail needs both halves: a token says which mailbox, the OAuth
	// client says who is asking, and refreshing an access token uses both. The
	// token is what decides the source is wanted (above), so the client file is
	// the half that can go missing — and it fails here, in the same sentence
	// shape as a half-configured Jira, rather than deeper in client
	// construction where the message does not mention the demo workspace.
	if cfg.DemoGmail && !fileExists(cfg.GmailCredentialsPath) {
		missing = append(missing, fmt.Sprintf(
			"GMAIL_CREDENTIALS_JSON (demo Gmail is partly configured: a cached token is present but "+
				"no OAuth client JSON is readable at %s — download the Desktop client there, or leave "+
				"demo Gmail out entirely)", cfg.GmailCredentialsPath))
	}

	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	// OPENAI_API_KEY is deliberately absent from this list, in every
	// configuration. It funds the server's own embedding work — the indexing
	// crawl and the knowledge_base query embedder — and nothing else:
	// completions run on each run owner's stored key (BYOK). So its absence
	// costs exactly one feature, the searchable corpus over the demo workspace,
	// and the operator who declines to fund it still gets a working product:
	// the demo's live Jira, Notion and Gmail tools are unaffected, because
	// reading a source costs no tokens.
	//
	// Refusing to boot over it would be demanding a paid credential to run
	// features that do not spend it. app.Build drops the indexer and the
	// knowledge-base tool when the key is missing and logs why.

	if cfg.GoogleOAuthClientID == "" {
		missing = append(missing, "GOOGLE_OAUTH_CLIENT_ID")
	}
	if cfg.GoogleOAuthClientSecret == "" {
		missing = append(missing, "GOOGLE_OAUTH_CLIENT_SECRET")
	}
	if cfg.AuthAPIToken == "" {
		missing = append(missing, "AUTH_API_TOKEN")
	}
	if cfg.LLMKeyEncryptionSecret == "" {
		missing = append(missing, "LLM_KEY_ENCRYPTION_SECRET")
	}
	// The source pins stopped being optional when sign-in opened: the demo
	// Gmail account is a real mailbox and the demo Jira site has real projects,
	// so an authenticated stranger must be structurally unable to reach outside
	// the demo workspace. Empty pins are a safety hole, not a wider default.
	//
	// They are now demanded per source rather than always: a pin for a source
	// that is not configured protects nothing, and demanding it would make a
	// demo-less deployment carry settings that do not apply to it. The pins
	// scope only the demo workspace — a user's own connected Jira and Gmail are
	// theirs to search in full (see the connections registry).
	if cfg.DemoGmail && cfg.GmailQueryScope == "" {
		missing = append(missing, "GMAIL_QUERY_SCOPE (required with demo Gmail: pins every demo Gmail search to a slice of a real mailbox)")
	}
	if cfg.DemoJira && len(cfg.JiraProjects) == 0 {
		missing = append(missing, "JIRA_PROJECTS (required with demo Jira: pins the agent to the demo Jira projects)")
	}
	if len(missing) > 0 {
		// Joined with "; " rather than ", ": an element may contain commas of
		// its own (a half-configured source names several fields), and a
		// comma-joined list of comma-containing items is one unreadable run-on
		// line at exactly the moment somebody is trying to fix their config.
		return nil, fmt.Errorf("config: missing required environment variables: %s", strings.Join(missing, "; "))
	}
	// A wrong-length key would otherwise surface as a crypto error on the first
	// key save, far from the .env line that caused it.
	if raw, err := hex.DecodeString(cfg.LLMKeyEncryptionSecret); err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("config: LLM_KEY_ENCRYPTION_SECRET must be 64 hex characters (32 bytes, e.g. from `openssl rand -hex 32`)")
	}

	return cfg, nil
}

// RepoPath resolves a relative path against the repository root rather than the
// working directory.
//
// Every make target runs the Go commands from backend/, but the credential
// files and .env itself live at the repository root — so "./gmail-credentials.json",
// which is what a person writing .env at the root means, would otherwise resolve
// to backend/gmail-credentials.json and not be found. The root is located by
// walking up for the .git directory; when that fails the path is returned
// unchanged, so an unusual layout degrades to the old behaviour rather than to a
// wrong absolute path.
//
// Absolute paths are returned untouched.
func RepoPath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	dir, err := os.Getwd()
	if err != nil {
		return path
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return filepath.Join(dir, filepath.Clean(path))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		dir = parent
	}
}

// envList reads a comma-separated setting into a slice, dropping empty entries.
//
// An unset or all-empty variable yields nil rather than a one-element slice
// containing "", which callers would otherwise have to special-case — and which
// would read as "restrict to the project named empty string".
func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// envDuration reads a Go duration setting (e.g. "15m", "1h"), falling back to
// def when unset, unparseable, or not positive — a tuning knob, like envInt.
func envDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return def
	}
	return parsed
}

// demoField is one environment variable belonging to a demo source.
type demoField struct {
	name  string
	value string
}

// requireWholeDemoSource reports the fields missing from a partially-configured
// demo source, and nothing when the source is either fully configured or fully
// absent.
//
// The asymmetry is the point: absent is a supported deployment (that source has
// no demo client), while half-present is a misconfiguration that would otherwise
// surface as every demo run failing against that source with nothing at startup
// to explain it.
func requireWholeDemoSource(source string, fields []demoField) []string {
	var set, unset []string
	for _, f := range fields {
		if f.value == "" {
			unset = append(unset, f.name)
			continue
		}
		set = append(set, f.name)
	}
	if len(set) == 0 || len(unset) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s (demo %s is partly configured: %s set, %s missing — configure the source fully or leave it out entirely)",
		strings.Join(unset, ", "), source, strings.Join(set, ", "), strings.Join(unset, ", "))}
}

// fileExists reports whether path names a readable file. A path that cannot be
// stat'd counts as absent: the caller is deciding whether an optional source is
// configured, and an unreadable file configures nothing.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads a positive integer setting, falling back to def when the variable
// is unset, unparseable, or not positive.
//
// A malformed value falls back rather than failing: these are tuning knobs, and
// refusing to boot over MAX_ITERATIONS="twelve" would be a worse outcome than
// running with the documented default.
func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return def
	}
	return parsed
}
