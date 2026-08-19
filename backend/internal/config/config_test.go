package config_test

import (
	"strings"
	"testing"

	"cortex/internal/config"
)

// envKeys is every variable internal/config reads. Each test case starts from a
// clean slate: every key is explicitly cleared, then the case's values applied.
var envKeys = []string{
	"DATABASE_URL",
	"OPENAI_API_KEY",
	"OPENAI_BASE_URL",
	"PORT",
	"LLM_MODEL",
	"LLM_UTILITY_MODEL",
	"EMBEDDING_MODEL",
}

const (
	testDatabaseURL = "postgres://u:p@localhost:5432/db?sslmode=disable"
	testAPIKey      = "sk-test-key"
)

// TEST-1.1: required variables are enforced, defaults applied (REQ-1.5, REQ-1.8).
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
			name:            "missing both required vars are reported in one error",
			env:             map[string]string{},
			wantErr:         true,
			wantErrContains: []string{"DATABASE_URL", "OPENAI_API_KEY"},
		},
		{
			name:            "missing DATABASE_URL only",
			env:             map[string]string{"OPENAI_API_KEY": testAPIKey},
			wantErr:         true,
			wantErrContains: []string{"DATABASE_URL"},
			wantErrOmits:    []string{"OPENAI_API_KEY"},
		},
		{
			name:            "missing OPENAI_API_KEY only",
			env:             map[string]string{"DATABASE_URL": testDatabaseURL},
			wantErr:         true,
			wantErrContains: []string{"OPENAI_API_KEY"},
			wantErrOmits:    []string{"DATABASE_URL"},
		},
		{
			name: "empty required var counts as missing",
			env: map[string]string{
				"DATABASE_URL":   testDatabaseURL,
				"OPENAI_API_KEY": "",
			},
			wantErr:         true,
			wantErrContains: []string{"OPENAI_API_KEY"},
		},
		{
			name: "defaults applied when optional vars unset",
			env: map[string]string{
				"DATABASE_URL":   testDatabaseURL,
				"OPENAI_API_KEY": testAPIKey,
			},
			want: config.Config{
				DatabaseURL:     testDatabaseURL,
				Port:            "8080",
				OpenAIAPIKey:    testAPIKey,
				OpenAIBaseURL:   "",
				LLMModel:        "gpt-4o",
				LLMUtilityModel: "gpt-4o-mini",
				EmbeddingModel:  "text-embedding-3-small",
			},
		},
		{
			name: "explicit values override every default",
			env: map[string]string{
				"DATABASE_URL":      testDatabaseURL,
				"OPENAI_API_KEY":    testAPIKey,
				"OPENAI_BASE_URL":   "http://127.0.0.1:1234/v1/",
				"PORT":              "9999",
				"LLM_MODEL":         "gpt-4.1",
				"LLM_UTILITY_MODEL": "gpt-4.1-mini",
				"EMBEDDING_MODEL":   "text-embedding-3-large",
			},
			want: config.Config{
				DatabaseURL:     testDatabaseURL,
				Port:            "9999",
				OpenAIAPIKey:    testAPIKey,
				OpenAIBaseURL:   "http://127.0.0.1:1234/v1/",
				LLMModel:        "gpt-4.1",
				LLMUtilityModel: "gpt-4.1-mini",
				EmbeddingModel:  "text-embedding-3-large",
			},
		},
		{
			name: "empty optional vars fall back to defaults",
			env: map[string]string{
				"DATABASE_URL":      testDatabaseURL,
				"OPENAI_API_KEY":    testAPIKey,
				"PORT":              "",
				"LLM_MODEL":         "",
				"LLM_UTILITY_MODEL": "",
				"EMBEDDING_MODEL":   "",
			},
			want: config.Config{
				DatabaseURL:     testDatabaseURL,
				Port:            "8080",
				OpenAIAPIKey:    testAPIKey,
				LLMModel:        "gpt-4o",
				LLMUtilityModel: "gpt-4o-mini",
				EmbeddingModel:  "text-embedding-3-small",
			},
		},
		{
			name: "unrelated later-day vars are ignored",
			env: map[string]string{
				"DATABASE_URL":    testDatabaseURL,
				"OPENAI_API_KEY":  testAPIKey,
				"FRONTEND_ORIGIN": "http://localhost:3000",
				"JIRA_BASE_URL":   "https://example.atlassian.net",
			},
			want: config.Config{
				DatabaseURL:     testDatabaseURL,
				Port:            "8080",
				OpenAIAPIKey:    testAPIKey,
				LLMModel:        "gpt-4o",
				LLMUtilityModel: "gpt-4o-mini",
				EmbeddingModel:  "text-embedding-3-small",
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
			if *cfg != tt.want {
				t.Errorf("Load() = %+v, want %+v", *cfg, tt.want)
			}
		})
	}
}

// Defaults are exported so callers (and .env.example) stay in sync with REQ-1.5.
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
