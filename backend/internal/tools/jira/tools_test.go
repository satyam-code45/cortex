package jira_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cortex/internal/tools"
	"cortex/internal/tools/jira"
)

// TEST-2.3 — the four Jira tools against recorded fixtures.
//
// Two things are being checked at once. The obvious one is field mapping: only
// the fields REQ-2.3 lists may reach the model, rendered compactly. The less
// obvious one is Evidence — REQ-2.1 makes populating it non-negotiable, because
// Day 4's citations are built by joining an answer back to evidence rows, and a
// tool that returns content without evidence produces claims nothing can source.

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

// assertEvidence checks the invariants every EvidenceItem must satisfy: the
// source system, the issue key, a clickable URL, and a timestamp that is either
// absent or right — never the zero time.
func assertEvidence(t *testing.T, item tools.EvidenceItem, wantKey, wantURL, wantTimestamp string) {
	t.Helper()
	if item.Source != "jira" {
		t.Errorf("evidence source = %q, want %q", item.Source, "jira")
	}
	if item.ExternalID != wantKey {
		t.Errorf("evidence external id = %q, want %q", item.ExternalID, wantKey)
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
			t.Errorf("evidence timestamp = %s, want %s", item.Timestamp.UTC().Format(time.RFC3339), wantTimestamp)
		}
	}
}

// ---------------------------------------------------------------------------
// jira_search_issues
// ---------------------------------------------------------------------------

// The search tool must page on the cursor against /rest/api/3/search/jql, and
// must never rely on a result count: the fixtures carry no `total` field
// because the replacement endpoint does not send one (REQ-2.3 amendment).
func TestSearchIssuesPagesOnCursor(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathSearchJQL:    fixtureRoute("search_jql_page1.json", "search_jql_page2.json"),
		pathSearchLegacy: bodyRoute(http.StatusGone, `{"errorMessages":["The endpoint has been removed (CHANGE-2046)"]}`),
	})
	client, tool := fake.tool("jira_search_issues")

	result := mustExecute(t, tool, `{"jql":"project = ATLAS AND status = Blocked"}`)

	// Two requests: the second must carry the cursor from the first.
	requests := fake.requestsTo(pathSearchJQL)
	if len(requests) != 2 {
		t.Fatalf("requests to %s = %d, want 2 (the cursor must be followed)", pathSearchJQL, len(requests))
	}
	if requests[0].method != http.MethodGet {
		t.Errorf("method = %s, want GET", requests[0].method)
	}
	if got := requests[0].query.Get("jql"); got != "project = ATLAS AND status = Blocked" {
		t.Errorf("jql = %q, want the tool's argument", got)
	}
	if got := requests[0].query.Get("fields"); got != "key,summary,status,assignee,duedate,updated" {
		t.Errorf("fields = %q, want only the fields REQ-2.3 lists", got)
	}
	if requests[0].query.Has("nextPageToken") {
		t.Error("first request carried a nextPageToken")
	}
	if got := requests[1].query.Get("nextPageToken"); got != "CAEaAggB" {
		t.Errorf("second request nextPageToken = %q, want the cursor from page 1", got)
	}
	if !requests[0].authOK || requests[0].authUser != testEmail {
		t.Errorf("basic auth = (%q, ok=%t), want the configured email", requests[0].authUser, requests[0].authOK)
	}

	// The removed endpoint must never be touched.
	if legacy := fake.requestsTo(pathSearchLegacy); len(legacy) != 0 {
		t.Errorf("client called the removed %s endpoint %d times", pathSearchLegacy, len(legacy))
	}

	// Every field REQ-2.3 lists appears on the issue's line, and nothing that
	// would only come from a wider field set does.
	wantLines := []string{
		"ATLAS-101 [Blocked] Checkout fails on expired payment tokens — assignee: Priya Raman; due: 2026-07-31; updated: 2026-06-12",
		"ATLAS-102 [In Progress] Vendor sandbox credentials not issued — assignee: unassigned; due: none; updated: 2026-06-14",
		"ATLAS-103 [To Do] Reconciliation job times out nightly — assignee: Priya Raman; due: 2026-08-14; updated: 2026-06-15",
	}
	for _, line := range wantLines {
		if !strings.Contains(result.Content, line) {
			t.Errorf("content is missing the line:\n  %s\ngot:\n%s", line, result.Content)
		}
	}
	if strings.Contains(result.Content, "result limit") {
		t.Errorf("content claims the limit was reached with 3 of 25 results:\n%s", result.Content)
	}

	// Evidence: one item per issue, with the browse URL and the update time.
	if len(result.Evidence) != 3 {
		t.Fatalf("evidence items = %d, want 3 (one per issue)", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], "ATLAS-101", client.BrowseURL("ATLAS-101"), "2026-06-12T10:31:02Z")
	assertEvidence(t, result.Evidence[1], "ATLAS-102", client.BrowseURL("ATLAS-102"), "2026-06-14T08:02:44Z")
	assertEvidence(t, result.Evidence[2], "ATLAS-103", client.BrowseURL("ATLAS-103"), "2026-06-15T21:14:00Z")
	if want := fake.server.URL + "/browse/ATLAS-101"; result.Evidence[0].URL != want {
		t.Errorf("browse url = %q, want %q", result.Evidence[0].URL, want)
	}
	if result.Evidence[0].Title != "Checkout fails on expired payment tokens" {
		t.Errorf("evidence title = %q, want the issue summary", result.Evidence[0].Title)
	}
}

