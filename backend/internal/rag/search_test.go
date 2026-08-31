package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/rag"
	"cortex/internal/tools"
)

// Vector search, against a real pgvector index.
//
// Known chunks are seeded with fixed embeddings so the cosine ordering is
// arithmetic rather than a guess, and the query embedding is pinned through the
// fake embedder. What is being asserted: top-k by cosine distance
// over document_chunks joined with documents, an optional source filter, and
// evidence built from the joined document rows so an indexed answer cites
// exactly like a live one.
//
// Skips when TEST_DATABASE_URL is unset.

// The seeded corpus. Each document's chunk sits on its own basis vector, so the
// similarity to the query below is just the query's component on that axis.
const (
	planQuery = "Atlas Q2 goals"
)

// knownSources are the source names the tool advertises and filters on.
var knownSources = []string{"jira", "notion", "gmail"}

// seedCorpus inserts three documents whose ranking against queryVector is,
// by construction: notion (two chunks) > jira > gmail.
func seedCorpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	published := time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC)

	notionID := seedDocument(t, pool, tools.Document{
		Source: "notion", ExternalID: "page-atlas-plan",
		Title:     "Atlas Q2 Plan",
		URL:       "https://www.notion.so/page-atlas-plan",
		Content:   "Atlas Q2 goals and the payments migration.",
		Timestamp: &published,
	})
	// The strongest match, and a second passage from the same page — evidence
	// must still name the page once.
	seedChunk(t, pool, notionID, 0, "Atlas Q2 goals: ship the payments migration.",
		sparseVector(map[int]float32{0: 1}))
	seedChunk(t, pool, notionID, 1, "Q2 success criteria for the Atlas programme.",
		sparseVector(map[int]float32{0: 0.9, 3: 0.1}))

	jiraID := seedDocument(t, pool, tools.Document{
		Source: "jira", ExternalID: "ATLAS-1",
		Title:   "Payments sandbox down",
		URL:     "https://example.atlassian.net/browse/ATLAS-1",
		Content: "The sandbox has been down since April.",
	})
	seedChunk(t, pool, jiraID, 0, "The payments sandbox has been down since April.",
		sparseVector(map[int]float32{1: 1}))

	gmailID := seedDocument(t, pool, tools.Document{
		Source: "gmail", ExternalID: "msg-01",
		Title:   "Vendor timeline",
		URL:     "https://mail.google.com/mail/u/0/#inbox/msg-01",
		Content: "The vendor moved delivery to July.",
	})
	seedChunk(t, pool, gmailID, 0, "The vendor moved delivery to July.",
		sparseVector(map[int]float32{2: 1}))
}

// searchEmbedder pins the query vector: strongest on the notion axis, then
// jira, then gmail.
func searchEmbedder() *fakeEmbedder {
	return &fakeEmbedder{
		fixed: map[string][]float32{
			planQuery: sparseVector(map[int]float32{0: 1, 1: 0.5, 2: 0.2}),
		},
	}
}

func newSearchTool(t *testing.T, pool *pgxpool.Pool, embedder rag.Embedder, limit int) tools.Tool {
	t.Helper()
	tool, err := rag.NewSearchTool(rag.SearchConfig{
		DB:       pool,
		Embedder: embedder,
		Sources:  knownSources,
		Limit:    limit,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("build search tool: %v", err)
	}
	return tool
}

// execute calls the tool with the given arguments.
func execute(t *testing.T, tool tools.Tool, args string) (tools.Result, error) {
	t.Helper()
	return tool.Execute(context.Background(), json.RawMessage(args))
}

// evidenceKeys renders evidence as "source/external_id" for readable failures.
func evidenceKeys(items []tools.EvidenceItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Source+"/"+item.ExternalID)
	}
	return out
}

func TestKnowledgeBaseSearchRanksAndFilters(t *testing.T) {
	pool := testPool(t)
	seedCorpus(t, pool)

	tests := []struct {
		name string
		args string
		// limit overrides the tool's top-k; 0 uses the default.
		limit int

		wantEvidence []string
		// wantContentOrder are substrings that must appear in the rendered
		// result in this order — the ranking as the model sees it.
		wantContentOrder []string
	}{
		{
			// Top-k order: the notion page outranks the Jira issue, which
			// outranks the email, by cosine distance alone.
			name:             "ranks by cosine similarity",
			args:             `{"query":"Atlas Q2 goals"}`,
			wantEvidence:     []string{"notion/page-atlas-plan", "jira/ATLAS-1", "gmail/msg-01"},
			wantContentOrder: []string{"Atlas Q2 Plan", "Payments sandbox down", "Vendor timeline"},
		},
		{
			// The source filter is the second half of the contract: same query, one
			// system.
			name:             "source filter restricts the search to one system",
			args:             `{"query":"Atlas Q2 goals","source":"jira"}`,
			wantEvidence:     []string{"jira/ATLAS-1"},
			wantContentOrder: []string{"Payments sandbox down"},
		},
		{
			name:             "source filter on notion returns the page once",
			args:             `{"query":"Atlas Q2 goals","source":"notion"}`,
			wantEvidence:     []string{"notion/page-atlas-plan"},
			wantContentOrder: []string{"Atlas Q2 Plan"},
		},
		{
			name:             "source filter is case-insensitive",
			args:             `{"query":"Atlas Q2 goals","source":"GMAIL"}`,
			wantEvidence:     []string{"gmail/msg-01"},
			wantContentOrder: []string{"Vendor timeline"},
		},
		{
			// top-k truncates the chunk list; both surviving chunks belong to
			// the same document, which is still one citable source.
			name:             "top-k is respected",
			args:             `{"query":"Atlas Q2 goals"}`,
			limit:            2,
			wantEvidence:     []string{"notion/page-atlas-plan"},
			wantContentOrder: []string{"2 indexed passages matched"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := newSearchTool(t, pool, searchEmbedder(), tt.limit)

			result, err := execute(t, tool, tt.args)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			if got := evidenceKeys(result.Evidence); !equalStrings(got, tt.wantEvidence) {
				t.Errorf("evidence = %v, want %v\ncontent:\n%s", got, tt.wantEvidence, result.Content)
			}

			cursor := 0
			for _, want := range tt.wantContentOrder {
				index := strings.Index(result.Content[cursor:], want)
				if index < 0 {
					t.Fatalf("result content does not carry %q after position %d\n%s",
						want, cursor, result.Content)
				}
				cursor += index + len(want)
			}
		})
	}
}

