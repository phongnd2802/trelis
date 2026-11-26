-- Source Files move to S3-compatible object storage (ADR-0006). A Document
-- row stores the object key of its current upload, never the bucket; each
-- upload gets its own upload_id so a re-upload never overwrites the bytes an
-- in-flight ingest is reading.
-- +goose Up
ALTER TABLE documents RENAME COLUMN file_path TO object_key;
ALTER TABLE documents ADD COLUMN upload_id uuid NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE documents ALTER COLUMN upload_id DROP DEFAULT;
COMMENT ON COLUMN documents.object_key IS
    'Key of the Source File in the bucket: documents/{id}/{upload_id}/source';

-- +goose Down
COMMENT ON COLUMN documents.object_key IS NULL;
ALTER TABLE documents DROP COLUMN upload_id;
ALTER TABLE documents RENAME COLUMN object_key TO file_path;
