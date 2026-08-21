// Package config loads and validates the process configuration from the
// environment. It is the only place that reads os.Getenv, so every other
// package receives its settings as plain struct fields.
package config

import (
	"fmt"
	"net/url"
	"os"
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
		"MaxIterations:%d AgentRunWorkers:%d JiraBaseURL:%s JiraEmail:%s JiraAPIToken:%s}",
		redactDSN(c.DatabaseURL), c.Host, c.Port,
		redact(c.OpenAIAPIKey), c.OpenAIBaseURL, c.LLMModel, c.LLMUtilityModel, c.EmbeddingModel,
		c.MaxIterations, c.AgentRunWorkers, c.JiraBaseURL, c.JiraEmail, redact(c.JiraAPIToken))
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
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required environment variables: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
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
