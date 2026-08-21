package notion

import (
	"encoding/json"
	"strings"
	"testing"
)

// TEST-3.1 — the Notion blocks → markdown converter.
//
// REQ-3.1 fixes what the converter owes the model: paragraphs, headings,
// bulleted and numbered lists, to-dos, quotes and code all render, nested
// content renders under its parent, and an unknown block type is *marked*
// rather than dropped. That last rule is the one worth the most: a plan whose
// unrecognized blocks vanish silently reads to the agent as a plan that never
// mentioned the thing it actually mentioned.
//
// The fixtures are recorded Notion block JSON, decoded through the production
// UnmarshalJSON so the type-named-payload lift is exercised too. Children are
// attached by the test helper because production fetches them separately
// (fetchBlocks), so they never arrive inside the parent's JSON.

// parseBlocks decodes a JSON array of blocks, recursively attaching any
// "children" array as the block's Children — the shape fetchBlocks assembles.
func parseBlocks(t *testing.T, data string) []block {
	t.Helper()

	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatalf("decode block fixture: %v", err)
	}

	blocks := make([]block, 0, len(raw))
	for _, item := range raw {
		var blk block
		if err := json.Unmarshal(item, &blk); err != nil {
			t.Fatalf("decode block: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			t.Fatalf("decode block fields: %v", err)
		}
		if children, ok := fields["children"]; ok {
			blk.Children = parseBlocks(t, string(children))
		}
		blocks = append(blocks, blk)
	}
	return blocks
}

func TestBlocksToMarkdown(t *testing.T) {
	tests := []struct {
		name   string
		blocks string
		// want is the exact expected markdown; used when wantContains is nil.
		want string
		// wantContains is used where the exact rendering is not fixed by the
		// spec but its content is.
		wantContains []string
		// wantMissing must not appear anywhere in the output.
		wantMissing []string
	}{
		{
			name: "paragraph",
			blocks: `[
			  {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Atlas Q2 ships on 2026-06-30."}]}}
			]`,
			want: "Atlas Q2 ships on 2026-06-30.",
		},
		{
			name: "headings at three levels",
			blocks: `[
			  {"type":"heading_1","heading_1":{"rich_text":[{"plain_text":"Atlas Q2 Plan"}]}},
			  {"type":"heading_2","heading_2":{"rich_text":[{"plain_text":"Goals"}]}},
			  {"type":"heading_3","heading_3":{"rich_text":[{"plain_text":"Payments"}]}}
			]`,
			want: "# Atlas Q2 Plan\n## Goals\n### Payments",
		},
		{
			name: "bulleted list",
			blocks: `[
			  {"type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"Ship refunds"}]}},
			  {"type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"Ship payouts"}]}}
			]`,
			want: "- Ship refunds\n- Ship payouts",
		},
		{
			name: "numbered list counts up",
			blocks: `[
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Scope"}]}},
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Build"}]}},
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Launch"}]}}
			]`,
			want: "1. Scope\n2. Build\n3. Launch",
		},
		{
			name: "second numbered list restarts at one",
			blocks: `[
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Scope"}]}},
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Build"}]}},
			  {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Risks"}]}},
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Vendor delay"}]}}
			]`,
			want: "1. Scope\n2. Build\nRisks\n1. Vendor delay",
		},
		{
			name: "to-dos carry their checked state",
			blocks: `[
			  {"type":"to_do","to_do":{"rich_text":[{"plain_text":"Sign vendor contract"}],"checked":true}},
			  {"type":"to_do","to_do":{"rich_text":[{"plain_text":"Get sandbox credentials"}],"checked":false}}
			]`,
			want: "- [x] Sign vendor contract\n- [ ] Get sandbox credentials",
		},
		{
			name: "quote",
			blocks: `[
			  {"type":"quote","quote":{"rich_text":[{"plain_text":"The vendor confirmed the slip."}]}}
			]`,
			want: "> The vendor confirmed the slip.",
		},
		{
			name: "code keeps its language fence",
			blocks: `[
			  {"type":"code","code":{"rich_text":[{"plain_text":"SELECT 1;"}],"language":"sql"}}
			]`,
			want: "```sql\nSELECT 1;\n```",
		},
		{
			name: "unsupported block type is marked, not dropped",
			blocks: `[
			  {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Before"}]}},
			  {"type":"video","video":{"external":{"url":"https://example.test/v.mp4"}}},
			  {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"After"}]}}
			]`,
			wantContains: []string{"Before", "[unsupported block: video]", "After"},
		},
		{
			name: "every unsupported block leaves its own marker",
			blocks: `[
			  {"type":"video","video":{}},
			  {"type":"pdf","pdf":{}},
			  {"type":"table_of_contents","table_of_contents":{}}
			]`,
			wantContains: []string{
				"[unsupported block: video]",
				"[unsupported block: pdf]",
				"[unsupported block: table_of_contents]",
			},
			wantMissing: []string{"paragraph"},
		},
		{
			name: "children render indented under their parent",
			blocks: `[
			  {"type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"Payments vendor"}]},
			   "children":[
			     {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Nordwind Payments"}]}}
			   ]}
			]`,
			want: "- Payments vendor\n  Nordwind Payments",
		},
		{
			name: "two levels of nesting indent cumulatively",
			blocks: `[
			  {"type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"Risks"}]},
			   "children":[
			     {"type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"Vendor"}]},
			      "children":[
			        {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Sandbox slipped to 2026-07-15"}]}}
			      ]}
			   ]}
			]`,
			want: "- Risks\n  - Vendor\n    Sandbox slipped to 2026-07-15",
		},
		{
			name: "children of an unsupported block still render",
			blocks: `[
			  {"type":"table","table":{},
			   "children":[
			     {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Nordwind Payments"}]}}
			   ]}
			]`,
			wantContains: []string{"[unsupported block: table]", "Nordwind Payments"},
		},
		{
			name: "nested numbered lists number independently of the parent list",
			blocks: `[
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Phase one"}]},
			   "children":[
			     {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Design"}]}},
			     {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Build"}]}}
			   ]},
			  {"type":"numbered_list_item","numbered_list_item":{"rich_text":[{"plain_text":"Phase two"}]}}
			]`,
			want: "1. Phase one\n  1. Design\n  2. Build\n2. Phase two",
		},
		{
			name: "empty spacer paragraphs do not stack up blank lines",
			blocks: `[
			  {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Goals"}]}},
			  {"type":"paragraph","paragraph":{"rich_text":[]}},
			  {"type":"paragraph","paragraph":{"rich_text":[]}},
			  {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Risks"}]}}
			]`,
			want: "Goals\nRisks",
		},
		{
			name:   "no blocks renders nothing",
			blocks: `[]`,
			want:   "",
		},
		{
			name: "mixed page renders every supported type in order",
			blocks: `[
			  {"type":"heading_1","heading_1":{"rich_text":[{"plain_text":"Atlas Q2 Plan"}]}},
			  {"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Payments vendor: Nordwind Payments."}]}},
			  {"type":"heading_2","heading_2":{"rich_text":[{"plain_text":"Deadlines"}]}},
			  {"type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"Refunds sandbox: 2026-06-15"}]}},
			  {"type":"to_do","to_do":{"rich_text":[{"plain_text":"Confirm date with vendor"}],"checked":false}},
			  {"type":"quote","quote":{"rich_text":[{"plain_text":"Original launch date is 2026-07-01."}]}}
			]`,
			want: "# Atlas Q2 Plan\n" +
				"Payments vendor: Nordwind Payments.\n" +
				"## Deadlines\n" +
				"- Refunds sandbox: 2026-06-15\n" +
				"- [ ] Confirm date with vendor\n" +
				"> Original launch date is 2026-07-01.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BlocksToMarkdown(parseBlocks(t, tc.blocks))

			if tc.wantContains == nil && tc.wantMissing == nil {
				if got != tc.want {
					t.Errorf("markdown mismatch\ngot:\n%s\nwant:\n%s", got, tc.want)
				}
				return
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("markdown is missing %q\ngot:\n%s", want, got)
				}
			}
			for _, missing := range tc.wantMissing {
				if strings.Contains(got, missing) {
					t.Errorf("markdown unexpectedly contains %q\ngot:\n%s", missing, got)
				}
			}
		})
	}
}

