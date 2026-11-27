-- name: CreateDocument :one
INSERT INTO documents (id, owner_id, title, file_name, file_type, file_size, file_hash, upload_id, object_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetDocumentByOwnerAndHash :one
SELECT * FROM documents WHERE owner_id = $1 AND file_hash = $2;

-- The system view of one Document, with no access check: the ingest
-- activities run with no Principal.
-- name: GetDocumentByID :one
SELECT * FROM documents WHERE id = $1;

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
