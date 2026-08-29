-- name: InsertCitation :one
INSERT INTO citations (agent_run_id, evidence_id, marker, claim_text)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListCitationsByRun :many
-- Joined with evidence because a citation is only meaningful alongside what it
-- points at: the trace endpoint renders the marker, the claim, and the source's
-- title and URL together.
SELECT c.id,
       c.marker,
       c.claim_text,
       c.created_at,
       e.id  AS evidence_id,
       e.seq AS evidence_seq,
       e.source,
       e.external_id,
       e.title,
       e.url,
       e.snippet,
       e.source_timestamp
FROM citations c
         JOIN evidence e ON e.id = c.evidence_id
WHERE c.agent_run_id = $1
-- Ordered by the marker's number, not by evidence.seq: the markers are what a
-- reader follows through the answer ([1], [2], [3]), while evidence.seq is
-- discovery order and does not match. A plain text sort would put [10] before
-- [2], hence the cast.
ORDER BY nullif(regexp_replace(c.marker, '\D', '', 'g'), '')::int NULLS LAST, c.marker;