// An unsupported block must leave exactly one marker per block, so an agent
// reading the page can tell how much it did not see.
func TestBlocksToMarkdownMarksEveryUnsupportedBlock(t *testing.T) {
	blocks := parseBlocks(t, `[
	  {"type":"video","video":{}},
	  {"type":"pdf","pdf":{}},
	  {"type":"breadcrumb","breadcrumb":{}}
	]`)

	got := BlocksToMarkdown(blocks)
	if n := strings.Count(got, "[unsupported block"); n != 3 {
		t.Errorf("unsupported markers = %d, want 3 (one per skipped block)\ngot:\n%s", n, got)
	}
}

// The rich-text lift must survive multi-span text: Notion splits a sentence at
// every formatting boundary, so a vendor name in bold arrives as its own span
// and a converter that keeps only the first span loses it.
func TestBlocksToMarkdownJoinsRichTextSpans(t *testing.T) {
	blocks := parseBlocks(t, `[
	  {"type":"paragraph","paragraph":{"rich_text":[
	    {"plain_text":"Payments vendor is ","annotations":{}},
	    {"plain_text":"Nordwind Payments","annotations":{"bold":true}},
	    {"plain_text":" for Q2.","annotations":{}}
	  ]}}
	]`)

	got := BlocksToMarkdown(blocks)
	for _, want := range []string{"Payments vendor is ", "Nordwind Payments", " for Q2."} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown is missing span %q\ngot:\n%s", want, got)
		}
	}
}

// The payload lift is keyed on the block's own type name, which is the one
// piece of Notion's shape the converter has to understand. A block whose
// payload key does not match its type must not silently render as empty text.
func TestBlockUnmarshalLiftsTypeNamedPayload(t *testing.T) {
	var blk block
	if err := json.Unmarshal([]byte(`{
	  "object":"block",
	  "id":"1a2b3c4d-0000-0000-0000-00000000000a",
	  "type":"heading_2",
	  "has_children":true,
	  "heading_2":{"rich_text":[{"plain_text":"Deadlines"}]}
	}`), &blk); err != nil {
		t.Fatalf("decode block: %v", err)
	}

	if blk.ID != "1a2b3c4d-0000-0000-0000-00000000000a" {
		t.Errorf("id = %q, want the block id", blk.ID)
	}
	if blk.Type != "heading_2" {
		t.Errorf("type = %q, want heading_2", blk.Type)
	}
	if !blk.HasChildren {
		t.Error("has_children = false, want true (the fetcher relies on it to recurse)")
	}
	if len(blk.Content.RichText) != 1 || blk.Content.RichText[0].PlainText != "Deadlines" {
		t.Errorf("lifted rich text = %+v, want the heading_2 payload", blk.Content.RichText)
	}
}
