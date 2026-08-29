package rag

import (
	"strconv"
	"strings"
)

// formatVector renders an embedding in pgvector's text form, e.g. "[0.1,-0.2]".
//
// Embeddings reach Postgres as text and are cast in SQL — see
// db/queries/document_chunks.sql. The alternative is registering pgvector's
// binary codec with pgx, which means looking up an extension-provided type OID
// at connection time and keeping that wiring correct on every pool the indexer
// might run on. One cast per row, on a path already dominated by an HTTP call to
// the embedding API, is not a cost worth that complexity.
//
// 'f' with precision -1 emits the shortest decimal that round-trips the float32,
// so nothing is lost and no digits are wasted: a 1536-dimension vector is sent
// once per chunk and the difference between shortest-form and %f is tens of
// kilobytes per document.
func formatVector(values []float32) string {
	var b strings.Builder
	b.Grow(len(values) * 12)
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
