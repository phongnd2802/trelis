-- Documents and their Access Lists. The file itself lives in file storage
-- (internal/application/service/file); file_path points at it.
-- +goose Up
CREATE TABLE documents (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Who uploaded it. The uploader is always on the Access List too.
    owner_id     uuid        NOT NULL REFERENCES principals (id),
    title        text        NOT NULL,
    file_name    text        NOT NULL CHECK (file_name <> ''),
    -- Lower-case extension, e.g. "pdf".
    file_type    text        NOT NULL,
    file_size    bigint      NOT NULL CHECK (file_size > 0),
    -- Hex SHA-256 of the content; an owner cannot upload the same bytes twice.
    file_hash    text        NOT NULL,
    -- Provider path from file storage, e.g. local://<document id>/<name>.
    file_path    text        NOT NULL,
    -- pending until ingest has made it searchable.
    parse_status text        NOT NULL DEFAULT 'pending'
                             CHECK (parse_status IN ('pending', 'processing', 'completed', 'failed')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, file_hash)
);

-- A Document's Access List: the Principals and Groups that may read it.
CREATE TABLE document_principals (
    document_id  uuid NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, principal_id)
);
CREATE INDEX document_principals_principal_id_idx ON document_principals (principal_id);

CREATE TABLE document_groups (
    document_id uuid NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    group_id    uuid NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, group_id)
);
CREATE INDEX document_groups_group_id_idx ON document_groups (group_id);

-- The one place that decides whether a Principal can read a Document: its
-- Access List names them or a Group they are in. Membership is read on every
-- call, so a change takes effect on the next request. A plain SQL function,
-- so the planner inlines it into the queries that use it.
-- +goose StatementBegin
CREATE FUNCTION can_read_document(doc uuid, principal uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
               SELECT 1 FROM document_principals dp
               WHERE dp.document_id = doc AND dp.principal_id = principal)
        OR EXISTS (
               SELECT 1 FROM document_groups dg
               JOIN group_members m ON m.group_id = dg.group_id
               WHERE dg.document_id = doc AND m.principal_id = principal)
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION can_read_document(uuid, uuid);
DROP TABLE document_groups;
DROP TABLE document_principals;
DROP TABLE documents;
