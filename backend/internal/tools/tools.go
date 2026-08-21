// Package tools defines the contract every agent capability implements and the
// registry the agent loop resolves calls through.
//
// A tool is the only way the agent reaches the outside world. Two properties of
// the contract matter more than the interface itself:
//
//   - Result.Content is what the model sees, and it is charged for on every
//     later iteration of the loop. Tools compact aggressively.
//   - Result.Evidence is what the *system* sees. Citations (Day 4) are built by
//     joining the answer back to evidence rows, so a tool that returns content
//     without evidence produces claims that can never be sourced. Returning
//     evidence is a hard convention, not a nicety.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"cortex/internal/llm"
)

// ErrInvalidArgument marks a tool failure caused by the arguments themselves
// rather than by the upstream system.
//
// The agent loop checks for it to decide whether a retry could possibly help:
// a malformed issue key will be malformed the second time too, so retrying only
// burns latency. Wrap it with %w from any tool-side argument check that Validate
// cannot express in JSON Schema.
var ErrInvalidArgument = errors.New("invalid argument")

// Tool is one capability the agent can invoke.
//
// Execute receives the raw arguments the model produced. The registry does not
// validate them — the agent loop does, via Validate, so that an invalid call
// becomes an observation the model can correct rather than an error that ends
// the run.
type Tool interface {
	// Name is the identifier the model calls, e.g. "jira_search_issues".
	Name() string
	// Description tells the model when to reach for this tool. It is prompt
	// text and is charged on every LLM call in the loop, so keep it tight.
	Description() string
	// Schema is a JSON Schema object describing the arguments.
	Schema() json.RawMessage
	// Execute runs the tool. Returning an error is expected and safe: the loop
	// turns it into an observation.
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Result is what a tool returns.
type Result struct {
	// Content is the observation handed to the model. Compact and plain-text.
	Content string
	// Evidence records what the content was derived from, one item per source
	// document. Every result must populate this.
	Evidence []EvidenceItem
}

// EvidenceItem is a single citable source behind a tool result.
type EvidenceItem struct {
	// Source is the system the item came from, e.g. "jira".
	Source string
	// ExternalID identifies the item within that system, e.g. "ATLAS-145".
	ExternalID string
	// Title is a human-readable label, e.g. the issue summary.
	Title string
	// URL points a reader at the original. Citations render this.
	URL string
	// Snippet is the specific text the claim rests on.
	Snippet string
	// Timestamp is when the source was last changed, where the source reports
	// it. Nil when unknown — an absent timestamp is honest, a zero one is not.
	Timestamp *time.Time
}

// Registry resolves tool names to tools and renders the definitions sent to the
// model.
type Registry struct {
	byName map[string]Tool
	// names is kept sorted so Definitions is deterministic. The definitions go
	// into the prompt, and a stable order keeps prompts (and therefore
	// provider-side prompt caching, and test assertions) reproducible.
	names []string
}

// NewRegistry builds a registry from the given tools.
//
// It fails on a duplicate name or an unparseable schema. Both are wiring
// mistakes, and both are far cheaper to hit at startup than halfway through a
// paid agent run: a duplicate name would silently shadow a capability, and a
// malformed schema is rejected by the provider on every single call.
func NewRegistry(list ...Tool) (*Registry, error) {
	r := &Registry{byName: make(map[string]Tool, len(list))}
	for _, t := range list {
		name := t.Name()
		if name == "" {
			return nil, fmt.Errorf("tools: a tool has an empty name")
		}
		if _, exists := r.byName[name]; exists {
			return nil, fmt.Errorf("tools: duplicate tool name %q", name)
		}
		var schema map[string]any
		if err := json.Unmarshal(t.Schema(), &schema); err != nil {
			return nil, fmt.Errorf("tools: tool %q has an invalid schema: %w", name, err)
		}
		r.byName[name] = t
		r.names = append(r.names, name)
	}
	sort.Strings(r.names)
	return r, nil
}

// Get returns the tool registered under name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}

// Len reports how many tools are registered.
func (r *Registry) Len() int { return len(r.byName) }

// Definitions renders the registry as the tool list the provider expects.
func (r *Registry) Definitions() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.names))
	for _, name := range r.names {
		t := r.byName[name]
		defs = append(defs, llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}
	return defs
}
