// Package httpx is the shared HTTP core behind every outbound integration:
// Jira, Notion, and Gmail.
//
// The three services differ in almost everything a REST client cares about —
// how they authenticate, how they spell an error, which paths they expose — but
// they are identical in the parts that are genuinely hard to get right:
//
//   - Retrying a throttled or server-side failure with exponential backoff,
//     while honouring a server-supplied Retry-After instead of guessing.
//   - Serializing a process's own requests so concurrent agent tool calls queue
//     rather than arriving as a burst against a per-account budget.
//   - Capping how much of a response is buffered, so one pathological document
//     cannot materialize megabytes.
//   - Never letting a credential or a request URL escape into an error string,
//     because tool errors become observations the model reads and rows in
//     run_events that a trace renders.
//
// Two hooks carry all the per-service variation. Authorize decorates a request
// with credentials, and ParseError turns a non-2xx response into the caller's
// own error type. Everything else is shared.
package httpx

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
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults applied when the corresponding Config field is zero.
const (
	// DefaultTimeout bounds a single HTTP round trip. The agent loop imposes its
	// own, shorter per-tool deadline; this is the backstop for callers without
	// one, notably the seeder.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is how many times a request is retried after a
	// throttling or server-side failure.
	DefaultMaxRetries = 4

	// DefaultMinInterval is the minimum gap between requests from one client.
	// It keeps bulk work under a provider's cost budget instead of discovering
	// the limit by being throttled.
	DefaultMinInterval = 120 * time.Millisecond

	// DefaultMaxRetryDelay caps a single backoff wait. It is deliberately short
	// for the agent path, where a tool call sits inside a per-tool budget.
	// Bulk callers raise it — see cmd/seed — because a provider answers a
	// throttled bulk write with a Retry-After of 30-60s, and clamping that to 8s
	// burns the whole retry budget inside the window the provider asked us to
	// wait, failing a request that would have succeeded.
	DefaultMaxRetryDelay = 8 * time.Second

	// DefaultMaxResponseBytes caps a successful response body. Nothing Cortex
	// reads is legitimately larger, and an unbounded decode means one maximal
	// document can materialize megabytes.
	DefaultMaxResponseBytes = 4 << 20

	// DefaultMaxErrorBodyBytes caps how much of an error response is read. A
	// service can return an HTML error page, and the whole thing is not worth
	// buffering.
	DefaultMaxErrorBodyBytes = 8 << 10

	// retryBaseDelay is the first backoff step; it doubles per attempt.
	retryBaseDelay = 500 * time.Millisecond
)

// Config configures a Client.
type Config struct {
	// Name identifies the service in error and log messages, e.g. "jira".
	Name string
	// BaseURL is the API root that paths are joined onto. It must be http(s)
	// and is stored without a trailing slash.
	BaseURL string

	// HTTPClient is optional; a timeout-bearing client is built when nil.
	HTTPClient *http.Client
	// MaxRetries defaults to DefaultMaxRetries.
	MaxRetries int
	// MinInterval defaults to DefaultMinInterval. A negative value disables
	// throttling, which is what tests use to avoid paying it per request.
	MinInterval time.Duration
	// MaxRetryDelay caps one backoff wait; it defaults to DefaultMaxRetryDelay.
	MaxRetryDelay time.Duration
	// MaxResponseBytes defaults to DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// MaxErrorBodyBytes defaults to DefaultMaxErrorBodyBytes.
	MaxErrorBodyBytes int64

	// Headers are sent on every request, e.g. a pinned API version.
	Headers map[string]string

	// Authorize adds credentials to a request.
	//
	// It is called once per *attempt* rather than once per request, so a token
	// refreshed between a 401 and its retry is actually used. Returning an
	// error aborts without a round trip, which is how an unusable cached
	// credential surfaces as itself instead of as an opaque 401.
	Authorize func(ctx context.Context, req *http.Request) error

	// ParseError converts a non-2xx response into the caller's error type. The
	// body is already read and capped. When nil, a *StatusError is returned.
	//
	// Each service keeps its own error type because each one spells failure
	// differently, and because the agent loop classifies errors by asking the
	// concrete type whether a retry could help.
	ParseError func(status int, body []byte) error

	// Logger receives retry and throttle diagnostics.
	Logger *slog.Logger
}

