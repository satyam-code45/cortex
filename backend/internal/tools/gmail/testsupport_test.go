package gmail_test

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
	"time"

	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
)

// TEST-3.2 / TEST-3.5 support: a fake Gmail API and a fake Google token
// endpoint.
//
// No test in this package may touch the network — the fixtures under testdata/
// are recorded response shapes, and every request is served by httptest. The
// requests are recorded too, because REQ-3.2 constrains them: the bearer token,
// the search query (including the optional GMAIL_QUERY_SCOPE narrowing) and the
// message format are properties of what we send, not of what we parse.

const (
	testAccessToken  = "ya29.not-a-real-access-token"
	testRefreshToken = "1//not-a-real-refresh-token"
	testClientID     = "1234567890-cortex.apps.googleusercontent.com"
	testClientSecret = "GOCSPX-not-a-real-secret"
)

// recordedRequest is one request the fake API received.
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

// fakeGmail is an httptest-backed Gmail API.
type fakeGmail struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	routes   map[string]*route
	hits     map[string]int
	requests []recordedRequest
}

func newFakeGmail(t *testing.T, routes map[string]*route) *fakeGmail {
	t.Helper()
	f := &fakeGmail{t: t, routes: routes, hits: make(map[string]int)}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGmail) handle(w http.ResponseWriter, r *http.Request) {
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
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"status":"NOT_FOUND",` +
			`"message":"fakeGmail: no route for ` + r.URL.Path + `"}}`))
		return
	}
	if index >= len(rt.responses) {
		f.t.Errorf("fakeGmail: %s requested %d time(s), only %d response(s) scripted",
			r.URL.Path, index+1, len(rt.responses))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"status":"INTERNAL",` +
			`"message":"fakeGmail: response sequence exhausted"}}`))
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
func (f *fakeGmail) requestsTo(path string) []recordedRequest {
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

func (f *fakeGmail) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// tokenSource builds a TokenSource holding an access token that is valid for
// the whole test, so no Gmail test depends on the OAuth path. The token
// endpoint still points at the fake server: a test that accidentally triggers a
// refresh gets a routing failure, not a call to Google.
func (f *fakeGmail) tokenSource() *gmail.TokenSource {
	f.t.Helper()
	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: &gmail.Credentials{
			ClientID:     testClientID,
			ClientSecret: testClientSecret,
			TokenURI:     f.server.URL + "/token",
		},
		Token: &gmail.Token{
			RefreshToken: testRefreshToken,
			AccessToken:  testAccessToken,
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(time.Hour),
		},
		HTTPClient: f.server.Client(),
	})
	if err != nil {
		f.t.Fatalf("build token source: %v", err)
	}
	return source
}

// client builds a Client pointed at the fake API.
//
// MinInterval is negative to disable the request throttle: it exists to keep a
// fan-out of metadata fetches under Gmail's per-user budget, and paying it per
// request here would only slow the suite down.
func (f *fakeGmail) client(queryScope string) *gmail.Client {
	f.t.Helper()
	c, err := gmail.NewClient(gmail.Config{
		BaseURL:     f.server.URL,
		TokenSource: f.tokenSource(),
		QueryScope:  queryScope,
		HTTPClient:  f.server.Client(),
		MaxRetries:  1,
		MinInterval: -1,
		Logger:      discardLogger(),
	})
	if err != nil {
		f.t.Fatalf("build gmail client: %v", err)
	}
	return c
}

// tool returns one tool by name, failing when it is not registered.
func (f *fakeGmail) tool(name string) tools.Tool {
	f.t.Helper()
	return f.scopedTool(name, "")
}

// scopedTool returns one tool built against a client with the given
// GMAIL_QUERY_SCOPE narrowing.
func (f *fakeGmail) scopedTool(name, queryScope string) tools.Tool {
	f.t.Helper()
	for _, tool := range gmail.NewTools(f.client(queryScope)) {
		if tool.Name() == name {
			return tool
		}
	}
	f.t.Fatalf("tool %q is not in the Gmail tool set", name)
	return nil
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

// Gmail REST paths used by the tools.
const (
	pathMessages  = "/gmail/v1/users/me/messages"
	testMessageID = "18f2a3b4c5d6e7f8"
	pathMessage1  = pathMessages + "/18f2a3b4c5d6e7f8"
	pathMessage2  = pathMessages + "/18f2a3b4c5d6e7f9"
)
