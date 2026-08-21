package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"cortex/internal/tools"
)

// The two read-only Gmail tools.
//
// Read-only is the same boundary the Jira and Notion tools draw: the agent
// searches and reads, never sends, replies, labels, or deletes. The seeder
// writes, but it is a CLI the operator runs — not something the model can reach.

const (
	// sourceGmail labels evidence produced by this package.
	sourceGmail = "gmail"

	// defaultSearchResults is used when the model omits max_results.
	defaultSearchResults = 10
	// maxSearchResults caps one search. Each hit costs its own metadata fetch,
	// so the ceiling bounds both latency and prompt budget.
	maxSearchResults = 25
)

// NewTools builds the Gmail tool set backed by c.
func NewTools(c *Client) []tools.Tool {
	return []tools.Tool{
		&searchTool{client: c},
		&getMessageTool{client: c},
	}
}

// ---------------------------------------------------------------------------
// gmail_search
// ---------------------------------------------------------------------------

type searchTool struct{ client *Client }

func (t *searchTool) Name() string { return "gmail_search" }

func (t *searchTool) Description() string {
	return "Search email using Gmail's own query syntax. Email is where anything from OUTSIDE " +
		"the company lives — a vendor announcing a slipped delivery date, a partner changing " +
		"terms, a customer escalating — along with the internal threads where decisions were " +
		"argued out before they were written down anywhere. When a ticket or a document says " +
		"something was communicated, agreed, or announced but does not say what was said, the " +
		"original is usually here. Supports from:, to:, subject:, after:/before: (YYYY/MM/DD), " +
		"has:attachment, and quoted phrases, e.g. `from:vendor.com after:2026/05/01`. Returns " +
		"one compact line per message; call gmail_get_message for the full text."
}

