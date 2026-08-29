package rag_test

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/tools"
)

// Test support for the RAG layer.
//
// Everything here is hand-written: the locked stack (CLAUDE.md) carries no test
// dependencies, and the two things worth faking — the embedding provider and a
// crawlable source — are small enough that a fake is clearer than a framework.
//
// The embedder is a *counting* fake on purpose. TEST-4.4 is entirely about how
// many times it was called: embedding is the only part of indexing that costs
// money, so "a reindex over unchanged content embeds nothing" is a property that
// can only be observed by counting calls.

// embeddingDimensions must match the vector(1536) column in migration 003. A
// mismatch is rejected by Postgres, which is the check we want: a fake that
// silently produced 8-dimension vectors would test nothing about the real path.
const embeddingDimensions = 1536

// ---------------------------------------------------------------------------
// embedder
// ---------------------------------------------------------------------------

// fakeEmbedder records every call and hands back deterministic vectors.
type fakeEmbedder struct {
	mu sync.Mutex
	// calls is the number of Embed invocations, i.e. API round trips.
	calls int
	// batches records the size of each call, so the ≤100 batching rule
	// (REQ-4.5) can be asserted.
	batches []int
	// texts is everything that was ever submitted for embedding.
	texts []string
	// fixed maps an input to a specific vector; anything else gets a
	// deterministic hash-derived one.
	fixed map[string][]float32
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.batches = append(f.batches, len(texts))
	f.texts = append(f.texts, texts...)
	if f.err != nil {
		return nil, f.err
	}

	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		if vector, ok := f.fixed[text]; ok {
			out = append(out, vector)
			continue
		}
		out = append(out, hashVector(text))
	}
	return out, nil
}

// reset zeroes the counters between phases of a test.
func (f *fakeEmbedder) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = 0
	f.batches = nil
	f.texts = nil
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeEmbedder) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.batches))
	copy(out, f.batches)
	return out
}

func (f *fakeEmbedder) submitted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.texts))
	copy(out, f.texts)
	return out
}

// hashVector derives a stable vector from a string, so the same chunk always
// embeds the same way and a test can re-run without surprises.
func hashVector(text string) []float32 {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(text))
	seed := sum.Sum32()

	vector := make([]float32, embeddingDimensions)
	vector[seed%embeddingDimensions] = 1
	vector[(seed/7)%embeddingDimensions] = 0.5
	return vector
}

// sparseVector builds an embedding with the given components set, which is what
// makes cosine ordering predictable by hand in TEST-4.5.
func sparseVector(components map[int]float32) []float32 {
	vector := make([]float32, embeddingDimensions)
	for index, value := range components {
		vector[index] = value
	}
	return vector
}

// ---------------------------------------------------------------------------
// document source
// ---------------------------------------------------------------------------

// fakeSource is a crawlable system whose contents the test dictates.
type fakeSource struct {
	name string

	mu      sync.Mutex
	docs    []tools.Document
	err     error
	fetches int
}

var _ tools.DocumentSource = (*fakeSource)(nil)

func (s *fakeSource) Name() string { return s.name }

func (s *fakeSource) FetchAll(_ context.Context) ([]tools.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]tools.Document, len(s.docs))
	copy(out, s.docs)
	return out, nil
}

// setDocs replaces what the next crawl will return.
func (s *fakeSource) setDocs(docs ...tools.Document) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs = docs
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// database
// ---------------------------------------------------------------------------

// testDatabaseLockID namespaces the advisory lock that serializes database-backed
// tests across packages. Any stable constant works; this one spells "cortex".
const testDatabaseLockID = int64(0x636F72746578)

