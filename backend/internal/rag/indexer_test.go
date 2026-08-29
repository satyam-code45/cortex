package rag_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cortex/internal/rag"
	"cortex/internal/tools"
)

// TEST-4.4 — upsert idempotence.
//
// REQ-4.5: the pipeline upserts documents keyed (source, external_id) and
// "skip[s] embedding entirely when content_hash is unchanged". Embedding is the
// only step that costs money and the only one that talks to OpenAI, so the
// property is stated in the spec's own terms: a reindex over unchanged content
// makes zero embedder calls.
//
// It needs a real Postgres, because the hash comparison is a query — the test
// skips when TEST_DATABASE_URL is unset.

// newIndexer wires an Indexer over the test pool.
func newIndexer(t *testing.T, cfg rag.Config) *rag.Indexer {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = discardLogger()
	}
	ix, err := rag.New(cfg)
	if err != nil {
		t.Fatalf("build indexer: %v", err)
	}
	return ix
}

func TestReindexingUnchangedContentEmbedsNothing(t *testing.T) {
	pool := testPool(t)

	published := time.Date(2026, 4, 1, 8, 0, 0, 0, time.UTC)
	plan := tools.Document{
		Source: "notion", ExternalID: "page-atlas-plan",
		Title:     "Atlas Q2 Plan",
		URL:       "https://www.notion.so/page-atlas-plan",
		Content:   "Atlas Q2 goals: ship the payments migration.\n\nRisk: the sandbox is down.",
		Metadata:  map[string]any{"kind": "page"},
		Timestamp: &published,
	}
	retro := tools.Document{
		Source: "notion", ExternalID: "page-q1-retro",
		Title:   "Q1 Retro",
		URL:     "https://www.notion.so/page-q1-retro",
		Content: "What went well: the migration plan landed.\n\nWhat did not: vendor timelines.",
	}

	source := &fakeSource{name: "notion"}
	source.setDocs(plan, retro)
	embedder := &fakeEmbedder{}

	ix := newIndexer(t, rag.Config{DB: pool, Embedder: embedder, Sources: []tools.DocumentSource{source}})
	ctx := context.Background()

	// --- first run: everything is new -------------------------------------
	first, err := ix.IndexSource(ctx, "notion")
	if err != nil {
		t.Fatalf("first IndexSource: %v", err)
	}
	if first.Fetched != 2 || first.Changed != 2 || first.Unchanged != 0 {
		t.Errorf("first run stats = %+v, want fetched 2, changed 2, unchanged 0", first)
	}
	if first.Chunks == 0 || first.Embedded != first.Chunks {
		t.Errorf("first run stats = %+v, want every chunk embedded", first)
	}
	if embedder.callCount() == 0 {
		t.Fatal("the first index made no embedding calls")
	}
	if got := queryInt(t, pool, `SELECT count(*) FROM documents WHERE source = 'notion'`); got != 2 {
		t.Errorf("documents = %d, want 2", got)
	}
	if got := queryInt(t, pool, `SELECT count(*) FROM document_chunks`); got != first.Chunks {
		t.Errorf("document_chunks = %d, want %d", got, first.Chunks)
	}
	planHash := contentHashOf(t, pool, "notion", "page-atlas-plan")
	planChunks := chunkContents(t, pool, "notion", "page-atlas-plan")

	// --- second run: nothing changed --------------------------------------
	embedder.reset()
	second, err := ix.IndexSource(ctx, "notion")
	if err != nil {
		t.Fatalf("second IndexSource: %v", err)
	}

	// The contract, stated exactly as TEST-4.4 does.
	if embedder.callCount() != 0 {
		t.Errorf("embedder calls on an unchanged reindex = %d, want 0 (submitted %d texts)",
			embedder.callCount(), len(embedder.submitted()))
	}
	if second.Unchanged != 2 || second.Changed != 0 {
		t.Errorf("second run stats = %+v, want unchanged 2, changed 0", second)
	}
	if second.Embedded != 0 || second.Chunks != 0 {
		t.Errorf("second run stats = %+v, want nothing chunked or embedded", second)
	}
	if second.Fetched != 2 {
		t.Errorf("second run fetched = %d, want 2 (the crawl still happens)", second.Fetched)
	}

	// The upsert is keyed (source, external_id): a second pass must not
	// duplicate rows, and must not disturb the chunks it skipped.
	if got := queryInt(t, pool, `SELECT count(*) FROM documents WHERE source = 'notion'`); got != 2 {
		t.Errorf("documents after reindex = %d, want 2 (upsert, not insert)", got)
	}
	if got := queryInt(t, pool, `SELECT count(*) FROM document_chunks`); got != first.Chunks {
		t.Errorf("document_chunks after reindex = %d, want %d unchanged", got, first.Chunks)
	}
	if got := contentHashOf(t, pool, "notion", "page-atlas-plan"); got != planHash {
		t.Errorf("content_hash changed on an unchanged reindex: %q -> %q", planHash, got)
	}
	if got := chunkContents(t, pool, "notion", "page-atlas-plan"); !equalStrings(got, planChunks) {
		t.Errorf("chunks changed on an unchanged reindex:\ngot  %q\nwant %q", got, planChunks)
	}

	// --- third run: one document actually changed -------------------------
	edited := plan
	edited.Content = "Atlas Q2 goals: ship the payments migration in June.\n\nRisk: the sandbox is back up."
	source.setDocs(edited, retro)

	embedder.reset()
	third, err := ix.IndexSource(ctx, "notion")
	if err != nil {
		t.Fatalf("third IndexSource: %v", err)
	}
	if third.Changed != 1 || third.Unchanged != 1 {
		t.Errorf("third run stats = %+v, want changed 1, unchanged 1", third)
	}
	if embedder.callCount() == 0 {
		t.Error("the changed document was not re-embedded")
	}
	// Only the changed document's text is sent to the API.
	for _, text := range embedder.submitted() {
		if strings.Contains(text, "What went well") {
			t.Errorf("the unchanged document was re-embedded: %q", text)
		}
	}
	if got := contentHashOf(t, pool, "notion", "page-atlas-plan"); got == planHash {
		t.Error("content_hash did not change after the document was edited")
	}
	// Chunks are replaced, not appended: the old text must be gone.
	for _, chunk := range chunkContents(t, pool, "notion", "page-atlas-plan") {
		if strings.Contains(chunk, "the sandbox is down") {
			t.Errorf("a stale chunk survived the reindex: %q", chunk)
		}
	}
	if got := queryInt(t, pool, `SELECT count(*) FROM documents WHERE source = 'notion'`); got != 2 {
		t.Errorf("documents after an edit = %d, want 2", got)
	}
}

