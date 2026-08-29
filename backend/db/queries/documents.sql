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

-- name: CountDocumentsBySource :one
SELECT count(*) FROM documents WHERE source = $1;

-- name: CountDocuments :one
-- Used to tell "the index is empty" apart from "this query matched nothing",
-- which are different answers for the agent: the first means stop searching the
-- knowledge base, the second means rephrase.
SELECT count(*) FROM documents;
