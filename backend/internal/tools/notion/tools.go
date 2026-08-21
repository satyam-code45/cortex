package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"cortex/internal/tools"
)

// The two read-only Notion tools.
//
// Read-only is the same deliberate boundary the Jira tools draw: the agent gets
// search and retrieval, never page creation or edits. The seeder writes, but it
// is a CLI the operator runs — not something the model can reach.

const (
	// sourceNotion labels evidence produced by this package.
	sourceNotion = "notion"

	// defaultSearchResults is used when the model omits max_results.
	defaultSearchResults = 10
	// maxSearchResults caps one search. Notion titles are long and the URLs
	// longer, so the ceiling is about prompt budget rather than what Notion
	// will serve.
	maxSearchResults = 25

	// blockPageSize is how many blocks are requested per API page. 100 is
	// Notion's maximum.
	blockPageSize = 100

	// maxBlockDepth is how far into a page's block tree notion_get_page
	// descends. Depth 1 is the page's own children; depth 2 is their children,
	// which is where the content of nested bullets and toggles lives. Going
	// deeper multiplies request count for text that is almost always incidental.
	maxBlockDepth = 2

	// maxBlocksPerPage bounds one page fetch. A runaway page — a meeting-notes
	// document someone has appended to for a year — would otherwise cost
	// hundreds of requests and blow the prompt budget in one observation.
	maxBlocksPerPage = 400
)

// NewTools builds the Notion tool set backed by c.
func NewTools(c *Client) []tools.Tool {
	return []tools.Tool{
		&searchTool{client: c},
		&getPageTool{client: c},
	}
}

// ---------------------------------------------------------------------------
// notion_search
// ---------------------------------------------------------------------------

type searchTool struct{ client *Client }

func (t *searchTool) Name() string { return "notion_search" }

func (t *searchTool) Description() string {
	return "Search Notion pages by title. Notion holds the written record around the work: " +
		"project plans, roadmaps, retro and meeting notes, and the decisions behind them — " +
		"including the names of vendors, partners, and people that tickets refer to only " +
		"obliquely. Matches on page TITLE only, not body text, so search broad terms " +
		"(\"Atlas plan\", \"retro\") and then read the page. Returns page id, title, last edited " +
		"date, and URL. Call notion_get_page with an id to read the contents."
}

func (t *searchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Words expected in the page title, e.g. Atlas plan. An empty query lists all pages the integration can see."
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum pages to return (1-25, default 10)",
      "minimum": 1,
      "maximum": 25
    }
  },
  "required": ["query"]
}`)
}

func (t *searchTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var in struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.Result{}, fmt.Errorf("decode arguments: %w", err)
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultSearchResults
	}
	limit = min(limit, maxSearchResults)

	pages, err := t.client.searchPages(ctx, strings.TrimSpace(in.Query), limit)
	if err != nil {
		return tools.Result{}, err
	}

	if len(pages) == 0 {
		// Same hazard as an empty Jira search, with an extra twist that is
		// specific to Notion and bites hard: search matches titles, not body
		// text, and an integration only ever sees pages explicitly shared with
		// it. So "no results" is a statement about titles and sharing, not
		// about whether the workspace records the fact.
		return tools.Result{
			Content: fmt.Sprintf("No Notion pages matched %q.\n\n"+
				"Note: Notion search matches page TITLES, not the text inside pages, and it only "+
				"sees pages shared with this integration. A page discussing the topic can exist "+
				"under a title that does not mention it. Before concluding the workspace has "+
				"nothing, retry with a broader term, or search with an empty query to list every "+
				"page available and pick by hand.", in.Query),
			Evidence: []tools.EvidenceItem{},
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d Notion page(s) matching %q:\n", len(pages), in.Query)
	evidence := make([]tools.EvidenceItem, 0, len(pages))
	for _, p := range pages {
		line := fmt.Sprintf("%s — id: %s; last edited: %s; url: %s",
			p.title(), p.ID, formatDate(p.LastEditedTime), p.URL)
		b.WriteString(line)
		b.WriteString("\n")
		evidence = append(evidence, tools.EvidenceItem{
			Source:     sourceNotion,
			ExternalID: p.ID,
			Title:      p.title(),
			URL:        p.URL,
			Snippet:    snippet(line),
			Timestamp:  parseNotionTime(p.LastEditedTime),
		})
	}
	if len(pages) == limit {
		fmt.Fprintf(&b, "(result limit of %d reached — there may be more pages)\n", limit)
	}

	return tools.Result{Content: strings.TrimRight(b.String(), "\n"), Evidence: evidence}, nil
}

// searchPages runs a title search, following the cursor until limit is reached.
func (c *Client) searchPages(ctx context.Context, query string, limit int) ([]page, error) {
	var collected []page
	cursor := ""

	for len(collected) < limit {
		body := map[string]any{
			"page_size": min(limit-len(collected), 100),
			// Pages only. The 2025-09-03 vocabulary replaced "database" with
			// "data_source"; Cortex reads neither, and asking for pages keeps
			// data-source rows out of results that would otherwise look like
			// pages with no readable body.
			"filter": map[string]string{"property": "object", "value": "page"},
			// Most-recently-edited first, so a truncated result set is the
			// currently-relevant end of the workspace rather than an arbitrary
			// slice of it.
			"sort": map[string]string{"direction": "descending", "timestamp": "last_edited_time"},
		}
		if query != "" {
			body["query"] = query
		}
		if cursor != "" {
			body["start_cursor"] = cursor
		}

		var resp searchResponse
		if err := c.post(ctx, "/v1/search", body, &resp); err != nil {
			return nil, err
		}
		for _, p := range resp.Results {
			// A trashed page is still returned by search; reading it would
			// present deleted content as current.
			if p.InTrash {
				continue
			}
			collected = append(collected, p)
		}
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}

	return collected[:min(len(collected), limit)], nil
}

// ---------------------------------------------------------------------------
// notion_get_page
// ---------------------------------------------------------------------------

type getPageTool struct{ client *Client }

func (t *getPageTool) Name() string { return "notion_get_page" }

func (t *getPageTool) Description() string {
	return "Read a Notion page's full contents as markdown, given its page id from notion_search. " +
		"Use this after a search: search returns only titles, and the facts worth having — " +
		"dates, owners, vendor names, decisions and their reasons — are in the body. " +
		"Nested content is included two levels deep."
}

func (t *getPageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "page_id": {
      "type": "string",
      "description": "The page id from notion_search, e.g. 3c34fab1-2b9b-80d6-8cfb-c64d5b8cebe0"
    }
  },
  "required": ["page_id"]
}`)
}