// testPool connects to TEST_DATABASE_URL and truncates. Integration tests SKIP
// (never fail) when the variable is unset, exactly as the Day 1 API tests do.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database-backed test")
	}
	assertNotDevDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping TEST_DATABASE_URL: %v", err)
	}

	// `go test ./...` runs packages in parallel, and every database-backed test
	// in this repository truncates. A session-level advisory lock serializes
	// them across packages.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test database connection: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", testDatabaseLockID); err != nil {
		conn.Release()
		t.Fatalf("lock test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", testDatabaseLockID); err != nil {
			t.Errorf("unlock test database: %v", err)
		}
		conn.Release()
	})

	// documents cascades to document_chunks; users cascades to everything the
	// agent tests write. The index is global, so it has to start empty.
	if _, err := pool.Exec(ctx, "TRUNCATE documents, users CASCADE"); err != nil {
		t.Fatalf("truncate test database: %v", err)
	}
	return pool
}

// assertNotDevDatabase refuses to run against the development database, which
// testPool would truncate.
func assertNotDevDatabase(t *testing.T, dsn string) {
	t.Helper()
	name, err := databaseName(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if devDSN := os.Getenv("DATABASE_URL"); devDSN != "" {
		if devName, err := databaseName(devDSN); err == nil && devName == name {
			t.Fatalf("TEST_DATABASE_URL points at the development database %q; tests truncate it", name)
		}
	}
	if !strings.HasSuffix(name, "_test") {
		t.Fatalf("TEST_DATABASE_URL database %q must end in _test; tests truncate it", name)
	}
}

// databaseName extracts the database name from a DSN without ever echoing the
// DSN (it carries the password).
func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// Deliberately not %w: url.Parse returns *url.Error, whose Error()
		// formats the raw URL — password included.
		return "", errors.New("invalid DSN")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return "", errors.New("DSN has no database name")
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// database fixtures and readbacks
// ---------------------------------------------------------------------------

// vectorLiteral renders an embedding in pgvector's text form for a raw INSERT.
func vectorLiteral(values []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", v)
	}
	b.WriteByte(']')
	return b.String()
}

// seedDocument inserts a document row directly, bypassing the indexer, so a
// search test does not depend on the pipeline it is not testing.
func seedDocument(t *testing.T, pool *pgxpool.Pool, doc tools.Document) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO documents (source, external_id, title, url, content, content_hash, source_timestamp)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		doc.Source, doc.ExternalID, doc.Title, doc.URL, doc.Content,
		"seeded-"+doc.Source+"-"+doc.ExternalID, doc.Timestamp).Scan(&id); err != nil {
		t.Fatalf("insert document %s/%s: %v", doc.Source, doc.ExternalID, err)
	}
	return id
}

// seedChunk inserts one chunk with a hand-chosen embedding.
func seedChunk(t *testing.T, pool *pgxpool.Pool, documentID uuid.UUID, index int, content string, embedding []float32) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO document_chunks (document_id, chunk_index, content, embedding)
		 VALUES ($1, $2, $3, ($4::text)::vector)`,
		documentID, index, content, vectorLiteral(embedding)); err != nil {
		t.Fatalf("insert chunk %d: %v", index, err)
	}
}

// chunkContents reads back the stored chunks of one document, in order.
func chunkContents(t *testing.T, pool *pgxpool.Pool, source, externalID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT c.content
		 FROM document_chunks c
		 JOIN documents d ON d.id = c.document_id
		 WHERE d.source = $1 AND d.external_id = $2
		 ORDER BY c.chunk_index`, source, externalID)
	if err != nil {
		t.Fatalf("query chunks: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan chunk: %v", err)
		}
		out = append(out, content)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunks: %v", err)
	}
	return out
}

// contentHashOf reads the stored idempotence key.
func contentHashOf(t *testing.T, pool *pgxpool.Pool, source, externalID string) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(context.Background(),
		`SELECT content_hash FROM documents WHERE source = $1 AND external_id = $2`,
		source, externalID).Scan(&hash); err != nil {
		t.Fatalf("load content_hash for %s/%s: %v", source, externalID, err)
	}
	return hash
}

func queryInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}
