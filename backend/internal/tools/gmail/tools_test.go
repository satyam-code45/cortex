package gmail_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
)

// TEST-3.2 — the two Gmail tools against recorded fixtures.
//
// Field mapping first: REQ-3.2 says gmail_search returns message id, from,
// subject, date and snippet, and gmail_get_message returns headers plus the
// plain-text body — nothing wider. Then Evidence, which CLAUDE.md makes
// non-negotiable: source "gmail", the message id, the subject, the date, and
// the web URL `https://mail.google.com/mail/u/0/#all/<id>` that Day 4's
// citations link to.

// mustExecute runs a tool and fails the test on error.
func mustExecute(t *testing.T, tool tools.Tool, args string) tools.Result {
	t.Helper()
	result, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s.Execute(%s): %v", tool.Name(), args, err)
	}
	return result
}

// mustTime parses an expected timestamp for comparison with Evidence.
func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse expected time %q: %v", value, err)
	}
	return parsed
}

// assertEvidence checks the invariants REQ-3.2 puts on a Gmail EvidenceItem.
func assertEvidence(t *testing.T, item tools.EvidenceItem, wantID, wantSubject, wantTimestamp string) {
	t.Helper()
	if item.Source != "gmail" {
		t.Errorf("evidence source = %q, want %q", item.Source, "gmail")
	}
	if item.ExternalID != wantID {
		t.Errorf("evidence external id = %q, want %q", item.ExternalID, wantID)
	}
	if item.Title != wantSubject {
		t.Errorf("evidence title = %q, want the subject %q", item.Title, wantSubject)
	}
	wantURL := "https://mail.google.com/mail/u/0/#all/" + wantID
	if item.URL != wantURL {
		t.Errorf("evidence url = %q, want %q", item.URL, wantURL)
	}
	if strings.TrimSpace(item.Snippet) == "" {
		t.Error("evidence snippet is empty; a citation has nothing to quote")
	}
	switch {
	case wantTimestamp == "":
		if item.Timestamp != nil {
			t.Errorf("evidence timestamp = %v, want nil", item.Timestamp)
		}
	case item.Timestamp == nil:
		t.Errorf("evidence timestamp is nil, want %s", wantTimestamp)
	default:
		if want := mustTime(t, wantTimestamp); !item.Timestamp.Equal(want) {
			t.Errorf("evidence timestamp = %s, want %s",
				item.Timestamp.UTC().Format(time.RFC3339), wantTimestamp)
		}
	}
}

// ---------------------------------------------------------------------------
// gmail_search
// ---------------------------------------------------------------------------

func TestSearchMessagesFieldMappingAndEvidence(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{
		pathMessages: fixtureRoute("messages_list.json"),
		pathMessage1: fixtureRoute("message_meta_1.json"),
		pathMessage2: fixtureRoute("message_meta_2.json"),
	})
	tool := fake.tool("gmail_search")

	result := mustExecute(t, tool, `{"query":"from:nordwind.example refunds"}`)

	list := fake.requestsTo(pathMessages)
	if len(list) != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathMessages, len(list))
	}
	if list[0].method != http.MethodGet {
		t.Errorf("method = %s, want GET", list[0].method)
	}
	// REQ-3.2: Gmail query syntax passthrough, unset scope = whole mailbox.
	if got := list[0].query.Get("q"); got != "from:nordwind.example refunds" {
		t.Errorf("q = %q, want the tool's query verbatim (no scope configured)", got)
	}
	if got := list[0].header.Get("Authorization"); got != "Bearer "+testAccessToken {
		t.Errorf("Authorization = %q, want the bearer access token", got)
	}

	// Each hit is fetched as metadata, with only the four headers the compact
	// line needs.
	detail := fake.requestsTo(pathMessage1)
	if len(detail) != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathMessage1, len(detail))
	}
	if got := detail[0].query.Get("format"); got != "metadata" {
		t.Errorf("format = %q, want metadata for a search hit", got)
	}
	if got := detail[0].query["metadataHeaders"]; len(got) == 0 {
		t.Error("metadataHeaders was not restricted; the model would be handed every Received: line")
	}

	// Field mapping: id, date, from, subject, snippet on one line per message.
	for _, want := range []string{
		"18f2a3b4c5d6e7f8",
		"2026-06-12",
		"ana.vogt@nordwind.example",
		"Nordwind v3 refunds sandbox: revised availability",
		"the v3 refunds sandbox will not be available until 2026-07-15",
		"18f2a3b4c5d6e7f9",
		"2026-06-15",
		"priya.raman@vantagelabs.test",
	} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing %q\ngot:\n%s", want, result.Content)
		}
	}

	if len(result.Evidence) != 2 {
		t.Fatalf("evidence items = %d, want 2 (one per message)", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], "18f2a3b4c5d6e7f8",
		"Nordwind v3 refunds sandbox: revised availability", "2026-06-12T09:14:00Z")
	assertEvidence(t, result.Evidence[1], "18f2a3b4c5d6e7f9",
		"Re: Nordwind v3 refunds sandbox: revised availability", "2026-06-15T16:40:00Z")
}