// max_results caps the result set and the model is told the cap was hit, so it
// cannot report a truncated list as complete.
func TestSearchIssuesRespectsMaxResults(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathSearchJQL: fixtureRoute("search_jql_page1.json", "search_jql_page2.json"),
	})
	_, tool := fake.tool("jira_search_issues")

	result := mustExecute(t, tool, `{"jql":"project = ATLAS","max_results":2}`)

	if got := len(fake.requestsTo(pathSearchJQL)); got != 1 {
		t.Errorf("requests = %d, want 1 (the limit was reached on the first page)", got)
	}
	if got := fake.requestsTo(pathSearchJQL)[0].query.Get("maxResults"); got != "2" {
		t.Errorf("maxResults = %q, want 2", got)
	}
	if len(result.Evidence) != 2 {
		t.Errorf("evidence items = %d, want 2", len(result.Evidence))
	}
	if strings.Contains(result.Content, "ATLAS-103") {
		t.Errorf("content exceeded max_results:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "limit") {
		t.Errorf("content does not warn that the limit was reached:\n%s", result.Content)
	}
}

// An empty result must not read as "there are none": REQ-2.2's prompt guidance
// and the tool's own observation both exist to stop that inference.
func TestSearchIssuesEmptyResultIsQualified(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathSearchJQL: fixtureRoute("search_jql_empty.json"),
	})
	_, tool := fake.tool("jira_search_issues")

	result := mustExecute(t, tool, `{"jql":"project = ATLAS AND status = Blocked"}`)

	if !strings.Contains(result.Content, "No issues matched") {
		t.Errorf("content = %q, want it to say nothing matched", result.Content)
	}
	if !strings.Contains(result.Content, "project = ATLAS AND status = Blocked") {
		t.Errorf("content = %q, want it to echo the query that returned nothing", result.Content)
	}
	if !strings.Contains(result.Content, "does not establish") {
		t.Errorf("content = %q, want it to qualify what zero results proves", result.Content)
	}
	if len(result.Evidence) != 0 {
		t.Errorf("evidence = %v, want none for an empty result", result.Evidence)
	}
}

