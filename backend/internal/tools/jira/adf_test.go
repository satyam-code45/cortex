package jira_test

import (
	"encoding/json"
	"strings"
	"testing"

	"cortex/internal/tools/jira"
)

// ADF conversion, both directions.
//
// ADF→text and text→ADF live in one file so the round trip is tested as
// a round trip: reads need the first (REST v3 returns descriptions and comment
// bodies as an ADF node tree) and the seeder's writes need the second (the same
// v3 endpoints reject a plain string).
//
// The failure mode that matters most is silent loss. An agent that reads a
// ticket whose content was dropped by the converter will confidently report that
// the ticket says nothing, so every case below is ultimately asking "did the
// text survive?".

func TestADFToText(t *testing.T) {
	tests := []struct {
		name string
		adf  string
		want string
	}{
		{
			name: "paragraphs",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[{"type":"text","text":"First line."}]},
				{"type":"paragraph","content":[{"type":"text","text":"Second line."}]}]}`,
			want: "First line.\nSecond line.",
		},
		{
			name: "empty paragraph is dropped",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[]},
				{"type":"paragraph","content":[{"type":"text","text":"Only line."}]}]}`,
			want: "Only line.",
		},
		{
			name: "heading keeps its level",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Problem"}]},
				{"type":"paragraph","content":[{"type":"text","text":"Body"}]}]}`,
			want: "## Problem\nBody",
		},
		{
			name: "bullet list",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"bulletList","content":[
					{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]}]},
					{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}]}]}`,
			want: "- one\n- two",
		},
		{
			name: "ordered list is numbered",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"orderedList","content":[
					{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]}]},
					{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}]}]}`,
			want: "1. first\n2. second",
		},
		{
			name: "blockquote",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"quoted"}]}]}]}`,
			want: "> quoted",
		},
		{
			name: "code block",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"codeBlock","content":[{"type":"text","text":"SELECT 1;"}]}]}`,
			want: "```\nSELECT 1;\n```",
		},
		{
			name: "hard break splits the line",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[
					{"type":"text","text":"before"},{"type":"hardBreak"},{"type":"text","text":"after"}]}]}`,
			want: "before\nafter",
		},
		{
			name: "mention renders the display name",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[
					{"type":"text","text":"owner: "},
					{"type":"mention","attrs":{"id":"5b10a2","text":"@Priya Raman"}}]}]}`,
			want: "owner: @Priya Raman",
		},
		{
			name: "mention without a display name falls back to the id",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[{"type":"mention","attrs":{"id":"5b10a2"}}]}]}`,
			want: "@5b10a2",
		},
		{
			name: "link keeps its href",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[
					{"type":"text","text":"runbook","marks":[{"type":"link","attrs":{"href":"https://wiki/x"}}]}]}]}`,
			want: "runbook (https://wiki/x)",
		},
		{
			name: "link whose text is already the href is not doubled",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[
					{"type":"text","text":"https://wiki/x","marks":[{"type":"link","attrs":{"href":"https://wiki/x"}}]}]}]}`,
			want: "https://wiki/x",
		},
		{
			name: "bold and italic marks cost no tokens",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[
					{"type":"text","text":"urgent","marks":[{"type":"strong"},{"type":"em"}]}]}]}`,
			want: "urgent",
		},
		{
			name: "status lozenge",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[{"type":"status","attrs":{"text":"Blocked"}}]}]}`,
			want: "[Blocked]",
		},
		{
			name: "inline card renders its url",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[{"type":"inlineCard","attrs":{"url":"https://jira/ATLAS-1"}}]}]}`,
			want: "https://jira/ATLAS-1",
		},
		{
			name: "table rows",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"table","content":[
					{"type":"tableRow","content":[
						{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"text","text":"owner"}]}]},
						{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"text","text":"date"}]}]}]}]}]}`,
			want: "owner | date",
		},
		{
			name: "attachment is named, not silently dropped",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"media","attrs":{"alt":"burndown.png"}}]}`,
			want: "[attachment: burndown.png]",
		},
		{
			name: "rule",
			adf:  `{"type":"doc","version":1,"content":[{"type":"rule"}]}`,
			want: "---",
		},
		{
			name: "unknown block type is descended into, not skipped",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"someFuturePanel","content":[
					{"type":"paragraph","content":[{"type":"text","text":"do not lose me"}]}]}]}`,
			want: "do not lose me",
		},
		{
			name: "unknown inline type is descended into",
			adf: `{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[
					{"type":"someFutureInline","content":[{"type":"text","text":"kept"}]}]}]}`,
			want: "kept",
		},
		{
			name: "a bare string body is accepted",
			adf:  `"plain v2 text"`,
			want: "plain v2 text",
		},
		{name: "null", adf: `null`, want: ""},
		{name: "empty", adf: ``, want: ""},
		{name: "not valid JSON", adf: `{"type":`, want: ""},
		{name: "empty document", adf: `{"type":"doc","version":1,"content":[]}`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jira.ADFToText(json.RawMessage(tt.adf))
			if got != tt.want {
				t.Errorf("ADFToText:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// A realistic description survives whole: every piece of text in the tree comes
// out, and no structural noise comes out with it.
func TestADFToTextKeepsEveryTextNode(t *testing.T) {
	raw := readFixture(t, "issue_atlas_101.json")
	var issue struct {
		Fields struct {
			Description json.RawMessage `json:"description"`
		} `json:"fields"`
	}
	if err := json.Unmarshal([]byte(raw), &issue); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	text := jira.ADFToText(issue.Fields.Description)
	for _, want := range []string{
		"Problem",
		"Checkout returns a 500 when the stored token has expired.",
		"@Priya Raman",
		"Blocked on the vendor sandbox (see ATLAS-102)",
		"Runbook",
		"https://wiki.example.com/runbook",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text lost %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{"listItem", "bulletList", "\"type\"", "attrs"} {
		if strings.Contains(text, leak) {
			t.Errorf("rendered text leaks ADF structure %q:\n%s", leak, text)
		}
	}
}

// TextToADF is what makes the seeder possible: v3 rejects a plain string for a
// description or a comment body.
func TestTextToADF(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		wantParagraphs []string
	}{
		{name: "single line", text: "Blocked on the vendor sandbox.", wantParagraphs: []string{"Blocked on the vendor sandbox."}},
		{name: "two lines", text: "First.\nSecond.", wantParagraphs: []string{"First.", "Second."}},
		{
			name:           "blank lines are dropped, not represented",
			text:           "First.\n\n\nSecond.\n",
			wantParagraphs: []string{"First.", "Second."},
		},
		{name: "windows line endings", text: "First.\r\nSecond.", wantParagraphs: []string{"First.", "Second."}},
		{name: "surrounding whitespace is trimmed", text: "   padded   ", wantParagraphs: []string{"padded"}},
		{name: "empty text produces an empty document", text: "", wantParagraphs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc struct {
				Type    string `json:"type"`
				Version int    `json:"version"`
				Content []struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"content"`
			}
			encoded := jira.TextToADF(tt.text)
			if err := json.Unmarshal(encoded, &doc); err != nil {
				t.Fatalf("TextToADF produced invalid JSON (%s): %v", encoded, err)
			}
			if doc.Type != "doc" || doc.Version != 1 {
				t.Errorf("document root = (%q, v%d), want (doc, v1)", doc.Type, doc.Version)
			}
			if len(doc.Content) != len(tt.wantParagraphs) {
				t.Fatalf("paragraphs = %d, want %d (%s)", len(doc.Content), len(tt.wantParagraphs), encoded)
			}
			for i, want := range tt.wantParagraphs {
				block := doc.Content[i]
				if block.Type != "paragraph" {
					t.Errorf("content[%d].type = %q, want paragraph", i, block.Type)
				}
				if len(block.Content) != 1 || block.Content[0].Type != "text" {
					t.Fatalf("content[%d] is not a single text node: %s", i, encoded)
				}
				if block.Content[0].Text != want {
					t.Errorf("content[%d] text = %q, want %q", i, block.Content[0].Text, want)
				}
			}
		})
	}
}

// The round trip is the real contract: what the seeder writes is what the tools
// read back, so text that survives one direction must survive both.
func TestADFRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "one line", text: "ATLAS-101 is blocked on the vendor sandbox.", want: "ATLAS-101 is blocked on the vendor sandbox."},
		{name: "several lines", text: "Owner: Priya Raman\nBlocked by: ATLAS-102\nDue: 2026-07-31",
			want: "Owner: Priya Raman\nBlocked by: ATLAS-102\nDue: 2026-07-31"},
		{name: "blank lines collapse", text: "Summary\n\nDetail", want: "Summary\nDetail"},
		{name: "non-ascii survives", text: "Dépendance externe — 待機中", want: "Dépendance externe — 待機中"},
		{name: "json-ish text is escaped, not interpreted", text: `{"status":"Blocked"}`, want: `{"status":"Blocked"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jira.ADFToText(jira.TextToADF(tt.text))
			if got != tt.want {
				t.Errorf("round trip:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}
