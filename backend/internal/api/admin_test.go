package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cortex/internal/api"
)

// REQ-4.5's trigger: POST /api/admin/index enqueues one job per source, or one
// job for the source named in the body.
//
// It is queued rather than performed inline because a full crawl of three
// sources makes thousands of API calls; what the handler owes the caller is the
// list of what it queued, and a refusal when it cannot queue all of it.

var indexSources = []string{"gmail", "jira", "notion"}

func newIndexRouter(enqueuer api.Enqueuer, sources []string) http.Handler {
	return api.NewRouter(api.Deps{
		DB:           &stubDB{},
		Enqueuer:     enqueuer,
		Model:        testModel,
		DevUserEmail: devUserEmail,
		IndexSources: sources,
		Logger:       discardLogger(),
	})
}

func postIndex(t *testing.T, h http.Handler, body string, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := localRequest(http.MethodPost, "/api/admin/index", reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminIndexQueuesJobs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantQueued  []string
	}{
		{
			// A bodyless POST is still the documented shape (the source list is
			// optional), but it must carry the JSON content type like any other
			// call. That header is the only thing forcing a browser to preflight
			// this endpoint, so accepting a request without it made the whole
			// protection conditional on the attacker choosing to send a body.
			name:        "no body reindexes every source",
			contentType: "application/json",
			wantStatus:  http.StatusAccepted,
			wantQueued:  indexSources,
		},
		{
			name:       "a bodyless request without a JSON content type is refused",
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:        "empty JSON object reindexes every source",
			body:        `{}`,
			contentType: "application/json",
			wantStatus:  http.StatusAccepted,
			wantQueued:  indexSources,
		},
		{
			name:        "a named source reindexes just that one",
			body:        `{"source":"notion"}`,
			contentType: "application/json",
			wantStatus:  http.StatusAccepted,
			wantQueued:  []string{"notion"},
		},
		{
			name:        "the source name is case-insensitive",
			body:        `{"source":"Notion"}`,
			contentType: "application/json",
			wantStatus:  http.StatusAccepted,
			wantQueued:  []string{"notion"},
		},
		{
			name:        "an unknown source is a client error",
			body:        `{"source":"slack"}`,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "a malformed body is a client error",
			body:        `{"source":`,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "a body without a JSON content type is refused",
			body:        `{"source":"notion"}`,
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			enqueuer := &stubEnqueuer{}
			h := newIndexRouter(enqueuer, indexSources)

			rec := postIndex(t, h, tt.body, tt.contentType)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusAccepted {
				if len(enqueuer.indexed) != 0 {
					t.Errorf("queued %v on a rejected request, want nothing", enqueuer.indexed)
				}
				return
			}

			var body struct {
				Queued []string `json:"queued"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response %q: %v", rec.Body.String(), err)
			}
			if !equalStringSlices(body.Queued, tt.wantQueued) {
				t.Errorf("queued = %v, want %v", body.Queued, tt.wantQueued)
			}
			if !equalStringSlices(enqueuer.indexed, tt.wantQueued) {
				t.Errorf("enqueuer received %v, want %v", enqueuer.indexed, tt.wantQueued)
			}
		})
	}
}

// A partial enqueue is reported as a failure: a 202 listing two of three sources
// reads as "done" to a script.
func TestAdminIndexReportsAQueueFailure(t *testing.T) {
	t.Parallel()

	enqueuer := &stubEnqueuer{err: errStubDBQuery}
	h := newIndexRouter(enqueuer, indexSources)

	rec := postIndex(t, h, "", "application/json")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "stubDB") {
		t.Errorf("the error body leaks the internal failure: %q", rec.Body.String())
	}
}

// A build with no indexer configured must say so rather than pretend to queue.
func TestAdminIndexIsUnavailableWithoutSources(t *testing.T) {
	t.Parallel()

	enqueuer := &stubEnqueuer{}
	h := newIndexRouter(enqueuer, nil)

	rec := postIndex(t, h, "", "application/json")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if len(enqueuer.indexed) != 0 {
		t.Errorf("queued %v with no sources configured", enqueuer.indexed)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