// REQ-3.2: GMAIL_QUERY_SCOPE is an optional narrowing. When set, every search
// must be confined to it — and the user's own top-level OR must not be able to
// escape, or a graded eval run could cite personal mail.
func TestSearchMessagesAppliesQueryScope(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		query string
		wantQ string
	}{
		{
			name:  "unset scope searches the whole mailbox",
			scope: "",
			query: "subject:refunds",
			wantQ: "subject:refunds",
		},
		{
			name:  "scope is ANDed into the query",
			scope: "label:vantage-labs",
			query: "subject:refunds",
			wantQ: "label:vantage-labs (subject:refunds)",
		},
		{
			name:  "a top-level OR cannot escape the scope",
			scope: "label:vantage-labs",
			query: "from:nordwind.example OR from:bank.example",
			wantQ: "label:vantage-labs (from:nordwind.example OR from:bank.example)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGmail(t, map[string]*route{
				pathMessages: fixtureRoute("messages_empty.json"),
			})
			tool := fake.scopedTool("gmail_search", tc.scope)

			mustExecute(t, tool, `{"query":`+jsonString(tc.query)+`}`)

			list := fake.requestsTo(pathMessages)
			if len(list) != 1 {
				t.Fatalf("requests to %s = %d, want 1", pathMessages, len(list))
			}
			if got := list[0].query.Get("q"); got != tc.wantQ {
				t.Errorf("q = %q, want %q", got, tc.wantQ)
			}
		})
	}
}

// jsonString JSON-quotes a string for embedding in a tool argument literal.
func jsonString(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

// max_results bounds both the list request and the fan-out of metadata fetches.
func TestSearchMessagesHonoursMaxResults(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{
		pathMessages: fixtureRoute("messages_list.json"),
		pathMessage1: fixtureRoute("message_meta_1.json"),
	})
	tool := fake.tool("gmail_search")

	result := mustExecute(t, tool, `{"query":"refunds","max_results":1}`)

	list := fake.requestsTo(pathMessages)
	if got := list[0].query.Get("maxResults"); got != "1" {
		t.Errorf("maxResults = %q, want 1", got)
	}
	if n := len(fake.requestsTo(pathMessage2)); n != 0 {
		t.Errorf("requests to %s = %d, want 0 (max_results was 1)", pathMessage2, n)
	}
	if len(result.Evidence) != 1 {
		t.Errorf("evidence items = %d, want 1", len(result.Evidence))
	}
}

// An empty result set must still carry a non-nil Evidence slice and must warn
// that Gmail answers an unrecognized operator with silence, not an error.
func TestSearchMessagesEmptyResult(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{
		pathMessages: fixtureRoute("messages_empty.json"),
	})
	tool := fake.tool("gmail_search")

	result := mustExecute(t, tool, `{"query":"from:nobody.example"}`)

	if result.Evidence == nil {
		t.Error("Evidence is nil on an empty search; it must always be populated (possibly empty)")
	}
	if len(result.Evidence) != 0 {
		t.Errorf("evidence items = %d, want 0", len(result.Evidence))
	}
	if strings.TrimSpace(result.Content) == "" {
		t.Error("content is empty; the model needs to be told the search found nothing")
	}
}

