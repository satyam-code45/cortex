// Package jira holds a thin Jira Cloud REST v3 client and the agent tools built
// on it.
//
// "Thin" is the design: no ORM over Jira's object model, no caching layer, and
// no attempt to model every field. Jira issue JSON is enormous — a single issue
// with all fields expanded runs to tens of kilobytes — and every byte that
// reaches the model is paid for on each subsequent iteration of the agent loop.
// So each tool asks for exactly the fields it needs and renders them as compact
// lines.
//
// The transport — retries, throttling, response caps — lives in
// internal/tools/httpx, shared with the Notion and Gmail clients. What stays
// here is what is genuinely Jira's: basic auth, its three error-body shapes,
// and the browse URL that citations link to.
package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"cortex/internal/tools/httpx"
)

const (
	// DefaultTimeout bounds a single HTTP round trip to Jira. The agent loop
	// imposes its own, shorter per-tool deadline; this is the backstop for
	// callers without one, notably the seeder.
	DefaultTimeout = httpx.DefaultTimeout

	// DefaultMaxRetries is how many times a request is retried after a
	// throttling or server-side failure. Jira Cloud rate-limits aggressively
	// during bulk writes, which is the seeder's whole workload.
	DefaultMaxRetries = httpx.DefaultMaxRetries

	// DefaultMinInterval is the minimum gap between requests from one client.
	// It keeps the seeder's ~600 sequential writes under Jira's cost budget
	// instead of discovering the limit by being throttled.
	DefaultMinInterval = httpx.DefaultMinInterval

	// DefaultMaxRetryDelay caps a single backoff wait. It is deliberately short
	// for the agent path, where a tool call sits inside a 15s per-tool budget.
	// The seeder overrides it: Jira Cloud answers a throttled bulk write with a
	// Retry-After of 30-60s, and clamping that to 8s burns the whole retry budget
	// inside the window Jira asked us to wait, failing a request that would have
	// succeeded.
	DefaultMaxRetryDelay = httpx.DefaultMaxRetryDelay

	// maxResponseBytes caps a successful response body. Nothing Cortex reads is
	// legitimately larger, and an unbounded decode means one issue with 50 maximal
	// comments can materialize ~1.6MB.
	maxResponseBytes = httpx.DefaultMaxResponseBytes

	// maxErrorBodyBytes caps how much of an error response is read. Jira can
	// return an HTML error page, and the whole thing is not worth buffering.
	maxErrorBodyBytes = httpx.DefaultMaxErrorBodyBytes
)

// Config configures a Client.
type Config struct {
	// BaseURL is the site root, e.g. https://your-site.atlassian.net.
	BaseURL string
	// Email is the Atlassian account email used for basic auth.
	Email string
	// APIToken is the account's API token.
	APIToken string

	// HTTPClient is optional; a timeout-bearing client is built when nil.
	HTTPClient *http.Client
	// MaxRetries defaults to DefaultMaxRetries.
	MaxRetries int
	// MinInterval defaults to DefaultMinInterval. Zero disables throttling only
	// if explicitly set negative.
	MinInterval time.Duration
	// MaxRetryDelay caps one backoff wait; it defaults to DefaultMaxRetryDelay.
	// Raise it for bulk work that can afford to honour a long Retry-After.
	MaxRetryDelay time.Duration
	// Logger receives retry and throttle diagnostics.
	Logger *slog.Logger
}

// Client talks to one Jira Cloud site.
//
// It is safe for concurrent use, and deliberately serializes its own requests
// through a throttle: the agent may run several tool calls at once, and Jira
// counts them against a single per-account budget.
type Client struct {
	http    *httpx.Client
	baseURL string
	logger  *slog.Logger
}

// NewClient validates the configuration and builds a Client.
func NewClient(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("jira: BaseURL is required")
	}
	if cfg.Email == "" {
		return nil, errors.New("jira: Email is required")
	}
	if cfg.APIToken == "" {
		return nil, errors.New("jira: APIToken is required")
	}

	email, apiToken := cfg.Email, cfg.APIToken
	transport, err := httpx.New(httpx.Config{
		Name:              "jira",
		BaseURL:           base,
		HTTPClient:        cfg.HTTPClient,
		MaxRetries:        cfg.MaxRetries,
		MinInterval:       cfg.MinInterval,
		MaxRetryDelay:     cfg.MaxRetryDelay,
		MaxResponseBytes:  maxResponseBytes,
		MaxErrorBodyBytes: maxErrorBodyBytes,
		Logger:            cfg.Logger,
		Authorize: func(_ context.Context, req *http.Request) error {
			req.SetBasicAuth(email, apiToken)
			return nil
		},
		ParseError: parseAPIError,
	})
	if err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{http: transport, baseURL: base, logger: logger}, nil
}

// BrowseURL returns the human-facing URL for an issue key. Evidence carries
// this so a citation can be clicked through to the source.
func (c *Client) BrowseURL(key string) string {
	return c.baseURL + "/browse/" + url.PathEscape(key)
}

// APIError is a non-2xx response from Jira.
//
// Messages holds Jira's own error strings, which are safe to surface and
// genuinely useful: a malformed JQL comes back as a precise explanation, and
// the agent loop feeds that back to the model as an observation so it can fix
// its own query. The request URL is deliberately not included — it is not
// needed here and keeping it out means an error can never carry credentials
// into the run_events transcript.
type APIError struct {
	StatusCode int
	Messages   []string
}

func (e *APIError) Error() string {
	if len(e.Messages) == 0 {
		return fmt.Sprintf("jira: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("jira: HTTP %d: %s", e.StatusCode, strings.Join(e.Messages, "; "))
}

// NotFound reports whether the error is a 404. The seeder uses this to tell
// "this project does not exist yet" from a real failure.
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// Permanent reports whether retrying is pointless, which is how the agent loop
// decides whether to give a failed tool call a second attempt.
//
// Client already retried 429s and 5xx with backoff before the error escaped, so
// by the time one reaches the loop the only genuinely retryable case left is a
// server-side failure that outlasted this client's own budget.
func (e *APIError) Permanent() bool { return httpx.PermanentStatus(e.StatusCode) }

// get performs a GET and decodes the JSON response into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.http.Get(ctx, path, query, out)
}

// post performs a POST with a JSON body.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.http.Post(ctx, path, body, out)
}

// put performs a PUT with a JSON body.
func (c *Client) put(ctx context.Context, path string, body, out any) error {
	return c.http.Put(ctx, path, body, out)
}

// parseAPIError extracts Jira's error strings from a failure response.
func parseAPIError(status int, raw []byte) error {
	apiErr := &APIError{StatusCode: status}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return apiErr
	}

	// Jira reports failures in three shapes depending on the endpoint.
	var body struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
		ErrorMessage  string            `json:"errorMessage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not JSON at all (an HTML error page, say). The status code is the
		// only trustworthy signal, and the body is not worth surfacing.
		return apiErr
	}

	apiErr.Messages = append(apiErr.Messages, body.ErrorMessages...)
	if body.ErrorMessage != "" {
		apiErr.Messages = append(apiErr.Messages, body.ErrorMessage)
	}
	// Sorted: this message becomes a tool observation AND is persisted to
	// run_events, so map iteration order would make two identical runs differ.
	for _, field := range slices.Sorted(maps.Keys(body.Errors)) {
		apiErr.Messages = append(apiErr.Messages, fmt.Sprintf("%s: %s", field, body.Errors[field]))
	}
	return apiErr
}
