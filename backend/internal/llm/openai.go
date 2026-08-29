package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// DefaultOpenAIBaseURL is the public OpenAI API endpoint, used when
// OpenAIConfig.BaseURL is empty.
const DefaultOpenAIBaseURL = "https://api.openai.com/v1/"

// requestTimeout is the transport-level backstop for a single provider call.
// It is deliberately looser than any per-request context deadline.
const requestTimeout = 2 * time.Minute

// OpenAIConfig configures an OpenAI-backed Provider.
type OpenAIConfig struct {
	// APIKey authenticates against the API (required).
	APIKey string
	// BaseURL overrides the API endpoint. Empty uses the SDK default; tests
	// point it at an httptest server.
	BaseURL string
	// DefaultModel is used when Request.Model is empty.
	DefaultModel string
	// EmbeddingModel is the model used by Embed.
	EmbeddingModel string
}

// OpenAI is a Provider backed by the OpenAI chat completions and embeddings
// APIs.
type OpenAI struct {
	client openai.Client
	cfg    OpenAIConfig
}

// compile-time check: OpenAI must satisfy Provider.
var _ Provider = (*OpenAI)(nil)

// NewOpenAI builds a Provider from cfg.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	// The base URL is always set explicitly. The SDK otherwise picks up
	// OPENAI_BASE_URL from the environment via os.LookupEnv, and the Makefile
	// exports every key in .env — so a present-but-empty OPENAI_BASE_URL would
	// silently leave the client with no endpoint at all.
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultOpenAIBaseURL
	}
	// The SDK defaults to http.DefaultClient, which has no timeout at all — a
	// hung connection would block forever for any caller that forgets to set a
	// deadline (Embed from an indexing job, say). Own the client so every call
	// has a backstop regardless of caller discipline.
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(&http.Client{Timeout: requestTimeout}),
	}
	return &OpenAI{client: openai.NewClient(opts...), cfg: cfg}
}

// Generate produces a text completion.
func (o *OpenAI) Generate(ctx context.Context, req Request) (Response, error) {
	return o.complete(ctx, req, nil, nil)
}

// GenerateWithTools produces either a text completion or a set of tool calls.
func (o *OpenAI) GenerateWithTools(ctx context.Context, req Request, tools []ToolDef) (Response, error) {
	return o.complete(ctx, req, tools, nil)
}

// GenerateStructured produces a completion constrained to a JSON Schema.
//
// Strict mode is requested rather than merely asking for JSON in the prompt:
// the caller (the citation pass) has to decode the result into a fixed shape,
// and a model that returns prose around its JSON, or renames a field, turns a
// recoverable step into a parse error. Strict mode makes the provider enforce
// the shape server-side.
func (o *OpenAI) GenerateStructured(ctx context.Context, req Request, schema Schema) (Response, error) {
	return o.complete(ctx, req, nil, &schema)
}

// complete is the single code path behind Generate, GenerateWithTools and
// GenerateStructured; they differ only in whether tool definitions or a
// response-format schema are attached.
func (o *OpenAI) complete(ctx context.Context, req Request, tools []ToolDef, schema *Schema) (Response, error) {
	messages, err := o.buildMessages(req)
	if err != nil {
		return Response{}, err
	}

	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(o.modelFor(req)),
		Messages: messages,
	}
	if req.MaxTokens > 0 {
		params.MaxTokens = openai.Int(int64(req.MaxTokens))
	}
	if len(tools) > 0 {
		params.Tools, err = toolParams(tools)
		if err != nil {
			return Response{}, err
		}
	}
	if schema != nil {
		format, err := responseFormat(*schema)
		if err != nil {
			return Response{}, err
		}
		params.ResponseFormat = format
	}

	completion, err := o.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, fmt.Errorf("openai: chat completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return Response{}, fmt.Errorf("openai: chat completion returned no choices")
	}

	msg := completion.Choices[0].Message
	resp := Response{
		Text:         msg.Content,
		InputTokens:  int(completion.Usage.PromptTokens),
		OutputTokens: int(completion.Usage.CompletionTokens),
	}
	for _, tc := range msg.ToolCalls {
		// Custom (non-function) tool calls are not part of Cortex's tool
		// protocol; ignore them rather than surfacing an unusable call.
		if tc.Type != "" && tc.Type != "function" {
			continue
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	return resp, nil
}

// buildMessages converts the provider-agnostic request into SDK message
// params, prepending the system prompt when one is set.
func (o *OpenAI) buildMessages(req Request) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, openai.SystemMessage(req.System))
	}
	for i, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case RoleUser:
			out = append(out, openai.UserMessage(m.Content))
		case RoleAssistant:
			out = append(out, assistantMessage(m))
		case RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		default:
			return nil, fmt.Errorf("openai: message %d has unknown role %q", i, m.Role)
		}
	}
	return out, nil
}

