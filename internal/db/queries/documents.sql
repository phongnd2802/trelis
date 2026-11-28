-- name: CreateDocument :one
INSERT INTO documents (id, owner_id, title, file_name, file_type, file_size, file_hash, upload_id, object_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetDocumentByOwnerAndHash :one
SELECT * FROM documents WHERE owner_id = $1 AND file_hash = $2;

-- Replacing a Document's Source File applies the new upload's metadata in
-- place and puts the Document back to pending. The upload_id predicate makes
-- the update conditional on the version the caller read: a concurrent
-- re-upload or delete changes no row, so the caller can answer a conflict
-- instead of overwriting the newer upload. The row is locked first, before
-- the caller deletes Chunks, the same order the delete cascade and the
-- ingest transactions use.
-- name: ReplaceDocumentUpload :one
UPDATE documents
SET upload_id = @upload_id,
    object_key = @object_key,
    file_name = @file_name,
    file_type = @file_type,
    file_size = @file_size,
    file_hash = @file_hash,
    title = @title,
    parse_status = 'pending',
    parse_error = NULL,
    updated_at = now()
WHERE id = @id AND upload_id = @previous_upload_id
RETURNING *;

-- The system view of one Document, with no access check: the ingest
-- activities run with no Principal.
-- name: GetDocumentByID :one
SELECT * FROM documents WHERE id = $1;

-- Deleting a Document removes its Chunks, Parent Chunks and Access List by
-- ON DELETE CASCADE and returns the upload the row pointed at when it was
-- deleted, so the cleanup cancels that upload's ingest even when a re-upload
-- won the race with the caller's read. Zero rows means it was already deleted.
-- name: DeleteDocument :one
DELETE FROM documents WHERE id = $1 RETURNING upload_id;

-- A failed parse keeps its reason; a pending, processing or completed
-- Document carries none, so the reason can neither go stale nor be set on a
-- Document that did not fail. execrows lets the caller notice a missing row.
-- name: SetDocumentParseStatus :execrows
UPDATE documents
SET parse_status = @parse_status,
    parse_error = CASE
        WHEN @parse_status = 'failed' THEN NULLIF(@parse_error::text, '')
        ELSE NULL
    END,
    updated_at = now()
WHERE id = @id;

-- name: GrantDocumentToPrincipal :exec
INSERT INTO document_principals (document_id, principal_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: GrantDocumentToGroup :exec
INSERT INTO document_groups (document_id, group_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- A page of the Documents a Principal can read, newest first.
-- name: ListReadableDocuments :many
SELECT * FROM documents
WHERE can_read_document(id, @principal_id::uuid)
ORDER BY created_at DESC, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountReadableDocuments :one
SELECT count(*) FROM documents WHERE can_read_document(id, @principal_id::uuid);

-- The Documents a Principal can read that finished ingest. Retrieval passes
-- this set into every search; a Document still processing has only part of
-- its Chunks embedded, so it is left out until it is completed.
-- name: ListReadableDocumentIDs :many
SELECT id FROM documents
WHERE parse_status = 'completed'
  AND can_read_document(id, @principal_id::uuid)
ORDER BY id;

-- No row when the Document does not exist or the Principal cannot read it,
-- so callers cannot tell the two apart.
-- name: GetReadableDocument :one
SELECT * FROM documents
WHERE id = @id AND can_read_document(id, @principal_id::uuid);

-- name: ListDocumentPrincipals :many
SELECT p.name
FROM document_principals dp
JOIN principals p ON p.id = dp.principal_id
WHERE dp.document_id = $1
ORDER BY p.name;

-- name: ListDocumentGroups :many
SELECT g.name
FROM document_groups dg
JOIN groups g ON g.id = dg.group_id
WHERE dg.document_id = $1
ORDER BY g.name;

-- Every Document whose parse_status is pending: its workflow start failed,
-- or the run is queued and has not set processing yet. Submitting it again is
-- safe because the workflow ID carries the Document and upload IDs and
-- StartIngest joins an open run (ADR-0005).
-- name: ListPendingDocuments :many
SELECT id, upload_id FROM documents WHERE parse_status = 'pending' ORDER BY id;

-- The current upload and parse status of each of the given Documents. The
-- reconciler deletes objects under a replaced upload only once the Document
-- has finished its new upload, because earlier the new upload's Chunks may
-- not be committed yet (ADR-0006).
-- name: ListDocumentUploads :many
SELECT id, upload_id, parse_status FROM documents WHERE id = ANY(sqlc.arg(document_ids)::uuid[]);

-- The Document's current upload ID. It is read after an Ingest Event insert,
-- which already holds the Document row lock, to notice an upload the event's
-- activity no longer belongs to.
-- name: GetDocumentUploadID :one
SELECT upload_id FROM documents WHERE id = $1;

-- The Document's current upload ID, locking the row so a concurrent re-upload
-- waits. FOR NO KEY UPDATE keeps the lock a Chunk insert's foreign-key check
-- (FOR KEY SHARE) can still take, the same row-lock mode the Ingest Event
-- insert uses.
-- name: LockDocumentUploadID :one
SELECT upload_id FROM documents WHERE id = $1 FOR NO KEY UPDATE;
