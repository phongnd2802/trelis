-- name: DeleteChunksByDocument :exec
DELETE FROM chunks WHERE document_id = $1;

-- name: InsertChunk :exec
INSERT INTO chunks (document_id, seq, content, context_header, start_offset, end_offset)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListChunksByDocument :many
SELECT * FROM chunks WHERE document_id = $1 ORDER BY seq;