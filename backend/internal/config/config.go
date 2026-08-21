// Package config loads and validates the process configuration from the
// environment. It is the only place that reads os.Getenv, so every other
// package receives its settings as plain struct fields.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	// DefaultHost binds to loopback only. Until auth exists, every request is
	// the dev user and every chat call spends real money, so the server must
	// not be reachable from the network by default. Set HOST=0.0.0.0 to expose
	// it deliberately.
	DefaultHost            = "127.0.0.1"
	DefaultPort            = "8080"
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

	// DefaultGmailCredentialsPath is where the OAuth Desktop client JSON is
	// expected, relative to the repository root.
	DefaultGmailCredentialsPath = "./gmail-credentials.json"
	// DefaultGmailTokenPath is where cmd/gmail-auth caches the refresh token.
	DefaultGmailTokenPath = "./.gmail-token.json"
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

	// JiraBaseURL is the Atlassian site root, e.g. https://site.atlassian.net.
	JiraBaseURL string
	// JiraEmail is the Atlassian account email used for API token auth.
	JiraEmail string
	// JiraAPIToken authenticates against the Jira REST API.
	JiraAPIToken string

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
	// GmailQueryScope, when set, is ANDed into every Gmail search.
	//
	// Empty - the default - means the agent searches the whole mailbox, which
	// is Cortex working as intended: it is a system that reads live sources.
	// Setting it to a label (e.g. `label:vantage-labs`) confines the agent to
	// the seeded fixtures, which is what makes a graded eval run reproducible
	// and keeps personal mail out of a scored answer.
	GmailQueryScope string
}

// String renders the configuration with its secrets redacted.
//
// Config exists to hold credentials, so the safe rendering is the default one:
// implementing Stringer means %v and %+v cannot print the OpenAI key or the Jira
// token, no matter who formats it — a log line, a debug print, or a test failure
// message. A test that dumped this struct on mismatch is exactly how a real
// token ends up in captured output.
func (c Config) String() string {
	return fmt.Sprintf("Config{DatabaseURL:%s Host:%s Port:%s "+
		"OpenAIAPIKey:%s OpenAIBaseURL:%s LLMModel:%s LLMUtilityModel:%s EmbeddingModel:%s "+
		"MaxIterations:%d AgentRunWorkers:%d JiraBaseURL:%s JiraEmail:%s JiraAPIToken:%s "+
		"NotionToken:%s NotionParentPageID:%s GmailCredentialsPath:%s GmailTokenPath:%s GmailQueryScope:%s}",
		redactDSN(c.DatabaseURL), c.Host, c.Port,
		redact(c.OpenAIAPIKey), c.OpenAIBaseURL, c.LLMModel, c.LLMUtilityModel, c.EmbeddingModel,
		c.MaxIterations, c.AgentRunWorkers, c.JiraBaseURL, c.JiraEmail, redact(c.JiraAPIToken),
		redact(c.NotionToken), c.NotionParentPageID,
		c.GmailCredentialsPath, c.GmailTokenPath, c.GmailQueryScope)
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
		OpenAIAPIKey:    os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:   os.Getenv("OPENAI_BASE_URL"),
		LLMModel:        envOr("LLM_MODEL", DefaultLLMModel),
		LLMUtilityModel: envOr("LLM_UTILITY_MODEL", DefaultLLMUtilityModel),
		EmbeddingModel:  envOr("EMBEDDING_MODEL", DefaultEmbeddingModel),

		MaxIterations:   envInt("MAX_ITERATIONS", DefaultMaxIterations),
		AgentRunWorkers: envInt("AGENT_RUN_WORKERS", DefaultAgentRunWorkers),

		JiraBaseURL:  strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/"),
		JiraEmail:    os.Getenv("JIRA_EMAIL"),
		JiraAPIToken: os.Getenv("JIRA_API_TOKEN"),

		NotionToken:        os.Getenv("NOTION_TOKEN"),
		NotionParentPageID: strings.TrimSpace(os.Getenv("NOTION_PARENT_PAGE_ID")),

		GmailCredentialsPath: RepoPath(envOr("GMAIL_CREDENTIALS_JSON", DefaultGmailCredentialsPath)),
		GmailTokenPath:       RepoPath(envOr("GMAIL_TOKEN_PATH", DefaultGmailTokenPath)),
		GmailQueryScope:      strings.TrimSpace(os.Getenv("GMAIL_QUERY_SCOPE")),
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
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required environment variables: %s", strings.Join(missing, ", "))
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
