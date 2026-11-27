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

-- Dense retrieval: rank the Chunks of the readable Documents by cosine
-- similarity to the query vector. Exact scan on purpose: no HNSW index yet,
-- so recall is perfect while the eval baseline is built; add the index when
-- latency, not recall, is the constraint. Only Chunks embedded by the same
-- model are comparable, and NULL embeddings cannot be searched at all.
--
-- #143: the generic plan re-parsed the repeated query-vector ::text::vector
-- cast for every Chunk; query_vec casts it once. The readable-ID filter stays
-- in the scan so the Access List limits the work.
-- name: SearchDenseChunks :many
WITH query_vec AS MATERIALIZED (SELECT sqlc.arg(query_embedding)::text::vector AS embedding)
SELECT id, document_id, seq, content, context_header,
       (1 - (chunks.embedding <=> query_vec.embedding))::float8 AS score
FROM chunks, query_vec
WHERE document_id = ANY(sqlc.arg(document_ids)::uuid[])
  AND embedding_model = sqlc.arg(embedding_model)::text
  AND chunks.embedding IS NOT NULL
  -- A zero-norm vector makes cosine distance NaN; it can never be a match
  -- and would not survive JSON encoding either.
  AND (chunks.embedding <=> query_vec.embedding) <> 'NaN'::float8
ORDER BY chunks.embedding <=> query_vec.embedding, id
LIMIT sqlc.arg(top_k);
