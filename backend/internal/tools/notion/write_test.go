package notion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The write direction: markdown → Notion blocks.
//
// This is the code that produces every committed Notion fixture, and therefore
// the middle hop of the whole multi-hop story. A regression in splitInline or
// MarkdownToBlocks would not fail any other test — it would just quietly ship a
// plan page with the vendor's name rendered as literal asterisks, and the
// acceptance question would start failing for reasons nothing points at.

// TestMarkdownToBlocks covers each line form the subset accepts.
func TestMarkdownToBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		markdown string
		wantType string
		wantText string
		// wantExtra checks a type-level field, e.g. a to-do's checked flag.
		wantExtra map[string]any
	}{
		{name: "paragraph", markdown: "Nordwind slipped the sandbox.", wantType: "paragraph", wantText: "Nordwind slipped the sandbox."},
		{name: "heading 1", markdown: "# Goal", wantType: "heading_1", wantText: "Goal"},
		{name: "heading 2", markdown: "## Payments vendor", wantType: "heading_2", wantText: "Payments vendor"},
		{name: "heading 3", markdown: "### Nordwind contacts", wantType: "heading_3", wantText: "Nordwind contacts"},
		{name: "bulleted item", markdown: "- v3 refunds API", wantType: "bulleted_list_item", wantText: "v3 refunds API"},
		{name: "numbered item", markdown: "1. Metered usage billing engine", wantType: "numbered_list_item", wantText: "Metered usage billing engine"},
		{name: "numbered item past nine", markdown: "12. Twelfth", wantType: "numbered_list_item", wantText: "Twelfth"},
		{name: "quote", markdown: "> Anything Nordwind tells us arrives in email.", wantType: "quote", wantText: "Anything Nordwind tells us arrives in email."},
		{
			name: "unchecked to-do", markdown: "- [ ] Mirror vendor notices into the ticket",
			wantType: "to_do", wantText: "Mirror vendor notices into the ticket",
			wantExtra: map[string]any{"checked": false},
		},
		{
			name: "checked to-do", markdown: "- [x] Record Nordwind's escalation contact",
			wantType: "to_do", wantText: "Record Nordwind's escalation contact",
			wantExtra: map[string]any{"checked": true},
		},
		{
			name: "capital-X to-do is still checked", markdown: "- [X] Done",
			wantType: "to_do", wantText: "Done",
			wantExtra: map[string]any{"checked": true},
		},
		{name: "divider", markdown: "---", wantType: "divider"},
		{
			name:     "a numbered-looking line that is not a list stays a paragraph",
			markdown: "2026-06-08. Nordwind gave notice.",
			wantType: "paragraph", wantText: "2026-06-08. Nordwind gave notice.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blocks := markdownToBlocks(tc.markdown)
			if len(blocks) != 1 {
				t.Fatalf("markdownToBlocks(%q) produced %d blocks, want exactly 1: %#v",
					tc.markdown, len(blocks), blocks)
			}
			block := blocks[0]

			if got := block["type"]; got != tc.wantType {
				t.Errorf("block type = %v, want %q", got, tc.wantType)
			}
			if block["object"] != "block" {
				t.Errorf(`block object = %v, want "block"`, block["object"])
			}

			if tc.wantText != "" {
				payload, ok := block[tc.wantType].(map[string]any)
				if !ok {
					t.Fatalf("payload under %q is %T, want map", tc.wantType, block[tc.wantType])
				}
				spans, ok := payload["rich_text"].([]richTextPayload)
				if !ok {
					t.Fatalf("rich_text is %T, want []richTextPayload", payload["rich_text"])
				}
				var text strings.Builder
				for _, span := range spans {
					text.WriteString(span.Text.Content)
				}
				if text.String() != tc.wantText {
					t.Errorf("text = %q, want %q", text.String(), tc.wantText)
				}
			}

			for key, want := range tc.wantExtra {
				payload := block[tc.wantType].(map[string]any)
				if got := payload[key]; got != want {
					t.Errorf("payload[%q] = %v, want %v", key, got, want)
				}
			}
		})
	}
}

// TestMarkdownToBlocksCodeFence checks that a fence becomes one code block
// carrying every line, rather than one block per line.
func TestMarkdownToBlocksCodeFence(t *testing.T) {
	t.Parallel()

	blocks := markdownToBlocks("Before\n```sql\nSELECT 1;\nSELECT 2;\n```\nAfter")
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3 (paragraph, code, paragraph)", len(blocks))
	}
	if blocks[1]["type"] != "code" {
		t.Fatalf("second block is %v, want code", blocks[1]["type"])
	}

	code := blocks[1]["code"].(map[string]any)
	if code["language"] != "sql" {
		t.Errorf("language = %v, want sql", code["language"])
	}
	spans := code["rich_text"].([]richTextPayload)
	if len(spans) != 1 || spans[0].Text.Content != "SELECT 1;\nSELECT 2;" {
		t.Errorf("code content = %#v, want both statements in one span", spans)
	}
}

