package jira_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cortex/internal/tools/jira"
)

// The indexing crawl.
//
// An unbounded-JQL bug is why this file exists. The crawl shipped with
// `indexJQL = "ORDER BY updated DESC"` — a sort with no restriction — which
// Atlassian rejects outright:
//
//	HTTP 400: Unbounded JQL queries are not allowed here.
//	          Please add a search restriction to your query.
//
// Every Jira index job failed all five attempts and nothing was ever indexed.
// It reached production green because no test ever executed the crawl: a code
// review flagged "no tests for the Jira and Gmail crawl pagination loops" and
// deferred it, and this is precisely the bug that lived in the gap.
//
// The important property is therefore not "the JQL is exactly this string" but
// "the JQL carries a restriction, not just a sort" — a query the API would
// refuse must fail here rather than at 2am against live Jira.

const (
	projectSearchPath = "/rest/api/3/project/search"
	searchJQLPath     = "/rest/api/3/search/jql"
)

// projectsBody is a /project/search response for the given keys.
func projectsBody(keys ...string) string {
	var b strings.Builder
	b.WriteString(`{"values":[`)
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		// The id is a JSON number on the wire, and the client decodes with
		// UseNumber — a quoted id fails to unmarshal.
		fmt.Fprintf(&b, `{"id":%d,"key":%q,"name":%q}`, 10000+i, key, key+" project")
	}
	b.WriteString(`],"isLast":true}`)
	return b.String()
}

// crawlJQL runs one crawl against the fake site and returns the jql the client
// sent on its first search request.
func crawlJQL(t *testing.T, f *fakeJira, source *jira.Source) string {
	t.Helper()
	if _, err := source.FetchAll(context.Background()); err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	searches := f.requestsTo(searchJQLPath)
	if len(searches) == 0 {
		t.Fatal("the crawl made no search request")
	}
	return searches[0].query.Get("jql")
}

// TEST-4.A — the crawl's JQL must restrict, not merely sort.
func TestIndexCrawlSendsABoundedJQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// configured is the project scope passed to NewSource.
		configured []string
		// discovered is what /project/search returns; only reached when
		// configured is empty.
		discovered []string
		wantIn     []string
		wantNotIn  []string
		// wantProjectLookup asserts whether the crawl had to ask the site which
		// projects exist.
		wantProjectLookup bool
	}{
		{
			name:              "configured projects are used verbatim",
			configured:        []string{"ATLAS", "BEACON", "COMET"},
			wantIn:            []string{"project IN (ATLAS, BEACON, COMET)", "ORDER BY updated DESC"},
			wantProjectLookup: false,
		},
		{
			// The default. Full access is the product decision — Cortex reads
			// real data — so an unconfigured crawl still reaches everything the
			// account can see, it just names those projects explicitly.
			name:              "an unscoped crawl discovers every visible project",
			discovered:        []string{"ATLAS", "BEACON", "COMET", "KAN"},
			wantIn:            []string{"project IN (ATLAS, BEACON, COMET, KAN)"},
			wantProjectLookup: true,
		},
		{
			// Scoping is how an unrelated project is kept out of the vector
			// store: the site's pre-existing sample project is visible, and the
			// crawl must not reach it once a scope is set.
			name:              "a scope excludes a visible project",
			configured:        []string{"ATLAS", "BEACON", "COMET"},
			discovered:        []string{"ATLAS", "BEACON", "COMET", "KAN"},
			wantNotIn:         []string{"KAN"},
			wantProjectLookup: false,
		},
		{
			name:              "keys are normalized and blanks dropped",
			configured:        []string{" atlas ", "", "Beacon"},
			wantIn:            []string{"project IN (ATLAS, BEACON)"},
			wantProjectLookup: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			routes := map[string]*route{
				searchJQLPath: fixtureRoute("search_jql_empty.json"),
			}
			if len(tt.discovered) > 0 {
				routes[projectSearchPath] = bodyRoute(200, projectsBody(tt.discovered...))
			}
			f := newFakeJira(t, routes)

			jql := crawlJQL(t, f, jira.NewSource(f.client(), 10, tt.configured, discardLogger()))

			// The regression itself: a bare sort is what Atlassian rejects.
			if strings.HasPrefix(strings.TrimSpace(jql), "ORDER BY") {
				t.Errorf("jql is an unbounded query that Atlassian rejects: %q", jql)
			}
			if !strings.Contains(jql, "project IN (") {
				t.Errorf("jql carries no project restriction: %q", jql)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(jql, want) {
					t.Errorf("jql = %q, want it to contain %q", jql, want)
				}
			}
			for _, unwanted := range tt.wantNotIn {
				if strings.Contains(jql, unwanted) {
					t.Errorf("jql = %q, want it NOT to contain %q", jql, unwanted)
				}
			}

			lookups := len(f.requestsTo(projectSearchPath))
			if tt.wantProjectLookup && lookups == 0 {
				t.Error("an unscoped crawl must discover the projects it can see")
			}
			if !tt.wantProjectLookup && lookups > 0 {
				t.Errorf("a scoped crawl asked the site for projects %d time(s); the scope is the answer", lookups)
			}
		})
	}
}

