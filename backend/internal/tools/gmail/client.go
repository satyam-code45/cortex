// Package gmail holds a thin Gmail REST client and the agent tools built on it.
//
// Gmail is the source of record for anything that arrived from outside the
// company. A vendor's slipped delivery date, a customer escalation, a partner's
// contract change: none of it is in a ticket, and by the time it reaches one it
// has been paraphrased by whoever transcribed it. The original is in email, and
// so is its date — which is usually the fact a timeline turns on.
//
// The client is thin for the same reason the Jira and Notion ones are: a Gmail
// message is a MIME tree with far more structure than the model needs, and
// every byte handed to it is paid for on each later iteration of the agent loop.
package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"cortex/internal/tools"
	"net/url"
	"strings"
	"time"

	"cortex/internal/tools/httpx"
)

const (
	// apiBaseURL is the Gmail REST root.
	apiBaseURL = "https://gmail.googleapis.com"

	// DefaultMinInterval is the gap between requests. Gmail's per-user quota is
	// generous, but a search that fans out into one metadata fetch per hit is
	// exactly the burst shape that trips it.
	DefaultMinInterval = 100 * time.Millisecond
)

// Config configures a Client.
type Config struct {
	// TokenSource supplies access tokens, refreshing as needed.
	TokenSource *TokenSource

	// QueryScope, when set, is ANDed into every search.
	//
	// Cortex reads a real mailbox, so unset — the default — means the agent
	// searches all of it, which is the product working as intended. Setting it
	// to a label (e.g. `label:vantage-labs`) confines the agent to the seeded
	// fixtures, which is what makes an eval score reproducible and keeps
	// personal mail out of a graded answer.
	QueryScope string

	// HTTPClient is optional; a timeout-bearing client is built when nil.
	HTTPClient *http.Client
	// BaseURL overrides the Gmail API root. Tests point it at an httptest
	// server; production leaves it empty.
	BaseURL string
	// MaxRetries defaults to httpx.DefaultMaxRetries.
	MaxRetries int
	// MinInterval defaults to DefaultMinInterval. A negative value disables the
	// throttle, which is what tests use.
	MinInterval time.Duration
	// MaxRetryDelay caps one backoff wait.
	MaxRetryDelay time.Duration
	// Logger receives retry and throttle diagnostics.
	Logger *slog.Logger
}

// Client talks to one Gmail mailbox.
//
// It carries no logger of its own: httpx does all the retry and throttle
// logging, and a second unused one only invites drift.
type Client struct {
	http       *httpx.Client
	queryScope string
}

// NewClient validates the configuration and builds a Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.TokenSource == nil {
		return nil, errors.New("gmail: TokenSource is required")
	}

	base := cfg.BaseURL
	if base == "" {
		base = apiBaseURL
	}
	minInterval := cfg.MinInterval
	if minInterval == 0 {
		minInterval = DefaultMinInterval
	}

	source := cfg.TokenSource
	transport, err := httpx.New(httpx.Config{
		Name:          "gmail",
		BaseURL:       base,
		HTTPClient:    cfg.HTTPClient,
		MaxRetries:    cfg.MaxRetries,
		MinInterval:   minInterval,
		MaxRetryDelay: cfg.MaxRetryDelay,
		Logger:        cfg.Logger,
		Authorize: func(ctx context.Context, req *http.Request) error {
			token, err := source.AccessToken(ctx)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
		ParseError: parseAPIError,
	})
	if err != nil {
		return nil, err
	}

	return &Client{
		http:       transport,
		queryScope: strings.TrimSpace(cfg.QueryScope),
	}, nil
}

// APIError is a non-2xx response from Gmail.
//
// Google's message is worth surfacing: a malformed search query comes back with
// a precise explanation, and the agent loop feeds that to the model as an
// observation so it can fix its own query. The request URL is never included —
// errors become tool observations and rows in run_events.
type APIError struct {
	StatusCode int
	// Status is Google's symbolic status, e.g. "PERMISSION_DENIED".
	Status string
	// Message is Google's human-readable explanation.
	Message string
}

func (e *APIError) Error() string {
	switch {
	case e.Status != "" && e.Message != "":
		return fmt.Sprintf("gmail: HTTP %d (%s): %s", e.StatusCode, e.Status, e.Message)
	case e.Message != "":
		return fmt.Sprintf("gmail: HTTP %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("gmail: HTTP %d", e.StatusCode)
	}
}

// NotFound reports whether the message does not exist.
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// Permanent reports whether retrying is pointless, which is how the agent loop
// decides whether to give a failed tool call a second attempt.
func (e *APIError) Permanent() bool { return httpx.PermanentStatus(e.StatusCode) }

// parseAPIError extracts Google's error envelope from a failure response.
func parseAPIError(status int, raw []byte) error {
	apiErr := &APIError{StatusCode: status}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return apiErr
	}
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return apiErr
	}
	apiErr.Status = body.Error.Status
	apiErr.Message = body.Error.Message
	return apiErr
}

// scopedQuery ANDs the configured scope into a user query.
//
// The user's query is parenthesized so that its own top-level OR cannot escape
// the scope: `label:x OR from:y` would otherwise match everything from y in the
// entire mailbox, quietly defeating the narrowing.
//
// Wrapping alone is not enough, which is the whole reason this returns an
// error. A query that is itself unbalanced — `) OR from:bank.example (` —
// renders as `label:x () OR from:bank.example ()`, putting an OR back at the
// top level and reading the entire mailbox. That matters because the query text
// is model-produced and the model reads attacker-influenced tool output: anyone
// who can send mail to the mailbox, comment on an issue, or edit a shared page
// can try to plant such a query. So an unbalanced query is rejected outright
// whenever a scope is configured.
func (c *Client) scopedQuery(query string) (string, error) {
	trimmed := strings.TrimSpace(query)
	if c.queryScope == "" {
		return trimmed, nil
	}
	if trimmed == "" {
		return c.queryScope, nil
	}
	if !parensBalanced(trimmed) {
		// Wrapped so the loop does not retry: the query is just as unbalanced
		// the second time.
		return "", fmt.Errorf("the search query has unbalanced parentheses, which is not allowed "+
			"while the mailbox is scoped to %q; rewrite it with matched parentheses: %w",
			c.queryScope, tools.ErrInvalidArgument)
	}
	return c.queryScope + " (" + trimmed + ")", nil
}

// parensBalanced reports whether parentheses outside quoted strings are matched.
//
// Quoted runs are skipped because a parenthesis inside them is literal text —
// `subject:"Q2 plan (draft)"` is a perfectly ordinary search and must not be
// rejected.
func parensBalanced(query string) bool {
	depth := 0
	inQuote := false
	for i := range len(query) {
		switch query[i] {
		case '"':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
				if depth < 0 {
					return false
				}
			}
		}
	}
	return depth == 0 && !inQuote
}

// get performs a GET and decodes the JSON response into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.http.Get(ctx, path, query, out)
}

// postRaw performs a POST with a pre-encoded JSON body and query parameters.
func (c *Client) postRaw(ctx context.Context, path string, query url.Values, body, out any) error {
	return c.http.Do(ctx, httpx.Request{
		Method: http.MethodPost,
		Path:   path,
		Query:  query,
		Body:   body,
	}, out)
}
