-- A revoked key stays listed, with when it was revoked, but no longer authenticates.
-- +goose Up
ALTER TABLE api_keys ADD COLUMN revoked_at timestamptz;

-- +goose Down
ALTER TABLE api_keys DROP COLUMN revoked_at;
