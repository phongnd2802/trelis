-- Parent-child chunking (#31): when chunking.parent_child is on, ingest
-- stores a wider Parent Chunk around consecutive Chunks. A Parent Chunk gives
-- an answer more context and is never embedded or searched itself, so it
-- lives in its own table and every Chunk query keeps working on chunks
-- unchanged. A retried ingest replaces a Document's Chunk set as a whole,
-- deleting its Chunks before their Parent Chunks; parent_id has no cascade
-- for that reason.
-- +goose Up
CREATE TABLE parent_chunks (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id    uuid        NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    -- Position of the Parent Chunk in its Document, starting at 0.
    seq            int         NOT NULL,
    -- The source text the Parent Chunk covers, exactly as it appears in the
    -- Markdown.
    content        text        NOT NULL,
    -- Context copied from the first of the Parent Chunk's Chunks, such as a
    -- heading breadcrumb or a repeated table header. Empty when there is none.
    context_header text        NOT NULL DEFAULT '',
    -- Rune offsets into the Document's Markdown, the way Postgres counts
    -- characters: end_offset - start_offset is content's length in runes.
    start_offset   int         NOT NULL,
    end_offset     int         NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, seq)
);
COMMENT ON COLUMN parent_chunks.seq IS 'Position of the Parent Chunk in its Document, from 0';
COMMENT ON COLUMN parent_chunks.start_offset IS 'First rune of the Markdown held in content';
COMMENT ON COLUMN parent_chunks.end_offset IS 'Rune just past the last one held in content';

ALTER TABLE chunks ADD COLUMN parent_id uuid REFERENCES parent_chunks (id);
COMMENT ON COLUMN chunks.parent_id IS
    'Parent Chunk of the Chunk; NULL when the Document was ingested without parent-child mode or before it was enabled, so a reader falls back to the Chunk itself';

-- #36 joins a Chunk to its Parent Chunk; the parent_id lookup needs its own
-- index because the foreign key does not create one.
CREATE INDEX chunks_parent_id_idx ON chunks (parent_id);

-- +goose Down
DROP INDEX chunks_parent_id_idx;
ALTER TABLE chunks DROP COLUMN parent_id;
DROP TABLE parent_chunks;