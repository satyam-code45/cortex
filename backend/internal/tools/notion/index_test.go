package notion_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cortex/internal/tools/notion"
)

// A7 — indexing reads through the source's own FetchAll.
//
// The crawl is new, untested surface that feeds the whole vector index: a
// document whose ExternalID or URL is wrong here produces a citation that points
// at nothing, and one whose body is empty is silently absent from every search.
// The contract is tools.Document — a stable id, a title, a clickable URL, the
// flattened body and a timestamp — plus the two things the crawl owes the
// pipeline: it must not present trashed pages as current, and one unreadable
// page must not cost the whole index.

const retroPageID = "7d21ee90-4f11-4c33-a2b7-9c0a1d5e2f88"

var (
	pathRetroBlocks = "/v1/blocks/" + retroPageID + "/children"
	// The nested block appears in both pages' block fixtures, so its children
	// are fetched once per page.
	nestedTwice = fixtureRoute("blocks_nested.json", "blocks_nested.json")
)

func TestNotionFetchAllBuildsIndexableDocuments(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathSearch:      fixtureRoute("search_pages.json"),
		pathRootBlocks:  fixtureRoute("blocks_root.json"),
		pathRetroBlocks: fixtureRoute("blocks_root.json"),
		pathChildBlocks: nestedTwice,
	})
	source := notion.NewSource(fake.client(), 0, discardLogger())

	if source.Name() != "notion" {
		t.Errorf("source name = %q, want notion", source.Name())
	}

	docs, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}

	// The fixture holds three pages, one of them in the trash. Indexing a
	// trashed page would make deleted content citable as current.
	if len(docs) != 2 {
		t.Fatalf("documents = %d, want 2 (the trashed page must be skipped)\n%+v", len(docs), docs)
	}
	for _, doc := range docs {
		if doc.ExternalID == "9999aaaa-bbbb-cccc-dddd-eeeeffff0000" {
			t.Error("the trashed page was indexed")
		}
	}

	plan := docs[0]
	if plan.Source != "notion" {
		t.Errorf("source = %q, want notion", plan.Source)
	}
	// The upsert key: it must be the page id, not the title, or a renamed page
	// becomes a second document.
	if plan.ExternalID != testPageID {
		t.Errorf("external id = %q, want the page id %q", plan.ExternalID, testPageID)
	}
	if plan.Title != testPageTitle {
		t.Errorf("title = %q, want %q", plan.Title, testPageTitle)
	}
	if plan.URL != testPageURL {
		t.Errorf("url = %q, want %q", plan.URL, testPageURL)
	}
	if plan.Timestamp == nil || plan.Timestamp.UTC().Format("2006-01-02T15:04:05Z") != testPageEdited {
		t.Errorf("timestamp = %v, want %s", plan.Timestamp, testPageEdited)
	}
	// The body is the flattened markdown, nested blocks included — the same
	// text the live notion_get_page tool would show.
	for _, want := range []string{testPageTitle, "Nordwind Payments", "ATLAS-101"} {
		if !strings.Contains(plan.Content, want) {
			t.Errorf("content does not carry %q\n%s", want, plan.Content)
		}
	}
	if plan.Metadata == nil {
		t.Error("metadata is nil; the indexed row loses the page's edit times")
	} else if _, ok := plan.Metadata["last_edited_time"]; !ok {
		t.Errorf("metadata = %v, want it to carry last_edited_time", plan.Metadata)
	}
}

// One page moved to the trash between the search and the block fetch must not
// cost the entire crawl — the next run picks it up anyway.
func TestNotionFetchAllSkipsAPageItCannotRead(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathSearch:      fixtureRoute("search_pages.json"),
		pathRootBlocks:  fixtureRoute("blocks_root.json"),
		pathChildBlocks: fixtureRoute("blocks_nested.json"),
		pathRetroBlocks: bodyRoute(http.StatusNotFound,
			`{"object":"error","status":404,"code":"object_not_found","message":"gone"}`),
	})
	source := notion.NewSource(fake.client(), 0, discardLogger())

	docs, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll returned an error for one unreadable page: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents = %d, want 1 (the readable page)\n%+v", len(docs), docs)
	}
	if docs[0].ExternalID != testPageID {
		t.Errorf("indexed %q, want the readable page %q", docs[0].ExternalID, testPageID)
	}
}

// The cap is what keeps a crawl bounded in time and OpenAI spend (A7).
func TestNotionFetchAllRespectsTheDocumentCap(t *testing.T) {
	fake := newFakeNotion(t, map[string]*route{
		pathSearch:      fixtureRoute("search_pages.json"),
		pathRootBlocks:  fixtureRoute("blocks_root.json"),
		pathChildBlocks: fixtureRoute("blocks_nested.json"),
	})
	source := notion.NewSource(fake.client(), 1, discardLogger())

	docs, err := source.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents = %d, want 1 under a cap of 1\n%+v", len(docs), docs)
	}
	// The cap has to bound the crawl, not just the returned slice: the second
	// page's blocks must never be fetched.
	if n := len(fake.requestsTo(pathRetroBlocks)); n != 0 {
		t.Errorf("requests for the capped-out page's blocks = %d, want 0", n)
	}
}
