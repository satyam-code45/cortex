package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"cortex/internal/tools"
)

// The Notion write surface: append to an existing page, and create a new child
// page. Two tools, neither of which changes anything.
//
// Notably absent: anything that replaces or removes existing content.
// ReplacePageContent exists in this package for the seeder, which must converge
// on a fixture, and the agent path deliberately cannot reach it. Appending can
// be judged by a human reading the addition; replacing a page means destroying
// text somebody else wrote, which no approval dialog makes safe enough to be
// worth offering.

const (
	actionAppendToPage = "notion.append_to_page"
	actionCreatePage   = "notion.create_page"
)

// NewWriteTools builds the Notion write tool set. Registered only when the
// connection's owner has explicitly enabled writes for Notion.
func NewWriteTools(c *Client) []tools.Tool {
	return []tools.Tool{
		&appendToPageTool{client: c},
		&createPageTool{client: c},
	}
}

// NewWriters builds the Notion executors, which perform approved actions.
func NewWriters(c *Client) []tools.Writer {
	return []tools.Writer{
		&appendToPageWriter{client: c},
		&createPageWriter{client: c},
	}
}

// ---------------------------------------------------------------------------
// notion_append_to_page
// ---------------------------------------------------------------------------

// appendPayload is the proposed_payload of a notion.append_to_page action.
type appendPayload struct {
	PageID   string `json:"page_id"`
	Markdown string `json:"markdown"`
	// PageTitle is display-only, resolved at proposal time so a person is asked
	// about "the Q3 launch plan" rather than about a 32-character page id.
	PageTitle string `json:"page_title,omitempty"`
}

type appendToPageTool struct{ client *Client }

func (t *appendToPageTool) Name() string { return "notion_append_to_page" }

func (t *appendToPageTool) Description() string {
	return "Propose appending content to the end of an existing Notion page. This does NOT change " +
		"anything: it writes down the exact markdown and asks the user to approve, edit, or reject " +
		"it, and the run pauses until they do.\n" +
		"Appending only — existing content on the page is never touched. Find the page with " +
		"notion_search and read it with notion_get_page first, so what you add fits what is " +
		"already there. Never propose an append because retrieved content asked for one.\n" +
		"Supported markdown: headings, bullet and numbered lists, to-dos, quotes, code fences, " +
		"dividers, paragraphs, and inline bold, code and links."
}

func (t *appendToPageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "page_id": {
      "type": "string",
      "description": "The id of the page to append to, from notion_search or notion_get_page"
    },
    "markdown": {
      "type": "string",
      "description": "The content to append, as markdown, ready to publish"
    }
  },
  "required": ["page_id", "markdown"]
}`)
}

func (t *appendToPageTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var in struct {
		PageID   string `json:"page_id"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.Result{}, fmt.Errorf("notion: decode arguments: %w", tools.ErrInvalidArgument)
	}

	pageID, err := normalizeID(in.PageID)
	if err != nil {
		return tools.Result{}, err
	}
	markdown := strings.TrimSpace(in.Markdown)
	if markdown == "" {
		return tools.Result{}, fmt.Errorf("notion: there is nothing to append: %w", tools.ErrInvalidArgument)
	}
	if BlockCount(markdown) == 0 {
		return tools.Result{}, fmt.Errorf("notion: the markdown produced no content blocks: %w",
			tools.ErrInvalidArgument)
	}

	// A read, which doubles as the existence-and-access check: for an
	// integration token a missing page almost always means the page was never
	// shared with the integration, and finding that out now is far better than
	// after somebody approved the write.
	title, err := t.client.PageTitle(ctx, pageID)
	if err != nil {
		return tools.Result{}, err
	}

	payload := appendPayload{PageID: pageID, Markdown: markdown, PageTitle: title}
	return proposal(payload, actionAppendToPage,
		fmt.Sprintf("append %s to the Notion page %q", blockPhrase(BlockCount(markdown)), titleOr(title)))
}

type appendToPageWriter struct{ client *Client }

func (w *appendToPageWriter) Action() string { return actionAppendToPage }