// A crawl that cannot name a single project must say so, not fall back to the
// unbounded query the API refuses.
func TestIndexCrawlFailsWhenNoProjectIsIndexable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured []string
		discovered []string
	}{
		{name: "the account sees no projects", discovered: []string{}},
		{name: "every configured key is malformed", configured: []string{"not a key", "9X"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			routes := map[string]*route{
				projectSearchPath: bodyRoute(200, projectsBody(tt.discovered...)),
				searchJQLPath:     fixtureRoute("search_jql_empty.json"),
			}
			f := newFakeJira(t, routes)
			source := jira.NewSource(f.client(), 10, tt.configured, discardLogger())

			if _, err := source.FetchAll(context.Background()); err == nil {
				t.Fatal("FetchAll succeeded with no indexable project")
			}
			if searches := f.requestsTo(searchJQLPath); len(searches) > 0 {
				t.Errorf("a query was sent anyway: %q", searches[0].query.Get("jql"))
			}
		})
	}
}

// The crawl's pagination loop: follow nextPageToken, stop on its absence, and
// never exceed the document cap.
func TestIndexCrawlPagination(t *testing.T) {
	t.Parallel()

	f := newFakeJira(t, map[string]*route{
		searchJQLPath:                         fixtureRoute("search_jql_page1.json", "search_jql_page2.json"),
		"/rest/api/3/issue/ATLAS-101/comment": fixtureRoute("comments_atlas_101.json"),
		"/rest/api/3/issue/ATLAS-102/comment": fixtureRoute("comments_empty.json"),
		"/rest/api/3/issue/ATLAS-103/comment": fixtureRoute("comments_empty.json"),
	})

	source := jira.NewSource(f.client(), 10, []string{"ATLAS"}, discardLogger())
	documents, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}

	if len(documents) != 3 {
		t.Fatalf("documents = %d, want 3 across both pages", len(documents))
	}
	searches := f.requestsTo(searchJQLPath)
	if len(searches) != 2 {
		t.Fatalf("search requests = %d, want 2 (the cursor is followed exactly once)", len(searches))
	}
	if token := searches[0].query.Get("nextPageToken"); token != "" {
		t.Errorf("the first request carried a cursor %q", token)
	}
	if token := searches[1].query.Get("nextPageToken"); token != "CAEaAggB" {
		t.Errorf("second request nextPageToken = %q, want the cursor from page 1", token)
	}

	// The wider field set is the reason searchIssues is not reused: an indexed
	// issue is read for meaning, and the description is the point.
	if fields := searches[0].query.Get("fields"); !strings.Contains(fields, "description") {
		t.Errorf("fields = %q, want the description included", fields)
	}

	// Every issue's comment thread is folded into its document.
	first := documents[0]
	if first.ExternalID != "ATLAS-101" {
		t.Errorf("documents[0].ExternalID = %q, want ATLAS-101", first.ExternalID)
	}
	if !strings.Contains(first.Content, "## Comments") {
		t.Errorf("ATLAS-101 was indexed without its comment thread:\n%s", first.Content)
	}
	if first.URL == "" || first.Timestamp == nil {
		t.Errorf("documents[0] is not citable: url=%q timestamp=%v", first.URL, first.Timestamp)
	}
}

// The document cap bounds the crawl, and it bounds the *requests* too — a cap of
// 2 must not fetch a second page it will discard.
func TestIndexCrawlStopsAtTheDocumentCap(t *testing.T) {
	t.Parallel()

	f := newFakeJira(t, map[string]*route{
		searchJQLPath:                         fixtureRoute("search_jql_page1.json"),
		"/rest/api/3/issue/ATLAS-101/comment": fixtureRoute("comments_atlas_101.json"),
		"/rest/api/3/issue/ATLAS-102/comment": fixtureRoute("comments_empty.json"),
	})

	source := jira.NewSource(f.client(), 2, []string{"ATLAS"}, discardLogger())
	documents, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("documents = %d, want 2 (the cap)", len(documents))
	}
	if searches := f.requestsTo(searchJQLPath); len(searches) != 1 {
		t.Errorf("search requests = %d, want 1 — the cap was already reached", len(searches))
	}
	if got := f.requestsTo(searchJQLPath)[0].query.Get("maxResults"); got != "2" {
		t.Errorf("maxResults = %q, want 2 — the cap should bound the page size too", got)
	}
}

// One unreadable comment thread must not cost the whole crawl: the issue is
// still worth indexing, and the next run picks the thread up.
func TestIndexCrawlSurvivesAnUnreadableCommentThread(t *testing.T) {
	t.Parallel()

	f := newFakeJira(t, map[string]*route{
		searchJQLPath: fixtureRoute("search_jql_page1.json"),
		// Two responses: a 500 is transient, so the client retries it once and a
		// single scripted response would exhaust the route.
		"/rest/api/3/issue/ATLAS-101/comment": sequenceRoute(
			response{status: 500, body: `{"errorMessages":["boom"]}`},
			response{status: 500, body: `{"errorMessages":["boom"]}`},
		),
		"/rest/api/3/issue/ATLAS-102/comment": fixtureRoute("comments_empty.json"),
	})

	source := jira.NewSource(f.client(), 2, []string{"ATLAS"}, discardLogger())
	documents, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("documents = %d, want 2 — a failed thread must not drop its issue", len(documents))
	}
	if !strings.Contains(documents[0].Content, "ATLAS-101") {
		t.Error("the issue whose comments failed was not indexed")
	}
}
