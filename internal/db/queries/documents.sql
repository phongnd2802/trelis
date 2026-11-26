-- name: CreateDocument :one
INSERT INTO documents (owner_id, filename, content_type, size_bytes, sha256)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, filename, content_type, size_bytes, status, created_at;

-- name: CreateDocumentFile :exec
INSERT INTO document_files (document_id, content) VALUES ($1, $2);

-- name: GrantDocumentToPrincipal :exec
INSERT INTO document_principals (document_id, principal_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: GrantDocumentToGroup :exec
INSERT INTO document_groups (document_id, group_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- The Documents a Principal can read: those whose Access List names them or
-- a Group they are in. Membership is read on every call, so a change to a
-- Group takes effect on the next request. Newest first.
-- name: ListReadableDocuments :many
SELECT d.id, d.filename, d.content_type, d.size_bytes, d.status, d.created_at
FROM documents d
WHERE EXISTS (
        SELECT 1 FROM document_principals dp
        WHERE dp.document_id = d.id AND dp.principal_id = @principal_id)
   OR EXISTS (
        SELECT 1 FROM document_groups dg
        JOIN group_members m ON m.group_id = dg.group_id
        WHERE dg.document_id = d.id AND m.principal_id = @principal_id)
ORDER BY d.created_at DESC, d.id;

-- No row when the Document does not exist or the Principal cannot read it,
-- so callers cannot tell the two apart.
-- name: GetReadableDocument :one
SELECT d.id, d.filename, d.content_type, d.size_bytes, d.status, d.created_at
FROM documents d
WHERE d.id = @id
  AND (EXISTS (
        SELECT 1 FROM document_principals dp
        WHERE dp.document_id = d.id AND dp.principal_id = @principal_id)
    OR EXISTS (
        SELECT 1 FROM document_groups dg
        JOIN group_members m ON m.group_id = dg.group_id
        WHERE dg.document_id = d.id AND m.principal_id = @principal_id));

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
