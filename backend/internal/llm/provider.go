// Package llm defines the provider-agnostic contract the rest of Cortex uses
// to talk to a large language model. Nothing outside this package imports an
// SDK type, so swapping providers — or standing in a fake during tests — is a
// matter of supplying another Provider implementation.
package llm

import (
	"context"
	"encoding/json"
)

// Role identifies the author of a Message, matching the OpenAI chat roles.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn of a conversation.
//
// For RoleTool messages, ToolCallID identifies the assistant tool call this
// message answers. For RoleAssistant messages that requested tools, ToolCalls
// carries those requests so the exchange can be replayed back to the model on
// the next iteration of the agent loop.
type Message struct {
	Role       Role
	Content    string
	ToolCallID string
	ToolCalls  []ToolCall
}

// Request is a single generation request. Model and MaxTokens fall back to the
// provider's configured defaults when zero.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
}

// ToolDef describes a tool the model may call. Parameters is a JSON Schema
// object describing the tool's arguments.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is the model's request to invoke a tool. Arguments is raw JSON
// produced by the model and is NOT guaranteed to match the tool's schema —
// callers must validate before executing.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Response is the result of a generation. Text and ToolCalls are mutually
// exclusive in practice: the model either answers or asks for tools.
type Response struct {
	Text         string
	ToolCalls    []ToolCall
	InputTokens  int
	OutputTokens int
}

// Schema names a structured-output contract.
//
// Definition is a JSON Schema object. It is sent in strict mode, which the
// OpenAI API only accepts for a restricted subset of JSON Schema: every property
// must appear in "required", and every object must set
// "additionalProperties": false. A schema that violates either is rejected at
// request time rather than producing a loosely-conforming answer, which is the
// behaviour we want — a citation payload that is missing a field is worse than
// no citation pass at all.
type Schema struct {
	// Name identifies the schema to the provider. Letters, digits, underscores
	// and dashes only.
	Name string
	// Description tells the model what the shape is for.
	Description string
	// Definition is the JSON Schema object itself.
	Definition json.RawMessage
}

// Provider is the interface every LLM backend implements.
type Provider interface {
	// Generate produces a text completion.
	Generate(ctx context.Context, req Request) (Response, error)
	// GenerateWithTools produces either a text completion or a set of tool
	// calls, given the tools the model is allowed to use.
	GenerateWithTools(ctx context.Context, req Request, tools []ToolDef) (Response, error)
	// GenerateStructured produces a completion constrained to schema.
	// Response.Text is the JSON document; it is guaranteed to parse against the
	// schema only insofar as the provider enforces strict mode, so callers still
	// decode defensively.
	GenerateStructured(ctx context.Context, req Request, schema Schema) (Response, error)
	// Embed returns one embedding vector per input text, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
