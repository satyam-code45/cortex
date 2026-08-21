// Package notion holds a thin Notion REST client and the agent tools built on
// it.
//
// Notion is the source that names things. Jira records that work is blocked;
// the plan document records *who* it is blocked on, which vendor was chosen,
// and which date was originally promised. Those are the facts a multi-hop
// investigation pivots through, and none of them exist in a ticket.
//
// The client is deliberately thin, for the same reason the Jira one is: a
// Notion page is a tree of block objects with far more structure than the model
// needs, and every byte handed to it is paid for on each later iteration of the
// agent loop. Blocks are flattened to markdown at the boundary.
package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cortex/internal/tools/httpx"
)

const (
	// apiBaseURL is the Notion REST root.
	apiBaseURL = "https://api.notion.com"

	// APIVersion is the Notion-Version header sent on every request.
	//
	// Pinned, not tracked: Notion versions its API by date and changes response
	// shapes between versions — 2025-09-03 split databases into data sources and
	// changed the search filter vocabulary, 2026-03-11 renamed `archived` to
	// `in_trash`. Sending a fixed version is what stops a server-side rollout
	// from silently changing what our parsing sees.
	APIVersion = "2026-03-11"

	// DefaultTimeout bounds a single HTTP round trip to Notion.
	DefaultTimeout = httpx.DefaultTimeout

	// DefaultMinInterval is the gap between requests. Notion documents an
	// average of three requests per second per integration, so this sits just
	// under it rather than discovering the limit by being throttled.
	DefaultMinInterval = 350 * time.Millisecond
)

// Config configures a Client.
type Config struct {
	// Token is the internal integration secret (NOTION_TOKEN).
	Token string

	// HTTPClient is optional; a timeout-bearing client is built when nil.
	HTTPClient *http.Client
	// BaseURL overrides the Notion API root. Tests point it at an httptest
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

// Client talks to one Notion workspace.
//
// It carries no logger of its own: httpx does all the retry and throttle
// logging, and a second unused one only invites drift.
type Client struct {
	http *httpx.Client
}

// NewClient validates the configuration and builds a Client.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("notion: Token is required")
	}

	base := cfg.BaseURL
	if base == "" {
		base = apiBaseURL
	}
	minInterval := cfg.MinInterval
	if minInterval == 0 {
		minInterval = DefaultMinInterval
	}

	token := cfg.Token
	transport, err := httpx.New(httpx.Config{
		Name:          "notion",
		BaseURL:       base,
		HTTPClient:    cfg.HTTPClient,
		MaxRetries:    cfg.MaxRetries,
		MinInterval:   minInterval,
		MaxRetryDelay: cfg.MaxRetryDelay,
		Logger:        cfg.Logger,
		Headers: map[string]string{
			"Notion-Version": APIVersion,
		},
		Authorize: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
		ParseError: parseAPIError,
	})
	if err != nil {
		return nil, err
	}

	return &Client{http: transport}, nil
}

// APIError is a non-2xx response from Notion.
//
// Notion returns a machine-readable code alongside a human message, and both
// are worth surfacing: the message explains a malformed request precisely
// enough for the model to correct itself, and the code is how the caller tells
// "this page is not shared with the integration" (object_not_found) from a
// genuine failure. The request URL is never included — errors become tool
// observations and rows in run_events.
type APIError struct {
	StatusCode int
	// Code is Notion's error code, e.g. "object_not_found".
	Code string
	// Message is Notion's human-readable explanation.
	Message string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("notion: HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("notion: HTTP %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("notion: HTTP %d", e.StatusCode)
	}
}

// NotFound reports whether Notion could not find the object.
//
// For an integration token this almost always means "the page exists but has
// not been shared with this integration" rather than "no such page", which is a
// distinction worth putting in front of both the operator and the model.
func (e *APIError) NotFound() bool {
	return e.StatusCode == http.StatusNotFound || e.Code == "object_not_found"
}

// Permanent reports whether retrying is pointless, which is how the agent loop
// decides whether to give a failed tool call a second attempt.
func (e *APIError) Permanent() bool { return httpx.PermanentStatus(e.StatusCode) }

// parseAPIError extracts Notion's error shape from a failure response.
func parseAPIError(status int, raw []byte) error {
	apiErr := &APIError{StatusCode: status}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return apiErr
	}
	var body struct {
		Object  string `json:"object"`
		Status  int    `json:"status"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not JSON at all (a gateway's HTML error page, say).
		return apiErr
	}
	apiErr.Code = body.Code
	apiErr.Message = body.Message
	return apiErr
}

// get performs a GET and decodes the JSON response into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.http.Get(ctx, path, query, out)
}

// post performs a POST with a JSON body.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.http.Post(ctx, path, body, out)
}

// patch performs a PATCH with a JSON body. Notion uses PATCH for appends and
// property updates.
func (c *Client) patch(ctx context.Context, path string, body, out any) error {
	return c.http.Do(ctx, httpx.Request{Method: http.MethodPatch, Path: path, Body: body}, out)
}
