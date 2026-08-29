package gmail_test

import (
	"context"
	"strings"
	"testing"

	"cortex/internal/tools/gmail"
)

// The indexing crawl.
//
// Written alongside the Jira crawl tests for the same reason: the Day 4 review
// flagged "no tests for the Jira and Gmail crawl pagination loops" and deferred
// it, and a query the Jira API rejects outright then shipped as green. These are
// the two hand-rolled cursor loops in the codebase, so they are where an
// off-by-one or a loop that never terminates would live.
//
// The property that matters most here is not pagination though — it is that the
// crawl goes through scopedQuery. GMAIL_QUERY_SCOPE is what confines Cortex to
// the seeded fixtures, and an indexer that ignored it would copy the operator's
// personal mail into the vector store permanently, where a citation can surface
// a snippet of it.

const (
	messagesListPath = "/gmail/v1/users/me/messages"
	messagePath      = "/gmail/v1/users/me/messages/18f2a3b4c5d6e7f8"
)

// listBody is a messages.list response with the given ids and cursor.
func listBody(nextPageToken string, ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"messages":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"` + id + `","threadId":"` + id + `"}`)
	}
	b.WriteString(`],"nextPageToken":"` + nextPageToken + `"}`)
	return b.String()
}

// The crawl must apply GMAIL_QUERY_SCOPE, and must apply it to the empty
// "everything" query rather than skipping the narrowing when there is no query
// of its own.
func TestIndexCrawlAppliesTheQueryScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		scope string
		wantQ string
	}{
		{
			// The default: a system that reads real data reaches the whole
			// mailbox unless someone narrows it deliberately.
			name:  "an unscoped crawl sends no query",
			scope: "",
			wantQ: "",
		},
		{
			name:  "a scoped crawl confines itself to the label",
			scope: "label:vantage-labs",
			wantQ: "label:vantage-labs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeGmail(t, map[string]*route{
				messagesListPath: bodyRoute(200, listBody("")),
			})
			source := gmail.NewSource(f.client(tt.scope), 10, discardLogger())

			if _, err := source.FetchAll(context.Background()); err != nil {
				t.Fatalf("FetchAll: %v", err)
			}

			lists := f.requestsTo(messagesListPath)
			if len(lists) != 1 {
				t.Fatalf("list requests = %d, want 1", len(lists))
			}
			if got := lists[0].query.Get("q"); got != tt.wantQ {
				t.Errorf("q = %q, want %q", got, tt.wantQ)
			}
		})
	}
}

// The crawl's pagination loop: follow pageToken, stop when the cursor is empty.
func TestIndexCrawlFollowsThePageCursor(t *testing.T) {
	t.Parallel()

	f := newFakeGmail(t, map[string]*route{
		messagesListPath: sequenceRoute(
			response{body: listBody("PAGE2", "18f2a3b4c5d6e7f8")},
			response{body: listBody("", "18f2a3b4c5d6e7f8")},
		),
		messagePath: fixtureRoute("message_full.json", "message_full.json"),
	})

	source := gmail.NewSource(f.client(""), 10, discardLogger())
	documents, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}

	lists := f.requestsTo(messagesListPath)
	if len(lists) != 2 {
		t.Fatalf("list requests = %d, want 2 (the cursor is followed exactly once)", len(lists))
	}
	if token := lists[0].query.Get("pageToken"); token != "" {
		t.Errorf("the first list request carried a cursor %q", token)
	}
	if token := lists[1].query.Get("pageToken"); token != "PAGE2" {
		t.Errorf("second pageToken = %q, want the cursor from page 1", token)
	}
	if len(documents) != 2 {
		t.Fatalf("documents = %d, want 2", len(documents))
	}

	// Each message is fetched in full: the body is the point of indexing, and
	// the metadata format does not carry one.
	for _, req := range f.requestsTo(messagePath) {
		if got := req.query.Get("format"); got != "full" {
			t.Errorf("message format = %q, want full", got)
		}
	}

	first := documents[0]
	if first.Source != "gmail" || first.ExternalID == "" {
		t.Errorf("document is not identifiable: %+v", first)
	}
	if first.URL == "" || first.Timestamp == nil {
		t.Errorf("document is not citable: url=%q timestamp=%v", first.URL, first.Timestamp)
	}
	if !strings.Contains(first.Content, "Nordwind") {
		t.Errorf("the message body was not indexed:\n%s", first.Content)
	}
	if !strings.Contains(first.Content, "From:") {
		t.Errorf("the headers were not indexed:\n%s", first.Content)
	}
}

// The document cap bounds the crawl and the requests it makes.
func TestIndexCrawlStopsAtTheDocumentCap(t *testing.T) {
	t.Parallel()

	f := newFakeGmail(t, map[string]*route{
		messagesListPath: bodyRoute(200, listBody("PAGE2", "18f2a3b4c5d6e7f8")),
		messagePath:      fixtureRoute("message_full.json"),
	})

	source := gmail.NewSource(f.client(""), 1, discardLogger())
	documents, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(documents) != 1 {
		t.Fatalf("documents = %d, want 1 (the cap)", len(documents))
	}
	if lists := f.requestsTo(messagesListPath); len(lists) != 1 {
		t.Errorf("list requests = %d, want 1 — the cap was reached, the cursor must not be followed", len(lists))
	}
	if got := f.requestsTo(messagesListPath)[0].query.Get("maxResults"); got != "1" {
		t.Errorf("maxResults = %q, want 1 — the cap should bound the page size too", got)
	}
}

// A message that cannot be read must not cost the crawl: the list returns ids,
// each is fetched separately, and one deleted between the two calls 404s.
func TestIndexCrawlSkipsAnUnreadableMessage(t *testing.T) {
	t.Parallel()

	const missing = "/gmail/v1/users/me/messages/dead0000dead0000"

	f := newFakeGmail(t, map[string]*route{
		messagesListPath: bodyRoute(200, listBody("", "dead0000dead0000", "18f2a3b4c5d6e7f8")),
		// A 404 is permanent, so it is not retried and one response suffices.
		missing:     bodyRoute(404, `{"error":{"code":404,"message":"Not Found"}}`),
		messagePath: fixtureRoute("message_full.json"),
	})

	source := gmail.NewSource(f.client(""), 10, discardLogger())
	documents, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(documents) != 1 {
		t.Fatalf("documents = %d, want 1 — the readable message must survive", len(documents))
	}
	if documents[0].ExternalID != "18f2a3b4c5d6e7f8" {
		t.Errorf("indexed the wrong message: %q", documents[0].ExternalID)
	}
}

// An empty mailbox is not an error, and must not produce a phantom document.
func TestIndexCrawlOnAnEmptyMailbox(t *testing.T) {
	t.Parallel()

	f := newFakeGmail(t, map[string]*route{
		messagesListPath: fixtureRoute("messages_empty.json"),
	})

	source := gmail.NewSource(f.client(""), 10, discardLogger())
	documents, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(documents) != 0 {
		t.Errorf("documents = %d, want 0", len(documents))
	}
}
