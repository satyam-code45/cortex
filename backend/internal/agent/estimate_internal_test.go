package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"cortex/internal/llm"
)

// TEST-6.4 — estimateTokens, the pure function the context guard is built on.
//
// It is deliberately an estimator with no provider: the guard's trigger has to
// be deterministic and testable. It must count everything that is re-sent to
// the model on every iteration — the system prompt, each message's content,
// and tool-call names and arguments.
func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name     string
		system   string
		messages []llm.Message
		want     int
	}{
		{
			name: "empty transcript",
			want: 0,
		},
		{
			name:   "system prompt alone",
			system: strings.Repeat("s", 40),
			want:   10,
		},
		{
			name:   "message content adds to the system prompt",
			system: strings.Repeat("s", 8),
			messages: []llm.Message{
				{Role: llm.RoleUser, Content: strings.Repeat("u", 12)},
			},
			want: 5,
		},
		{
			name: "tool-call names and arguments are counted",
			messages: []llm.Message{
				{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{{
						// 4 name chars + 8 argument chars = 12 chars.
						Name:      "srch",
						Arguments: json.RawMessage(`{"q":12}`),
					}},
				},
			},
			want: 3,
		},
		{
			name: "tool observations are counted",
			messages: []llm.Message{
				{Role: llm.RoleTool, ToolCallID: "c1", Content: strings.Repeat("o", 20)},
			},
			want: 5,
		},
		{
			name:   "remainder chars round down",
			system: strings.Repeat("s", 7),
			want:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateTokens(tt.system, tt.messages); got != tt.want {
				t.Errorf("estimateTokens = %d, want %d", got, tt.want)
			}
		})
	}
}
