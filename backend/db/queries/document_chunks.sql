-- name: DeleteChunksByDocument :exec
DELETE FROM document_chunks WHERE document_id = $1;

-- name: InsertDocumentChunk :exec
-- The embedding arrives as pgvector's text form and is cast in SQL. Passing it
-- as text keeps pgx out of the business of knowing the vector type's OID (which
-- is dynamic, since vector comes from an extension) at the cost of one cast per
-- row on a path that is already dominated by the embedding API call.
INSERT INTO document_chunks (document_id, chunk_index, content, embedding)
VALUES ($1, $2, $3, (sqlc.arg(embedding)::text)::vector);

-- name: SearchDocumentChunks :many
-- The join is the point: the vector index finds the chunk, and the
-- relational half supplies the title, URL and metadata that make it citable.
-- Doing both in one query is only possible because the vectors live in the same
-- Postgres as everything else.
--
-- <=> is cosine DISTANCE (0 = identical), so similarity is 1 - distance and the
-- ordering is ascending. The operator must match the index's vector_cosine_ops
-- or the planner silently ignores the index.
SELECT c.id            AS chunk_id,
       c.chunk_index,
       c.content,
       d.id            AS document_id,
       d.source,
       d.external_id,
       d.title,
       d.url,
       d.metadata,
       d.source_timestamp,
       (1 - (c.embedding <=> (sqlc.arg(query_embedding)::text)::vector))::float8 AS similarity
FROM document_chunks c
         JOIN documents d ON d.id = c.document_id
WHERE sqlc.narg(source)::text IS NULL
   OR d.source = sqlc.narg(source)::text
ORDER BY c.embedding <=> (sqlc.arg(query_embedding)::text)::vector
LIMIT sqlc.arg(result_limit);
