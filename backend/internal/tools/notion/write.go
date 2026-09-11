package notion

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"cortex/internal/tools/httpx"
)

// Page authoring.
//
// Two callers: the seeder, which writes the demo fixtures, and the execution job
// that performs a page write a human approved. Nothing a model can invoke
// reaches this file directly — the agent's write tools propose, and a person
// decides.
//
// This is the inverse of blocks.go, and it lives in the same package on purpose:
// the fixtures are written as markdown, uploaded as Notion blocks, and read back
// as markdown by notion_get_page, so the round trip is only trustworthy if both
// halves are tested together.
//
// The markdown accepted here is a deliberate subset — headings, bullets,
// numbered items, to-dos, quotes, code fences, dividers, paragraphs, and inline
// bold/code/links. A fixture that reaches for anything else silently loses it,
// so the subset is kept small enough to hold in your head.
//
// The round trip is lossy in three known ways, all of which today's fixtures
// avoid: BlocksToMarkdown emits *italic* and ~~strikethrough~~, which
// splitInline does not parse back, and it indents children two spaces, which
// MarkdownToBlocks strips. Nesting a fixture bullet therefore flattens it. If a
// fixture ever needs those, teach this parser first.

const (
	// maxBlocksPerAppend is Notion's limit on children per request.
	maxBlocksPerAppend = 100
	// maxRichTextRunes is Notion's limit on one text span. Longer paragraphs
	// are split across spans rather than rejected.
	maxRichTextRunes = 2000
)

// richTextPayload is one span in a create/append request.
type richTextPayload struct {
	Type string `json:"type"`
	Text struct {
		Content string   `json:"content"`
		Link    *linkRef `json:"link,omitempty"`
	} `json:"text"`
	Annotations *annotationPayload `json:"annotations,omitempty"`
}

type linkRef struct {
	URL string `json:"url"`
}

type annotationPayload struct {
	Bold bool `json:"bold,omitempty"`
	Code bool `json:"code,omitempty"`
}

// blockPayload is one block in a create/append request. The content sits under
// a type-named key, so it is built as a map.
type blockPayload map[string]any

// CreatePage creates a child page under parentID with the given title and
// markdown body, and returns the new page's id and URL.
func (c *Client) CreatePage(ctx context.Context, parentID, title, markdown string) (id, pageURL string, err error) {
	parent, err := normalizeID(parentID)
	if err != nil {
		return "", "", err
	}

	blocks := markdownToBlocks(markdown)
	first := blocks
	var rest []blockPayload
	if len(blocks) > maxBlocksPerAppend {
		first, rest = blocks[:maxBlocksPerAppend], blocks[maxBlocksPerAppend:]
	}

	body := map[string]any{
		"parent": map[string]string{"page_id": parent},
		"properties": map[string]any{
			"title": map[string]any{"title": textSpans(title)},
		},
		"children": first,
	}

	var created page
	if err := c.post(ctx, "/v1/pages", body, &created); err != nil {
		return "", "", err
	}
	if len(rest) > 0 {
		if err := c.appendBlocks(ctx, created.ID, rest); err != nil {
			return created.ID, created.URL, err
		}
	}
	return created.ID, created.URL, nil
}

// appendBlocks appends blocks to a page or block, chunked to Notion's limit.
func (c *Client) appendBlocks(ctx context.Context, blockID string, blocks []blockPayload) error {
	id, err := normalizeID(blockID)
	if err != nil {
		return err
	}
	for start := 0; start < len(blocks); start += maxBlocksPerAppend {
		chunk := blocks[start:min(start+maxBlocksPerAppend, len(blocks))]
		body := map[string]any{"children": chunk}
		if err := c.patch(ctx, "/v1/blocks/"+url.PathEscape(id)+"/children", body, nil); err != nil {
			return fmt.Errorf("append blocks to %s: %w", id, err)
		}
	}
	return nil
}

// ReplacePageContent trashes a page's existing blocks and writes the markdown
// in their place, leaving the page id and URL — and therefore any link to it —
// intact.
//
// Re-running the seeder after editing a fixture has to converge on the fixture,
// not append a second copy of it. Trashing rather than deleting matches what
// Notion's own UI does, so a mis-seed is recoverable from the workspace trash.
func (c *Client) ReplacePageContent(ctx context.Context, pageID, markdown string) error {
	id, err := normalizeID(pageID)
	if err != nil {
		return err
	}

	existing, truncated, err := c.fetchBlocks(ctx, id, maxBlockDepth)
	if err != nil {
		return err
	}
	if truncated {
		// Half-replacing is the one outcome worse than not replacing: the page
		// would end up neither the old content nor the fixture, and a re-run
		// would not converge either.
		return fmt.Errorf("notion: page %s has more than %d top-level blocks, so replacing its "+
			"contents would leave it half-old and half-new; clear it by hand first", id, maxBlocksPerPage)
	}
	for _, blk := range existing {
		if err := c.deleteBlock(ctx, blk.ID); err != nil {
			return err
		}
	}
	return c.appendBlocks(ctx, id, markdownToBlocks(markdown))
}

