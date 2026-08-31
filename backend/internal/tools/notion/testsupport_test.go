package notion_test

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
	"cortex/internal/tools/notion"
)

// Test support: a fake Notion workspace.
//
// No test in this package may touch the network — the fixtures under testdata/
// are recorded response shapes, and every request is served by httptest. The
// requests are recorded too, because the contract constrains them: the pinned
// Notion-Version header, the bearer token, and the `object=page` search filter
// are all properties of what we send, not of what we parse.

const testToken = "secret_not-a-real-notion-token"

// recordedRequest is one request the fake workspace received.
type recordedRequest struct {
	method string
	path   string
	query  url.Values
	header http.Header
	body   string
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

// fakeNotion is an httptest-backed Notion workspace.
type fakeNotion struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	routes   map[string]*route
	hits     map[string]int
	requests []recordedRequest
}

func newFakeNotion(t *testing.T, routes map[string]*route) *fakeNotion {
	t.Helper()
	f := &fakeNotion{t: t, routes: routes, hits: make(map[string]int)}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeNotion) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		header: r.Header.Clone(),
		body:   string(raw),
	})
	index := f.hits[r.URL.Path]
	f.hits[r.URL.Path]++
	rt := f.routes[r.URL.Path]
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if rt == nil {
		// An unrouted path is a test failure: it means the client called an
		// endpoint the test did not expect.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"object":"error","status":404,"code":"object_not_found",` +
			`"message":"fakeNotion: no route for ` + r.URL.Path + `"}`))
		return
	}
	if index >= len(rt.responses) {
		f.t.Errorf("fakeNotion: %s requested %d time(s), only %d response(s) scripted",
			r.URL.Path, index+1, len(rt.responses))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"object":"error","status":500,"code":"internal_server_error",` +
			`"message":"fakeNotion: response sequence exhausted"}`))
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

	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// requestsTo returns the requests received for a path.
func (f *fakeNotion) requestsTo(path string) []recordedRequest {
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

func (f *fakeNotion) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// client builds a Client pointed at the fake workspace.
//
// MinInterval is negative to disable the request throttle: it exists to keep
// the seeder under Notion's three-requests-per-second budget, and paying it per
// request here would only slow the suite down.
func (f *fakeNotion) client() *notion.Client {
	f.t.Helper()
	c, err := notion.NewClient(notion.Config{
		BaseURL:     f.server.URL,
		Token:       testToken,
		HTTPClient:  f.server.Client(),
		MaxRetries:  1,
		MinInterval: -1,
		Logger:      discardLogger(),
	})
	if err != nil {
		f.t.Fatalf("build notion client: %v", err)
	}
	return c
}

// tool returns one tool by name, failing when it is not registered.
func (f *fakeNotion) tool(name string) (*notion.Client, tools.Tool) {
	f.t.Helper()
	client := f.client()
	for _, tool := range notion.NewTools(client) {
		if tool.Name() == name {
			return client, tool
		}
	}
	f.t.Fatalf("tool %q is not in the Notion tool set", name)
	return nil, nil
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

// Notion REST paths used by the tools.
const (
	pathSearch      = "/v1/search"
	testPageID      = "3c34fab1-2b9b-80d6-8cfb-c64d5b8cebe0"
	testPageURL     = "https://www.notion.so/Atlas-Q2-Plan-3c34fab12b9b80d68cfbc64d5b8cebe0"
	testPageTitle   = "Atlas Q2 Plan"
	testPageEdited  = "2026-06-18T14:22:31Z"
	pathPage        = "/v1/pages/" + testPageID
	pathRootBlocks  = "/v1/blocks/" + testPageID + "/children"
	nestedBlockID   = "1a2b3c4d-0000-0000-0000-000000000003"
	pathChildBlocks = "/v1/blocks/" + nestedBlockID + "/children"
)
