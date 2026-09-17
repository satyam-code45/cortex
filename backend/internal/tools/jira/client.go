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

	"cortex/internal/tools"
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

	// Projects confines this client to a set of project keys. Every JQL search
	// is ANDed with `project IN (...)`, issue-addressed reads refuse a key
	// outside the set, and project listing reports only these.
	//
	// It exists for the demo workspace, which is somebody's real Jira site
	// reachable by anyone who can sign in: the account's API token can see
	// every project on the site, so the confinement has to be enforced here,
	// on the way out, rather than hoped for from the model's JQL.
	Projects []string
	// AllowUnscoped permits an empty Projects. Required, and not merely
	// implied, so that a caller who forgot to pass the pin gets an error
	// instead of a client that can read the whole site. A user's own connected
	// Jira sets it: their site is theirs to search in full.
	AllowUnscoped bool

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
	// projects is the normalized, validated pin; empty means unscoped.
	projects []string
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

	// The pin is normalized and validated once, here, because it is
	// interpolated into JQL rather than parameterized — Jira has no bound
	// parameters. Anything that is not a well-formed project key never reaches
	// a query.
	projects := make([]string, 0, len(cfg.Projects))
	for _, key := range cfg.Projects {
		normalized := strings.ToUpper(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}
		if !projectKeyPattern.MatchString(normalized) {
			return nil, fmt.Errorf("jira: %q is not a valid project key", key)
		}
		if !slices.Contains(projects, normalized) {
			projects = append(projects, normalized)
		}
	}
	if len(projects) == 0 && !cfg.AllowUnscoped {
		return nil, errors.New("jira: Projects is required — an unscoped client could read every project on the site")
	}
	if len(projects) > 0 && cfg.AllowUnscoped {
		return nil, errors.New("jira: AllowUnscoped contradicts a non-empty Projects — pick one")
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

	return &Client{http: transport, baseURL: base, logger: logger, projects: projects}, nil
}

// Projects reports the project keys this client is confined to, empty for an
// unscoped client. It exists so a caller that must be scoped — or must not be —
// can assert which one it built, rather than trusting that it passed the right
// config.
func (c *Client) Projects() []string {
	return slices.Clone(c.projects)
}

// orderByAt reports whether a JQL ORDER BY keyword begins at jql[i].
//
// Hand-rolled rather than a regexp deliberately. The obvious spelling —
// re-running `\bORDER\s+BY\b` against jql[i:] at every candidate byte — is
// quadratic, and the query it scans is written by the model from text the agent
// has read, with no length bound between there and here. Measured on the
// original: 8 KB took 690ms and 100 KB took over two minutes, all of it
// synchronous on the API server before a single Jira request. This walks each
// byte a constant number of times.
//
// Keywords are case-insensitive and any run of whitespace separates the two
// words, matching what Jira accepts.
func orderByAt(jql string, i int) bool {
	if i > 0 && isJQLWord(jql[i-1]) {
		return false // mid-word, e.g. the tail of "REORDER"
	}
	j := skipFolded(jql, i, "ORDER")
	if j < 0 {
		return false
	}
	spaced := j
	for spaced < len(jql) && isJQLSpace(jql[spaced]) {
		spaced++
	}
	if spaced == j {
		return false // ORDER must be followed by whitespace
	}
	k := skipFolded(jql, spaced, "BY")
	if k < 0 {
		return false
	}
	return k == len(jql) || !isJQLWord(jql[k])
}

// skipFolded returns the index just past word if it appears at jql[i],
// compared case-insensitively, or -1.
func skipFolded(jql string, i int, word string) int {
	if i+len(word) > len(jql) {
		return -1
	}
	for n := range len(word) {
		c := jql[i+n]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != word[n] {
			return -1
		}
	}
	return i + len(word)
}

func isJQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// isJQLWord reports whether c can appear inside a JQL bareword, which is what
// makes ORDER a keyword here and part of an identifier there.
func isJQLWord(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') ||
		('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}

// splitOrderBy separates a JQL query's condition from its trailing ORDER BY.
//
// JQL requires the ordering last, so the scope cannot simply be prepended to a
// query that has one: `project IN (X) AND (foo ORDER BY updated)` is a syntax
// error. The condition is wrapped and the ordering re-appended after it.
//
// The scan skips string literals through the same jqlLexer parensBalanced uses,
// because `summary ~ 'order by priority'` is a search for a phrase, not an
// ordering, and splitting there would both corrupt the query and move the
// closing parenthesis somewhere it was not meant to go.
// Only an ORDER BY at parenthesis depth zero is the query's ordering. One
// inside parentheses is not a clause boundary the query can be cut at, and
// cutting there strands the matching `)` in the tail: `(ORDER BY )0` became
// `project IN (DEMO) AND (() ORDER BY )0`, where the wrapper closes early and
// the rest falls outside the scope. Depth is tracked in the same pass, off the
// same lexer, so the two can never disagree about which parentheses are real.
func splitOrderBy(jql string) (condition, order string) {
	var lex jqlLexer
	depth := 0
	for i := range len(jql) {
		if !lex.step(jql[i]) {
			continue
		}
		switch jql[i] {
		case '(':
			depth++
			continue
		case ')':
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		// Anchor on the keyword itself so that an ordering at position zero —
		// a query that is nothing but an ORDER BY — is found like any other.
		if orderByAt(jql, i) {
			return strings.TrimSpace(jql[:i]), strings.TrimSpace(jql[i:])
		}
	}
	return strings.TrimSpace(jql), ""
}

// scopedJQL confines a model-supplied JQL query to the pinned projects.
//
// The model is asked for JQL, so the query is attacker-influenced twice over:
// by whoever writes the question, and by whatever text the agent has already
// read from a Jira comment or an email. Confinement therefore cannot be a
// prompt instruction. The user's condition is parenthesized so that a top-level
// OR inside it cannot escape the AND — `project IN (DEMO) AND (a OR b)` is
// confined, `project IN (DEMO) AND a OR b` is not.
func (c *Client) scopedJQL(jql string) (string, error) {
	trimmed := strings.TrimSpace(jql)
	if len(c.projects) == 0 {
		return trimmed, nil
	}

	scope := "project IN (" + strings.Join(c.projects, ", ") + ")"
	if trimmed == "" {
		return scope, nil
	}

	// An unbalanced query would let the added parentheses change where the
	// condition ends, which is the one way a crafted string could break out of
	// the scope. Checked before the split, so an unbalanced quote cannot steer
	// where the ordering is found either. Rejected outright rather than
	// repaired, and wrapped as an invalid argument so the loop reports it to
	// the model instead of retrying an identically broken query.
	if !parensBalanced(trimmed) {
		return "", fmt.Errorf("the JQL has unbalanced parentheses or quotes, which is not allowed "+
			"while this Jira is scoped to %s; rewrite it with matched pairs: %w",
			strings.Join(c.projects, ", "), tools.ErrInvalidArgument)
	}

	condition, order := splitOrderBy(trimmed)
	switch {
	case condition == "" && order == "":
		return scope, nil
	case condition == "":
		return scope + " " + order, nil
	case order == "":
		return scope + " AND (" + condition + ")", nil
	default:
		return scope + " AND (" + condition + ") " + order, nil
	}
}

// jqlLexer tracks whether the byte being read sits inside a string literal.
//
// It exists because the confinement in scopedJQL is only as good as Cortex's
// agreement with Jira about where string literals start and end. JQL accepts
// BOTH ' and " as delimiters, and a backslash inside a literal escapes the next
// character. A scanner that knows only bare double quotes can be driven into
// "inside a string" state while Jira is outside one — and in that window it
// skips a `)` that Jira parses for real, closing the scope wrapper early:
//
//	summary ~ 'a"b' ) OR created >= -3650d AND (summary ~ 'c"d'
//
// wraps to `project IN (DEMO) AND (summary ~ 'a"b' ) OR created >= -3650d AND
// (summary ~ 'c"d')`, which Jira reads as `(scoped) OR (unscoped)` because AND
// binds tighter than OR — every issue on the site, no project key needed.
//
// One lexer shared by both scanners, deliberately. Two implementations of this
// that drift apart reintroduce exactly the desynchronization the type exists to
// prevent.
type jqlLexer struct {
	// quote is the delimiter that opened the current literal, 0 when outside one.
	quote byte
	// escaped records that the previous byte was a backslash inside a literal.
	escaped bool
}

// step consumes one byte and reports whether it lies outside a string literal.
func (l *jqlLexer) step(c byte) bool {
	switch {
	case l.escaped:
		l.escaped = false
	case l.quote != 0:
		switch c {
		case '\\':
			l.escaped = true
		case l.quote:
			l.quote = 0
		}
	case c == '\'' || c == '"':
		l.quote = c
	default:
		return true
	}
	return false
}

// unterminated reports whether the input ended inside a string literal.
func (l *jqlLexer) unterminated() bool { return l.quote != 0 }

// parensBalanced reports whether parentheses outside string literals are
// balanced and every literal is closed.
//
// Quoted text is skipped, so `summary ~ "Q2 plan (draft)"` and its
// single-quoted equivalent are ordinary queries and must not be rejected. An
// unterminated literal is rejected: the wrapper scopedJQL appends would land
// inside it.
func parensBalanced(jql string) bool {
	var lex jqlLexer
	depth := 0
	for i := range len(jql) {
		if !lex.step(jql[i]) {
			continue
		}
		switch jql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !lex.unterminated()
}

// requireAllowedIssue refuses an issue key outside the pinned projects.
//
// Searching is confined by scopedJQL, but an issue can also be addressed
// directly by key — and a key is guessable, so confining search alone would
// leave DEMO-1 reachable and SECRET-1 equally reachable to anyone who tries.
func (c *Client) requireAllowedIssue(key string) error {
	if len(c.projects) == 0 {
		return nil
	}
	project, _, found := strings.Cut(strings.ToUpper(strings.TrimSpace(key)), "-")
	if found && slices.Contains(c.projects, project) {
		return nil
	}
	return fmt.Errorf("issue %s is outside the projects this Cortex deployment can read (%s): %w",
		key, strings.Join(c.projects, ", "), tools.ErrInvalidArgument)
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
