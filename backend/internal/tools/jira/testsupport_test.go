package jira_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"cortex/internal/tools"
	"cortex/internal/tools/jira"
)

// Test support: a fake Jira site.
//
// No test in this package may touch the network — the fixtures under testdata/
// are recorded response shapes, and every request is served by httptest. That
// also makes the *requests* assertable, which matters for the endpoint
// migration: the client must call GET /rest/api/3/search/jql and must never
// fall back to the removed /rest/api/3/search (410 Gone on a real site).

const (
	testEmail    = "dev@cortex.test"
	testAPIToken = "not-a-real-token"
)

// recordedRequest is one request the fake site received.
type recordedRequest struct {
	method string
	path   string
	query  url.Values
	// authUser is the basic-auth user the client sent, if any.
	authUser string
	authOK   bool
}

// response is one scripted HTTP response.
type response struct {
	// status defaults to 200.
	status int
	// fixture is a file under testdata/, served as the body.
	fixture string
	// body overrides fixture when set.
	body string
}

// route is the scripted response sequence for one path: the first request gets
// responses[0], the second responses[1], and so on. A request past the end is a
// test failure, which is what makes "how many times did the client call this?"
// assertable.
type route struct {
	responses []response
}

// fixtureRoute serves one fixture per request, in order.
func fixtureRoute(names ...string) *route {
	r := &route{}
	for _, name := range names {
		r.responses = append(r.responses, response{fixture: name})
	}
	return r
}

// bodyRoute serves a single literal body with the given status.
func bodyRoute(status int, body string) *route {
	return &route{responses: []response{{status: status, body: body}}}
}

// sequenceRoute serves an explicit response sequence.
func sequenceRoute(responses ...response) *route {
	return &route{responses: responses}
}

// fakeJira is an httptest-backed Jira Cloud site.
type fakeJira struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	routes   map[string]*route
	hits     map[string]int
	requests []recordedRequest
}

func newFakeJira(t *testing.T, routes map[string]*route) *fakeJira {
	t.Helper()
	f := &fakeJira{t: t, routes: routes, hits: make(map[string]int)}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeJira) handle(w http.ResponseWriter, r *http.Request) {
	user, _, authOK := r.BasicAuth()

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		method:   r.Method,
		path:     r.URL.Path,
		query:    r.URL.Query(),
		authUser: user,
		authOK:   authOK,
	})
	index := f.hits[r.URL.Path]
	f.hits[r.URL.Path]++
	rt := f.routes[r.URL.Path]
	f.mu.Unlock()

	if rt == nil {
		// An unrouted path is a test failure: it means the client called an
		// endpoint the test did not expect.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["fakeJira: no route for ` + r.URL.Path + `"]}`))
		return
	}

	if index >= len(rt.responses) {
		f.t.Errorf("fakeJira: %s requested %d time(s), only %d response(s) scripted",
			r.URL.Path, index+1, len(rt.responses))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errorMessages":["fakeJira: response sequence exhausted"]}`))
		return
	}
	scripted := rt.responses[index]

	status := scripted.status
	if status == 0 {
		status = http.StatusOK
	}
	body := scripted.body
	if body == "" && scripted.fixture != "" {
		body = readFixture(f.t, scripted.fixture)
	}
	if body == "" {
		body = "{}"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// requestsTo returns the requests received for a path.
func (f *fakeJira) requestsTo(path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeJira) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// client builds an unscoped Client pointed at the fake site.
//
// Unscoped because these tests assert what the tools do with a response, not
// how a project pin rewrites a request — the pin has its own tests in
// scope_test.go, which use scopedClient below. A test that wants to see the
// exact JQL the client sends should use the scoped one.
//
// MinInterval is negative to disable the request throttle: the throttle exists
// to keep the seeder under Jira's rate limit, and paying 120ms per request here
// would only slow the suite down.
func (f *fakeJira) client() *jira.Client {
	f.t.Helper()
	return f.buildClient(nil, true)
}

// scopedClient builds a Client confined to the given project keys, as the demo
// workspace's client is.
func (f *fakeJira) scopedClient(projects ...string) *jira.Client {
	f.t.Helper()
	return f.buildClient(projects, false)
}

func (f *fakeJira) buildClient(projects []string, allowUnscoped bool) *jira.Client {
	f.t.Helper()
	c, err := jira.NewClient(jira.Config{
		BaseURL:       f.server.URL,
		Email:         testEmail,
		APIToken:      testAPIToken,
		Projects:      projects,
		AllowUnscoped: allowUnscoped,
		HTTPClient:    f.server.Client(),
		MaxRetries:    1,
		MinInterval:   -1,
		Logger:        discardLogger(),
	})
	if err != nil {
		f.t.Fatalf("build jira client: %v", err)
	}
	return c
}

// scopedToolSet builds the tool set against a project-confined client.
func (f *fakeJira) scopedToolSet(projects ...string) map[string]tools.Tool {
	f.t.Helper()
	byName := make(map[string]tools.Tool)
	for _, tool := range jira.NewTools(f.scopedClient(projects...)) {
		byName[tool.Name()] = tool
	}
	return byName
}

// toolSet builds the four tools against the fake site, keyed by name.
func (f *fakeJira) toolSet() (*jira.Client, map[string]tools.Tool) {
	f.t.Helper()
	client := f.client()
	byName := make(map[string]tools.Tool)
	for _, tool := range jira.NewTools(client) {
		byName[tool.Name()] = tool
	}
	return client, byName
}

// tool returns one tool by name, failing when it is not registered.
func (f *fakeJira) tool(name string) (*jira.Client, tools.Tool) {
	f.t.Helper()
	client, byName := f.toolSet()
	tool, ok := byName[name]
	if !ok {
		f.t.Fatalf("tool %q is not in the Jira tool set", name)
	}
	return client, tool
}

// readFixture loads a recorded response body.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Jira REST paths used by the tools.
const (
	pathSearchJQL    = "/rest/api/3/search/jql"
	pathSearchLegacy = "/rest/api/3/search"
	pathIssue        = "/rest/api/3/issue/ATLAS-101"
	pathChangelog    = "/rest/api/3/issue/ATLAS-101/changelog"
	pathComments     = "/rest/api/3/issue/ATLAS-101/comment"
)
