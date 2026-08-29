package notion

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"cortex/internal/tools"
)

// The indexing crawl.
//
// This lives here rather than in internal/rag because it reuses the same
// unexported client helpers the tools do — searchPages, fetchBlocks,
// BlocksToMarkdown. Duplicating that pagination and block-flattening in the RAG
// layer would mean two implementations of "read a Notion page" that drift, and
// the indexed copy of a page would stop matching the live one the agent reads.

// IndexMaxPages is the default ceiling on one crawl when no cap is given.
const IndexMaxPages = 400

// Source adapts a Client to the indexing pipeline.
type Source struct {
	client *Client
	// limit caps how many pages one crawl reads.
	limit  int
	logger *slog.Logger
}

var _ tools.DocumentSource = (*Source)(nil)

// NewSource builds the indexing source. limit <= 0 uses IndexMaxPages.
func NewSource(c *Client, limit int, logger *slog.Logger) *Source {
	if limit <= 0 {
		limit = IndexMaxPages
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Source{client: c, limit: limit, logger: logger}
}

// Name identifies the source.
func (s *Source) Name() string { return sourceNotion }

// FetchAll crawls every page the integration can see.
//
// An empty search query is Notion's "everything shared with this integration",
// sorted most-recently-edited first — so a crawl cut off by the limit keeps the
// current end of the workspace rather than an arbitrary slice.
//
// A page that fails to read is skipped rather than failing the crawl. One page
// moved to the trash between the search and the block fetch would otherwise cost
// the entire index, and the next run picks it up anyway.
func (s *Source) FetchAll(ctx context.Context) ([]tools.Document, error) {
	pages, err := s.client.searchPages(ctx, "", s.limit)
	if err != nil {
		return nil, fmt.Errorf("notion: list pages for indexing: %w", err)
	}

	documents := make([]tools.Document, 0, len(pages))
	for _, p := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		blocks, truncated, err := s.client.fetchBlocks(ctx, p.ID, maxBlockDepth)
		if err != nil {
			s.logger.Warn("notion: skipping a page that could not be read for indexing",
				"page_id", p.ID, "error", err)
			continue
		}

		body := BlocksToMarkdown(blocks)
		if strings.TrimSpace(body) == "" && p.title() == "" {
			continue
		}

		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n", p.title())
		b.WriteString(body)

		metadata := map[string]any{
			"last_edited_time": p.LastEditedTime,
			"created_time":     p.CreatedTime,
		}
		if truncated {
			// Recorded rather than hidden: a chunk from a page that was cut off
			// is still useful, but anyone reading the index should be able to
			// tell that the page continues past what was embedded.
			metadata["truncated"] = true
		}

		documents = append(documents, tools.Document{
			Source:     sourceNotion,
			ExternalID: p.ID,
			Title:      p.title(),
			URL:        pageURL(p),
			Content:    b.String(),
			Metadata:   metadata,
			Timestamp:  parseNotionTime(p.LastEditedTime),
		})
	}
	return documents, nil
}

// pageURL falls back to the canonical URL form when search omits it.
func pageURL(p page) string {
	if p.URL != "" {
		return p.URL
	}
	return "https://www.notion.so/" + url.PathEscape(strings.ReplaceAll(p.ID, "-", ""))
}
