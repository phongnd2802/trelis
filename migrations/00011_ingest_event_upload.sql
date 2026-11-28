-- Ingest Events belong to one upload (#25). A re-upload starts a new
-- upload_id and records upload_replaced; every later event names the new
-- upload, so the SSE stream can tell an older upload's terminal event from
-- the Document's current one. Existing events are backfilled from the
-- Document's current upload, which is the only upload they could have come
-- from before this column existed.
-- +goose Up
ALTER TABLE ingest_events ADD COLUMN upload_id uuid;
UPDATE ingest_events e
SET upload_id = d.upload_id
FROM documents d
WHERE d.id = e.document_id;
ALTER TABLE ingest_events ALTER COLUMN upload_id SET NOT NULL;
COMMENT ON COLUMN ingest_events.upload_id IS
    'Upload the event belongs to; the SSE stream ends only at a terminal event of the Document''s current upload';

-- +goose Down
ALTER TABLE ingest_events DROP COLUMN upload_id;