// An empty query would return the whole mailbox newest-first, which is never
// what was meant — and a retry would return the same thing.
func TestSearchMessagesRejectsEmptyQuery(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{})
	tool := fake.tool("gmail_search")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"   "}`))
	if err == nil {
		t.Fatal("Execute with an empty query returned no error")
	}
	if !errors.Is(err, tools.ErrInvalidArgument) {
		t.Errorf("error = %v, want it to wrap tools.ErrInvalidArgument", err)
	}
	if fake.requestCount() != 0 {
		t.Errorf("requests = %d, want 0", fake.requestCount())
	}
}

// ---------------------------------------------------------------------------
// gmail_get_message
// ---------------------------------------------------------------------------

func TestGetMessageReturnsHeadersAndPlainBody(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{
		pathMessage1: fixtureRoute("message_full.json"),
	})
	tool := fake.tool("gmail_get_message")

	result := mustExecute(t, tool, `{"message_id":"`+testMessageID+`"}`)

	requests := fake.requestsTo(pathMessage1)
	if len(requests) != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathMessage1, len(requests))
	}
	if got := requests[0].query.Get("format"); got != "full" {
		t.Errorf("format = %q, want full (the body is the point of this tool)", got)
	}
	if got := requests[0].header.Get("Authorization"); got != "Bearer "+testAccessToken {
		t.Errorf("Authorization = %q, want the bearer access token", got)
	}

	// Headers plus the text/plain body, and none of the HTML alternative.
	for _, want := range []string{
		"From: Ana Vogt <ana.vogt@nordwind.example>",
		"To: priya.raman@vantagelabs.test",
		"Date: 2026-06-12",
		"Subject: Nordwind v3 refunds sandbox: revised availability",
		testMessageID,
		"The v3 refunds sandbox will not be available until 2026-07-15",
		"certification partner delayed the PCI re-audit",
	} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing %q\ngot:\n%s", want, result.Content)
		}
	}
	for _, unwanted := range []string{"<p>", "margin:0", "&mdash;"} {
		if strings.Contains(result.Content, unwanted) {
			t.Errorf("content leaks HTML %q\ngot:\n%s", unwanted, result.Content)
		}
	}
	// The plain part was present, so the HTML-fallback disclaimer must not fire.
	if strings.Contains(result.Content, "extracted from HTML") {
		t.Errorf("content claims an HTML fallback for a message that has a text/plain part\ngot:\n%s", result.Content)
	}

	if len(result.Evidence) != 1 {
		t.Fatalf("evidence items = %d, want 1 (the message)", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], testMessageID,
		"Nordwind v3 refunds sandbox: revised availability", "2026-06-12T09:14:00Z")
}

// An id the model invented (a subject line, a URL) must come back as an
// invalid-argument error, not a request.
func TestGetMessageRejectsInvalidID(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{})
	tool := fake.tool("gmail_get_message")

	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"message_id":"Nordwind v3 refunds sandbox"}`))
	if err == nil {
		t.Fatal("Execute with a subject line as an id returned no error")
	}
	if !errors.Is(err, tools.ErrInvalidArgument) {
		t.Errorf("error = %v, want it to wrap tools.ErrInvalidArgument", err)
	}
	if fake.requestCount() != 0 {
		t.Errorf("requests = %d, want 0 (a malformed id must not reach Gmail)", fake.requestCount())
	}
}

// A 404 is permanent and its error must never carry the request URL or the
// access token — tool errors become observations the model reads and rows in
// run_events a trace renders.
func TestGetMessageNotFoundIsPermanentAndURLFree(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{
		pathMessage1: bodyRoute(http.StatusNotFound,
			`{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found."}}`),
	})
	tool := fake.tool("gmail_get_message")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"message_id":"`+testMessageID+`"}`))
	if err == nil {
		t.Fatal("Execute against a 404 returned no error")
	}

	var apiErr *gmail.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T (%v), want *gmail.APIError", err, err)
	}
	if !apiErr.NotFound() {
		t.Error("NotFound() = false on a 404")
	}
	if !apiErr.Permanent() {
		t.Error("Permanent() = false on a 404; retrying a missing message is pointless")
	}
	if !strings.Contains(err.Error(), "Requested entity was not found") {
		t.Errorf("error = %q, want Google's own message", err.Error())
	}
	for _, secret := range []string{"http://", testAccessToken, testRefreshToken, testClientSecret} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %s", secret, err.Error())
		}
	}
}

// A 5xx is transient, so the loop must be told a retry is worth it.
func TestGmailServerErrorIsRetryable(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{
		pathMessages: {responses: []response{
			{status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"status":"UNAVAILABLE","message":"backend error"}}`},
			{status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"status":"UNAVAILABLE","message":"backend error"}}`},
			{status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"status":"UNAVAILABLE","message":"backend error"}}`},
		}},
	})
	tool := fake.tool("gmail_search")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"refunds"}`))
	if err == nil {
		t.Fatal("Execute against a 503 returned no error")
	}
	var apiErr *gmail.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T (%v), want *gmail.APIError", err, err)
	}
	if apiErr.Permanent() {
		t.Error("Permanent() = true on a 503; a server-side failure is worth retrying")
	}
}

// The Gmail tool set is exactly the two read-only tools REQ-3.2 names. The
// agent must not be handed a way to send, label, or delete mail.
func TestGmailToolSetIsReadOnly(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{})
	set := gmail.NewTools(fake.client(""))

	got := make([]string, 0, len(set))
	for _, tool := range set {
		got = append(got, tool.Name())
		if strings.TrimSpace(tool.Description()) == "" {
			t.Errorf("tool %q has no description; descriptions steer selection", tool.Name())
		}
		var schema struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
			t.Errorf("tool %q has an unparseable schema: %v", tool.Name(), err)
		} else if schema.Type != "object" || len(schema.Properties) == 0 {
			t.Errorf("tool %q schema = %+v, want an object with properties", tool.Name(), schema)
		}
	}
	want := []string{"gmail_search", "gmail_get_message"}
	if len(got) != len(want) {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool names = %v, want %v", got, want)
			break
		}
	}
}