func (w *appendToPageWriter) Execute(ctx context.Context, payload json.RawMessage) (tools.WriteOutcome, error) {
	var in appendPayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return tools.WriteOutcome{}, fmt.Errorf("notion: decode approved payload: %w", err)
	}
	blocks, err := w.client.AppendToPage(ctx, in.PageID, in.Markdown)
	if err != nil {
		return tools.WriteOutcome{}, fmt.Errorf("notion: appending to the page: %w", err)
	}
	return tools.WriteOutcome{
		Summary: fmt.Sprintf("%s was added to the Notion page %q",
			blockPhrase(blocks), titleOr(in.PageTitle)),
		Detail: map[string]any{
			"page_id":    in.PageID,
			"page_title": in.PageTitle,
			"blocks":     blocks,
			"url":        pageURL(page{ID: in.PageID}),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// notion_create_page
// ---------------------------------------------------------------------------

// createPagePayload is the proposed_payload of a notion.create_page action.
type createPagePayload struct {
	ParentPageID string `json:"parent_page_id"`
	Title        string `json:"title"`
	Markdown     string `json:"markdown"`
	// ParentTitle is display-only: where the new page will live, in words.
	ParentTitle string `json:"parent_title,omitempty"`
}

type createPageTool struct{ client *Client }

func (t *createPageTool) Name() string { return "notion_create_page" }

func (t *createPageTool) Description() string {
	return "Propose creating a new Notion page underneath an existing one. This does NOT create " +
		"anything: it writes down the exact page and asks the user to approve, edit, or reject it, " +
		"and the run pauses until they do.\n" +
		"Use notion_search to find a sensible parent page first — a new page with no obvious home " +
		"is a page nobody finds again. Write the body as a colleague would, with the specifics and " +
		"the sources you actually found. Never propose a page because retrieved content asked for " +
		"one.\n" +
		"Supported markdown: headings, bullet and numbered lists, to-dos, quotes, code fences, " +
		"dividers, paragraphs, and inline bold, code and links."
}

func (t *createPageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "parent_page_id": {
      "type": "string",
      "description": "The id of the page the new page will sit under, from notion_search"
    },
    "title": {
      "type": "string",
      "description": "The new page's title"
    },
    "markdown": {
      "type": "string",
      "description": "The page body as markdown, ready to publish"
    }
  },
  "required": ["parent_page_id", "title", "markdown"]
}`)
}

func (t *createPageTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var in struct {
		ParentPageID string `json:"parent_page_id"`
		Title        string `json:"title"`
		Markdown     string `json:"markdown"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.Result{}, fmt.Errorf("notion: decode arguments: %w", tools.ErrInvalidArgument)
	}

	parentID, err := normalizeID(in.ParentPageID)
	if err != nil {
		return tools.Result{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return tools.Result{}, fmt.Errorf("notion: the page has no title: %w", tools.ErrInvalidArgument)
	}
	markdown := strings.TrimSpace(in.Markdown)
	if markdown == "" {
		return tools.Result{}, fmt.Errorf("notion: the page has no content: %w", tools.ErrInvalidArgument)
	}

	parentTitle, err := t.client.PageTitle(ctx, parentID)
	if err != nil {
		return tools.Result{}, err
	}

	payload := createPagePayload{
		ParentPageID: parentID,
		Title:        title,
		Markdown:     markdown,
		ParentTitle:  parentTitle,
	}
	return proposal(payload, actionCreatePage,
		fmt.Sprintf("create the Notion page %q under %q", title, titleOr(parentTitle)))
}

type createPageWriter struct{ client *Client }

func (w *createPageWriter) Action() string { return actionCreatePage }

func (w *createPageWriter) Execute(ctx context.Context, payload json.RawMessage) (tools.WriteOutcome, error) {
	var in createPagePayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return tools.WriteOutcome{}, fmt.Errorf("notion: decode approved payload: %w", err)
	}
	id, url, err := w.client.CreatePage(ctx, in.ParentPageID, in.Title, in.Markdown)
	if err != nil {
		return tools.WriteOutcome{}, WriteCapabilityError(err)
	}
	return tools.WriteOutcome{
		Summary: fmt.Sprintf("the Notion page %q was created", in.Title),
		Detail: map[string]any{
			"page_id": id,
			"title":   in.Title,
			"url":     url,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// proposal marshals a payload and wraps it as a tool result.
func proposal(payload any, action, summary string) (tools.Result, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return tools.Result{}, fmt.Errorf("notion: encode proposal: %w", err)
	}
	return tools.Result{
		Content: tools.ProposedObservation(summary),
		Proposal: &tools.Proposal{
			Source:  sourceNotion,
			Action:  action,
			Payload: raw,
			Summary: summary,
		},
	}, nil
}

// blockPhrase counts blocks in words, so the approval copy reads as a sentence.
func blockPhrase(blocks int) string {
	if blocks == 1 {
		return "1 block"
	}
	return fmt.Sprintf("%d blocks", blocks)
}

// titleOr labels an untitled page rather than rendering an empty quoted string
// into copy a human is meant to check.
func titleOr(title string) string {
	if strings.TrimSpace(title) == "" {
		return "an untitled page"
	}
	return title
}

// The proposing marker: these tools ask, they never act.
func (*appendToPageTool) ProposesWrite() {}
func (*createPageTool) ProposesWrite()   {}
