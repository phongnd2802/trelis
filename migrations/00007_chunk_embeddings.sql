-- Dense retrieval reads an embedding per Chunk; the ingest workflow writes
-- one before it marks a Document completed. The column has no fixed
-- dimension because the embedding model decides it, and swapping models then
-- needs no migration. The HNSW index (#32) will pin the dimension it indexes
-- in an expression index instead.
-- +goose Up
ALTER TABLE chunks ADD COLUMN embedding vector;
ALTER TABLE chunks ADD COLUMN embedding_model text;
COMMENT ON COLUMN chunks.embedding IS
    'Vector of the Chunk text, produced by the model named in embedding_model; NULL until embedded';
COMMENT ON COLUMN chunks.embedding_model IS
    'Model that produced embedding; NULL while embedding is NULL';

-- +goose Down
ALTER TABLE chunks DROP COLUMN embedding_model;
ALTER TABLE chunks DROP COLUMN embedding;