// Client is one service's HTTP transport.
//
// It is safe for concurrent use and deliberately serializes its own requests
// through a throttle: the agent may run several tool calls at once, and a
// provider counts them against a single per-account budget.
type Client struct {
	name              string
	baseURL           string
	httpClient        *http.Client
	maxRetries        int
	minInterval       time.Duration
	maxRetryDelay     time.Duration
	maxResponseBytes  int64
	maxErrorBodyBytes int64
	headers           map[string]string
	authorize         func(context.Context, *http.Request) error
	parseError        func(int, []byte) error
	logger            *slog.Logger

	// mu guards next, and is held across the throttle's sleep so that
	// concurrent callers queue rather than all firing at once.
	mu   sync.Mutex
	next time.Time
}

// New validates the configuration and builds a Client.
func New(cfg Config) (*Client, error) {
	name := cfg.Name
	if name == "" {
		name = "httpx"
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("%s: BaseURL is required", name)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("%s: BaseURL is not a valid URL: %w", name, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("%s: BaseURL must be http(s), got %q", name, parsed.Scheme)
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
	maxResponseBytes := cfg.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = DefaultMaxResponseBytes
	}
	maxErrorBodyBytes := cfg.MaxErrorBodyBytes
	if maxErrorBodyBytes <= 0 {
		maxErrorBodyBytes = DefaultMaxErrorBodyBytes
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Copied so a caller mutating its map afterwards cannot change what this
	// client sends.
	headers := make(map[string]string, len(cfg.Headers))
	maps.Copy(headers, cfg.Headers)

	return &Client{
		name:              name,
		baseURL:           base,
		httpClient:        httpClient,
		maxRetries:        maxRetries,
		minInterval:       minInterval,
		maxRetryDelay:     maxRetryDelay,
		maxResponseBytes:  maxResponseBytes,
		maxErrorBodyBytes: maxErrorBodyBytes,
		headers:           headers,
		authorize:         cfg.Authorize,
		parseError:        cfg.ParseError,
		logger:            logger,
	}, nil
}

// Request describes one call.
type Request struct {
	// Method is the HTTP method; it defaults to GET.
	Method string
	// Path is joined onto the client's BaseURL, e.g. "/v1/search".
	Path string
	// Query is appended as the query string.
	Query url.Values
	// Body is JSON-encoded when non-nil. Ignored when RawBody is set.
	Body any
	// RawBody is sent verbatim, for the endpoints that are not JSON.
	RawBody []byte
	// ContentType overrides the default of application/json for a body.
	ContentType string
	// Header carries per-request headers, merged over the client's.
	Header http.Header
}

// StatusError is the fallback error when a Config supplies no ParseError.
type StatusError struct {
	// Service is the Config.Name of the client that produced it.
	Service string
	// StatusCode is the HTTP status.
	StatusCode int
	// Body is the (capped) response body.
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Service, e.StatusCode)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Service, e.StatusCode, e.Body)
}

// Do issues a request, retrying throttled and server-side failures, and decodes
// a JSON response into out. A nil out drains and discards the body.
//
// The body is marshalled once up front rather than per attempt: a retry needs
// to replay the same bytes, and an io.Reader cannot be rewound.
func (c *Client) Do(ctx context.Context, req Request, out any) error {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	payload := req.RawBody
	contentType := req.ContentType
	if payload == nil && req.Body != nil {
		encoded, err := json.Marshal(req.Body)
		if err != nil {
			return fmt.Errorf("%s: encode request body: %w", c.name, err)
		}
		payload = encoded
		if contentType == "" {
			contentType = "application/json"
		}
	}

	endpoint := c.baseURL + req.Path
	if len(req.Query) > 0 {
		endpoint += "?" + req.Query.Encode()
	}

	var (
		lastErr error
		// retryAfter carries a server-specified delay from one attempt to the
		// next. Tracking it here rather than by wrapping lastErr keeps the error
		// that escapes on the non-retryable path exactly what ParseError
		// returned, with no shim type in between for a reader to decode.
		retryAfter time.Duration
	)
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt, retryAfter, c.maxRetryDelay)
			c.logger.Warn(c.name+": retrying request",
				"method", method, "path", req.Path, "attempt", attempt, "delay", delay, "error", lastErr)
			if err := Sleep(ctx, delay); err != nil {
				return err
			}
		}
		if err := c.throttle(ctx); err != nil {
			return err
		}

		result := c.attempt(ctx, method, endpoint, payload, contentType, req.Header, out)
		if result.err == nil {
			return nil
		}
		lastErr = result.err
		retryAfter = result.retryAfter
		if !result.retryable {
			return result.err
		}
		// A cancelled context is never worth retrying.
		if ctx.Err() != nil {
			return result.err
		}
	}
	return fmt.Errorf("%s: %s %s failed after %d attempts: %w",
		c.name, method, req.Path, c.maxRetries+1, lastErr)
}