func (t *getPageTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var in struct {
		PageID string `json:"page_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.Result{}, fmt.Errorf("decode arguments: %w", err)
	}
	pageID, err := normalizeID(in.PageID)
	if err != nil {
		return tools.Result{}, err
	}

	var p page
	if err := t.client.get(ctx, "/v1/pages/"+url.PathEscape(pageID), nil, &p); err != nil {
		return tools.Result{}, err
	}

	blocks, truncated, err := t.client.fetchBlocks(ctx, pageID, 1)
	if err != nil {
		return tools.Result{}, err
	}

	markdown := BlocksToMarkdown(blocks)
	if markdown == "" {
		markdown = "(this page has no readable text content)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", p.title())
	fmt.Fprintf(&b, "(Notion page %s, last edited %s)\n\n", p.ID, formatDate(p.LastEditedTime))
	b.WriteString(markdown)
	if truncated {
		fmt.Fprintf(&b, "\n\n(page truncated at %d blocks — it continues beyond what is shown)", maxBlocksPerPage)
	}

	content := b.String()
	return tools.Result{
		Content: content,
		Evidence: []tools.EvidenceItem{{
			Source:     sourceNotion,
			ExternalID: p.ID,
			Title:      p.title(),
			URL:        p.URL,
			Snippet:    snippet(markdown),
			Timestamp:  parseNotionTime(p.LastEditedTime),
		}},
	}, nil
}

// fetchBlocks reads a block's children, recursing to maxBlockDepth.
//
// It reports whether the fetch hit maxBlocksPerPage, because a silently
// truncated page is the worst possible failure here: the model would read half
// a plan and conclude the other half does not exist.
func (c *Client) fetchBlocks(ctx context.Context, blockID string, depth int) ([]block, bool, error) {
	budget := maxBlocksPerPage
	return c.fetchBlocksBudgeted(ctx, blockID, depth, &budget)
}

// fetchBlocksBudgeted is fetchBlocks with a budget shared across the recursion,
// so the cap counts every block on the page rather than every block per level.
func (c *Client) fetchBlocksBudgeted(ctx context.Context, blockID string, depth int, budget *int) ([]block, bool, error) {
	var (
		collected []block
		truncated bool
		cursor    string
	)

	for {
		if *budget <= 0 {
			return collected, true, nil
		}

		query := url.Values{}
		query.Set("page_size", strconv.Itoa(min(blockPageSize, *budget)))
		if cursor != "" {
			query.Set("start_cursor", cursor)
		}

		var resp blockListResponse
		if err := c.get(ctx, "/v1/blocks/"+url.PathEscape(blockID)+"/children", query, &resp); err != nil {
			return nil, false, err
		}

		for _, blk := range resp.Results {
			*budget--
			if blk.HasChildren && depth < maxBlockDepth {
				children, childTruncated, err := c.fetchBlocksBudgeted(ctx, blk.ID, depth+1, budget)
				if err != nil {
					return nil, false, err
				}
				blk.Children = children
				truncated = truncated || childTruncated
			}
			collected = append(collected, blk)
		}

		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}

	return collected, truncated, nil
}
