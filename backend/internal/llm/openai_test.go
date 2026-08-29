package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"cortex/internal/llm"
)

const (
	fakeAPIKey       = "sk-test-123"
	fakeDefaultModel = "gpt-4o"
	fakeEmbedModel   = "text-embedding-3-small"
)

// capturedRequest is the wire body the provider sends, decoded loosely so the
// test asserts on the JSON the OpenAI API would actually receive rather than on
// SDK types.
type capturedRequest struct {
	Model     string            `json:"model"`
	MaxTokens *int64            `json:"max_tokens"`
	Messages  []capturedMessage `json:"messages"`
	Tools     []capturedTool    `json:"tools"`
	Input     []string          `json:"input"`
}

type capturedMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type capturedTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// textContent renders a message's content as plain text, tolerating both the
// string and the content-part array encodings.
func (m capturedMessage) textContent(t *testing.T) string {
	t.Helper()
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		t.Fatalf("content %s is neither a string nor content parts: %v", m.Content, err)
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// recorder captures what the provider sent to the fake endpoint.
type recorder struct {
	path    string
	auth    string
	request capturedRequest
	// raw is the untouched request body, for assertions on fields
	// capturedRequest does not model (e.g. response_format).
	raw   []byte
	calls int
}

// newFakeProvider stands an httptest server in for api.openai.com and returns a
// provider pointed at it (REQ-1.5: base URL overridable so fakes are possible).
func newFakeProvider(t *testing.T, status int, body string) (*llm.OpenAI, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.calls++
		rec.path = r.URL.Path
		rec.auth = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		rec.raw = raw
		if err := json.Unmarshal(raw, &rec.request); err != nil {
			t.Errorf("decode request body %q: %v", raw, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	p := llm.NewOpenAI(llm.OpenAIConfig{
		APIKey:         fakeAPIKey,
		BaseURL:        srv.URL + "/",
		DefaultModel:   fakeDefaultModel,
		EmbeddingModel: fakeEmbedModel,
	})
	return p, rec
}

func completionBody(content string, promptTokens, completionTokens int) string {
	b, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 1,
		"model":   fakeDefaultModel,
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})
	return string(b)
}

type wantMessage struct {
	role    string
	content string
}

// TEST-1.2: Generate/GenerateWithTools against an httptest fake — request shape
// (model, messages, tools) and response mapping (text, tool calls, tokens).
func TestOpenAIGenerate(t *testing.T) {
	toolCallBody, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-tools",
		"object":  "chat.completion",
		"created": 1,
		"model":   fakeDefaultModel,
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":       "call_abc",
					"type":     "function",
					"function": map[string]any{"name": "jira_search", "arguments": `{"jql":"project = ENG"}`},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 42, "completion_tokens": 7, "total_tokens": 49},
	})

	tests := []struct {
		name       string
		req        llm.Request
		tools      []llm.ToolDef
		status     int
		body       string
		wantErr    bool
		wantResp   llm.Response
		wantModel  string
		wantMsgs   []wantMessage
		wantMaxTok int64
		wantTools  []string
	}{
		{
			name: "text completion maps text and token counts",
			req: llm.Request{
				System:   "You are Cortex.",
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "say hello"}},
			},
			status:    http.StatusOK,
			body:      completionBody("Hello there.", 11, 3),
			wantResp:  llm.Response{Text: "Hello there.", InputTokens: 11, OutputTokens: 3},
			wantModel: fakeDefaultModel,
			wantMsgs: []wantMessage{
				{role: "system", content: "You are Cortex."},
				{role: "user", content: "say hello"},
			},
		},
		{
			name: "request model overrides the configured default and max_tokens is sent",
			req: llm.Request{
				Model:     "gpt-4o-mini",
				Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
				MaxTokens: 256,
			},
			status:     http.StatusOK,
			body:       completionBody("hi back", 2, 2),
			wantResp:   llm.Response{Text: "hi back", InputTokens: 2, OutputTokens: 2},
			wantModel:  "gpt-4o-mini",
			wantMsgs:   []wantMessage{{role: "user", content: "hi"}},
			wantMaxTok: 256,
		},
		{
			name: "multi-turn history is forwarded in order",
			req: llm.Request{
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: "first"},
					{Role: llm.RoleAssistant, Content: "second"},
					{Role: llm.RoleUser, Content: "third"},
				},
			},
			status:    http.StatusOK,
			body:      completionBody("fourth", 5, 1),
			wantResp:  llm.Response{Text: "fourth", InputTokens: 5, OutputTokens: 1},
			wantModel: fakeDefaultModel,
			wantMsgs: []wantMessage{
				{role: "user", content: "first"},
				{role: "assistant", content: "second"},
				{role: "user", content: "third"},
			},
		},
		{
			name: "tool definitions are sent and tool calls are mapped back",
			req:  llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "find tickets"}}},
			tools: []llm.ToolDef{{
				Name:        "jira_search",
				Description: "Search Jira issues",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"jql":{"type":"string"}}}`),
			}},
			status: http.StatusOK,
			body:   string(toolCallBody),
			wantResp: llm.Response{
				ToolCalls: []llm.ToolCall{{
					ID:        "call_abc",
					Name:      "jira_search",
					Arguments: json.RawMessage(`{"jql":"project = ENG"}`),
				}},
				InputTokens:  42,
				OutputTokens: 7,
			},
			wantModel: fakeDefaultModel,
			wantMsgs:  []wantMessage{{role: "user", content: "find tickets"}},
			wantTools: []string{"jira_search"},
		},
		{
			name:    "api error is surfaced",
			req:     llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "boom"}}},
			status:  http.StatusBadRequest,
			body:    `{"error":{"message":"bad request","type":"invalid_request_error"}}`,
			wantErr: true,
		},
		{
			name:    "completion without choices is an error",
			req:     llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "empty"}}},
			status:  http.StatusOK,
			body:    `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0}}`,
			wantErr: true,
		},
		{
			name:    "unknown message role is rejected before the call",
			req:     llm.Request{Messages: []llm.Message{{Role: llm.Role("robot"), Content: "?"}}},
			status:  http.StatusOK,
			body:    completionBody("unused", 0, 0),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, rec := newFakeProvider(t, tt.status, tt.body)

			var (
				got llm.Response
				err error
			)
			if tt.tools == nil {
				got, err = p.Generate(context.Background(), tt.req)
			} else {
				got, err = p.GenerateWithTools(context.Background(), tt.req, tt.tools)
			}

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got response %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			if !reflect.DeepEqual(got, tt.wantResp) {
				t.Errorf("response = %+v, want %+v", got, tt.wantResp)
			}

			if rec.path != "/chat/completions" {
				t.Errorf("request path = %q, want %q", rec.path, "/chat/completions")
			}
			if rec.auth != "Bearer "+fakeAPIKey {
				t.Errorf("Authorization = %q, want %q", rec.auth, "Bearer "+fakeAPIKey)
			}
			if rec.request.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", rec.request.Model, tt.wantModel)
			}
			if tt.wantMaxTok == 0 {
				if rec.request.MaxTokens != nil {
					t.Errorf("max_tokens = %d, want it omitted", *rec.request.MaxTokens)
				}
			} else if rec.request.MaxTokens == nil || *rec.request.MaxTokens != tt.wantMaxTok {
				t.Errorf("max_tokens = %v, want %d", rec.request.MaxTokens, tt.wantMaxTok)
			}

			if len(rec.request.Messages) != len(tt.wantMsgs) {
				t.Fatalf("sent %d messages, want %d: %+v", len(rec.request.Messages), len(tt.wantMsgs), rec.request.Messages)
			}
			for i, want := range tt.wantMsgs {
				gotMsg := rec.request.Messages[i]
				if gotMsg.Role != want.role {
					t.Errorf("message %d role = %q, want %q", i, gotMsg.Role, want.role)
				}
				if c := gotMsg.textContent(t); c != want.content {
					t.Errorf("message %d content = %q, want %q", i, c, want.content)
				}
			}

			if len(rec.request.Tools) != len(tt.wantTools) {
				t.Fatalf("sent %d tools, want %d: %+v", len(rec.request.Tools), len(tt.wantTools), rec.request.Tools)
			}
			for i, name := range tt.wantTools {
				if rec.request.Tools[i].Type != "function" {
					t.Errorf("tool %d type = %q, want %q", i, rec.request.Tools[i].Type, "function")
				}
				if rec.request.Tools[i].Function.Name != name {
					t.Errorf("tool %d name = %q, want %q", i, rec.request.Tools[i].Function.Name, name)
				}
				if len(rec.request.Tools[i].Function.Parameters) == 0 {
					t.Errorf("tool %d parameters schema was not sent", i)
				}
			}
		})
	}
}

// A tool result turn must be replayable: the assistant's tool_calls and the
// matching tool message both have to reach the API (REQ-1.5 Message shape).
func TestOpenAIGenerateSendsToolResultTurn(t *testing.T) {
	p, rec := newFakeProvider(t, http.StatusOK, completionBody("done", 9, 1))

	_, err := p.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "find tickets"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID:        "call_abc",
				Name:      "jira_search",
				Arguments: json.RawMessage(`{"jql":"project = ENG"}`),
			}}},
			{Role: llm.RoleTool, ToolCallID: "call_abc", Content: "3 issues"},
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if len(rec.request.Messages) != 3 {
		t.Fatalf("sent %d messages, want 3: %+v", len(rec.request.Messages), rec.request.Messages)
	}
	assistant := rec.request.Messages[1]
	if assistant.Role != "assistant" {
		t.Errorf("message 1 role = %q, want assistant", assistant.Role)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant carried %d tool calls, want 1", len(assistant.ToolCalls))
	}
	if assistant.ToolCalls[0].ID != "call_abc" || assistant.ToolCalls[0].Function.Name != "jira_search" {
		t.Errorf("assistant tool call = %+v, want id call_abc name jira_search", assistant.ToolCalls[0])
	}
	toolMsg := rec.request.Messages[2]
	if toolMsg.Role != "tool" {
		t.Errorf("message 2 role = %q, want tool", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "call_abc" {
		t.Errorf("tool_call_id = %q, want call_abc", toolMsg.ToolCallID)
	}
	if c := toolMsg.textContent(t); c != "3 issues" {
		t.Errorf("tool content = %q, want %q", c, "3 issues")
	}
}

// TEST-1.2 (Embed half of REQ-1.5): vectors come back in input order even when
// the API returns them shuffled.
func TestOpenAIEmbed(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"object": "list",
		"model":  fakeEmbedModel,
		"data": []any{
			map[string]any{"object": "embedding", "index": 1, "embedding": []float64{0.3, 0.4}},
			map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}},
		},
		"usage": map[string]any{"prompt_tokens": 4, "total_tokens": 4},
	})

	p, rec := newFakeProvider(t, http.StatusOK, string(body))

	got, err := p.Embed(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	want := [][]float32{{0.1, 0.2}, {0.3, 0.4}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Embed() = %v, want %v", got, want)
	}
	if rec.path != "/embeddings" {
		t.Errorf("path = %q, want /embeddings", rec.path)
	}
	if !reflect.DeepEqual(rec.request.Input, []string{"alpha", "beta"}) {
		t.Errorf("input = %v, want [alpha beta]", rec.request.Input)
	}
	if rec.request.Model != fakeEmbedModel {
		t.Errorf("model = %q, want %q", rec.request.Model, fakeEmbedModel)
	}
}

func TestOpenAIEmbedEdgeCases(t *testing.T) {
	t.Run("no texts makes no call", func(t *testing.T) {
		p, rec := newFakeProvider(t, http.StatusOK, `{}`)
		got, err := p.Embed(context.Background(), nil)
		if err != nil {
			t.Fatalf("Embed() error = %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Embed() = %v, want empty", got)
		}
		if rec.calls != 0 {
			t.Errorf("made %d HTTP calls, want 0", rec.calls)
		}
	})

	t.Run("vector count mismatch is an error", func(t *testing.T) {
		body := `{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`
		p, _ := newFakeProvider(t, http.StatusOK, body)
		if _, err := p.Embed(context.Background(), []string{"a", "b"}); err == nil {
			t.Fatal("expected error for 1 vector / 2 inputs, got nil")
		}
	})
}

// REQ-1.5: an empty BaseURL must still produce a usable client pointed at the
// public API, not at an empty endpoint.
func TestNewOpenAIDefaultsBaseURL(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	p := llm.NewOpenAI(llm.OpenAIConfig{APIKey: fakeAPIKey, DefaultModel: fakeDefaultModel})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // do not actually reach the network

	_, err := p.Generate(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled (a URL/endpoint error means the base URL was not defaulted)", err)
	}
	if llm.DefaultOpenAIBaseURL == "" {
		t.Error("DefaultOpenAIBaseURL must not be empty")
	}
}
