package jira

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Atlassian Document Format conversion, both directions.
//
// Reads need ADF→text: REST v3 returns issue descriptions and comment bodies as
// an ADF node tree, and handing that tree to the model would spend most of the
// token budget on structural noise.
//
// Writes need text→ADF: the same v3 endpoints *reject* a plain string for those
// fields, so the seeder cannot post a description or a comment without building
// a document. Keeping both in one file means the round trip is tested as a
// round trip.

// adfNode is one node of an ADF document. The format is a recursive tree where
// leaves carry Text and containers carry Content.
type adfNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Content []adfNode      `json:"content"`
	Marks   []adfMark      `json:"marks"`
}

// adfMark is inline formatting applied to a text node.
type adfMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

// ADFToText renders an ADF document as plain text.
//
// Unknown node types are descended into rather than skipped: Atlassian adds
// node types over time, and silently dropping an unrecognized container would
// lose real content — the exact failure that makes an agent confidently claim
// a ticket says nothing.
func ADFToText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}

	// Some endpoints (and API v2 responses) return a bare string where v3
	// returns a document. Accept both so a mixed response cannot panic.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}

	var doc adfNode
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}

	var blocks []string
	for _, child := range doc.Content {
		blocks = append(blocks, renderBlock(child, "")...)
	}
	return strings.TrimSpace(strings.Join(blocks, "\n"))
}

// renderBlock renders one block-level node as zero or more output lines, with
// prefix applied to each (used for list markers and quote bars).
func renderBlock(n adfNode, prefix string) []string {
	switch n.Type {
	case "paragraph":
		text := renderInline(n.Content)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return prefixLines(text, prefix)

	case "heading":
		text := renderInline(n.Content)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		// Markdown-style hashes: cheap, and the model reads them as structure.
		level := 1
		if raw, ok := n.Attrs["level"]; ok {
			if f, ok := raw.(float64); ok && f >= 1 && f <= 6 {
				level = int(f)
			}
		}
		return prefixLines(strings.Repeat("#", level)+" "+text, prefix)

	case "bulletList", "orderedList":
		var out []string
		for i, item := range n.Content {
			marker := "- "
			if n.Type == "orderedList" {
				marker = fmt.Sprintf("%d. ", i+1)
			}
			// The marker goes on the item's first line; continuation lines are
			// indented to match so nested content stays readable.
			itemLines := renderBlocks(item.Content, "")
			for j, line := range itemLines {
				if j == 0 {
					out = append(out, prefix+marker+line)
					continue
				}
				out = append(out, prefix+strings.Repeat(" ", len(marker))+line)
			}
		}
		return out

	case "listItem":
		return renderBlocks(n.Content, prefix)

	case "blockquote":
		return renderBlocks(n.Content, prefix+"> ")

	case "codeBlock":
		var code strings.Builder
		for _, child := range n.Content {
			code.WriteString(child.Text)
		}
		body := strings.TrimRight(code.String(), "\n")
		if body == "" {
			return nil
		}
		lines := []string{prefix + "```"}
		lines = append(lines, prefixLines(body, prefix)...)
		return append(lines, prefix+"```")

	case "rule":
		return []string{prefix + "---"}

	case "table":
		var out []string
		for _, row := range n.Content {
			var cells []string
			for _, cell := range row.Content {
				cells = append(cells, strings.Join(renderBlocks(cell.Content, ""), " "))
			}
			if len(cells) > 0 {
				out = append(out, prefix+strings.Join(cells, " | "))
			}
		}
		return out

	case "mediaSingle", "mediaGroup", "media":
		// Attachments carry no text the agent can reason over; name them so the
		// model knows something exists rather than seeing a gap.
		if alt := attrString(n.Attrs, "alt"); alt != "" {
			return []string{prefix + "[attachment: " + alt + "]"}
		}
		return []string{prefix + "[attachment]"}

	case "":
		return nil

	default:
		// Unknown block: descend. If it turns out to be inline, renderBlocks
		// returns nothing and the inline path below picks it up.
		if lines := renderBlocks(n.Content, prefix); len(lines) > 0 {
			return lines
		}
		if text := renderInline([]adfNode{n}); strings.TrimSpace(text) != "" {
			return prefixLines(text, prefix)
		}
		return nil
	}
}

// renderBlocks renders a slice of block nodes.
func renderBlocks(nodes []adfNode, prefix string) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, renderBlock(n, prefix)...)
	}
	return out
}

// renderInline renders inline nodes into a single line of text.
func renderInline(nodes []adfNode) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text":
			b.WriteString(applyMarks(n.Text, n.Marks))
		case "hardBreak":
			b.WriteString("\n")
		case "mention":
			// attrs.text is the display form, e.g. "@Priya Raman".
			if text := attrString(n.Attrs, "text"); text != "" {
				b.WriteString(text)
			} else if id := attrString(n.Attrs, "id"); id != "" {
				b.WriteString("@")
				b.WriteString(id)
			}
		case "emoji":
			if text := attrString(n.Attrs, "text"); text != "" {
				b.WriteString(text)
			} else {
				b.WriteString(attrString(n.Attrs, "shortName"))
			}
		case "date":
			b.WriteString(attrString(n.Attrs, "timestamp"))
		case "status":
			if text := attrString(n.Attrs, "text"); text != "" {
				b.WriteString("[")
				b.WriteString(text)
				b.WriteString("]")
			}
		case "inlineCard", "blockCard", "embedCard":
			b.WriteString(attrString(n.Attrs, "url"))
		default:
			// Unknown inline node: recurse so nested text survives.
			b.WriteString(renderInline(n.Content))
		}
	}
	return b.String()
}

// applyMarks renders a text node's inline formatting. Only link is materialized
// — bold and italic carry no meaning for a model reading plain text, and the
// markup would just cost tokens.
func applyMarks(text string, marks []adfMark) string {
	for _, mark := range marks {
		if mark.Type != "link" {
			continue
		}
		href := attrString(mark.Attrs, "href")
		if href == "" || href == text {
			continue
		}
		return fmt.Sprintf("%s (%s)", text, href)
	}
	return text
}

// prefixLines applies prefix to every line of a possibly multi-line string.
func prefixLines(text, prefix string) []string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, prefix+line)
	}
	return out
}

// attrString reads a string attribute, tolerating a missing or non-string one.
func attrString(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	if v, ok := attrs[key].(string); ok {
		return v
	}
	return ""
}

// TextToADF builds a minimal ADF document from plain text, one paragraph per
// non-empty line.
//
// Required for writes: REST v3 rejects a plain string for `description` and
// comment `body`, so the seeder has to send a document even for a single
// sentence.
func TextToADF(text string) json.RawMessage {
	type inline struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type block struct {
		Type    string   `json:"type"`
		Content []inline `json:"content"`
	}
	doc := struct {
		Type    string  `json:"type"`
		Version int     `json:"version"`
		Content []block `json:"content"`
	}{Type: "doc", Version: 1, Content: []block{}}

	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			// An empty paragraph is invalid ADF, so blank lines are dropped
			// rather than represented.
			continue
		}
		doc.Content = append(doc.Content, block{
			Type:    "paragraph",
			Content: []inline{{Type: "text", Text: trimmed}},
		})
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		// The document is built from plain strings, so this is unreachable.
		return json.RawMessage(`{"type":"doc","version":1,"content":[]}`)
	}
	return encoded
}
