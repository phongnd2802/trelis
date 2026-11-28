-- Ingest Events (#23): each step a Document's ingest starts or
-- finishes, in one ordered stream per Document. They live in Postgres beside
-- the data they describe, so a step that claims to have finished is committed
-- in the same transaction as that step's output, and a deleted Document takes
-- its events with it.
-- +goose Up
-- The last Ingest Event seq allocated for each Document. The insert in
-- internal/db/queries/ingest_events.sql advances it under the Document's row
-- lock, so a later event cannot take its seq until the earlier transaction has
-- committed: seq order is commit order within one Document, and a reader that
-- resumes after a seq cannot miss an event that commits later with a lower one.
-- The UPDATE changes no key column, so it takes FOR NO KEY UPDATE, which does
-- not conflict with the FOR KEY SHARE a Chunk insert's foreign-key check takes.
ALTER TABLE documents ADD COLUMN ingest_event_seq bigint NOT NULL DEFAULT 0;
COMMENT ON COLUMN documents.ingest_event_seq IS
    'Last Ingest Event seq allocated for the Document; advanced under the row lock so seq order is commit order';

CREATE TABLE ingest_events (
    document_id uuid   NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    -- Per-Document sequence number, starting at 1. It is the SSE event id and
    -- the resume cursor: WHERE seq > Last-Event-ID replays exactly what a
    -- client missed.
    seq         bigint NOT NULL,
    type        text   NOT NULL,
    -- Event detail: IDs and counts only, never file names, Chunk text or raw
    -- error text.
    detail      jsonb  NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- The primary-key index also serves (document_id, seq) filters and order.
    PRIMARY KEY (document_id, seq)
);
COMMENT ON TABLE ingest_events IS 'Ingest Events of each Document, in seq order';
COMMENT ON COLUMN ingest_events.seq IS
    'Per-Document sequence number; the SSE event id and the resume cursor';
COMMENT ON COLUMN ingest_events.type IS
    'Step event: parse_started, parse_finished, embed_batch_finished, completed or failed';
COMMENT ON COLUMN ingest_events.detail IS
    'Event detail carrying IDs and counts only, never Document content, file names or raw errors';

-- +goose Down
DROP TABLE ingest_events;
ALTER TABLE documents DROP COLUMN ingest_event_seq;
