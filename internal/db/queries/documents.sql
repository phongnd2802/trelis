-- name: CreateDocument :one
INSERT INTO documents (id, owner_id, title, file_name, file_type, file_size, file_hash, file_path)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetDocumentByOwnerAndHash :one
SELECT * FROM documents WHERE owner_id = $1 AND file_hash = $2;

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