// assistantMessage rebuilds an assistant turn, carrying any tool calls it made
// so the model sees its own prior requests alongside the tool results.
func assistantMessage(m Message) openai.ChatCompletionMessageParamUnion {
	if len(m.ToolCalls) == 0 {
		return openai.AssistantMessage(m.Content)
	}

	param := openai.ChatCompletionAssistantMessageParam{
		ToolCalls: make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolCalls)),
	}
	if m.Content != "" {
		param.Content.OfString = openai.String(m.Content)
	}
	for _, tc := range m.ToolCalls {
		param.ToolCalls = append(param.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: tc.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
			},
		})
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &param}
}

// responseFormat converts a Schema into the SDK's response_format param.
func responseFormat(schema Schema) (openai.ChatCompletionNewParamsResponseFormatUnion, error) {
	var zero openai.ChatCompletionNewParamsResponseFormatUnion
	if schema.Name == "" {
		return zero, fmt.Errorf("openai: structured output requires a schema name")
	}
	var definition map[string]any
	if err := json.Unmarshal(schema.Definition, &definition); err != nil {
		return zero, fmt.Errorf("openai: schema %q is not a valid JSON Schema object: %w", schema.Name, err)
	}

	param := shared.ResponseFormatJSONSchemaJSONSchemaParam{
		Name:   schema.Name,
		Schema: definition,
		Strict: openai.Bool(true),
	}
	if schema.Description != "" {
		param.Description = openai.String(schema.Description)
	}
	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{JSONSchema: param},
	}, nil
}

// toolParams converts tool definitions into SDK function-tool params.
func toolParams(tools []ToolDef) ([]openai.ChatCompletionToolUnionParam, error) {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		fn := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			fn.Description = openai.String(t.Description)
		}
		if len(t.Parameters) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return nil, fmt.Errorf("openai: tool %q has an invalid parameter schema: %w", t.Name, err)
			}
			fn.Parameters = schema
		}
		out = append(out, openai.ChatCompletionFunctionTool(fn))
	}
	return out, nil
}

// Embed returns one embedding vector per input text, in input order.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	resp, err := o.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(o.cfg.EmbeddingModel),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
	})
	if err != nil {
		return nil, fmt.Errorf("openai: embeddings: %w", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("openai: embeddings: got %d vectors for %d inputs", len(resp.Data), len(texts))
	}

	// The API may return embeddings out of order; Index says where each belongs.
	vectors := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || int(d.Index) >= len(vectors) {
			return nil, fmt.Errorf("openai: embeddings: index %d out of range", d.Index)
		}
		v := make([]float32, len(d.Embedding))
		for i, f := range d.Embedding {
			v[i] = float32(f)
		}
		vectors[d.Index] = v
	}
	return vectors, nil
}

// modelFor resolves the model for a request, falling back to the configured
// default.
func (o *OpenAI) modelFor(req Request) string {
	if req.Model != "" {
		return req.Model
	}
	return o.cfg.DefaultModel
}

// SafeErrorMessage renders err as a short, classified reason that is safe to
// persist and to show to operators.
//
// The raw error must never be stored: openai-go's *openai.Error.Error()
// formats the full request URL and the verbatim upstream response body. A 401
// body contains the partially-masked API key, and a proxy/Azure-style
// OPENAI_BASE_URL can carry credentials in its query string — both would
// otherwise land in agent_runs.error and in the run_events transcript, which
// the trace panel is designed to render. Log the original error instead.
func SafeErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		msg := fmt.Sprintf("llm provider returned HTTP %d", apiErr.StatusCode)
		// Type and Code are short enumerated identifiers (e.g.
		// "invalid_request_error" / "rate_limit_exceeded"), not free text.
		if apiErr.Type != "" {
			msg += ", type=" + apiErr.Type
		}
		if apiErr.Code != "" {
			msg += ", code=" + apiErr.Code
		}
		return msg
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "llm request timed out"
	case errors.Is(err, context.Canceled):
		return "llm request canceled"
	default:
		// Anything else (DNS, TLS, connection errors) can embed the endpoint
		// URL, so report only the category.
		return "llm request failed"
	}
}
