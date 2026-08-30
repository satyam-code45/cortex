-- name: GetDocumentBySourceExternalID :one
SELECT * FROM documents
WHERE source = $1 AND external_id = $2;

-- name: UpsertDocument :one
-- Called only for documents whose content_hash actually changed (the indexer
-- short-circuits before this on an unchanged hash), so the update branch always
-- has work to do.
INSERT INTO documents (source, external_id, title, url, content, metadata,
                       content_hash, source_timestamp)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (source, external_id) DO UPDATE
    SET title            = excluded.title,
        url              = excluded.url,
        content          = excluded.content,
        metadata         = excluded.metadata,
        content_hash     = excluded.content_hash,
        source_timestamp = excluded.source_timestamp,
        updated_at       = now()
RETURNING *;

-- name: ListDocuments :many
-- The Sources view listing. Both filters are optional (NULL disables them);
-- ILIKE over title/content is deliberate — 121 docs need no FTS index, and the
-- snippet windowing happens in Go where it can be rune-safe.
SELECT id, source, external_id, title, url, content, source_timestamp, updated_at
FROM documents
WHERE (sqlc.narg('source')::text IS NULL OR source = sqlc.narg('source')::text)
  AND (sqlc.narg('query')::text IS NULL
       OR title ILIKE '%' || sqlc.narg('query')::text || '%'
       OR content ILIKE '%' || sqlc.narg('query')::text || '%')
ORDER BY source_timestamp DESC NULLS LAST, id
LIMIT sqlc.arg('limit_') OFFSET sqlc.arg('offset_');

-- name: CountDocumentsFiltered :many
-- Per-source tab counts under the same filters as ListDocuments, so the tabs
-- and the list never disagree.
SELECT source, count(*) AS count
FROM documents
WHERE (sqlc.narg('source')::text IS NULL OR source = sqlc.narg('source')::text)
  AND (sqlc.narg('query')::text IS NULL
       OR title ILIKE '%' || sqlc.narg('query')::text || '%'
       OR content ILIKE '%' || sqlc.narg('query')::text || '%')
GROUP BY source;

-- name: SourceLastIndexed :many
-- Last content change per source (updated_at only moves when content_hash
-- changes) — distinct from "last refreshed", which comes from River job rows.
SELECT source, max(updated_at)::timestamptz AS last_indexed
FROM documents
GROUP BY source;

-- name: GetDocumentByID :one
SELECT * FROM documents WHERE id = $1;

-- name: CountDocumentsBySource :one
SELECT count(*) FROM documents WHERE source = $1;

-- name: CountDocuments :one
-- Used to tell "the index is empty" apart from "this query matched nothing",
-- which are different answers for the agent: the first means stop searching the
-- knowledge base, the second means rephrase.
SELECT count(*) FROM documents;
