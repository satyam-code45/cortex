package gmail

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"cortex/internal/tools"
)

// The indexing crawl.
//
// It lives here so it can reuse scopedQuery, getMessage and extractBody. The
// scope reuse is the part that matters: GMAIL_QUERY_SCOPE is what confines
// Cortex to the seeded fixtures when someone wants a reproducible eval run, and
// an indexer that ignored it would quietly copy the operator's personal mail
// into the vector store — permanently, and where a citation could surface it.

const (
	// IndexMaxMessages is the default ceiling on one crawl.
	IndexMaxMessages = 400

	// listPageSize is how many message ids are requested per page. 500 is
	// Gmail's maximum.
	listPageSize = 500
)

// Source adapts a Client to the indexing pipeline.
type Source struct {
	client *Client
	limit  int
	logger *slog.Logger
}

var _ tools.DocumentSource = (*Source)(nil)

// NewSource builds the indexing source. limit <= 0 uses IndexMaxMessages.
func NewSource(c *Client, limit int, logger *slog.Logger) *Source {
	if limit <= 0 {
		limit = IndexMaxMessages
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Source{client: c, limit: limit, logger: logger}
}

// Name identifies the source.
func (s *Source) Name() string { return sourceGmail }

// FetchAll crawls the mailbox within the configured scope.
//
// Two round trips per message is Gmail's shape: the list endpoint returns bare
// ids, and the body only comes from a full fetch. The list is paginated so the
// cap is reached with as few requests as possible, and each message is then
// fetched individually.
//
// A message that fails to fetch is skipped. Anything else would let one deleted
// id between the list call and the fetch call cost the entire crawl.
func (s *Source) FetchAll(ctx context.Context) ([]tools.Document, error) {
	refs, err := s.listMessages(ctx)
	if err != nil {
		return nil, err
	}

	documents := make([]tools.Document, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		msg, err := s.client.getMessage(ctx, ref.ID, "full")
		if err != nil {
			s.logger.Warn("gmail: skipping a message that could not be read for indexing",
				"message_id", ref.ID, "error", err)
			continue
		}

		body, fromHTML := extractBody(msg.Payload)
		if strings.TrimSpace(body) == "" {
			// Nothing to embed. Attachment-only and calendar messages are real
			// and common, and a document whose only content is its headers
			// retrieves noise for every query.
			continue
		}

		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n", msg.subject())
		fmt.Fprintf(&b, "From: %s\n", msg.header("From"))
		if to := msg.header("To"); to != "" {
			fmt.Fprintf(&b, "To: %s\n", to)
		}
		fmt.Fprintf(&b, "Date: %s\n\n", formatDate(msg.sentAt()))
		b.WriteString(body)

		metadata := map[string]any{
			"from":      msg.header("From"),
			"to":        msg.header("To"),
			"thread_id": msg.ThreadID,
			"labels":    msg.LabelIDs,
		}
		if fromHTML {
			// Recorded because a quote drawn from an HTML-extracted body is a
			// paraphrase of the original, and a reader of a citation should be
			// able to tell.
			metadata["body_from_html"] = true
		}

		documents = append(documents, tools.Document{
			Source:     sourceGmail,
			ExternalID: msg.ID,
			Title:      msg.subject(),
			URL:        webURL(msg.ID),
			Content:    b.String(),
			Metadata:   metadata,
			Timestamp:  msg.sentAt(),
		})
	}
	return documents, nil
}

// listMessages pages through message ids within the configured scope.
func (s *Source) listMessages(ctx context.Context) ([]messageRef, error) {
	// The empty query is "everything", narrowed by GMAIL_QUERY_SCOPE when one is
	// set — the same narrowing every agent search goes through.
	scoped, err := s.client.scopedQuery("")
	if err != nil {
		return nil, err
	}

	var (
		refs      []messageRef
		pageToken string
	)
	for len(refs) < s.limit {
		query := url.Values{}
		if scoped != "" {
			query.Set("q", scoped)
		}
		query.Set("maxResults", strconv.Itoa(min(s.limit-len(refs), listPageSize)))
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}

		var page listResponse
		if err := s.client.get(ctx, "/gmail/v1/users/me/messages", query, &page); err != nil {
			return nil, fmt.Errorf("gmail: list messages for indexing: %w", err)
		}
		refs = append(refs, page.Messages...)

		if page.NextPageToken == "" || len(page.Messages) == 0 {
			break
		}
		pageToken = page.NextPageToken
	}

	return refs[:min(len(refs), s.limit)], nil
}
