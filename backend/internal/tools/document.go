package tools

import (
	"context"
	"time"
)

// The indexing contract.
//
// A Document is one thing worth remembering: a Notion page, a Jira issue with
// its comments, an email. It is the input to the RAG pipeline (chunk → embed →
// pgvector), and it is deliberately declared here rather than in internal/rag.
//
// The reason is dependency direction. The crawl for each source has to live in
// that source's package, because it reuses the same unexported client helpers
// the tools use — and if the shared type lived in internal/rag, every tool
// package would import the RAG layer. Declaring it next to Result and
// EvidenceItem keeps the arrows pointing one way: tool packages depend on
// tools, internal/rag depends on both, and nothing depends on internal/rag.

// Document is one indexable unit of source content.
type Document struct {
	// Source is the system it came from, e.g. "notion". It matches the Source
	// on EvidenceItem, so an indexed citation and a live one are the same shape.
	Source string
	// ExternalID identifies the document within that system. Together with
	// Source it is the upsert key, so it must be stable across crawls — an id,
	// not a title.
	ExternalID string
	// Title is the human-readable label rendered in citations.
	Title string
	// URL points a reader at the original.
	URL string
	// Content is the full text to chunk and embed, already flattened out of
	// whatever markup the source uses.
	Content string
	// Metadata carries source-specific fields worth keeping (an issue's status,
	// an email's sender). Stored as jsonb; not embedded.
	Metadata map[string]any
	// Timestamp is when the source last changed it, where the source reports it.
	// Nil when unknown — an absent timestamp is honest, a zero one is not.
	Timestamp *time.Time
}

// DocumentSource is a system that can be crawled for indexable content.
//
// FetchAll returns everything in one slice rather than streaming: the corpora
// here are thousands of documents at most, the crawl is bounded by a per-source
// cap, and the indexer needs the whole set anyway to batch its embedding calls
// efficiently.
type DocumentSource interface {
	// Name is the source identifier, matching Document.Source.
	Name() string
	// FetchAll crawls the source.
	FetchAll(ctx context.Context) ([]Document, error)
}