// Get issues a GET and decodes the JSON response into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.Do(ctx, Request{Method: http.MethodGet, Path: path, Query: query}, out)
}

// Post issues a POST with a JSON body.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, Request{Method: http.MethodPost, Path: path, Body: body}, out)
}

// Put issues a PUT with a JSON body.
func (c *Client) Put(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, Request{Method: http.MethodPut, Path: path, Body: body}, out)
}

// attemptResult is the outcome of a single round trip.
type attemptResult struct {
	err        error
	retryable  bool
	retryAfter time.Duration
}

// attempt performs a single round trip.
func (c *Client) attempt(
	ctx context.Context,
	method, endpoint string,
	payload []byte,
	contentType string,
	header http.Header,
	out any,
) attemptResult {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return attemptResult{err: fmt.Errorf("%s: build request: %w", c.name, err)}
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	for k, values := range header {
		req.Header.Del(k)
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	if payload != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.authorize != nil {
		if err := c.authorize(ctx, req); err != nil {
			// Not retryable: a credential that could not be produced now will
			// not appear on its own a second later.
			return attemptResult{err: fmt.Errorf("%s: authorize request: %w", c.name, err)}
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport failures (connection reset, timeout) are usually transient.
		// The URL is stripped from the message: it is not needed, and *url.Error
		// renders it verbatim — which for some services means credentials.
		return attemptResult{
			err:       fmt.Errorf("%s: %s request failed: %w", c.name, method, TransportError(err)),
			retryable: true,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // response body close on a read-only path

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			// Drain so the connection can be reused rather than dropped.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.maxErrorBodyBytes))
			return attemptResult{}
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, c.maxResponseBytes)).Decode(out); err != nil {
			return attemptResult{err: fmt.Errorf("%s: decode response: %w", c.name, err)}
		}
		return attemptResult{}
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, c.maxErrorBodyBytes))
	return attemptResult{
		err:        c.buildError(resp.StatusCode, raw),
		retryable:  isRetryable(resp.StatusCode),
		retryAfter: RetryAfter(resp.Header.Get("Retry-After")),
	}
}

// buildError turns a failure response into the caller's error type.
func (c *Client) buildError(status int, body []byte) error {
	if c.parseError != nil {
		return c.parseError(status, body)
	}
	return &StatusError{
		Service:    c.name,
		StatusCode: status,
		Body:       strings.TrimSpace(string(body)),
	}
}

// isRetryable reports whether a status is worth another attempt.
func isRetryable(status int) bool {
	return !PermanentStatus(status)
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
		if err := Sleep(ctx, wait); err != nil {
			return err
		}
	}
	c.next = time.Now().Add(c.minInterval)
	return nil
}

// backoff picks the delay before the given attempt, preferring a
// server-supplied Retry-After over exponential growth.
func backoff(attempt int, retryAfter, maxDelay time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, maxDelay)
	}
	delay := time.Duration(float64(retryBaseDelay) * math.Pow(2, float64(attempt-1)))
	return min(delay, maxDelay)
}

// PermanentStatus reports whether a status makes retrying pointless.
//
// It is the mirror of the default Retryable, and every integration's error type
// exposes it as Permanent() so the agent loop can decide whether to give a
// failed tool call a second attempt. A client has already exhausted its own
// retry budget by the time an error reaches the loop, so the only genuinely
// retryable case left is a server-side failure that outlasted it.
func PermanentStatus(status int) bool {
	return status < 500 && status != http.StatusTooManyRequests
}

// RetryAfter parses a Retry-After header value, which services send as either a
// count of seconds or an HTTP date. It returns 0 when absent or unparseable.
func RetryAfter(value string) time.Duration {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(trimmed); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(trimmed); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// TransportError strips the request URL from a transport-level error, so a
// credential embedded in a URL can never reach a log line or run_events.
func TransportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// Sleep waits for d, or returns early if the context is cancelled.
func Sleep(ctx context.Context, d time.Duration) error {
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
