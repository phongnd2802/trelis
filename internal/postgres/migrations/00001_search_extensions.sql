-- pgvector for dense retrieval, ParadeDB pg_search for BM25.
-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_search;

-- +goose Down
DROP EXTENSION IF EXISTS pg_search;
DROP EXTENSION IF EXISTS vector;
