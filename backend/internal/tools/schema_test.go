package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cortex/internal/tools"
)

// Argument validation and canonicalization are the two pieces of the tool
// contract the agent loop leans on hardest:
//
//   - Validate's message is not a log line, it is an observation the model reads
//     and acts on, so it has to name the offending argument.
//   - CanonicalJSON is the dedupe key. If two spellings of the same call
//     canonicalize differently the loop pays twice for the same Jira round trip.

// searchSchema mirrors the shape every Cortex tool schema has: a flat object of
// scalars, with required fields and bounded numbers.
const searchSchema = `{
  "type": "object",
  "properties": {
    "jql": {"type": "string"},
    "max_results": {"type": "integer", "minimum": 1, "maximum": 50},
    "include_done": {"type": "boolean"},
    "labels": {"type": "array", "items": {"type": "string"}},
    "order": {"type": "string", "enum": ["asc", "desc"]}
  },
  "required": ["jql"]
}`

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		wantErr bool
		// mentions are substrings the model needs in order to self-correct.
		mentions []string
	}{
		{name: "minimal valid call", args: `{"jql":"project = ATLAS"}`},
		{name: "every argument valid", args: `{"jql":"project = ATLAS","max_results":25,"include_done":false,"labels":["blocked"],"order":"desc"}`},
		{
			name:     "required argument missing",
			args:     `{"max_results":25}`,
			wantErr:  true,
			mentions: []string{"jql", "required"},
		},
		{
			name:     "unknown argument is rejected, not dropped",
			args:     `{"jql":"project = ATLAS","project":"ATLAS"}`,
			wantErr:  true,
			mentions: []string{"project"},
		},
		{
			name:     "string given a number",
			args:     `{"jql":42}`,
			wantErr:  true,
			mentions: []string{"jql", "string"},
		},
		{
			name:     "integer given a string",
			args:     `{"jql":"x","max_results":"25"}`,
			wantErr:  true,
			mentions: []string{"max_results", "integer"},
		},
		{
			// JSON Schema has treated a zero fractional part as a valid integer
			// since draft 6, and models do emit that form. CanonicalJSON already
			// collapses 10 and 10.0 to one dedupe key, so rejecting it here would
			// contradict our own canonicalization.
			name: "integer written as a float with no fractional part",
			args: `{"jql":"x","max_results":25.0}`,
		},
		{
			name:     "integer with a real fractional part is rejected",
			args:     `{"jql":"x","max_results":25.5}`,
			wantErr:  true,
			mentions: []string{"max_results", "whole number"},
		},
		{
			name:     "float bounds still apply",
			args:     `{"jql":"x","max_results":500.0}`,
			wantErr:  true,
			mentions: []string{"max_results", "at most 50"},
		},
		{
			name:     "integer below the minimum",
			args:     `{"jql":"x","max_results":0}`,
			wantErr:  true,
			mentions: []string{"max_results", "at least 1"},
		},
		{
			name:     "integer above the maximum",
			args:     `{"jql":"x","max_results":500}`,
			wantErr:  true,
			mentions: []string{"max_results", "at most 50"},
		},
		{
			name:     "boolean given a string",
			args:     `{"jql":"x","include_done":"yes"}`,
			wantErr:  true,
			mentions: []string{"include_done"},
		},
		{
			name:     "array given a string",
			args:     `{"jql":"x","labels":"blocked"}`,
			wantErr:  true,
			mentions: []string{"labels", "array"},
		},
		{
			name:     "array item of the wrong type",
			args:     `{"jql":"x","labels":["blocked",7]}`,
			wantErr:  true,
			mentions: []string{"labels[1]"},
		},
		{
			name:     "value outside the enum",
			args:     `{"jql":"x","order":"sideways"}`,
			wantErr:  true,
			mentions: []string{"order", "asc", "desc"},
		},
		{
			name:     "arguments are not a JSON object",
			args:     `"project = ATLAS"`,
			wantErr:  true,
			mentions: []string{"JSON object"},
		},
		{
			name:     "arguments are truncated JSON",
			args:     `{"jql":`,
			wantErr:  true,
			mentions: []string{"JSON"},
		},
		{
			name:    "several problems are reported together",
			args:    `{"max_results":"many","project":"ATLAS"}`,
			wantErr: true,
			// One round trip to fix three mistakes, not three.
			mentions: []string{"jql", "max_results", "project"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tools.Validate(json.RawMessage(searchSchema), json.RawMessage(tt.args))
			if tt.wantErr && err == nil {
				t.Fatalf("Validate(%s) = nil, want an error", tt.args)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Validate(%s) = %v, want nil", tt.args, err)
				}
				return
			}
			for _, want := range tt.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q; the model cannot correct what it is not told", err, want)
				}
			}
		})
	}
}

// A tool with no required arguments is legitimately called with nothing, and
// providers spell that three different ways.
func TestValidateAcceptsAbsentArguments(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	for _, args := range []string{``, `{}`, `null`, `  `} {
		if err := tools.Validate(schema, json.RawMessage(args)); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", args, err)
		}
	}
}

