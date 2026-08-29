package httpx_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cortex/internal/tools/httpx"
)

// TEST-6.2 (transport level, spec A4) — the shared HTTP core retries throttled
// and server-side failures with backoff, honours Retry-After, and treats
// ordinary 4xx as permanent: a 429 is retried, a 400 is not.

// recordingServer scripts a sequence of status codes and records when each
// request arrived, so a test can assert both the retry count and that a real
// wait separated the attempts.
type recordingServer struct {
	t *testing.T

	mu sync.Mutex
	// responses is consumed one per request; the last entry repeats.
	responses []scriptedResponse
	arrivals  []time.Time

	*httptest.Server
}

type scriptedResponse struct {
	status int
	header map[string]string
	body   string
}

func newRecordingServer(t *testing.T, responses ...scriptedResponse) *recordingServer {
	t.Helper()
	s := &recordingServer{t: t, responses: responses}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		index := len(s.arrivals)
		s.arrivals = append(s.arrivals, time.Now())
		if index >= len(s.responses) {
			index = len(s.responses) - 1
		}
		resp := s.responses[index]
		s.mu.Unlock()

		for k, v := range resp.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.status)
		io.WriteString(w, resp.body) //nolint:errcheck // test server
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *recordingServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.arrivals)
}

// gap returns the wait between request i and i+1.
func (s *recordingServer) gap(t *testing.T, i int) time.Duration {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i+1 >= len(s.arrivals) {
		t.Fatalf("gap(%d): only %d requests arrived", i, len(s.arrivals))
	}
	return s.arrivals[i+1].Sub(s.arrivals[i])
}

func newClient(t *testing.T, baseURL string, maxRetries int, maxRetryDelay time.Duration) *httpx.Client {
	t.Helper()
	client, err := httpx.New(httpx.Config{
		Name:    "testsvc",
		BaseURL: baseURL,
		// Negative disables the inter-request throttle so the only waits the
		// test observes are the retry backoffs.
		MinInterval:   -1,
		MaxRetries:    maxRetries,
		MaxRetryDelay: maxRetryDelay,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	return client
}

func TestDoRetries429WithBackoff(t *testing.T) {
	const retryDelay = 30 * time.Millisecond
	server := newRecordingServer(t,
		scriptedResponse{status: http.StatusTooManyRequests, body: `{"error":"throttled"}`},
		scriptedResponse{status: http.StatusTooManyRequests, body: `{"error":"throttled"}`},
		scriptedResponse{status: http.StatusOK, body: `{"ok":true}`},
	)
	client := newClient(t, server.URL, 4, retryDelay)

	var out struct {
		OK bool `json:"ok"`
	}
	if err := client.Get(context.Background(), "/v1/search", nil, &out); err != nil {
		t.Fatalf("Get after two 429s: %v", err)
	}
	if !out.OK {
		t.Error("response was not decoded after the successful retry")
	}
	if got := server.requestCount(); got != 3 {
		t.Errorf("requests = %d, want 3 (two 429s, then success)", got)
	}
	// Backoff means the retries actually waited, not fired immediately.
	for i := 0; i < 2; i++ {
		if gap := server.gap(t, i); gap < retryDelay {
			t.Errorf("gap between attempt %d and %d = %v, want >= %v (retry must back off)",
				i+1, i+2, gap, retryDelay)
		}
	}
}

func TestDoHonorsRetryAfter(t *testing.T) {
	server := newRecordingServer(t,
		scriptedResponse{
			status: http.StatusTooManyRequests,
			header: map[string]string{"Retry-After": "1"},
		},
		scriptedResponse{status: http.StatusOK, body: `{}`},
	)
	// MaxRetryDelay well above the server's ask, so the observed wait can only
	// have come from Retry-After (the base backoff step is shorter).
	client := newClient(t, server.URL, 4, 10*time.Second)

	if err := client.Get(context.Background(), "/v1/search", nil, nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := server.requestCount(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if gap := server.gap(t, 0); gap < time.Second {
		t.Errorf("retry waited %v, want >= 1s (the server-supplied Retry-After)", gap)
	}
}

func TestDoDoesNotRetryPermanentFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{name: "400 bad request", status: http.StatusBadRequest, body: `{"error":"bad jql"}`, wantStatus: 400},
		{name: "404 not found", status: http.StatusNotFound, body: `{"error":"gone"}`, wantStatus: 404},
		{name: "401 unauthorized", status: http.StatusUnauthorized, body: `{"error":"nope"}`, wantStatus: 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newRecordingServer(t, scriptedResponse{status: tt.status, body: tt.body})
			client := newClient(t, server.URL, 4, time.Millisecond)

			err := client.Get(context.Background(), "/v1/search", nil, nil)
			if err == nil {
				t.Fatalf("Get succeeded on a %d", tt.status)
			}
			if got := server.requestCount(); got != 1 {
				t.Errorf("requests = %d, want 1 (a %d must not be retried)", got, tt.status)
			}
			var statusErr *httpx.StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error = %v (%T), want *httpx.StatusError", err, err)
			}
			if statusErr.StatusCode != tt.wantStatus {
				t.Errorf("StatusCode = %d, want %d", statusErr.StatusCode, tt.wantStatus)
			}
			// The permanent path must return ParseError's result directly,
			// not the "failed after N attempts" wrapper.
			if strings.Contains(err.Error(), "attempts") {
				t.Errorf("error = %q reads like a retries-exhausted failure", err)
			}
		})
	}
}

