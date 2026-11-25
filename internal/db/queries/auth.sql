-- name: CreatePrincipal :one
INSERT INTO principals (name) VALUES ($1) RETURNING id, name;

-- name: GetPrincipalByName :one
SELECT id, name FROM principals WHERE name = $1;

-- name: CreateGroup :one
INSERT INTO groups (name) VALUES ($1) RETURNING id, name;

-- name: GetGroupByName :one
SELECT id, name FROM groups WHERE name = $1;

-- name: AddGroupMember :exec
INSERT INTO group_members (group_id, principal_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: CreateAPIKey :one
INSERT INTO api_keys (principal_id, key_hash) VALUES ($1, $2)
RETURNING id, created_at, revoked_at;

-- name: ListAPIKeys :many
SELECT id, created_at, revoked_at
FROM api_keys
WHERE principal_id = $1
ORDER BY created_at, id;

-- Revoking twice keeps the first revocation time.
-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1;

-- name: GetPrincipalByKeyHash :one
SELECT p.id, p.name
FROM api_keys k
JOIN principals p ON p.id = k.principal_id
WHERE k.key_hash = $1 AND k.revoked_at IS NULL;

-- name: ListPrincipalGroups :many
SELECT g.id, g.name
FROM group_members m
JOIN groups g ON g.id = m.group_id
WHERE m.principal_id = $1
ORDER BY g.name;
