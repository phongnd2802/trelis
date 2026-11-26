-- Ingest output (ADR-0005): the Chunks of each Document, and why its parse
-- failed when it did. The ingest activity replaces a Document's Chunks as a
-- set, so a retried run never duplicates them.
-- +goose Up
ALTER TABLE documents ADD COLUMN parse_error text;
COMMENT ON COLUMN documents.parse_error IS
    'Why the last parse failed; NULL unless parse_status is failed';

CREATE TABLE chunks (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id    uuid        NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    -- Position of the Chunk in its Document, starting at 0.
    seq            int         NOT NULL,
    -- The source text of the Chunk, exactly as it appears in the Markdown.
    content        text        NOT NULL,
    -- Context that is not in the source at the offsets, such as a heading
    -- breadcrumb or a repeated table header. Empty when there is none.
    context_header text        NOT NULL DEFAULT '',
    -- Rune offsets into the Document's Markdown, the way Postgres counts
    -- characters: end_offset - start_offset is content's length in runes.
    start_offset   int         NOT NULL,
    end_offset     int         NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, seq)
);
COMMENT ON COLUMN chunks.seq IS 'Position of the Chunk in its Document, from 0';
COMMENT ON COLUMN chunks.start_offset IS 'First rune of the Markdown held in content';
COMMENT ON COLUMN chunks.end_offset IS 'Rune just past the last one held in content';

-- +goose Down
DROP TABLE chunks;
ALTER TABLE documents DROP COLUMN parse_error;