func (t *searchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "A Gmail search query, e.g. subject:refunds after:2026/05/01"
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum messages to return (1-25, default 10)",
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
	query := strings.TrimSpace(in.Query)
	if query == "" {
		// Wrapped so the loop does not retry: Validate cannot express minLength,
		// and an empty query would return the entire mailbox newest-first, which
		// is never what was meant.
		return tools.Result{}, fmt.Errorf("query must not be empty: %w", tools.ErrInvalidArgument)
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultSearchResults
	}
	limit = min(limit, maxSearchResults)

	messages, unavailable, err := t.client.searchMessages(ctx, query, limit)
	if err != nil {
		return tools.Result{}, err
	}

	if len(messages) == 0 {
		// The same trap as an empty Jira search, sharpened by Gmail's syntax:
		// an unknown operator or a malformed date silently matches nothing
		// rather than erroring, so zero results is at least as often a broken
		// query as an empty mailbox.
		return tools.Result{
			Content: fmt.Sprintf("No messages matched the Gmail query: %s\n\n"+
				"Note: this does not establish that no such mail exists. Gmail matches whole "+
				"words and silently returns nothing for an operator it does not recognize or a "+
				"date it cannot parse (dates must be YYYY/MM/DD). Before concluding there is "+
				"nothing, retry with fewer terms — a sender's domain alone, or a single "+
				"distinctive word — and widen the date range.", query),
			Evidence: []tools.EvidenceItem{},
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d message(s) matching `%s`:\n", len(messages), query)
	evidence := make([]tools.EvidenceItem, 0, len(messages))
	for _, msg := range messages {
		line := formatMessageLine(&msg)
		b.WriteString(line)
		b.WriteString("\n")
		evidence = append(evidence, tools.EvidenceItem{
			Source:     sourceGmail,
			ExternalID: msg.ID,
			Title:      msg.subject(),
			URL:        webURL(msg.ID),
			Snippet:    snippet(msg.Snippet),
			Timestamp:  msg.sentAt(),
		})
	}
	if len(messages) == limit {
		fmt.Fprintf(&b, "(result limit of %d reached — there may be more matches)\n", limit)
	}
	if unavailable > 0 {
		// Stated rather than swallowed: a silently short list reads to the model
		// as the complete set of matches.
		fmt.Fprintf(&b, "(%d further match(es) could not be retrieved and are not shown)\n", unavailable)
	}

	return tools.Result{Content: strings.TrimRight(b.String(), "\n"), Evidence: evidence}, nil
}

// formatMessageLine renders one message as a single compact line.
func formatMessageLine(msg *message) string {
	return fmt.Sprintf("%s [%s] from: %s — %s | %s",
		msg.ID,
		formatDate(msg.sentAt()),
		msg.header("From"),
		msg.subject(),
		snippet(msg.Snippet),
	)
}

// searchMessages lists matching ids and then fetches each one's metadata.
//
// Two round trips per result is Gmail's shape, not a choice: the list endpoint
// returns bare ids, and an id is useless to the model — it cannot tell which of
// ten results is worth opening without a sender, a subject, and a date.
func (c *Client) searchMessages(ctx context.Context, query string, limit int) ([]message, int, error) {
	scoped, err := c.scopedQuery(query)
	if err != nil {
		return nil, 0, err
	}
	listQuery := url.Values{}
	listQuery.Set("q", scoped)
	listQuery.Set("maxResults", strconv.Itoa(limit))

	var list listResponse
	if err := c.get(ctx, "/gmail/v1/users/me/messages", listQuery, &list); err != nil {
		return nil, 0, err
	}

	messages := make([]message, 0, len(list.Messages))
	unavailable := 0
	for _, ref := range list.Messages {
		if len(messages) >= limit {
			break
		}
		msg, err := c.getMessage(ctx, ref.ID, "metadata")
		if err != nil {
			// A single unreadable message must not cost the whole hop. list
			// returns ids and each is then fetched separately, so a message
			// deleted between the two calls 404s — and a 404 is permanent, so
			// failing here would lose nine good results to one stale id, and the
			// agent would report the whole source as unavailable.
			if ctx.Err() != nil {
				return nil, 0, err
			}
			unavailable++
			continue
		}
		messages = append(messages, *msg)
	}
	return messages, unavailable, nil
}

// getMessage fetches one message in the given format.
func (c *Client) getMessage(ctx context.Context, id, format string) (*message, error) {
	query := url.Values{}
	query.Set("format", format)
	if format == "metadata" {
		// Without this Gmail returns every header — forty-odd lines of
		// Received:, DKIM-Signature: and X-Google-* per message, none of which
		// the model has any use for.
		query["metadataHeaders"] = []string{"From", "To", "Subject", "Date"}
	}

	var msg message
	if err := c.get(ctx, "/gmail/v1/users/me/messages/"+url.PathEscape(id), query, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ---------------------------------------------------------------------------
// gmail_get_message
// ---------------------------------------------------------------------------

type getMessageTool struct{ client *Client }

func (t *getMessageTool) Name() string { return "gmail_get_message" }

func (t *getMessageTool) Description() string {
	return "Read one email in full — headers plus the plain-text body — given a message id from " +
		"gmail_search. Search returns only a one-line snippet, and the detail that matters " +
		"(the reason given, the date promised, who was copied) is in the body."
}

func (t *getMessageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "message_id": {
      "type": "string",
      "description": "The message id from gmail_search, e.g. 18f2a3b4c5d6e7f8"
    }
  },
  "required": ["message_id"]
}`)
}

func (t *getMessageTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var in struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.Result{}, fmt.Errorf("decode arguments: %w", err)
	}
	messageID, err := normalizeMessageID(in.MessageID)
	if err != nil {
		return tools.Result{}, err
	}

	msg, err := t.client.getMessage(ctx, messageID, "full")
	if err != nil {
		return tools.Result{}, err
	}

	body, fromHTML := extractBody(msg.Payload)
	if strings.TrimSpace(body) == "" {
		// Saying so beats returning an empty string, which reads to the model
		// as an empty email rather than as a body it could not extract.
		body = "(no plain-text body could be extracted from this message; it may be an " +
			"attachment-only or calendar message)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\n", msg.header("From"))
	if to := msg.header("To"); to != "" {
		fmt.Fprintf(&b, "To: %s\n", to)
	}
	fmt.Fprintf(&b, "Date: %s\n", formatDate(msg.sentAt()))
	fmt.Fprintf(&b, "Subject: %s\n", msg.subject())
	fmt.Fprintf(&b, "(Gmail message %s)\n", msg.ID)
	if fromHTML {
		// Flagged, not hidden: the words below are a lossy rendering of markup,
		// so a quote drawn from them is a paraphrase of the original.
		b.WriteString("(this message had no plain-text part; the body below was extracted from HTML)\n")
	}
	b.WriteString("\n")
	b.WriteString(body)

	return tools.Result{
		Content: b.String(),
		Evidence: []tools.EvidenceItem{{
			Source:     sourceGmail,
			ExternalID: msg.ID,
			Title:      msg.subject(),
			URL:        webURL(msg.ID),
			Snippet:    snippet(body),
			Timestamp:  msg.sentAt(),
		}},
	}, nil
}