// The dedupe key: everything that differs only in spelling must collapse.
func TestCanonicalJSON(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{name: "already canonical", args: `{"a":1}`, want: `{"a":1}`},
		{name: "key order", args: `{"b":2,"a":1}`, want: `{"a":1,"b":2}`},
		{name: "whitespace", args: "{\n  \"a\" : 1\n}", want: `{"a":1}`},
		{name: "integer written as a float", args: `{"a":1.0}`, want: `{"a":1}`},
		{name: "nested object keys", args: `{"o":{"z":1,"y":2}}`, want: `{"o":{"y":2,"z":1}}`},
		{name: "empty", args: ``, want: `{}`},
		{name: "null", args: `null`, want: `{}`},
		{name: "whitespace only", args: "  \n", want: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tools.CanonicalJSON(json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("CanonicalJSON(%q): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("CanonicalJSON(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// Two spellings of the same call must produce the same key; two different calls
// must not.
func TestCanonicalJSONDedupeKey(t *testing.T) {
	same := [][2]string{
		{`{"jql":"project = ATLAS","max_results":10}`, `{"max_results":10,"jql":"project = ATLAS"}`},
		{`{"key":"ATLAS-1"}`, "{ \"key\" : \"ATLAS-1\" }"},
	}
	for _, pair := range same {
		a, err := tools.CanonicalJSON(json.RawMessage(pair[0]))
		if err != nil {
			t.Fatalf("CanonicalJSON(%s): %v", pair[0], err)
		}
		b, err := tools.CanonicalJSON(json.RawMessage(pair[1]))
		if err != nil {
			t.Fatalf("CanonicalJSON(%s): %v", pair[1], err)
		}
		if a != b {
			t.Errorf("%s and %s canonicalize differently (%q vs %q); the same call would run twice",
				pair[0], pair[1], a, b)
		}
	}

	differ := [][2]string{
		{`{"key":"ATLAS-1"}`, `{"key":"ATLAS-2"}`},
		{`{"jql":"project = ATLAS"}`, `{"jql":"project = BEACON"}`},
		{`{"jql":"x","max_results":10}`, `{"jql":"x","max_results":50}`},
	}
	for _, pair := range differ {
		a, _ := tools.CanonicalJSON(json.RawMessage(pair[0]))
		b, _ := tools.CanonicalJSON(json.RawMessage(pair[1]))
		if a == b {
			t.Errorf("%s and %s canonicalize identically (%q); a distinct call would be served from cache",
				pair[0], pair[1], a)
		}
	}
}

func TestCanonicalJSONRejectsInvalidJSON(t *testing.T) {
	if _, err := tools.CanonicalJSON(json.RawMessage(`{"a":`)); err == nil {
		t.Error("CanonicalJSON accepted truncated JSON, want an error")
	}
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

// stubTool is the smallest thing satisfying tools.Tool.
type stubTool struct {
	name   string
	schema string
}

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return "stub" }
func (s stubTool) Schema() json.RawMessage {
	if s.schema == "" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return json.RawMessage(s.schema)
}
func (s stubTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Content: "ok"}, nil
}

// Wiring mistakes must surface at startup, not halfway through a paid run.
func TestNewRegistryRejectsWiringMistakes(t *testing.T) {
	tests := []struct {
		name string
		list []tools.Tool
	}{
		{name: "duplicate name", list: []tools.Tool{stubTool{name: "a"}, stubTool{name: "a"}}},
		{name: "empty name", list: []tools.Tool{stubTool{name: ""}}},
		{name: "unparseable schema", list: []tools.Tool{stubTool{name: "a", schema: `{"type":`}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tools.NewRegistry(tt.list...); err == nil {
				t.Error("NewRegistry accepted the tool set, want an error")
			}
		})
	}
}

// Definitions are sorted so prompts (and therefore provider-side prompt caching)
// are reproducible.
func TestRegistryDefinitionsAreSorted(t *testing.T) {
	registry, err := tools.NewRegistry(stubTool{name: "zeta"}, stubTool{name: "alpha"}, stubTool{name: "mid"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	want := []string{"alpha", "mid", "zeta"}
	defs := registry.Definitions()
	if len(defs) != len(want) {
		t.Fatalf("definitions = %d, want %d", len(defs), len(want))
	}
	for i, name := range want {
		if defs[i].Name != name {
			t.Errorf("definitions[%d].Name = %q, want %q", i, defs[i].Name, name)
		}
	}
	if registry.Len() != 3 {
		t.Errorf("Len() = %d, want 3", registry.Len())
	}
	if _, ok := registry.Get("alpha"); !ok {
		t.Error("Get(alpha) = not found")
	}
	if _, ok := registry.Get("nope"); ok {
		t.Error("Get(nope) found a tool that was never registered")
	}
}