// The hash covers what is rendered in a citation, not just the body: a page
// renamed with its text untouched still has to be re-stored, or the trace shows
// the old title.
func TestRenamingADocumentIsAChange(t *testing.T) {
	pool := testPool(t)

	doc := tools.Document{
		Source: "jira", ExternalID: "ATLAS-1",
		Title:   "Payments sandbox down",
		URL:     "https://example.atlassian.net/browse/ATLAS-1",
		Content: "The sandbox has been down since April.",
	}
	source := &fakeSource{name: "jira"}
	source.setDocs(doc)
	embedder := &fakeEmbedder{}

	ix := newIndexer(t, rag.Config{DB: pool, Embedder: embedder, Sources: []tools.DocumentSource{source}})
	ctx := context.Background()

	if _, err := ix.IndexSource(ctx, "jira"); err != nil {
		t.Fatalf("first IndexSource: %v", err)
	}
	before := contentHashOf(t, pool, "jira", "ATLAS-1")

	renamed := doc
	renamed.Title = "Payments sandbox restored"
	source.setDocs(renamed)

	embedder.reset()
	stats, err := ix.IndexSource(ctx, "jira")
	if err != nil {
		t.Fatalf("second IndexSource: %v", err)
	}
	if stats.Changed != 1 {
		t.Errorf("stats = %+v, want the renamed document counted as changed", stats)
	}
	if contentHashOf(t, pool, "jira", "ATLAS-1") == before {
		t.Error("content_hash is unchanged after a rename; the citation would show the old title")
	}
	var title string
	if err := pool.QueryRow(ctx,
		`SELECT title FROM documents WHERE source = 'jira' AND external_id = 'ATLAS-1'`).Scan(&title); err != nil {
		t.Fatalf("load title: %v", err)
	}
	if title != "Payments sandbox restored" {
		t.Errorf("stored title = %q, want the new one", title)
	}
}

