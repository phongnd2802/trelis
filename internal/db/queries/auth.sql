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

-- name: CreateAPIKey :exec
INSERT INTO api_keys (principal_id, key_hash) VALUES ($1, $2);

-- name: GetPrincipalByKeyHash :one
SELECT p.id, p.name
FROM api_keys k
JOIN principals p ON p.id = k.principal_id
WHERE k.key_hash = $1;

-- name: ListPrincipalGroups :many
SELECT g.id, g.name
FROM group_members m
JOIN groups g ON g.id = m.group_id
WHERE m.principal_id = $1
ORDER BY g.name;
