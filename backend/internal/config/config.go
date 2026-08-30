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
	// identity when Google sign-in landed (Day 7).
	DefaultDevUserEmail = "dev@cortex.local"
	// DefaultRunsPerUserPerHour bounds run creation per user. The LLM spend is
	// the user's own key, but every run also consumes the server's Jira, Notion
	// and Gmail quotas — the limit protects the shared part.
	DefaultRunsPerUserPerHour = 30
	// DefaultIndexRefreshCooldown is the minimum interval between user-triggered
	// Sources refreshes. The upstream APIs are paginated and rate-limited; a
	// 15-minute-fresh local copy beats a live crawl per page view.
	DefaultIndexRefreshCooldown = 15 * time.Minute
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

	// OpenAIAPIKey authenticates against the OpenAI API (required).
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

	// JiraBaseURL is the Atlassian site root, e.g. https://site.atlassian.net.
	JiraBaseURL string
	// JiraEmail is the Atlassian account email used for API token auth.
	JiraEmail string
	// JiraAPIToken authenticates against the Jira REST API.
	JiraAPIToken string
	// JiraProjects restricts the agent and the indexing crawl to these project
	// keys (required since Day 7).
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
	// GmailQueryScope is ANDed into every Gmail search (required since Day 7).
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
}

// String renders the configuration with its secrets redacted.
//
// Config exists to hold credentials, so the safe rendering is the default one:
// implementing Stringer means %v and %+v cannot print the OpenAI key or the Jira
// token, no matter who formats it — a log line, a debug print, or a test failure
// message. A test that dumped this struct on mismatch is exactly how a real
// token ends up in captured output.
func (c Config) String() string {
	return fmt.Sprintf("Config{DatabaseURL:%s Host:%s Port:%s FrontendOrigin:%s "+
		"OpenAIAPIKey:%s OpenAIBaseURL:%s LLMModel:%s LLMUtilityModel:%s EmbeddingModel:%s "+
		"MaxIterations:%d AgentRunWorkers:%d ContextTokenBudget:%d IndexMaxDocuments:%d IndexWorkers:%d "+
		"JiraBaseURL:%s JiraEmail:%s JiraAPIToken:%s "+
		"JiraProjects:%s NotionToken:%s NotionParentPageID:%s "+
		"GmailCredentialsPath:%s GmailTokenPath:%s GmailQueryScope:%s "+
		"GoogleOAuthClientID:%s GoogleOAuthClientSecret:%s AuthAllowedEmails:%s "+
		"AuthAPIToken:%s AdminEmails:%s DevUserEmail:%s LLMKeyEncryptionSecret:%s "+
		"RunsPerUserPerHour:%d IndexRefreshCooldown:%s}",
		redactDSN(c.DatabaseURL), c.Host, c.Port, c.FrontendOrigin,
		redact(c.OpenAIAPIKey), c.OpenAIBaseURL, c.LLMModel, c.LLMUtilityModel, c.EmbeddingModel,
		c.MaxIterations, c.AgentRunWorkers, c.ContextTokenBudget, c.IndexMaxDocuments, c.IndexWorkers,
		c.JiraBaseURL, c.JiraEmail, redact(c.JiraAPIToken),
		strings.Join(c.JiraProjects, ","), redact(c.NotionToken), c.NotionParentPageID,
		c.GmailCredentialsPath, c.GmailTokenPath, c.GmailQueryScope,
		c.GoogleOAuthClientID, redact(c.GoogleOAuthClientSecret), strings.Join(c.AuthAllowedEmails, ","),
		redact(c.AuthAPIToken), strings.Join(c.AdminEmails, ","), c.DevUserEmail, redact(c.LLMKeyEncryptionSecret),
		c.RunsPerUserPerHour, c.IndexRefreshCooldown)
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
	}
	if len(cfg.AdminEmails) == 0 {
		cfg.AdminEmails = []string{cfg.DevUserEmail}
	}

	var missing []string
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.OpenAIAPIKey == "" {
		missing = append(missing, "OPENAI_API_KEY")
	}
	// Jira is required, not optional. Every tool the agent has reaches Jira, so
	// a server without these credentials cannot answer any question — failing at
	// startup is far easier to diagnose than every run failing mid-loop.
	if cfg.JiraBaseURL == "" {
		missing = append(missing, "JIRA_BASE_URL")
	}
	if cfg.JiraEmail == "" {
		missing = append(missing, "JIRA_EMAIL")
	}
	if cfg.JiraAPIToken == "" {
		missing = append(missing, "JIRA_API_TOKEN")
	}
	// Notion and Gmail are required for the same reason Jira is: from Day 3 the
	// agent investigates across all three, and a server missing one of them
	// cannot answer a multi-hop question. It would still answer - badly, and
	// without ever saying which source it could not reach - which is far worse
	// than refusing to start.
	if cfg.NotionToken == "" {
		missing = append(missing, "NOTION_TOKEN")
	}
	// GMAIL_CREDENTIALS_JSON is not checked for emptiness: it has a default, so
	// it is never empty. What matters is whether the file is there, and that is
	// checked where it is read — cmd/server fails at startup naming the file,
	// and cmd/gmail-auth names it too.
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
	// The source pins stopped being optional when sign-in opened: the Gmail
	// account is a real mailbox and the Jira site has real projects, so an
	// authenticated stranger must be structurally unable to reach outside the
	// demo workspace. Empty pins are a safety hole, not a wider default.
	if cfg.GmailQueryScope == "" {
		missing = append(missing, "GMAIL_QUERY_SCOPE (required: pins every Gmail search to the demo slice of a real mailbox)")
	}
	if len(cfg.JiraProjects) == 0 {
		missing = append(missing, "JIRA_PROJECTS (required: pins the agent to the demo Jira projects)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required environment variables: %s", strings.Join(missing, ", "))
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

// envOr returns the value of key, or def when the variable is unset or empty.
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
