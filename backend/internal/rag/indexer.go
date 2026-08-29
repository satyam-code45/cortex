package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"cortex/internal/llm"
	"cortex/internal/store"
	"cortex/internal/tools"
)

const (
	// DefaultEmbedBatch is how many chunks go into one embeddings request.
	//
	// 100 is the practical ceiling for the OpenAI embeddings endpoint: larger
	// batches start bumping the per-request token limit on real documents, and
	// smaller ones pay the round trip more often than they need to.
	DefaultEmbedBatch = 100

	// embedTimeout bounds one embeddings request.
	embedTimeout = 60 * time.Second

	// writeTimeout bounds the per-document write-back.
	writeTimeout = 30 * time.Second
)

// DB is the subset of *pgxpool.Pool the indexer needs.
//
// store.DBTX is embedded so the read half — checking content hashes — can go
// through the pool directly instead of opening a transaction it has no reason to
// hold. Only the write half needs Begin.
type DB interface {
	store.DBTX

	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrUnknownSource marks a request to index a source that is not registered.
//
// Sentinel rather than a string match so the River worker can tell a permanent
// failure (this name will never be valid) from a transient one (Jira returned a
// 503) and cancel instead of burning its retry budget.
var ErrUnknownSource = errors.New("rag: unknown source")

// Embedder produces embedding vectors. llm.Provider satisfies it; a test
// supplies a counting fake.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Config configures an Indexer.
type Config struct {
	DB       DB
	Embedder Embedder
	// Sources are the crawlable systems, keyed by their Name.
	Sources []tools.DocumentSource

	// MaxDocuments caps how many documents are taken from one source per run.
	MaxDocuments int
	// ChunkTokens sizes the chunker; zero uses DefaultChunkTokens.
	ChunkTokens int
	// OverlapTokens is how much of each chunk repeats the previous one.
	//
	// Note the asymmetry with ChunkTokens: zero means "no overlap", not "the
	// default". Zero is a coherent request — a caller wanting exact,
	// non-repeating chunks should be able to make it — but it does mean a caller
	// who simply forgets this field gets no overlap rather than the documented
	// 50, so wire it explicitly. Only a negative value falls back.
	OverlapTokens int
	// EmbedBatch is how many chunks are embedded per API call.
	EmbedBatch int

	Logger *slog.Logger
}

// Indexer builds and refreshes the vector index.
type Indexer struct {
	db       DB
	embedder Embedder
	sources  map[string]tools.DocumentSource
	names    []string

	maxDocuments  int
	chunkTokens   int
	overlapTokens int
	embedBatch    int

	logger *slog.Logger
}

// New builds an Indexer.
func New(cfg Config) (*Indexer, error) {
	if cfg.DB == nil {
		return nil, errors.New("rag: DB is required")
	}
	if cfg.Embedder == nil {
		return nil, errors.New("rag: Embedder is required")
	}
	if len(cfg.Sources) == 0 {
		return nil, errors.New("rag: at least one source is required")
	}

	ix := &Indexer{
		db:            cfg.DB,
		embedder:      cfg.Embedder,
		sources:       make(map[string]tools.DocumentSource, len(cfg.Sources)),
		maxDocuments:  cfg.MaxDocuments,
		chunkTokens:   cfg.ChunkTokens,
		overlapTokens: cfg.OverlapTokens,
		embedBatch:    cfg.EmbedBatch,
		logger:        cfg.Logger,
	}
	for _, source := range cfg.Sources {
		name := source.Name()
		if name == "" {
			return nil, errors.New("rag: a source has an empty name")
		}
		if _, exists := ix.sources[name]; exists {
			return nil, fmt.Errorf("rag: duplicate source %q", name)
		}
		ix.sources[name] = source
		ix.names = append(ix.names, name)
	}
	sort.Strings(ix.names)

	if ix.chunkTokens <= 0 {
		ix.chunkTokens = DefaultChunkTokens
	}
	if ix.overlapTokens < 0 {
		ix.overlapTokens = DefaultOverlapTokens
	}
	if ix.embedBatch <= 0 {
		ix.embedBatch = DefaultEmbedBatch
	}
	if ix.logger == nil {
		ix.logger = slog.Default()
	}
	return ix, nil
}

// Sources returns the registered source names, sorted.
func (ix *Indexer) Sources() []string {
	out := make([]string, len(ix.names))
	copy(out, ix.names)
	return out
}

// Has reports whether a source is registered.
func (ix *Indexer) Has(name string) bool {
	_, ok := ix.sources[name]
	return ok
}

// Stats summarizes one indexing run.
type Stats struct {
	Source    string `json:"source"`
	Fetched   int    `json:"fetched"`
	Changed   int    `json:"changed"`
	Unchanged int    `json:"unchanged"`
	Chunks    int    `json:"chunks"`
	Embedded  int    `json:"embedded"`
}

// IndexSource crawls one source and refreshes its documents and chunks.
//
// The shape is: fetch everything, decide what actually changed, embed only that,
// then write. Each step exists for a reason worth stating.
//
// Re-indexing is meant to be cheap and frequent, so the content hash is checked
// before anything is embedded — embedding is the only part of this that costs
// money, and on a second run over unchanged data the answer is zero API calls.
//
// Embedding happens outside any transaction, and in batches across documents: a
// transaction held open across a network call would pin a pool connection for
// the length of the crawl, and one request per chunk would turn a 400-document
// index into thousands of round trips.
//
// The write is one transaction per document, and it updates the content_hash in
// the same transaction that replaces the chunks. That ordering is the important
// part: writing the hash first and the chunks second would let a failure halfway
// through leave a document marked current with no chunks behind it, and every
// later run would skip it — a document silently missing from the index forever.
func (ix *Indexer) IndexSource(ctx context.Context, name string) (Stats, error) {
	source, ok := ix.sources[name]
	if !ok {
		return Stats{}, fmt.Errorf("rag: unknown source %q (have %s): %w",
			name, strings.Join(ix.names, ", "), ErrUnknownSource)
	}

	stats := Stats{Source: name}
	started := time.Now()

	documents, err := source.FetchAll(ctx)
	if err != nil {
		return stats, fmt.Errorf("fetch %s: %w", name, err)
	}
	if ix.maxDocuments > 0 && len(documents) > ix.maxDocuments {
		ix.logger.Warn("rag: source returned more documents than the cap allows",
			"source", name, "fetched", len(documents), "cap", ix.maxDocuments)
		documents = documents[:ix.maxDocuments]
	}
	stats.Fetched = len(documents)

	pending, unchanged, err := ix.selectChanged(ctx, name, documents)
	if err != nil {
		return stats, err
	}
	stats.Unchanged = unchanged
	stats.Changed = len(pending)
	if len(pending) == 0 {
		ix.logger.Info("rag: index is up to date",
			"source", name, "documents", stats.Fetched, "elapsed", time.Since(started))
		return stats, nil
	}

	for _, doc := range pending {
		stats.Chunks += len(doc.chunks)
	}
	if err := ix.embedAll(ctx, pending); err != nil {
		return stats, err
	}
	stats.Embedded = stats.Chunks

	for _, doc := range pending {
		if err := ix.write(ctx, doc); err != nil {
			return stats, err
		}
	}

	ix.logger.Info("rag: indexed source",
		"source", name, "fetched", stats.Fetched, "changed", stats.Changed,
		"unchanged", stats.Unchanged, "chunks", stats.Chunks, "elapsed", time.Since(started))
	return stats, nil
}

// pendingDocument is a document whose content changed, with its chunks.
type pendingDocument struct {
	doc    tools.Document
	hash   string
	chunks []Chunk
	// vectors is filled by embedAll, one per chunk, in chunk order.
	vectors [][]float32
}

// selectChanged partitions the crawl into documents that need re-embedding and
// documents whose stored hash already matches.
func (ix *Indexer) selectChanged(ctx context.Context, source string, documents []tools.Document) ([]pendingDocument, int, error) {
	var (
		pending   []pendingDocument
		unchanged int
		seen      = make(map[string]struct{}, len(documents))
	)

	q := store.New(ix.db)

	for _, doc := range documents {
		if strings.TrimSpace(doc.ExternalID) == "" {
			ix.logger.Warn("rag: skipping a document with no external id", "source", source, "title", doc.Title)
			continue
		}
		// A source that returns the same id twice would make the second upsert
		// overwrite the first and leave orphaned chunks behind; dropping the
		// duplicate is the honest reading of "one row per (source, external_id)".
		if _, duplicate := seen[doc.ExternalID]; duplicate {
			ix.logger.Warn("rag: source returned a duplicate document id",
				"source", source, "external_id", doc.ExternalID)
			continue
		}
		seen[doc.ExternalID] = struct{}{}

		hash := contentHash(doc)

		existing, err := q.GetDocumentBySourceExternalID(ctx, store.GetDocumentBySourceExternalIDParams{
			Source:     doc.Source,
			ExternalID: doc.ExternalID,
		})
		switch {
		case err == nil && existing.ContentHash == hash:
			unchanged++
			continue
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return nil, 0, fmt.Errorf("load document %s/%s: %w", doc.Source, doc.ExternalID, err)
		}

		chunks := ChunkText(doc.Content, ix.chunkTokens, ix.overlapTokens)
		if len(chunks) == 0 {
			// An empty document is still worth a row — a Notion page with a
			// title and no body is a real thing to cite — but it has nothing to
			// embed, so it goes through the same write path with no chunks.
			ix.logger.Debug("rag: document has no indexable content",
				"source", doc.Source, "external_id", doc.ExternalID)
		}
		pending = append(pending, pendingDocument{doc: doc, hash: hash, chunks: chunks})
	}
	return pending, unchanged, nil
}

// embedAll fills in the vectors for every pending chunk, batching across
// documents so a corpus of small documents does not pay one request each.
func (ix *Indexer) embedAll(ctx context.Context, pending []pendingDocument) error {
	// index maps a position in the flat batch back to (document, chunk).
	type slot struct{ doc, chunk int }

	var (
		texts []string
		slots []slot
	)
	for d := range pending {
		pending[d].vectors = make([][]float32, len(pending[d].chunks))
		for c, chunk := range pending[d].chunks {
			texts = append(texts, chunk.Content)
			slots = append(slots, slot{doc: d, chunk: c})
		}
	}
	if len(texts) == 0 {
		return nil
	}

	for start := 0; start < len(texts); start += ix.embedBatch {
		end := min(start+ix.embedBatch, len(texts))

		batchCtx, cancel := context.WithTimeout(ctx, embedTimeout)
		vectors, err := ix.embedder.Embed(batchCtx, texts[start:end])
		cancel()
		if err != nil {
			// Classified rather than wrapped: this error is stored in River's
			// river_job.errors and logged, and the raw provider error formats
			// the request URL and the upstream body (see llm.SafeErrorMessage).
			ix.logger.Warn("rag: embedding batch failed", "start", start, "end", end, "error", err)
			return fmt.Errorf("embed chunks %d-%d: %s", start, end, llm.SafeErrorMessage(err))
		}
		if len(vectors) != end-start {
			return fmt.Errorf("embed chunks %d-%d: got %d vectors for %d inputs",
				start, end, len(vectors), end-start)
		}
		for i, vector := range vectors {
			s := slots[start+i]
			pending[s.doc].vectors[s.chunk] = vector
		}
	}
	return nil
}

// write upserts one document and replaces its chunks, in a single transaction.
func (ix *Indexer) write(ctx context.Context, pending pendingDocument) error {
	metadata, err := json.Marshal(pending.doc.Metadata)
	if err != nil || pending.doc.Metadata == nil {
		// Metadata is a nicety; a source that returns something unmarshalable
		// must not stop its document being indexed.
		if err != nil {
			ix.logger.Warn("rag: dropping unmarshalable document metadata",
				"source", pending.doc.Source, "external_id", pending.doc.ExternalID, "error", err)
		}
		metadata = []byte(`{}`)
	}

	params := store.UpsertDocumentParams{
		Source:      pending.doc.Source,
		ExternalID:  pending.doc.ExternalID,
		Title:       pending.doc.Title,
		Url:         pending.doc.URL,
		Content:     pending.doc.Content,
		Metadata:    metadata,
		ContentHash: pending.hash,
	}
	if pending.doc.Timestamp != nil {
		params.SourceTimestamp = pgtype.Timestamptz{Time: pending.doc.Timestamp.UTC(), Valid: true}
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	return ix.inTx(writeCtx, func(q store.Querier) error {
		document, err := q.UpsertDocument(writeCtx, params)
		if err != nil {
			return fmt.Errorf("upsert document %s/%s: %w", params.Source, params.ExternalID, err)
		}
		// Replace rather than reconcile. Chunk boundaries move when the content
		// changes, so chunk 3 of the old text and chunk 3 of the new one are not
		// the same passage and there is nothing to update in place.
		if err := q.DeleteChunksByDocument(writeCtx, document.ID); err != nil {
			return fmt.Errorf("delete chunks for %s: %w", params.ExternalID, err)
		}
		for i, chunk := range pending.chunks {
			vector := pending.vectors[i]
			if len(vector) == 0 {
				return fmt.Errorf("chunk %d of %s has no embedding", i, params.ExternalID)
			}
			if err := q.InsertDocumentChunk(writeCtx, store.InsertDocumentChunkParams{
				DocumentID: document.ID,
				ChunkIndex: int32(chunk.Index),
				Content:    chunk.Content,
				Embedding:  formatVector(vector),
			}); err != nil {
				return fmt.Errorf("insert chunk %d of %s: %w", i, params.ExternalID, err)
			}
		}
		return nil
	})
}

// contentHash is the idempotence key: the same source content must produce the
// same hash on every crawl, and any change to what would be indexed or cited
// must change it.
//
// Title and URL are hashed alongside the body because both are rendered in
// citations — a page renamed with its text untouched has to be re-stored, even
// though its chunks would embed identically.
func contentHash(doc tools.Document) string {
	sum := sha256.New()
	for _, part := range []string{doc.Title, doc.URL, doc.Content} {
		sum.Write([]byte(part))
		// A separator, so that moving text between the title and the body
		// cannot produce the same digest.
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// inTx runs fn inside a transaction.
func (ix *Indexer) inTx(ctx context.Context, fn func(q store.Querier) error) error {
	return store.WithTx(ctx, ix.db, fn)
}