// FindChildPage returns the id of a direct child page with the given title, and
// whether one was found.
func (c *Client) FindChildPage(ctx context.Context, parentID, title string) (string, bool, error) {
	parent, err := normalizeID(parentID)
	if err != nil {
		return "", false, err
	}
	blocks, _, err := c.fetchBlocks(ctx, parent, maxBlockDepth)
	if err != nil {
		return "", false, err
	}
	for _, blk := range blocks {
		if blk.Type == "child_page" && strings.EqualFold(strings.TrimSpace(blk.Content.Title), strings.TrimSpace(title)) {
			return blk.ID, true, nil
		}
	}
	return "", false, nil
}

// PageTitle reads one page's title, for the seeder's plan output.
func (c *Client) PageTitle(ctx context.Context, pageID string) (string, error) {
	id, err := normalizeID(pageID)
	if err != nil {
		return "", err
	}
	var p page
	if err := c.get(ctx, "/v1/pages/"+url.PathEscape(id), nil, &p); err != nil {
		return "", err
	}
	return p.title(), nil
}

// Bot describes the integration a token authenticates as. WorkspaceName may
// be empty — Notion only reports it for workspace-owned integrations.
type Bot struct {
	Name          string
	WorkspaceName string
}

// CurrentBot returns the integration the token authenticates as, via
// GET /v1/users/me. It doubles as the live credential check for the
// paste-a-key connection flow: a bad token surfaces here as an
// APIError before anything is stored.
func (c *Client) CurrentBot(ctx context.Context) (Bot, error) {
	var me struct {
		Name string `json:"name"`
		Bot  struct {
			WorkspaceName string `json:"workspace_name"`
		} `json:"bot"`
	}
	if err := c.get(ctx, "/v1/users/me", nil, &me); err != nil {
		return Bot{}, fmt.Errorf("get current bot: %w", err)
	}
	return Bot{Name: me.Name, WorkspaceName: me.Bot.WorkspaceName}, nil
}

// SharedPage is one page the integration can see.
type SharedPage struct {
	ID    string
	Title string
	// ParentPageID is the page this one sits under, empty when it is not
	// parented by another page.
	ParentPageID string
}

