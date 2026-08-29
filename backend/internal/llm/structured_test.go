package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cortex/internal/llm"
)

// A4 — llm.Provider gains GenerateStructured, implemented on the OpenAI provider
// through response_format: {type: json_schema, strict: true}.
//
// The citation pass (REQ-4.3) decodes the result into a fixed shape, so strict
// mode is the whole point: a model that wraps its JSON in prose or renames a
// field turns a recoverable step into a parse error. That the request actually
// carries the schema, and carries strict:true, is a wire contract worth pinning
// — nothing downstream can tell the difference until a model starts drifting.

// structuredFormat is the response_format the provider must send.
type structuredFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Strict      *bool          `json:"strict"`
		Schema      map[string]any `json:"schema"`
	} `json:"json_schema"`
}

const citedAnswerSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["answer_markdown", "citations"],
  "properties": {
    "answer_markdown": {"type": "string"},
    "citations": {"type": "array", "items": {"type": "integer"}}
  }
}`

func TestGenerateStructuredRequestsAStrictSchema(t *testing.T) {
	payload := `{"answer_markdown":"Atlas is behind [1].","citations":[1]}`
	provider, rec := newFakeProvider(t, http.StatusOK, completionBody(payload, 30, 9))

	resp, err := provider.GenerateStructured(context.Background(),
		llm.Request{
			Model:    fakeDefaultModel,
			System:   "You attach citations.",
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "cite the draft"}},
		},
		llm.Schema{
			Name:        "cited_answer",
			Description: "The final answer with inline [n] markers.",
			Definition:  json.RawMessage(citedAnswerSchema),
		})
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}

	// The response is handed back verbatim: the caller unmarshals it.
	if resp.Text != payload {
		t.Errorf("text = %q, want the model's JSON %q", resp.Text, payload)
	}
	if resp.InputTokens != 30 || resp.OutputTokens != 9 {
		t.Errorf("tokens = %d/%d, want 30/9", resp.InputTokens, resp.OutputTokens)
	}

	var body struct {
		ResponseFormat structuredFormat `json:"response_format"`
	}
	if err := json.Unmarshal(rec.raw, &body); err != nil {
		t.Fatalf("decode request %q: %v", rec.raw, err)
	}

	if body.ResponseFormat.Type != "json_schema" {
		t.Errorf("response_format.type = %q, want json_schema (request: %s)",
			body.ResponseFormat.Type, rec.raw)
	}
	if body.ResponseFormat.JSONSchema.Name != "cited_answer" {
		t.Errorf("schema name = %q, want cited_answer", body.ResponseFormat.JSONSchema.Name)
	}
	if body.ResponseFormat.JSONSchema.Description != "The final answer with inline [n] markers." {
		t.Errorf("schema description = %q, want the one that was passed",
			body.ResponseFormat.JSONSchema.Description)
	}
	if strict := body.ResponseFormat.JSONSchema.Strict; strict == nil || !*strict {
		t.Errorf("strict = %v, want true; A4 requires strict mode", strict)
	}

	var want map[string]any
	if err := json.Unmarshal([]byte(citedAnswerSchema), &want); err != nil {
		t.Fatalf("decode expected schema: %v", err)
	}
	if !reflect.DeepEqual(body.ResponseFormat.JSONSchema.Schema, want) {
		t.Errorf("schema sent = %v\nwant %v", body.ResponseFormat.JSONSchema.Schema, want)
	}

	// The conversation still travels normally: system prompt plus the turns.
	if len(rec.request.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (system + user)", len(rec.request.Messages))
	}
	if rec.request.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want system", rec.request.Messages[0].Role)
	}
	// No tools: a structured call must not also offer the model an escape into
	// another tool call.
	if len(rec.request.Tools) != 0 {
		t.Errorf("tools = %d, want none on a structured call", len(rec.request.Tools))
	}
}

// A schema the provider cannot send is a caller error, and it must be caught
// before the request goes out — the alternative is paying for a 400 from OpenAI.
func TestGenerateStructuredRejectsAnUnusableSchema(t *testing.T) {
	tests := []struct {
		name   string
		schema llm.Schema
	}{
		{
			name:   "no name",
			schema: llm.Schema{Definition: json.RawMessage(`{"type":"object"}`)},
		},
		{
			name:   "definition is not a JSON object",
			schema: llm.Schema{Name: "cited_answer", Definition: json.RawMessage(`not json`)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, rec := newFakeProvider(t, http.StatusOK, completionBody("{}", 1, 1))

			_, err := provider.GenerateStructured(context.Background(),
				llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, tt.schema)
			if err == nil {
				t.Fatal("GenerateStructured succeeded, want a schema error")
			}
			if rec.calls != 0 {
				t.Errorf("the provider made %d request(s) with an unusable schema, want 0", rec.calls)
			}
			if strings.Contains(err.Error(), fakeAPIKey) {
				t.Error("the error leaks the API key")
			}
		})
	}
}
