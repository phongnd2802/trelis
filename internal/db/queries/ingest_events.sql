-- Appends one Ingest Event to its Document's stream and returns it with the
-- per-Document seq and created_at Postgres assigned. The UPDATE advances the
-- Document's counter under its row lock, held until this transaction commits:
-- the next event of that Document cannot take a seq until this one has
-- committed, so seq order is commit order and a reader that resumes after a
-- seq never misses an event. Zero rows means the Document is gone; the
-- repository maps that to ErrDocumentNotFound.
--
-- A caller inside a transaction inserts the event first: the UPDATE locks the
-- Document row before the transaction touches any Chunk rows, the same order
-- the delete cascade uses, so the two cannot deadlock. The lock is held until
-- commit, which is what makes seq order commit order.
-- name: InsertIngestEvent :one
WITH next AS (
    UPDATE documents
    SET ingest_event_seq = ingest_event_seq + 1
    WHERE id = sqlc.arg(document_id)
    RETURNING ingest_event_seq
)
INSERT INTO ingest_events (document_id, seq, type, detail)
SELECT sqlc.arg(document_id), next.ingest_event_seq, sqlc.arg(type), sqlc.arg(detail)
FROM next
RETURNING *;

-- The events of one Document after the client's Last-Event-ID, in seq order.
-- The primary-key index on (document_id, seq) serves both the filter and the
-- order.
-- name: ListIngestEventsAfter :many
SELECT * FROM ingest_events
WHERE document_id = sqlc.arg(document_id)
  AND seq > sqlc.arg(after_seq)
ORDER BY seq;