// SharedPages returns every page the integration can see, newest first. The
// seeder uses it to default the parent page when none is configured.
func (c *Client) SharedPages(ctx context.Context, limit int) ([]SharedPage, error) {
	pages, err := c.searchPages(ctx, "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]SharedPage, 0, len(pages))
	for _, p := range pages {
		out = append(out, SharedPage{
			ID:           p.ID,
			Title:        p.title(),
			ParentPageID: normalizedParentID(p),
		})
	}
	return out, nil
}

// RootSharedPages returns the shared pages that are not themselves children of
// another shared page.
//
// This is what keeps auto-discovery working after the first seed. Sharing a
// page with an integration also exposes everything under it, so once the
// fixtures exist, "the pages this integration can see" is the parent plus five
// children — and picking between them by hand every time would be a poor trade
// for a command whose whole point is being re-runnable.
func RootSharedPages(pages []SharedPage) []SharedPage {
	shared := make(map[string]bool, len(pages))
	for _, p := range pages {
		shared[p.ID] = true
	}
	var roots []SharedPage
	for _, p := range pages {
		if p.ParentPageID == "" || !shared[p.ParentPageID] {
			roots = append(roots, p)
		}
	}
	return roots
}

// normalizedParentID returns a page's parent page id in the dashed form the API
// reports elsewhere, so the two can be compared.
func normalizedParentID(p page) string {
	if p.Parent.Type != "page_id" || p.Parent.PageID == "" {
		return ""
	}
	normalized, err := normalizeID(p.Parent.PageID)
	if err != nil {
		return p.Parent.PageID
	}
	return normalized
}

// deleteBlock moves a block to the workspace trash.
func (c *Client) deleteBlock(ctx context.Context, blockID string) error {
	id, err := normalizeID(blockID)
	if err != nil {
		return err
	}
	return c.http.Do(ctx, httpx.Request{Method: http.MethodDelete, Path: "/v1/blocks/" + url.PathEscape(id)}, nil)
}

// BlockCount reports how many Notion blocks a markdown body will produce. The
// seeder's plan output uses it; the blocks themselves are an internal shape.
func BlockCount(markdown string) int { return len(markdownToBlocks(markdown)) }

// markdownToBlocks converts the supported markdown subset into Notion blocks.
func markdownToBlocks(markdown string) []blockPayload {
	var (
		blocks    []blockPayload
		inCode    bool
		codeLines []string
		codeLang  string
	)

	for _, line := range strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimRight(line, " \t")

		if strings.HasPrefix(strings.TrimSpace(trimmed), "```") {
			if inCode {
				blocks = append(blocks, codeBlock(strings.Join(codeLines, "\n"), codeLang))
				inCode, codeLines, codeLang = false, nil, ""
			} else {
				inCode = true
				codeLang = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trimmed), "```"))
			}
			continue
		}
		if inCode {
			codeLines = append(codeLines, line)
			continue
		}

		content := strings.TrimSpace(trimmed)
		if content == "" {
			continue
		}

		switch {
		case content == "---":
			blocks = append(blocks, blockPayload{"object": "block", "type": "divider", "divider": map[string]any{}})
		case strings.HasPrefix(content, "### "):
			blocks = append(blocks, textBlock("heading_3", strings.TrimPrefix(content, "### "), nil))
		case strings.HasPrefix(content, "## "):
			blocks = append(blocks, textBlock("heading_2", strings.TrimPrefix(content, "## "), nil))
		case strings.HasPrefix(content, "# "):
			blocks = append(blocks, textBlock("heading_1", strings.TrimPrefix(content, "# "), nil))
		case strings.HasPrefix(content, "> "):
			blocks = append(blocks, textBlock("quote", strings.TrimPrefix(content, "> "), nil))
		case strings.HasPrefix(content, "- [ ] "):
			blocks = append(blocks, textBlock("to_do", strings.TrimPrefix(content, "- [ ] "), map[string]any{"checked": false}))
		case strings.HasPrefix(content, "- [x] "), strings.HasPrefix(content, "- [X] "):
			blocks = append(blocks, textBlock("to_do", content[6:], map[string]any{"checked": true}))
		case strings.HasPrefix(content, "- "):
			blocks = append(blocks, textBlock("bulleted_list_item", strings.TrimPrefix(content, "- "), nil))
		default:
			if rest, ok := numberedItem(content); ok {
				blocks = append(blocks, textBlock("numbered_list_item", rest, nil))
				continue
			}
			blocks = append(blocks, textBlock("paragraph", content, nil))
		}
	}

	// An unterminated fence still has content worth keeping.
	if inCode && len(codeLines) > 0 {
		blocks = append(blocks, codeBlock(strings.Join(codeLines, "\n"), codeLang))
	}
	return blocks
}

// numberedItem splits "12. text" into its text, reporting whether it matched.
func numberedItem(content string) (string, bool) {
	dot := strings.Index(content, ". ")
	if dot <= 0 {
		return "", false
	}
	for _, r := range content[:dot] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return content[dot+2:], true
}

// textBlock builds a block of the given type carrying rich text, plus any extra
// type-level fields (a to-do's checked flag).
func textBlock(blockType, text string, extra map[string]any) blockPayload {
	payload := map[string]any{"rich_text": textSpans(text)}
	for k, v := range extra {
		payload[k] = v
	}
	return blockPayload{"object": "block", "type": blockType, blockType: payload}
}

// codeBlock builds a code block. Notion rejects an unknown language, so
// anything unrecognized falls back to plain text.
func codeBlock(code, language string) blockPayload {
	if language == "" {
		language = "plain text"
	}
	return blockPayload{
		"object": "block",
		"type":   "code",
		"code": map[string]any{
			"rich_text": textSpans(code),
			"language":  language,
		},
	}
}

// textSpans converts inline markdown into Notion rich-text spans.
//
// Only **bold**, `code`, and [text](url) are recognized — the three the
// fixtures use. Anything else passes through as literal characters, which is
// the honest failure mode: the text still arrives, it just is not styled.
func textSpans(text string) []richTextPayload {
	var spans []richTextPayload
	for _, chunk := range splitInline(text) {
		for _, part := range chunkRunes(chunk.text, maxRichTextRunes) {
			span := richTextPayload{Type: "text"}
			span.Text.Content = part
			if chunk.link != "" {
				span.Text.Link = &linkRef{URL: chunk.link}
			}
			if chunk.bold || chunk.code {
				span.Annotations = &annotationPayload{Bold: chunk.bold, Code: chunk.code}
			}
			spans = append(spans, span)
		}
	}
	if len(spans) == 0 {
		// Notion accepts an empty array, but a block with no spans renders as a
		// blank line, which is what an empty input means anyway.
		return []richTextPayload{}
	}
	return spans
}