func TestDoRetriesServerSideFailures(t *testing.T) {
	server := newRecordingServer(t,
		scriptedResponse{status: http.StatusInternalServerError, body: `oops`},
		scriptedResponse{status: http.StatusServiceUnavailable, body: `busy`},
		scriptedResponse{status: http.StatusOK, body: `{}`},
	)
	client := newClient(t, server.URL, 4, time.Millisecond)

	if err := client.Get(context.Background(), "/v1/things", nil, nil); err != nil {
		t.Fatalf("Get after 500 and 503: %v", err)
	}
	if got := server.requestCount(); got != 3 {
		t.Errorf("requests = %d, want 3 (5xx failures are retried)", got)
	}
}

func TestDoGivesUpAfterMaxRetries(t *testing.T) {
	server := newRecordingServer(t,
		scriptedResponse{status: http.StatusTooManyRequests, body: `throttled`},
	)
	client := newClient(t, server.URL, 2, time.Millisecond)

	err := client.Get(context.Background(), "/v1/search", nil, nil)
	if err == nil {
		t.Fatal("Get succeeded although every attempt was throttled")
	}
	if got := server.requestCount(); got != 3 {
		t.Errorf("requests = %d, want 3 (1 initial + 2 retries)", got)
	}
	var statusErr *httpx.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("error = %v, want it to wrap the final 429", err)
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "seconds", value: "7", want: 7 * time.Second},
		{name: "zero seconds", value: "0", want: 0},
		{name: "padded", value: " 3 ", want: 3 * time.Second},
		{name: "absent", value: "", want: 0},
		{name: "garbage", value: "soon", want: 0},
		{name: "negative", value: "-2", want: 0},
		{name: "http date in the past", value: "Mon, 02 Jan 2006 15:04:05 GMT", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := httpx.RetryAfter(tt.value); got != tt.want {
				t.Errorf("RetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}

	t.Run("http date in the future", func(t *testing.T) {
		future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
		got := httpx.RetryAfter(future)
		if got <= 0 || got > 30*time.Second {
			t.Errorf("RetryAfter(%q) = %v, want a positive duration up to 30s", future, got)
		}
	})
}

// PermanentStatus is what every integration's Permanent() defers to, and the
// agent loop's second-attempt decision hangs off it.
func TestPermanentStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{status: http.StatusBadRequest, want: true},
		{status: http.StatusUnauthorized, want: true},
		{status: http.StatusNotFound, want: true},
		{status: http.StatusTooManyRequests, want: false},
		{status: http.StatusInternalServerError, want: false},
		{status: http.StatusBadGateway, want: false},
		{status: http.StatusServiceUnavailable, want: false},
	}
	for _, tt := range tests {
		if got := httpx.PermanentStatus(tt.status); got != tt.want {
			t.Errorf("PermanentStatus(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
