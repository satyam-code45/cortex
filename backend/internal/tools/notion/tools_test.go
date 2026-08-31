package notion_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cortex/internal/tools"
	"cortex/internal/tools/notion"
)

// The two Notion tools against recorded fixtures.
//
// Two things are checked at once. Field mapping: notion_search
// returns page id, title, last_edited and url, and notion_get_page returns the
// page's blocks as markdown — nothing wider. And Evidence: every tool result
// must carry it (the tool contract makes that non-negotiable), because the
// citations are built by joining an answer back to evidence rows, and a tool
// that returns content without evidence produces claims nothing can source.

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

// assertEvidence checks the invariants required of a Notion EvidenceItem:
// the source system, the page id, the title, a clickable URL, a non-empty
// snippet, and a last-edited timestamp that is either absent or right — never
// the zero time.
func assertEvidence(t *testing.T, item tools.EvidenceItem, wantID, wantTitle, wantURL, wantTimestamp string) {
	t.Helper()
	if item.Source != "notion" {
		t.Errorf("evidence source = %q, want %q", item.Source, "notion")
	}
	if item.ExternalID != wantID {
		t.Errorf("evidence external id = %q, want %q", item.ExternalID, wantID)
	}
	if item.Title != wantTitle {
		t.Errorf("evidence title = %q, want %q", item.Title, wantTitle)
	}
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
// notion_search
// ---------------------------------------------------------------------------

func TestSearchPagesFieldMappingAndEvidence(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathSearch: fixtureRoute("search_pages.json"),
	})
	_, tool := fake.tool("notion_search")

	result := mustExecute(t, tool, `{"query":"Atlas"}`)

	requests := fake.requestsTo(pathSearch)
	if len(requests) != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathSearch, len(requests))
	}
	req := requests[0]
	if req.method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.method)
	}
	// The version header is pinned, not tracked.
	if got := req.header.Get("Notion-Version"); got != notion.APIVersion {
		t.Errorf("Notion-Version = %q, want the pinned %q", got, notion.APIVersion)
	}
	if notion.APIVersion != "2026-03-11" {
		t.Errorf("pinned APIVersion = %q, want 2026-03-11", notion.APIVersion)
	}
	if got := req.header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the bearer integration secret", got)
	}

	// Pages only, in the 2025-09-03+ vocabulary (`page`, never
	// `database`).
	var body struct {
		Query  string `json:"query"`
		Filter struct {
			Property string `json:"property"`
			Value    string `json:"value"`
		} `json:"filter"`
	}
	if err := json.Unmarshal([]byte(req.body), &body); err != nil {
		t.Fatalf("decode search body %q: %v", req.body, err)
	}
	if body.Query != "Atlas" {
		t.Errorf("search query = %q, want the tool's argument", body.Query)
	}
	if body.Filter.Property != "object" || body.Filter.Value != "page" {
		t.Errorf("search filter = %+v, want {property:object value:page}", body.Filter)
	}

	// Field mapping: id, title and last-edited date on the line; nothing that
	// would only come from a wider field set.
	for _, want := range []string{
		"Atlas Q2 Plan",
		testPageID,
		"2026-06-18",
		"Atlas Retro — June",
		"7d21ee90-4f11-4c33-a2b7-9c0a1d5e2f88",
		"2026-06-02",
	} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing %q\ngot:\n%s", want, result.Content)
		}
	}
	// A trashed page is deleted content; presenting it as current would be a
	// citation to something that no longer exists.
	if strings.Contains(result.Content, "Deleted Draft") {
		t.Errorf("content includes a trashed page\ngot:\n%s", result.Content)
	}

	if len(result.Evidence) != 2 {
		t.Fatalf("evidence items = %d, want 2 (one per live page)", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], testPageID, testPageTitle, testPageURL, testPageEdited)
	assertEvidence(t, result.Evidence[1],
		"7d21ee90-4f11-4c33-a2b7-9c0a1d5e2f88",
		"Atlas Retro — June",
		"https://www.notion.so/Atlas-Retro-7d21ee904f114c33a2b79c0a1d5e2f88",
		"2026-06-02T08:00:00Z")
}

// The compact search result is "page id, title, last_edited,
// url" — four fields, and the Evidence contract is stated separately on the
// next line, so the URL is owed to the model in the observation itself and not
// only to the citation layer.
func TestSearchPagesContentCarriesTheURL(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathSearch: fixtureRoute("search_pages.json"),
	})
	_, tool := fake.tool("notion_search")

	result := mustExecute(t, tool, `{"query":"Atlas"}`)

	if !strings.Contains(result.Content, testPageURL) {
		t.Errorf("content omits the page URL %q, one of the compact result fields\ngot:\n%s",
			testPageURL, result.Content)
	}
}

// An empty result set must still return a usable result with a non-nil (if
// empty) Evidence slice, and must say what "no results" does and does not mean
// — Notion search matches titles only.
func TestSearchPagesEmptyResult(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathSearch: fixtureRoute("search_empty.json"),
	})
	_, tool := fake.tool("notion_search")

	result := mustExecute(t, tool, `{"query":"nordwind"}`)

	if result.Evidence == nil {
		t.Error("Evidence is nil on an empty search; it must always be populated (possibly empty)")
	}
	if len(result.Evidence) != 0 {
		t.Errorf("evidence items = %d, want 0", len(result.Evidence))
	}
	if strings.TrimSpace(result.Content) == "" {
		t.Error("content is empty; the model needs to be told the search found nothing")
	}
	if !strings.Contains(strings.ToUpper(result.Content), "TITLE") {
		t.Errorf("content does not warn that Notion search matches titles only\ngot:\n%s", result.Content)
	}
}

