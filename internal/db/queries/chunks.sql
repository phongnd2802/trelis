-- name: DeleteChunksByDocument :exec
DELETE FROM chunks WHERE document_id = $1;

-- name: InsertChunk :exec
INSERT INTO chunks (document_id, seq, content, context_header, start_offset, end_offset)
VALUES ($1, $2, $3, $4, $5, $6);

-- One batch of embedding work: the Chunks of a seq range that the embed
-- activity has not written a vector for yet. Re-running a range, as a retry
-- does, skips the Chunks already done.
-- name: ListUnembeddedChunksByDocument :many
SELECT id, seq, content, context_header, start_offset, end_offset
FROM chunks
WHERE document_id = sqlc.arg(document_id)
  AND seq >= sqlc.arg(from_seq)
  AND seq < sqlc.arg(to_seq)
  AND embedding IS NULL
ORDER BY seq;

-- The vector goes in as its text form and is cast here, which is also what
-- keeps sqlc from having to map the vector type. execrows lets the caller
-- notice a Chunk that disappeared between listing and updating.
-- name: SetChunkEmbedding :execrows
UPDATE chunks
SET embedding = sqlc.arg(embedding)::text::vector,
    embedding_model = sqlc.arg(embedding_model)::text
WHERE id = sqlc.arg(id);

-- name: CountChunksWithoutEmbedding :one
SELECT count(*) FROM chunks WHERE document_id = $1 AND embedding IS NULL;