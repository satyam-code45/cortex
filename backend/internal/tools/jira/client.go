// Package jira holds a thin Jira Cloud REST v3 client and the agent tools built
// on it.
//
// "Thin" is the design: no ORM over Jira's object model, no caching layer, and
// no attempt to model every field. Jira issue JSON is enormous — a single issue
// with all fields expanded runs to tens of kilobytes — and every byte that
// reaches the model is paid for on each subsequent iteration of the agent loop.
// So each tool asks for exactly the fields it needs and renders them as compact
// lines.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds a single HTTP round trip to Jira. The agent loop
	// imposes its own, shorter per-tool deadline; this is the backstop for
	// callers without one, notably the seeder.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is how many times a request is retried after a
	// throttling or server-side failure. Jira Cloud rate-limits aggressively
	// during bulk writes, which is the seeder's whole workload.
	DefaultMaxRetries = 4

	// DefaultMinInterval is the minimum gap between requests from one client.
	// It keeps the seeder's ~600 sequential writes under Jira's cost budget
	// instead of discovering the limit by being throttled.
	DefaultMinInterval = 120 * time.Millisecond

	// retryBaseDelay is the first backoff step; it doubles per attempt.
	retryBaseDelay = 500 * time.Millisecond
	// DefaultMaxRetryDelay caps a single backoff wait. It is deliberately short
	// for the agent path, where a tool call sits inside a 15s per-tool budget.
	// The seeder overrides it: Jira Cloud answers a throttled bulk write with a
	// Retry-After of 30-60s, and clamping that to 8s burns the whole retry budget
	// inside the window Jira asked us to wait, failing a request that would have
	// succeeded.
	DefaultMaxRetryDelay = 8 * time.Second

	// maxResponseBytes caps a successful response body. Nothing Cortex reads is
	// legitimately larger, and an unbounded decode means one issue with 50 maximal
	// comments can materialize ~1.6MB.
	maxResponseBytes = 4 << 20

	// maxErrorBodyBytes caps how much of an error response is read. Jira can
	// return an HTML error page, and the whole thing is not worth buffering.
	maxErrorBodyBytes = 8 << 10
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
	baseURL       string
	email         string
	apiToken      string
	httpClient    *http.Client
	maxRetries    int
	minInterval   time.Duration
	maxRetryDelay time.Duration
	logger        *slog.Logger

	// mu guards next, and is held across the throttle's sleep so that
	// concurrent callers queue rather than all firing at once.
	mu   sync.Mutex
	next time.Time
}

// NewClient validates the configuration and builds a Client.
func NewClient(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("jira: BaseURL is required")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("jira: BaseURL is not a valid URL: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("jira: BaseURL must be http(s), got %q", parsed.Scheme)
	}
	if cfg.Email == "" {
		return nil, errors.New("jira: Email is required")
	}
	if cfg.APIToken == "" {
		return nil, errors.New("jira: APIToken is required")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	minInterval := cfg.MinInterval
	if minInterval == 0 {
		minInterval = DefaultMinInterval
	}
	if minInterval < 0 {
		minInterval = 0
	}
	maxRetryDelay := cfg.MaxRetryDelay
	if maxRetryDelay <= 0 {
		maxRetryDelay = DefaultMaxRetryDelay
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		baseURL:       base,
		email:         cfg.Email,
		apiToken:      cfg.APIToken,
		httpClient:    httpClient,
		maxRetries:    maxRetries,
		minInterval:   minInterval,
		maxRetryDelay: maxRetryDelay,
		logger:        logger,
	}, nil
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
func (e *APIError) Permanent() bool {
	return e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
}

// get performs a GET and decodes the JSON response into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// post performs a POST with a JSON body.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

// put performs a PUT with a JSON body.
func (c *Client) put(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPut, path, nil, body, out)
}

// do issues one request, retrying throttled and server-side failures.
//
// The body is marshalled once up front rather than per attempt: a retry needs
// to replay the same bytes, and an io.Reader cannot be rewound.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("jira: encode request body: %w", err)
		}
		payload = encoded
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt, lastErr, c.maxRetryDelay)
			c.logger.Warn("jira: retrying request",
				"method", method, "path", path, "attempt", attempt, "delay", delay, "error", lastErr)
			if err := sleep(ctx, delay); err != nil {
				return err
			}
		}
		if err := c.throttle(ctx); err != nil {
			return err
		}

		retryable, err := c.attempt(ctx, method, endpoint, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
		// A cancelled context is never worth retrying.
		if ctx.Err() != nil {
			return err
		}
	}
	return fmt.Errorf("jira: %s %s failed after %d attempts: %w", method, path, c.maxRetries+1, lastErr)
}

// attempt performs a single round trip. It reports whether the failure is worth
// retrying.
func (c *Client) attempt(ctx context.Context, method, endpoint string, payload []byte, out any) (retryable bool, err error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return false, fmt.Errorf("jira: build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.apiToken)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport failures (connection reset, timeout) are usually transient.
		// The URL is stripped from the message: it is not needed, and *url.Error
		// renders it verbatim.
		return true, fmt.Errorf("jira: %s request failed: %w", method, transportError(err))
	}
	defer resp.Body.Close() //nolint:errcheck // response body close on a read-only path

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			// Drain so the connection can be reused rather than dropped.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
			return false, nil
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
			return false, fmt.Errorf("jira: decode response: %w", err)
		}
		return false, nil
	}

	apiErr := parseAPIError(resp)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, &retryAfterError{APIError: apiErr, after: retryAfter(resp)}
	case resp.StatusCode >= 500:
		return true, apiErr
	default:
		return false, apiErr
	}
}

// throttle enforces the minimum gap between requests.
//
// The lock is intentionally held across the sleep: that is what makes
// concurrent callers form a queue instead of all waking at once and firing
// together, which would defeat the point.
func (c *Client) throttle(ctx context.Context) error {
	if c.minInterval <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if wait := time.Until(c.next); wait > 0 {
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
	c.next = time.Now().Add(c.minInterval)
	return nil
}

// retryAfterError carries a server-specified retry delay alongside the API
// error, so backoff can honour Jira's own instruction instead of guessing.
type retryAfterError struct {
	*APIError
	after time.Duration
}

// backoff picks the delay before the given attempt, preferring a
// server-supplied Retry-After over exponential growth.
func backoff(attempt int, lastErr error, maxDelay time.Duration) time.Duration {
	var retry *retryAfterError
	if errors.As(lastErr, &retry) && retry.after > 0 {
		return min(retry.after, maxDelay)
	}
	delay := time.Duration(float64(retryBaseDelay) * math.Pow(2, float64(attempt-1)))
	return min(delay, maxDelay)
}

// retryAfter reads the Retry-After header, which Jira sends as either a count
// of seconds or an HTTP date.
func retryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// parseAPIError extracts Jira's error strings from a failure response.
func parseAPIError(resp *http.Response) *APIError {
	apiErr := &APIError{StatusCode: resp.StatusCode}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
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

// transportError strips the request URL from a transport-level error.
func transportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// sleep waits for d, or returns early if the context is cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