// REQ-4.5: chunks are embedded in batches of at most 100 per API call.
func TestEmbeddingIsBatchedAtOneHundred(t *testing.T) {
	pool := testPool(t)

	// 250 tiny paragraphs, chunked one-per-paragraph, is 250 chunks — three
	// batches, the last one short.
	paragraphs := make([]string, 0, 250)
	for i := range 250 {
		paragraphs = append(paragraphs, fmt.Sprintf("para%03d body", i))
	}
	source := &fakeSource{name: "gmail"}
	source.setDocs(tools.Document{
		Source: "gmail", ExternalID: "thread-1",
		Title:   "A long thread",
		Content: strings.Join(paragraphs, "\n\n"),
	})
	embedder := &fakeEmbedder{}

	ix := newIndexer(t, rag.Config{
		DB:            pool,
		Embedder:      embedder,
		Sources:       []tools.DocumentSource{source},
		ChunkTokens:   5,
		OverlapTokens: 0,
	})
	stats, err := ix.IndexSource(context.Background(), "gmail")
	if err != nil {
		t.Fatalf("IndexSource: %v", err)
	}
	if stats.Chunks != 250 {
		t.Fatalf("chunks = %d, want 250 (one per paragraph)", stats.Chunks)
	}

	batches := embedder.batchSizes()
	if len(batches) != 3 {
		t.Fatalf("embedding calls = %d (%v), want 3", len(batches), batches)
	}
	for i, size := range batches {
		if size > rag.DefaultEmbedBatch {
			t.Errorf("batch %d carried %d texts, want <= %d", i, size, rag.DefaultEmbedBatch)
		}
	}
	if batches[0] != 100 || batches[1] != 100 || batches[2] != 50 {
		t.Errorf("batch sizes = %v, want [100 100 50]", batches)
	}
}

// A7: the crawl is bounded by a per-source document cap, or a full mailbox crawl
// is unbounded in both time and OpenAI spend.
func TestDocumentCapBoundsTheCrawl(t *testing.T) {
	pool := testPool(t)

	docs := make([]tools.Document, 0, 10)
	for i := range 10 {
		docs = append(docs, tools.Document{
			Source: "gmail", ExternalID: fmt.Sprintf("msg-%02d", i),
			Title:   fmt.Sprintf("Message %d", i),
			Content: fmt.Sprintf("Body of message %d.", i),
		})
	}
	source := &fakeSource{name: "gmail"}
	source.setDocs(docs...)
	embedder := &fakeEmbedder{}

	ix := newIndexer(t, rag.Config{
		DB:           pool,
		Embedder:     embedder,
		Sources:      []tools.DocumentSource{source},
		MaxDocuments: 4,
	})
	stats, err := ix.IndexSource(context.Background(), "gmail")
	if err != nil {
		t.Fatalf("IndexSource: %v", err)
	}
	if stats.Fetched != 4 {
		t.Errorf("fetched = %d, want 4 (capped)", stats.Fetched)
	}
	if got := queryInt(t, pool, `SELECT count(*) FROM documents WHERE source = 'gmail'`); got != 4 {
		t.Errorf("documents = %d, want 4", got)
	}
}

// An unknown source is a caller error, not a crawl of everything.
func TestIndexSourceRejectsAnUnknownSource(t *testing.T) {
	pool := testPool(t)

	source := &fakeSource{name: "notion"}
	ix := newIndexer(t, rag.Config{DB: pool, Embedder: &fakeEmbedder{}, Sources: []tools.DocumentSource{source}})

	if _, err := ix.IndexSource(context.Background(), "slack"); err == nil {
		t.Fatal("IndexSource(\"slack\") succeeded, want an error naming the known sources")
	}
	if source.fetches != 0 {
		t.Errorf("the registered source was crawled %d times for an unknown source name", source.fetches)
	}
}

func equalStrings(a, b []string) bool {
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
