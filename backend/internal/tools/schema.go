package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// Validate checks model-produced arguments against a tool's JSON Schema.
//
// This is a deliberately small validator rather than a full JSON Schema
// implementation: every schema in Cortex is a flat object of scalars and string
// arrays, and we own all of them. It covers `type`, `properties`, `required`,
// `enum`, `minimum`/`maximum`, and `items`, and rejects unknown properties.
// If a tool ever needs `$ref` or `oneOf`, swap in a real library then.
//
// The returned error is written for the *model* to read, not for a log: the
// agent loop feeds it back as an observation so the model can correct itself
// and retry, which is why every message names the offending argument and what
// was expected. Errors are joined into one message so a call with three bad
// arguments takes one round trip to fix instead of three.
func Validate(schema, args json.RawMessage) error {
	var doc schemaDoc
	if err := json.Unmarshal(schema, &doc); err != nil {
		// The schema is ours, not the model's, so this is a bug on our side.
		return fmt.Errorf("tool schema is not valid JSON Schema: %w", err)
	}
	if doc.Type != "" && doc.Type != "object" {
		return fmt.Errorf("tool schema root must be an object, got %q", doc.Type)
	}

	values, err := decodeArgs(args)
	if err != nil {
		return err
	}

	var problems []string

	for _, name := range doc.Required {
		if _, present := values[name]; !present {
			problems = append(problems, fmt.Sprintf("%q is required but was not provided", name))
		}
	}

	// Unknown arguments are rejected unless the schema opts in. A model that
	// invents an argument has usually misread the tool, and silently dropping
	// it produces a confidently wrong result instead of a correction.
	allowUnknown := doc.AdditionalProperties != nil && *doc.AdditionalProperties

	for _, name := range sortedKeys(values) {
		prop, known := doc.Properties[name]
		if !known {
			if !allowUnknown {
				problems = append(problems, fmt.Sprintf(
					"%q is not a recognized argument (expected: %s)", name, strings.Join(sortedKeys(doc.Properties), ", ")))
			}
			continue
		}
		if err := validateValue(name, prop, values[name]); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("invalid arguments: %s", strings.Join(problems, "; "))
	}
	return nil
}

