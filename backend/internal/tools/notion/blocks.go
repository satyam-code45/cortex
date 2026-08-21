package notion

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Notion blocks → markdown.
//
// Notion returns a page as a list of block objects, each with its payload under
// a key named for the block's own type. Handing that JSON to the model would
// spend most of the token budget on structure, so it is flattened to markdown
// here — markdown because the model already reads it fluently and because it
// survives being embedded in a tool observation without escaping.
//
// The governing rule is that content must never vanish silently. An unknown
// block type renders as a visible marker rather than nothing: an agent that
// reads a plan and sees no marker can trust that it saw the plan, and one that
// sees a marker knows to say so. Dropping unrecognized blocks is exactly how a
// system ends up confidently reporting that a document says nothing about the
// thing it actually says.

// UnmarshalJSON decodes a block, lifting the type-named payload into Content.
//
// Notion nests a block's content under a key matching its type — a paragraph's
// text lives at .paragraph.rich_text, a heading's at .heading_2.rich_text — so
// there is no single static field to decode into. Doing the lift here means
// every consumer sees one uniform shape.
func (b *block) UnmarshalJSON(data []byte) error {
	type blockAlias block // avoids recursing into this method
	var alias blockAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*b = block(alias)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if payload, ok := fields[b.Type]; ok {
		// A payload that does not fit textContent (a divider's empty object, an
		// unmodelled block's own shape) is not an error: the block still
		// renders, either as its marker or as itself with no text.
		_ = json.Unmarshal(payload, &b.Content)
	}
	return nil
}

// BlocksToMarkdown renders a block tree as markdown.
//
// Children are rendered indented under their parent. The caller controls how
// deep the tree goes by choosing how far to fetch — see fetchBlocks, which
// stops at depth 2.
func BlocksToMarkdown(blocks []block) string {
	var b strings.Builder
	renderBlocks(&b, blocks, "")
	return strings.TrimSpace(collapseBlankLines(b.String()))
}

// renderBlocks writes a sibling list, tracking numbering across consecutive
// numbered items.
//
// The counter resets on any non-numbered block, which is what makes two
// separate numbered lists in one document start at 1 each rather than the
// second continuing the first.
func renderBlocks(b *strings.Builder, blocks []block, indent string) {
	number := 0
	for _, blk := range blocks {
		if blk.Type == "numbered_list_item" {
			number++
		} else {
			number = 0
		}
		renderBlock(b, blk, indent, number)
	}
}

// renderBlock writes one block and its children.
func renderBlock(b *strings.Builder, blk block, indent string, number int) {
	text := renderRichText(blk.Content.RichText)

	switch blk.Type {
	case "paragraph":
		if strings.TrimSpace(text) != "" {
			writeLines(b, indent, text)
		}
	case "heading_1":
		writeLines(b, indent, "# "+text)
	case "heading_2":
		writeLines(b, indent, "## "+text)
	case "heading_3":
		writeLines(b, indent, "### "+text)
	case "bulleted_list_item", "toggle":
		writeLines(b, indent, "- "+text)
	case "numbered_list_item":
		writeLines(b, indent, fmt.Sprintf("%d. %s", max(number, 1), text))
	case "to_do":
		mark := " "
		if blk.Content.Checked {
			mark = "x"
		}
		writeLines(b, indent, fmt.Sprintf("- [%s] %s", mark, text))
	case "quote":
		writeLines(b, indent, "> "+text)
	case "callout":
		// Rendered as a quote: a callout is a paragraph with a coloured box and
		// an emoji, and the box is not a fact.
		writeLines(b, indent, "> "+text)
	case "code":
		language := blk.Content.Language
		writeLines(b, indent, "```"+language)
		writeLines(b, indent, text)
		writeLines(b, indent, "```")
	case "divider":
		writeLines(b, indent, "---")
	case "child_page":
		// Surfaced as a link rather than skipped: a child page is how the agent
		// discovers that a plan has sub-documents, and the id is what it needs
		// to call notion_get_page on one.
		writeLines(b, indent, fmt.Sprintf("- [sub-page] %s (id: %s)", blk.Content.Title, blk.ID))
	default:
		writeLines(b, indent, fmt.Sprintf("[unsupported block: %s]", blk.Type))
	}

	if len(blk.Children) > 0 {
		renderBlocks(b, blk.Children, indent+"  ")
	}
}

// writeLines writes text with the indent applied to every line, so a multi-line
// paragraph nested under a bullet stays nested.
func writeLines(b *strings.Builder, indent, text string) {
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// renderRichText renders a rich-text run as markdown, preserving the
// annotations that carry meaning.
//
// Bold and code are kept because they are load-bearing in the documents this
// reads: a plan writes the vendor name in bold and an identifier in code, and
// stripping that flattens the emphasis an author used to mark what matters.
func renderRichText(spans []richText) string {
	var b strings.Builder
	for _, span := range spans {
		text := span.PlainText
		if text == "" {
			continue
		}
		// Whitespace is moved outside the markers: "**bold **text" renders
		// wrong in every markdown implementation, and Notion produces trailing
		// spaces inside annotated spans routinely.
		leading := text[:len(text)-len(strings.TrimLeft(text, " \t"))]
		trailing := text[len(strings.TrimRight(text, " \t")):]
		core := strings.TrimSpace(text)
		if core == "" {
			b.WriteString(text)
			continue
		}

		annotations := span.Annotations
		if annotations.Code {
			core = "`" + core + "`"
		}
		if annotations.Bold {
			core = "**" + core + "**"
		}
		if annotations.Italic {
			core = "*" + core + "*"
		}
		if annotations.Strikethrough {
			core = "~~" + core + "~~"
		}
		if span.Href != "" {
			core = "[" + core + "](" + span.Href + ")"
		}
		b.WriteString(leading)
		b.WriteString(core)
		b.WriteString(trailing)
	}
	return b.String()
}

// collapseBlankLines squeezes runs of blank lines to one.
//
// Notion pages are full of empty paragraphs used as spacing, and each one is a
// token the model pays for on every iteration.
func collapseBlankLines(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	return strings.Join(out, "\n")
}
