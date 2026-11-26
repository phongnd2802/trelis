-- Documents, the file bytes they were uploaded as, and their Access Lists.
-- +goose Up
CREATE TABLE documents (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Who uploaded it. The uploader is always on the Access List too.
    owner_id     uuid        NOT NULL REFERENCES principals (id),
    filename     text        NOT NULL CHECK (filename <> ''),
    content_type text        NOT NULL,
    size_bytes   bigint      NOT NULL CHECK (size_bytes > 0),
    sha256       bytea       NOT NULL,
    -- pending until ingest has made it searchable.
    status       text        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'ready', 'failed')),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX documents_owner_id_idx ON documents (owner_id);

-- The uploaded bytes, kept apart from the metadata so listing never reads
-- them. Postgres is the source of truth (ADR-0002); ingest reads them by
-- Document ID, since workflows pass IDs only (ADR-0005).
CREATE TABLE document_files (
    document_id uuid  PRIMARY KEY REFERENCES documents (id) ON DELETE CASCADE,
    content     bytea NOT NULL
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
DROP TABLE document_files;
DROP TABLE documents;