// schemaDoc is the root of a tool's argument schema.
type schemaDoc struct {
	Type                 string                 `json:"type"`
	Properties           map[string]*schemaNode `json:"properties"`
	Required             []string               `json:"required"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
}

// schemaNode describes one argument.
type schemaNode struct {
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Enum        []any       `json:"enum"`
	Minimum     *float64    `json:"minimum"`
	Maximum     *float64    `json:"maximum"`
	Items       *schemaNode `json:"items"`
}

// decodeArgs parses the model's arguments into a map.
//
// Numbers are kept as json.Number so that an integer-typed argument can be
// told apart from a float: decoding into float64 would make 1.5 and 1
// indistinguishable, and "max_results": 1.5 must be rejected.
func decodeArgs(args json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(args)
	// A tool with no required arguments is legitimately called with no
	// arguments at all; providers spell that as "", "{}", or "null".
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return nil, fmt.Errorf("arguments are not a valid JSON object: %w", err)
	}
	if values == nil {
		return map[string]any{}, nil
	}
	return values, nil
}

// validateValue checks one argument against its schema node.
func validateValue(name string, node *schemaNode, value any) error {
	// A null property in the schema ({"properties":{"q":null}}) decodes to a nil
	// node, and the validator's whole job is to never be the thing that crashes
	// the loop. NewRegistry's schema check only proves the schema is valid JSON,
	// so it would not catch this.
	if node == nil {
		return nil
	}
	if err := checkType(name, node, value); err != nil {
		return err
	}
	if err := checkEnum(name, node, value); err != nil {
		return err
	}
	return checkRange(name, node, value)
}

// checkType enforces the node's `type`, recursing into array items.
func checkType(name string, node *schemaNode, value any) error {
	switch node.Type {
	case "": // untyped: anything goes
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return typeError(name, "a string", value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return typeError(name, "true or false", value)
		}
	case "integer":
		num, ok := value.(json.Number)
		if !ok {
			return typeError(name, "an integer", value)
		}
		if !isWholeNumber(num) {
			return fmt.Errorf("%q must be a whole number, got %s", name, num.String())
		}
	case "number":
		num, ok := value.(json.Number)
		if !ok {
			return typeError(name, "a number", value)
		}
		if _, err := num.Float64(); err != nil {
			return fmt.Errorf("%q must be a number, got %s", name, num.String())
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return typeError(name, "an array", value)
		}
		if node.Items == nil {
			return nil
		}
		for i, item := range items {
			if err := validateValue(fmt.Sprintf("%s[%d]", name, i), node.Items, item); err != nil {
				return err
			}
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return typeError(name, "an object", value)
		}
	default:
		return fmt.Errorf("tool schema for %q declares unsupported type %q", name, node.Type)
	}
	return nil
}

// isWholeNumber reports whether a JSON number denotes an integer.
//
// "10.0" counts. JSON Schema has treated a zero fractional part as a valid
// integer since draft 6, models do emit that form, and CanonicalJSON already
// collapses 10 and 10.0 to the same dedupe key — so rejecting it here would both
// break the schema contract and disagree with our own canonicalization, costing
// the model an iteration on a correction it should never have been asked to make.
func isWholeNumber(num json.Number) bool {
	if _, err := num.Int64(); err == nil {
		return true
	}
	f, err := num.Float64()
	if err != nil {
		return false
	}
	return f == math.Trunc(f)
}

// checkEnum enforces the node's `enum`, if any.
func checkEnum(name string, node *schemaNode, value any) error {
	if len(node.Enum) == 0 {
		return nil
	}
	got := scalarString(value)
	allowed := make([]string, 0, len(node.Enum))
	for _, candidate := range node.Enum {
		allowed = append(allowed, scalarString(candidate))
	}
	if slices.Contains(allowed, got) {
		return nil
	}
	return fmt.Errorf("%q must be one of [%s], got %s", name, strings.Join(allowed, ", "), got)
}

// checkRange enforces `minimum` and `maximum` on numeric values.
func checkRange(name string, node *schemaNode, value any) error {
	if node.Minimum == nil && node.Maximum == nil {
		return nil
	}
	num, ok := value.(json.Number)
	if !ok {
		return nil // a non-number already failed checkType, or is untyped
	}
	f, err := num.Float64()
	if err != nil {
		return nil
	}
	if node.Minimum != nil && f < *node.Minimum {
		return fmt.Errorf("%q must be at least %s, got %s", name, formatBound(*node.Minimum), num.String())
	}
	if node.Maximum != nil && f > *node.Maximum {
		return fmt.Errorf("%q must be at most %s, got %s", name, formatBound(*node.Maximum), num.String())
	}
	return nil
}

// typeError renders a consistent "wrong type" message naming what arrived, so
// the model can see its own mistake.
func typeError(name, want string, got any) error {
	return fmt.Errorf("%q must be %s, got %s", name, want, describe(got))
}

// describe names the JSON type of a decoded value.
func describe(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("the string %q", v)
	case bool:
		return fmt.Sprintf("the boolean %t", v)
	case json.Number:
		return fmt.Sprintf("the number %s", v.String())
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return "an unexpected value"
	}
}

// scalarString renders a scalar for enum comparison and error text.
func scalarString(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return v
	case bool:
		return fmt.Sprintf("%t", v)
	case json.Number:
		return v.String()
	case float64:
		// Enum values come from our own schema, which is decoded without
		// UseNumber, so numeric literals land here.
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", v), "0"), ".")
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return "?"
		}
		return string(encoded)
	}
}

// formatBound renders a minimum/maximum without a trailing ".000000".
func formatBound(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// sortedKeys returns a map's keys in sorted order, for deterministic messages.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// CanonicalJSON renders arguments in a stable form so that two calls the model
// spelled differently — key order, whitespace, 10 versus 10.0 — produce the
// same string.
//
// This is the dedupe key for the agent loop. Without canonicalization the model
// re-issuing the same search with its keys in a different order would pay for
// the same Jira round trip twice, and the loop would burn iterations
// rediscovering what it already knows.
func CanonicalJSON(args json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "{}", nil
	}
	// Decoded into `any` (not json.Number), so 10 and 10.0 collapse to the same
	// canonical form. Marshal sorts object keys for us.
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", fmt.Errorf("canonicalize arguments: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("canonicalize arguments: %w", err)
	}
	return string(encoded), nil
}