// ---------------------------------------------------------------------------
// notion_get_page
// ---------------------------------------------------------------------------

func TestGetPageRendersMarkdownWithEvidence(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathPage:        fixtureRoute("page_atlas_plan.json"),
		pathRootBlocks:  fixtureRoute("blocks_root.json"),
		pathChildBlocks: fixtureRoute("blocks_nested.json"),
	})
	_, tool := fake.tool("notion_get_page")

	result := mustExecute(t, tool, `{"page_id":"`+testPageID+`"}`)

	// The page itself, then its blocks, then the nested block's children:
	// the converter recurses to depth 2 and no further.
	if n := len(fake.requestsTo(pathPage)); n != 1 {
		t.Errorf("requests to %s = %d, want 1", pathPage, n)
	}
	if n := len(fake.requestsTo(pathRootBlocks)); n != 1 {
		t.Errorf("requests to %s = %d, want 1", pathRootBlocks, n)
	}
	if n := len(fake.requestsTo(pathChildBlocks)); n != 1 {
		t.Errorf("requests to %s = %d, want 1 (children must be fetched to depth 2)", pathChildBlocks, n)
	}
	for _, req := range fake.requestsTo(pathRootBlocks) {
		if got := req.header.Get("Notion-Version"); got != notion.APIVersion {
			t.Errorf("block fetch Notion-Version = %q, want %q", got, notion.APIVersion)
		}
	}

	// The page header carries the title and last-edited date; the body is the
	// converted markdown, nested content included.
	for _, want := range []string{
		testPageTitle,
		testPageID,
		"2026-06-18",
		"# Atlas Q2 Plan",
		"Nordwind Payments",
		"ATLAS-101",
		"- Refunds sandbox promised 2026-06-15",
		"  Vendor contact: ops@nordwind.example",
		"[unsupported block",
	} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing %q\ngot:\n%s", want, result.Content)
		}
	}

	if len(result.Evidence) != 1 {
		t.Fatalf("evidence items = %d, want 1 (the page)", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], testPageID, testPageTitle, testPageURL, testPageEdited)
}

// An id the model invented (a title, a truncated id) must come back as an
// invalid-argument error, not a request: the loop feeds that straight back as
// an observation the model can correct, and retrying it would be pointless.
func TestGetPageRejectsInvalidID(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{})
	_, tool := fake.tool("notion_get_page")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"page_id":"Atlas Q2 Plan"}`))
	if err == nil {
		t.Fatal("Execute with a page title as an id returned no error")
	}
	if !errors.Is(err, tools.ErrInvalidArgument) {
		t.Errorf("error = %v, want it to wrap tools.ErrInvalidArgument", err)
	}
	if fake.requestCount() != 0 {
		t.Errorf("requests = %d, want 0 (a malformed id must not reach Notion)", fake.requestCount())
	}
}

// A page that exists but was never shared with the integration comes back 404
// with object_not_found. The error must survive as a typed, permanent failure
// and must never carry the request URL — errors become tool observations and
// rows in run_events.
func TestGetPageNotFoundIsPermanentAndURLFree(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathPage: bodyRoute(http.StatusNotFound,
			`{"object":"error","status":404,"code":"object_not_found",`+
				`"message":"Could not find page with ID: `+testPageID+`."}`),
	})
	_, tool := fake.tool("notion_get_page")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"page_id":"`+testPageID+`"}`))
	if err == nil {
		t.Fatal("Execute against a 404 returned no error")
	}

	var apiErr *notion.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T (%v), want *notion.APIError", err, err)
	}
	if !apiErr.NotFound() {
		t.Error("NotFound() = false on a 404/object_not_found")
	}
	if !apiErr.Permanent() {
		t.Error("Permanent() = false on a 404; retrying a missing page is pointless")
	}
	if !strings.Contains(err.Error(), "Could not find page") {
		t.Errorf("error = %q, want Notion's own message", err.Error())
	}
	if strings.Contains(err.Error(), "http://") || strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaks the request URL or the token: %q", err.Error())
	}
}

// A 5xx is transient, so the loop must be told a retry is worth it.
func TestServerErrorIsRetryable(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathSearch: {responses: []response{
			{status: http.StatusBadGateway, body: `{"object":"error","status":502,"code":"internal_server_error","message":"upstream"}`},
			{status: http.StatusBadGateway, body: `{"object":"error","status":502,"code":"internal_server_error","message":"upstream"}`},
			{status: http.StatusBadGateway, body: `{"object":"error","status":502,"code":"internal_server_error","message":"upstream"}`},
		}},
	})
	_, tool := fake.tool("notion_search")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"Atlas"}`))
	if err == nil {
		t.Fatal("Execute against a 502 returned no error")
	}
	var apiErr *notion.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T (%v), want *notion.APIError", err, err)
	}
	if apiErr.Permanent() {
		t.Error("Permanent() = true on a 502; a server-side failure is worth retrying")
	}
}

// The Notion tool set is exactly the two read-only tools. The
// agent must not be handed a way to write to the workspace.
func TestToolSetIsReadOnly(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{})
	set := notion.NewTools(fake.client())

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
	want := []string{"notion_search", "notion_get_page"}
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