// A JQL error comes back as Jira's own message, which is what lets the agent
// loop hand it to the model as a correctable observation. It must carry no URL
// and no credentials, since it is persisted into run_events.
func TestSearchIssuesSurfacesJiraErrorSafely(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathSearchJQL: bodyRoute(http.StatusBadRequest,
			`{"errorMessages":["Field 'blocked' does not exist or you do not have permission to view it."]}`),
	})
	_, tool := fake.tool("jira_search_issues")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"jql":"blocked = true"}`))
	if err == nil {
		t.Fatal("Execute succeeded on a 400, want an error")
	}
	if !strings.Contains(err.Error(), "does not exist or you do not have permission") {
		t.Errorf("error = %q, want Jira's own message", err)
	}

	var apiErr *jira.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not a *jira.APIError; the loop cannot classify it", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", apiErr.StatusCode)
	}
	if !apiErr.Permanent() {
		t.Error("a 400 reports Permanent() = false; the loop would retry a query that cannot succeed")
	}
	if strings.Contains(err.Error(), testAPIToken) || strings.Contains(err.Error(), fake.server.URL) {
		t.Errorf("error %q leaks the request URL or the token into the transcript", err)
	}
	// One attempt: retrying a malformed query only burns latency.
	if got := len(fake.requestsTo(pathSearchJQL)); got != 1 {
		t.Errorf("requests = %d, want 1 (a 400 is not retried)", got)
	}
}

// A 5xx is transient, so the client retries it within its own budget before the
// error escapes to the loop.
func TestSearchIssuesRetriesServerErrors(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathSearchJQL: sequenceRoute(
			response{status: http.StatusServiceUnavailable, body: `{"errorMessages":["Internal server error"]}`},
			response{fixture: "search_jql_page2.json"},
		),
	})
	_, tool := fake.tool("jira_search_issues")

	result := mustExecute(t, tool, `{"jql":"project = ATLAS"}`)
	if got := len(fake.requestsTo(pathSearchJQL)); got != 2 {
		t.Errorf("requests = %d, want 2 (a 503 is retried inside the client)", got)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence items = %d, want 1", len(result.Evidence))
	}
	if !strings.Contains(result.Content, "ATLAS-103") {
		t.Errorf("content = %q, want the retried page", result.Content)
	}
}

// ---------------------------------------------------------------------------
// jira_get_issue
// ---------------------------------------------------------------------------

// Field mapping plus ADF→text: the description is an ADF node tree on the wire
// and must reach the model as plain text.
func TestGetIssueMapsFieldsAndRendersADF(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathIssue: fixtureRoute("issue_atlas_101.json"),
	})
	client, tool := fake.tool("jira_get_issue")

	result := mustExecute(t, tool, `{"key":"ATLAS-101"}`)

	requests := fake.requestsTo(pathIssue)
	if len(requests) != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathIssue, len(requests))
	}
	if fields := requests[0].query.Get("fields"); !strings.Contains(fields, "description") {
		t.Errorf("fields = %q, want the description requested explicitly", fields)
	}

	wantFragments := []string{
		"ATLAS-101 — Checkout fails on expired payment tokens",
		"project: ATLAS (Atlas)",
		"type: Bug",
		"status: Blocked",
		"priority: High",
		"resolution: unresolved",
		"assignee: Priya Raman",
		"reporter: Dev Account",
		"created: 2026-05-02",
		"updated: 2026-06-12",
		"due: 2026-07-31",
		"labels: owner-priya-raman, blocked",
		"parent: ATLAS-90 — Payments hardening",
	}
	for _, want := range wantFragments {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing %q:\n%s", want, result.Content)
		}
	}

	// ADF→text: a heading, a mention, a bullet list and a link, all as text.
	wantText := []string{
		"## Problem",
		"Checkout returns a 500 when the stored token has expired. Owner: @Priya Raman.",
		"- Blocked on the vendor sandbox (see ATLAS-102)",
		"Runbook (https://wiki.example.com/runbook)",
	}
	for _, want := range wantText {
		if !strings.Contains(result.Content, want) {
			t.Errorf("rendered description is missing %q:\n%s", want, result.Content)
		}
	}
	// No raw ADF may survive into the prompt.
	for _, leak := range []string{`"type"`, "paragraph", "bulletList", "listItem"} {
		if strings.Contains(result.Content, leak) {
			t.Errorf("content leaks raw ADF (%q):\n%s", leak, result.Content)
		}
	}

	if len(result.Evidence) != 1 {
		t.Fatalf("evidence items = %d, want 1", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], "ATLAS-101", client.BrowseURL("ATLAS-101"), "2026-06-12T10:31:02Z")
	if result.Evidence[0].Title != "Checkout fails on expired payment tokens" {
		t.Errorf("evidence title = %q, want the summary", result.Evidence[0].Title)
	}
}

// ---------------------------------------------------------------------------
// jira_get_issue_history
// ---------------------------------------------------------------------------

// The changelog is where deadline changes live (REQ-2.3), so each entry must
// reach the model as field, from, to, author, date.
func TestGetIssueHistoryMapsChangelog(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathChangelog: fixtureRoute("changelog_atlas_101.json"),
	})
	client, tool := fake.tool("jira_get_issue_history")

	result := mustExecute(t, tool, `{"key":"ATLAS-101"}`)

	if got := len(fake.requestsTo(pathChangelog)); got != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathChangelog, got)
	}

	wantLines := []string{
		// A due-date change: both sides collapsed to bare dates, since Jira
		// reports them as "2026-06-15 00:00:00.0".
		"2026-05-20 | Dev Account | duedate: 2026-06-15 → 2026-07-31",
		// A status transition uses the display strings, not the numeric ids.
		"2026-06-01 | Dev Account | status: In Progress → Blocked",
		// An absent side reads as "none" rather than an empty gap.
		"2026-06-01 | Dev Account | assignee: none → Priya Raman",
	}
	for _, want := range wantLines {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing the change line:\n  %s\ngot:\n%s", want, result.Content)
		}
	}
	if strings.Contains(result.Content, "00:00:00") {
		t.Errorf("due-date change kept Jira's midnight suffix:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "10001") || strings.Contains(result.Content, "10002") {
		t.Errorf("status change leaked numeric ids instead of names:\n%s", result.Content)
	}

	// One evidence item per changed field, all timestamped at the entry's date.
	if len(result.Evidence) != 3 {
		t.Fatalf("evidence items = %d, want 3 (one per field change)", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], "ATLAS-101", client.BrowseURL("ATLAS-101"), "2026-05-20T14:03:11Z")
	assertEvidence(t, result.Evidence[1], "ATLAS-101", client.BrowseURL("ATLAS-101"), "2026-06-01T11:45:00Z")
	assertEvidence(t, result.Evidence[2], "ATLAS-101", client.BrowseURL("ATLAS-101"), "2026-06-01T11:45:00Z")
}

// An issue with no recorded changes says so plainly. The seeder's whole
// "history must be performed, not declared" amendment exists because this is
// what an un-replayed history looks like.
func TestGetIssueHistoryEmpty(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathChangelog: fixtureRoute("changelog_empty.json"),
	})
	_, tool := fake.tool("jira_get_issue_history")

	result := mustExecute(t, tool, `{"key":"ATLAS-101"}`)
	if !strings.Contains(result.Content, "no recorded field changes") {
		t.Errorf("content = %q, want it to state there are no changes", result.Content)
	}
	if len(result.Evidence) != 0 {
		t.Errorf("evidence = %v, want none", result.Evidence)
	}
}

// ---------------------------------------------------------------------------
// jira_get_comments
// ---------------------------------------------------------------------------

// Comment bodies are ADF too, and they are where blockers get explained.
func TestGetCommentsMapsThread(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathComments: fixtureRoute("comments_atlas_101.json"),
	})
	client, tool := fake.tool("jira_get_comments")

	result := mustExecute(t, tool, `{"key":"ATLAS-101"}`)

	requests := fake.requestsTo(pathComments)
	if len(requests) != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathComments, len(requests))
	}
	if got := requests[0].query.Get("orderBy"); got != "created" {
		t.Errorf("orderBy = %q, want created (oldest first)", got)
	}

	wantFragments := []string{
		"2026-05-28 | Priya Raman",
		"We cannot test the fix: the vendor has not issued sandbox credentials.",
		"2026-06-11 | Dev Account",
		"Escalated to procurement. Due date moved to 2026-07-31 as a result.",
		"- Root cause: expired vendor contract",
	}
	for _, want := range wantFragments {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing %q:\n%s", want, result.Content)
		}
	}
	// Oldest first: the thread order is the argument's chronology.
	if first, second := strings.Index(result.Content, "We cannot test"),
		strings.Index(result.Content, "Escalated to procurement"); first > second {
		t.Errorf("comments are not oldest-first:\n%s", result.Content)
	}

	if len(result.Evidence) != 2 {
		t.Fatalf("evidence items = %d, want 2 (one per comment)", len(result.Evidence))
	}
	assertEvidence(t, result.Evidence[0], "ATLAS-101", client.BrowseURL("ATLAS-101"), "2026-05-28T16:20:05Z")
	assertEvidence(t, result.Evidence[1], "ATLAS-101", client.BrowseURL("ATLAS-101"), "2026-06-11T09:02:00Z")
	if !strings.Contains(result.Evidence[0].Title, "Priya Raman") {
		t.Errorf("evidence title = %q, want the comment's author", result.Evidence[0].Title)
	}
	if !strings.Contains(result.Evidence[0].Snippet, "sandbox credentials") {
		t.Errorf("evidence snippet = %q, want the comment text", result.Evidence[0].Snippet)
	}
}

func TestGetCommentsEmpty(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathComments: fixtureRoute("comments_empty.json"),
	})
	_, tool := fake.tool("jira_get_comments")

	result := mustExecute(t, tool, `{"key":"ATLAS-101"}`)
	if !strings.Contains(result.Content, "no comments") {
		t.Errorf("content = %q, want it to state there are no comments", result.Content)
	}
	if len(result.Evidence) != 0 {
		t.Errorf("evidence = %v, want none", result.Evidence)
	}
}

// ---------------------------------------------------------------------------
// cross-tool contract
// ---------------------------------------------------------------------------

// A malformed issue key is rejected before any request is made, and marked as an
// argument error so the agent loop does not retry it.
func TestIssueToolsRejectInvalidKeys(t *testing.T) {
	toolNames := []string{"jira_get_issue", "jira_get_issue_history", "jira_get_comments"}
	badKeys := []string{"", "the payments ticket", "ATLAS", "ATLAS-0", "../../secrets", "ATLAS-101/../ATLAS-1"}

	for _, name := range toolNames {
		for _, key := range badKeys {
			t.Run(name+"/"+key, func(t *testing.T) {
				fake := newFakeJira(t, map[string]*route{})
				_, tool := fake.tool(name)

				args, err := json.Marshal(map[string]string{"key": key})
				if err != nil {
					t.Fatalf("marshal args: %v", err)
				}
				_, execErr := tool.Execute(context.Background(), args)
				if execErr == nil {
					t.Fatalf("Execute succeeded for key %q, want an error", key)
				}
				if !errors.Is(execErr, tools.ErrInvalidArgument) {
					t.Errorf("error %v does not wrap tools.ErrInvalidArgument; the loop would retry it", execErr)
				}
				if fake.requestCount() != 0 {
					t.Errorf("%d requests were made for an invalid key", fake.requestCount())
				}
			})
		}
	}
}

// The tool set is the four tools REQ-2.3 names plus jira_list_projects, added
// for BUG-3.C: without it a project key guessed from the question wording is
// unrecoverable, because Jira answers an unknown key with zero results rather
// than an error. Each has a schema the registry accepts and a description the
// model can act on.
func TestNewToolsSurface(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{})
	client := fake.client()

	list := jira.NewTools(client)
	if len(list) != 5 {
		t.Fatalf("NewTools returned %d tools, want 5", len(list))
	}

	registry, err := tools.NewRegistry(list...)
	if err != nil {
		t.Fatalf("NewRegistry rejected the Jira tools: %v", err)
	}
	want := []string{
		"jira_get_comments", "jira_get_issue", "jira_get_issue_history",
		"jira_list_projects", "jira_search_issues",
	}
	got := registry.Names()
	if len(got) != len(want) {
		t.Fatalf("registry names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("registry names = %v, want %v (sorted)", got, want)
		}
	}

	for _, def := range registry.Definitions() {
		if strings.TrimSpace(def.Description) == "" {
			t.Errorf("tool %q has no description", def.Name)
		}
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(def.Parameters, &schema); err != nil {
			t.Errorf("tool %q has an unparseable schema: %v", def.Name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("tool %q schema type = %q, want object", def.Name, schema.Type)
		}
		// jira_list_projects genuinely takes no arguments — "what projects
		// exist" has nothing to parameterize. Every other tool must declare its
		// arguments and mark the ones it cannot work without, or the loop will
		// happily execute a call that was missing them.
		if def.Name == "jira_list_projects" {
			if len(schema.Properties) != 0 || len(schema.Required) != 0 {
				t.Errorf("tool %q takes no arguments, so it must declare neither properties nor required, got %v / %v",
					def.Name, schema.Properties, schema.Required)
			}
		} else {
			if len(schema.Properties) == 0 {
				t.Errorf("tool %q schema declares no properties", def.Name)
			}
			if len(schema.Required) == 0 {
				t.Errorf("tool %q schema declares nothing required", def.Name)
			}
		}
		// Every documented argument must validate: the loop rejects a call
		// against this schema before executing it.
		for name := range schema.Properties {
			if name == "" {
				t.Errorf("tool %q has an unnamed property", def.Name)
			}
		}
	}
}

// Every non-empty result carries evidence. REQ-2.1 calls this non-negotiable,
// so it is asserted once across the whole tool set rather than trusted per tool.
func TestEveryToolPopulatesEvidence(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathSearchJQL: fixtureRoute("search_jql_page2.json"),
		pathIssue:     fixtureRoute("issue_atlas_101.json"),
		pathChangelog: fixtureRoute("changelog_atlas_101.json"),
		pathComments:  fixtureRoute("comments_atlas_101.json"),
	})
	_, byName := fake.toolSet()

	tests := []struct {
		tool string
		args string
	}{
		{tool: "jira_search_issues", args: `{"jql":"project = ATLAS"}`},
		{tool: "jira_get_issue", args: `{"key":"ATLAS-101"}`},
		{tool: "jira_get_issue_history", args: `{"key":"ATLAS-101"}`},
		{tool: "jira_get_comments", args: `{"key":"ATLAS-101"}`},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			tool, ok := byName[tt.tool]
			if !ok {
				t.Fatalf("tool %q is missing", tt.tool)
			}
			result := mustExecute(t, tool, tt.args)
			if strings.TrimSpace(result.Content) == "" {
				t.Fatal("result content is empty")
			}
			if len(result.Evidence) == 0 {
				t.Fatal("result carries no evidence; citations cannot source this content")
			}
			for i, item := range result.Evidence {
				if item.Source == "" || item.ExternalID == "" || item.URL == "" {
					t.Errorf("evidence[%d] = %+v, want source, external id and URL populated", i, item)
				}
			}
		})
	}
}

// BUG-3.C regression: the agent had no way to discover which projects exist.
//
// Run a3b7a833 opened with `project = PAYMENT`, a key invented from the wording
// of the question. Jira answers an unknown key with zero results rather than an
// error, so the run could not tell "this project does not exist" from "this
// project has no such issues" and gave up with an empty answer.
func TestListProjectsNamesTheKeysToSearch(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		"/rest/api/3/project/search": bodyRoute(200, `{"isLast":true,"values":[
			{"id":"10000","key":"ATLAS","name":"Atlas"},
			{"id":"10001","key":"BEACON","name":"Beacon"},
			{"id":"10002","key":"COMET","name":"Comet"}
		]}`),
	})
	client, tool := fake.tool("jira_list_projects")

	result, err := tool.Execute(t.Context(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	for _, key := range []string{"ATLAS", "BEACON", "COMET"} {
		if !strings.Contains(result.Content, key) {
			t.Errorf("content omits project key %q:\n%s", key, result.Content)
		}
	}
	if !strings.Contains(result.Content, "Atlas") {
		t.Errorf("content omits the project name, so a key cannot be matched to a subject:\n%s", result.Content)
	}

	if len(result.Evidence) != 3 {
		t.Fatalf("got %d evidence items, want one per project", len(result.Evidence))
	}
	for _, item := range result.Evidence {
		if item.Source != "jira" {
			t.Errorf("evidence source = %q, want jira", item.Source)
		}
		if item.ExternalID == "" || item.URL == "" {
			t.Errorf("evidence item %+v is missing an id or URL", item)
		}
	}
	_ = client
}

// TestSearchEmptyResultWarnsAboutUnknownProjectKeys checks the other half of the
// BUG-3.C fix: an empty result must not read as "no such issues" when the cause
// may be a key that does not exist.
func TestSearchEmptyResultWarnsAboutUnknownProjectKeys(t *testing.T) {
	fake := newFakeJira(t, map[string]*route{
		pathSearchJQL: fixtureRoute("search_jql_empty.json"),
	})
	_, tool := fake.tool("jira_search_issues")

	result, err := tool.Execute(t.Context(), json.RawMessage(`{"jql":"project = PAYMENT"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, want := range []string{"jira_list_projects", "does not exist"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("empty-result observation omits %q, so a guessed key reads as an empty project:\n%s",
				want, result.Content)
		}
	}
}
