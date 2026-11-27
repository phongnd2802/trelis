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
--
-- #139: the distance is guarded by CASE because a plan may evaluate it before
-- the model filter, and a Chunk of another model may have another dimension.
-- The model filter stays in the scan too, so it narrows the rows before any
-- scoring. A NULL score (another model or no embedding) or a NaN score (a
-- zero-norm vector) can never be a match; NaN would also break JSON encoding.
-- name: SearchDenseChunks :many
WITH query_vec AS MATERIALIZED (SELECT sqlc.arg(query_embedding)::text::vector AS embedding)
SELECT id, document_id, seq, content, context_header, score
FROM (
    SELECT chunks.id, chunks.document_id, chunks.seq, chunks.content, chunks.context_header,
           (1 - CASE WHEN chunks.embedding_model = sqlc.arg(embedding_model)::text
                     THEN chunks.embedding <=> query_vec.embedding END)::float8 AS score
    FROM chunks, query_vec
    WHERE document_id = ANY(sqlc.arg(document_ids)::uuid[])
      AND embedding_model = sqlc.arg(embedding_model)::text
      AND chunks.embedding IS NOT NULL
) ranked
WHERE score <> 'NaN'::float8
ORDER BY score DESC, id
LIMIT sqlc.arg(top_k);

-- BM25 retrieval: rank the Chunks of the readable Documents by pg_search's
-- BM25 score over the text dense embeds, ContextHeader + "\n\n" + Content.
-- The index has that text twice, exact and ASCII-folded, so a query typed
-- without diacritics ("thue tncn") matches "thuế TNCN" and one typed with
-- diacritics matches the exact field.
--
-- The readable-ID filter is indexed and stays in the scan: pg_search pushes
-- it into the Tantivy query before the top-K, so a Chunk the caller cannot
-- read neither comes back nor takes a readable Chunk's place (ADR-0004).
--
-- ||| is a match-disjunction query, not the Tantivy query-string parser: it
-- tokenizes the query like the field, so punctuation in a caller's query is
-- only a separator and cannot error or change its meaning.
--
-- The text expression below must stay identical to the index's expression in
-- migrations/00008_bm25_chunks.sql: pg_search only uses the index when it
-- matches exactly. It must also stay identical to the text ingest embeds,
-- ContextHeader + "\n\n" + Content (chunkText in
-- internal/application/service/ingest.go).
-- name: SearchBM25Chunks :many
SELECT id, document_id, seq, content, context_header, score::float8 AS score
FROM (
    SELECT chunks.id, chunks.document_id, chunks.seq, chunks.content, chunks.context_header,
           pdb.score(chunks) AS score
    FROM chunks
    WHERE chunks.document_id = ANY(sqlc.arg(document_ids)::uuid[])
      AND ((chunks.context_header || E'\n\n' || chunks.content)::pdb.alias('chunk_text') ||| sqlc.arg(query)::text
        OR (chunks.context_header || E'\n\n' || chunks.content)::pdb.alias('chunk_text_folded') ||| sqlc.arg(query)::text)
    ORDER BY pdb.score(chunks) DESC, chunks.id
    LIMIT sqlc.arg(top_k)
) ranked
ORDER BY score DESC, id;