// TestSplitInline covers the three inline forms the subset parses, and the
// malformed cases that must degrade to literal text rather than swallow it.
func TestSplitInline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []inlineChunk
	}{
		{name: "plain", input: "no markup", want: []inlineChunk{{text: "no markup"}}},
		{
			name:  "bold in the middle",
			input: "vendor is **Nordwind Payments** as of March",
			want: []inlineChunk{
				{text: "vendor is "},
				{text: "Nordwind Payments", bold: true},
				{text: " as of March"},
			},
		},
		{
			name:  "code span",
			input: "set `GMAIL_QUERY_SCOPE` first",
			want: []inlineChunk{
				{text: "set "},
				{text: "GMAIL_QUERY_SCOPE", code: true},
				{text: " first"},
			},
		},
		{
			name:  "link",
			input: "see [the plan](https://notion.test/plan) for detail",
			want: []inlineChunk{
				{text: "see "},
				{text: "the plan", link: "https://notion.test/plan"},
				{text: " for detail"},
			},
		},
		{
			name:  "unterminated bold stays literal",
			input: "**Nordwind never closed",
			want:  []inlineChunk{{text: "**Nordwind never closed"}},
		},
		{
			name:  "unterminated code stays literal",
			input: "a ` backtick",
			want:  []inlineChunk{{text: "a ` backtick"}},
		},
		{
			name:  "bracket without a target stays literal",
			input: "an [aside] with no link",
			want:  []inlineChunk{{text: "an [aside] with no link"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := splitInline(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d chunks, want %d: %#v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("chunk %d = %#v, want %#v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestFixtureBodiesRoundTrip runs every committed fixture body through the
// writer and back through the reader.
//
// This is the assertion the package comment promises. It reads the real
// seeder/fixtures/notion.json rather than an inline sample, so that editing a
// fixture into a construct the writer does not support — a table, a nested
// bullet — fails here instead of silently producing a page that is missing the
// line the acceptance question depends on.
func TestFixtureBodiesRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "..", "seeder", "fixtures", "notion.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("committed fixtures not readable from here (%v); skipping", err)
	}

	var file struct {
		Pages []struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(file.Pages) == 0 {
		t.Fatalf("%s contains no pages", path)
	}

	for _, page := range file.Pages {
		t.Run(page.Title, func(t *testing.T) {
			t.Parallel()

			got := BlocksToMarkdown(decodeAsNotionWould(t, markdownToBlocks(page.Body)))

			// The writer drops blank lines (they are spacing, not content), so
			// the comparison is line-for-line over the non-blank lines. Every
			// one of them must survive, with its marker intact.
			wantLines := nonBlankLines(page.Body)
			gotLines := nonBlankLines(got)
			if len(gotLines) != len(wantLines) {
				t.Fatalf("round trip produced %d lines, want %d\n--- got ---\n%s\n--- want ---\n%s",
					len(gotLines), len(wantLines), strings.Join(gotLines, "\n"), strings.Join(wantLines, "\n"))
			}
			for i := range wantLines {
				if gotLines[i] != wantLines[i] {
					t.Errorf("line %d round-tripped to %q, want %q", i+1, gotLines[i], wantLines[i])
				}
			}
		})
	}
}

// decodeAsNotionWould turns write payloads into the read shape.
//
// Notion computes plain_text server-side from text.content, and lifts a link
// target to href; without that step a round trip through our own JSON would
// compare the writer against nothing. Doing it here keeps the test honest about
// which half it is exercising.
func decodeAsNotionWould(t *testing.T, payloads []blockPayload) []block {
	t.Helper()

	encoded, err := json.Marshal(payloads)
	if err != nil {
		t.Fatalf("marshal block payloads: %v", err)
	}
	var generic []map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("decode block payloads: %v", err)
	}

	for _, raw := range generic {
		blockType, _ := raw["type"].(string)
		payload, ok := raw[blockType].(map[string]any)
		if !ok {
			continue
		}
		spans, ok := payload["rich_text"].([]any)
		if !ok {
			continue
		}
		for _, span := range spans {
			item, ok := span.(map[string]any)
			if !ok {
				continue
			}
			text, ok := item["text"].(map[string]any)
			if !ok {
				continue
			}
			item["plain_text"] = text["content"]
			if link, ok := text["link"].(map[string]any); ok {
				item["href"] = link["url"]
			}
		}
	}

	rewritten, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("re-marshal blocks: %v", err)
	}
	var blocks []block
	if err := json.Unmarshal(rewritten, &blocks); err != nil {
		t.Fatalf("decode into blocks: %v", err)
	}
	return blocks
}

// nonBlankLines returns the trimmed, non-empty lines of text.
func nonBlankLines(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