// inlineChunk is one styled run of text.
type inlineChunk struct {
	text string
	bold bool
	code bool
	link string
}

// splitInline tokenizes inline markdown into styled runs.
func splitInline(text string) []inlineChunk {
	var (
		chunks []inlineChunk
		plain  strings.Builder
	)
	flush := func() {
		if plain.Len() > 0 {
			chunks = append(chunks, inlineChunk{text: plain.String()})
			plain.Reset()
		}
	}

	for i := 0; i < len(text); {
		switch {
		case strings.HasPrefix(text[i:], "**"):
			if end := strings.Index(text[i+2:], "**"); end >= 0 {
				flush()
				chunks = append(chunks, inlineChunk{text: text[i+2 : i+2+end], bold: true})
				i += 2 + end + 2
				continue
			}
		case text[i] == '`':
			if end := strings.IndexByte(text[i+1:], '`'); end >= 0 {
				flush()
				chunks = append(chunks, inlineChunk{text: text[i+1 : i+1+end], code: true})
				i += 1 + end + 1
				continue
			}
		case text[i] == '[':
			if label, target, width, ok := parseLink(text[i:]); ok {
				flush()
				chunks = append(chunks, inlineChunk{text: label, link: target})
				i += width
				continue
			}
		}
		plain.WriteByte(text[i])
		i++
	}
	flush()
	return chunks
}

// parseLink reads a [label](url) at the start of text.
func parseLink(text string) (label, target string, width int, ok bool) {
	closeBracket := strings.IndexByte(text, ']')
	if closeBracket < 0 || closeBracket+1 >= len(text) || text[closeBracket+1] != '(' {
		return "", "", 0, false
	}
	closeParen := strings.IndexByte(text[closeBracket+2:], ')')
	if closeParen < 0 {
		return "", "", 0, false
	}
	label = text[1:closeBracket]
	target = text[closeBracket+2 : closeBracket+2+closeParen]
	if label == "" || target == "" {
		return "", "", 0, false
	}
	return label, target, closeBracket + 2 + closeParen + 1, true
}

// chunkRunes splits text into pieces of at most size runes.
func chunkRunes(text string, size int) []string {
	runes := []rune(text)
	if len(runes) <= size {
		return []string{text}
	}
	var out []string
	for start := 0; start < len(runes); start += size {
		out = append(out, string(runes[start:min(start+size, len(runes))]))
	}
	return out
}

// AppendToPage appends markdown to the end of an existing page and reports how
// many blocks it added.
//
// The exported counterpart to appendBlocks, and the difference is what it does
// with markdown rather than with blocks: an approved payload is markdown a human
// read, so the conversion happens here where a caller cannot pass blocks that
// nobody saw.
//
// Append rather than replace, deliberately. ReplacePageContent next door exists
// for the seeder, which must converge on a fixture; a write proposed by the agent
// must never destroy text somebody else wrote, so there is no
// replace-a-page capability on the agent path at all.
func (c *Client) AppendToPage(ctx context.Context, pageID, markdown string) (int, error) {
	id, err := normalizeID(pageID)
	if err != nil {
		return 0, err
	}
	blocks := markdownToBlocks(markdown)
	if len(blocks) == 0 {
		return 0, fmt.Errorf("notion: nothing to append to %s — the markdown is empty", id)
	}
	if err := c.appendBlocks(ctx, id, blocks); err != nil {
		return 0, WriteCapabilityError(err)
	}
	return len(blocks), nil
}

// WriteCapabilityError rewrites Notion's permission refusal into an instruction.
//
// Notion has no endpoint that reports what an integration is allowed to do:
// /v1/users/me returns its name and workspace and nothing about its
// capabilities. So a missing "Insert content" capability cannot be caught when a
// user enables writes — it can only surface here, on the first real attempt, and
// what Notion says at that point is "restricted_resource", which tells nobody
// what to go and change.
//
// The remedy is a checkbox in Notion's own integration settings, so the error
// says that. It reaches a human through the action row and the audit view,
// behind the approval gate, which is the one place this failure mode is
// harmless.
func WriteCapabilityError(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.StatusCode != http.StatusForbidden && apiErr.Code != "restricted_resource" {
		return err
	}
	return fmt.Errorf("notion refused the write: the integration lacks the \"Insert content\" / "+
		"\"Update content\" capability. Grant it in Notion under Settings → Connections → your "+
		"integration → Capabilities, then try again: %w", err)
}
