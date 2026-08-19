// Package config loads and validates the process configuration from the
// environment. It is the only place that reads os.Getenv, so every other
// package receives its settings as plain struct fields.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	DefaultPort            = "8080"
	DefaultLLMModel        = "gpt-4o"
	DefaultLLMUtilityModel = "gpt-4o-mini"
	DefaultEmbeddingModel  = "text-embedding-3-small"
)

// Config holds every setting the server needs. Fields map 1:1 to the variables
// documented in .env.example.
type Config struct {
	// DatabaseURL is the Postgres connection string (required).
	DatabaseURL string
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
}

// Load reads the configuration from the environment. All missing required
// variables are reported in a single error so they can be fixed in one pass.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Port:            envOr("PORT", DefaultPort),
		OpenAIAPIKey:    os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:   os.Getenv("OPENAI_BASE_URL"),
		LLMModel:        envOr("LLM_MODEL", DefaultLLMModel),
		LLMUtilityModel: envOr("LLM_UTILITY_MODEL", DefaultLLMUtilityModel),
		EmbeddingModel:  envOr("EMBEDDING_MODEL", DefaultEmbeddingModel),
	}

	var missing []string
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.OpenAIAPIKey == "" {
		missing = append(missing, "OPENAI_API_KEY")
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
