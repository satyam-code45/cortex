package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"cortex/internal/tools"
)

// maxDetail caps one step's text; the full session stays on the runner's disk.
const maxDetail = 2048

// streamLine is the subset of Claude Code's stream-json output the runner
// reads. Shapes measured against Claude Code 2.1.283.
type streamLine struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         *struct {
		Content []contentBlock `json:"content"`
	} `json:"message"`

	// Set on the final "result" line only.
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	SessionID    string  `json:"session_id"`
	Result       string  `json:"result"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// runResult is what the final line reported, plus what the stream showed.
type runResult struct {
	Subtype      string
	IsError      bool
	NumTurns     int
	DurationMS   int64
	TotalCostUSD float64
	SessionID    string
	Text         string
	Subagents    []string
	// Seen is false when the process ended without a result line.
	Seen bool
}

// readStream turns stdout into steps and returns the final result. root is
// stripped from file paths so steps name repo-relative files.
func readStream(r io.Reader, root string, emit func(tools.Step)) (runResult, error) {
	var res runResult
	// Line by line rather than json.Decoder: a stray non-JSON line (a warning
	// on stdout) is skipped, where a decoder would stop reading and leave
	// claude blocked on a full pipe until the timeout.
	br := bufio.NewReader(r)
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return res, fmt.Errorf("claude_code: read stream: %w", err)
		}
		var line streamLine
		if json.Unmarshal(bytes.TrimSpace(raw), &line) == nil {
			handleLine(line, &res, root, emit)
		}
		if err != nil {
			return res, nil
		}
	}
}

func handleLine(line streamLine, res *runResult, root string, emit func(tools.Step)) {
	parent := ""
	if line.ParentToolUseID != nil {
		parent = *line.ParentToolUseID
	}
	switch line.Type {
	case "result":
		res.Seen = true
		res.Subtype = line.Subtype
		res.IsError = line.IsError
		res.NumTurns = line.NumTurns
		res.DurationMS = line.DurationMS
		res.TotalCostUSD = line.TotalCostUSD
		res.SessionID = line.SessionID
		res.Text = line.Result
	case "assistant", "user":
		if line.Message == nil {
			return
		}
		for _, b := range line.Message.Content {
			step, ok := toStep(b, parent, root)
			if !ok {
				continue
			}
			if step.Kind == "tool_use" && (step.Tool == "Agent" || step.Tool == "Task") {
				if kind := subagentType(b.Input); kind != "" && !slices.Contains(res.Subagents, kind) {
					res.Subagents = append(res.Subagents, kind)
				}
			}
			emit(step)
		}
	}
}

func toStep(b contentBlock, parent, root string) (tools.Step, bool) {
	switch b.Type {
	case "tool_use":
		detail, cut := clip(relPaths(describeInput(b.Name, b.Input), root))
		return tools.Step{ID: b.ID, ParentID: parent, Kind: "tool_use", Tool: b.Name,
			Detail: detail, Truncated: cut}, true
	case "tool_result":
		detail, cut := clip(relPaths(resultText(b.Content), root))
		return tools.Step{ID: b.ToolUseID + ":result", ParentID: b.ToolUseID, Kind: "tool_result",
			Detail: detail, Truncated: cut, IsError: b.IsError}, true
	case "text":
		if strings.TrimSpace(b.Text) == "" {
			return tools.Step{}, false
		}
		detail, cut := clip(b.Text)
		return tools.Step{ParentID: parent, Kind: "text", Detail: detail, Truncated: cut}, true
	}
	// thinking blocks and anything newer are not shown.
	return tools.Step{}, false
}

// describeInput names what a tool call touched, the way a person would.
func describeInput(tool string, input json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch tool {
	case "Read", "Edit", "Write", "NotebookEdit":
		return str("file_path")
	case "Grep", "Glob":
		if p := str("path"); p != "" {
			return str("pattern") + " in " + p
		}
		return str("pattern")
	case "Bash":
		return str("command")
	case "Agent", "Task":
		return strings.TrimSpace(str("subagent_type") + ": " + str("description"))
	}
	return string(input)
}

func subagentType(input json.RawMessage) string {
	var in struct {
		SubagentType string `json:"subagent_type"`
	}
	_ = json.Unmarshal(input, &in)
	return in.SubagentType
}

// resultText flattens tool_result content, which is a string or a block list.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func relPaths(s, root string) string {
	if root == "" {
		return s
	}
	root = strings.TrimSuffix(root, "/")
	return strings.ReplaceAll(strings.ReplaceAll(s, root+"/", ""), root, ".")
}

// clip cuts s to maxDetail bytes without splitting a rune.
func clip(s string) (string, bool) {
	if len(s) <= maxDetail {
		return s, false
	}
	cut := maxDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