// Evidence is built from the joined document rows, so an indexed
// citation carries the same title, URL and timestamp a live one does.
func TestKnowledgeBaseEvidenceIsCitable(t *testing.T) {
	pool := testPool(t)
	seedCorpus(t, pool)

	tool := newSearchTool(t, pool, searchEmbedder(), 0)
	result, err := execute(t, tool, `{"query":"Atlas Q2 goals"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Evidence) == 0 {
		t.Fatal("search returned no evidence")
	}

	top := result.Evidence[0]
	if top.Source != "notion" || top.ExternalID != "page-atlas-plan" {
		t.Fatalf("top evidence = %s/%s, want notion/page-atlas-plan", top.Source, top.ExternalID)
	}
	if top.Title != "Atlas Q2 Plan" {
		t.Errorf("title = %q, want %q", top.Title, "Atlas Q2 Plan")
	}
	if top.URL != "https://www.notion.so/page-atlas-plan" {
		t.Errorf("url = %q, want the document's URL", top.URL)
	}
	if strings.TrimSpace(top.Snippet) == "" {
		t.Error("snippet is empty; the citation would show nothing")
	}
	want := time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC)
	if top.Timestamp == nil || !top.Timestamp.Equal(want) {
		t.Errorf("source_timestamp = %v, want %v", top.Timestamp, want)
	}

	// The document has two matching chunks and must still be one citable item.
	seen := 0
	for _, item := range result.Evidence {
		if item.ExternalID == "page-atlas-plan" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the same document produced %d evidence items, want 1", seen)
	}

	// Every hit's URL reaches the reader — the acceptance test's "a real URL".
	for _, item := range result.Evidence {
		if item.URL == "" {
			t.Errorf("evidence %s/%s has no URL", item.Source, item.ExternalID)
		}
	}
}

// A bad argument must come back as ErrInvalidArgument so the agent loop tells
// the model to fix its call instead of retrying it.
func TestKnowledgeBaseSearchRejectsBadArguments(t *testing.T) {
	pool := testPool(t)
	seedCorpus(t, pool)

	tool := newSearchTool(t, pool, searchEmbedder(), 0)

	tests := []struct {
		name string
		args string
	}{
		{name: "unknown source", args: `{"query":"Atlas Q2 goals","source":"slack"}`},
		{name: "empty query", args: `{"query":"   "}`},
		{name: "missing query", args: `{}`},
		{name: "malformed json", args: `{"query":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execute(t, tool, tt.args)
			if err == nil {
				t.Fatal("Execute succeeded, want an argument error")
			}
			if !errors.Is(err, tools.ErrInvalidArgument) {
				t.Errorf("error = %v, want it to wrap tools.ErrInvalidArgument", err)
			}
		})
	}
}

// "Nothing is indexed" and "no match" are different facts about the system, and
// the model has to be able to tell them apart: one means stop searching, the
// other means rephrase.
func TestKnowledgeBaseSearchOnAnEmptyIndex(t *testing.T) {
	pool := testPool(t)

	tool := newSearchTool(t, pool, searchEmbedder(), 0)
	result, err := execute(t, tool, `{"query":"Atlas Q2 goals"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Evidence) != 0 {
		t.Errorf("evidence = %v, want none from an empty index", evidenceKeys(result.Evidence))
	}
	if !strings.Contains(strings.ToLower(result.Content), "empty") {
		t.Errorf("content = %q, want it to say the knowledge base is empty", result.Content)
	}
}

// The schema is what the model reads before calling; the source names have to be
// in it or the filter is a guess.
func TestKnowledgeBaseToolSchemaNamesItsSources(t *testing.T) {
	pool := testPool(t)

	tool := newSearchTool(t, pool, searchEmbedder(), 0)
	if tool.Name() != "search_knowledge_base" {
		t.Errorf("tool name = %q, want search_knowledge_base", tool.Name())
	}

	var schema struct {
		Type       string `json:"type"`
		Required   []string
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema.Properties["query"]; !ok {
		t.Error("schema has no query property")
	}
	source, ok := schema.Properties["source"]
	if !ok {
		t.Fatal("schema has no source property")
	}
	for _, name := range knownSources {
		if !strings.Contains(source.Description, name) {
			t.Errorf("schema does not name the %q source: %q", name, source.Description)
		}
	}
}
