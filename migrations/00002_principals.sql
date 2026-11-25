-- Principals, Groups and the API keys Principals authenticate with.
-- +goose Up
CREATE TABLE principals (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL UNIQUE CHECK (name <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE groups (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL UNIQUE CHECK (name <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id     uuid NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, principal_id)
);
CREATE INDEX group_members_principal_id_idx ON group_members (principal_id);

-- Only the SHA-256 of a key is stored; the key itself is shown once, at creation.
CREATE TABLE api_keys (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid        NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    key_hash     bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_principal_id_idx ON api_keys (principal_id);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE group_members;
DROP TABLE groups;
DROP TABLE principals;
