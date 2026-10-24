-- +goose Up
-- secrets: values sealed with the deployment master key (AES-256-GCM, name and key id bound as associated data). Never returned by any API.
CREATE TABLE secrets (name TEXT PRIMARY KEY, key_id TEXT NOT NULL, nonce BYTEA NOT NULL, ciphertext BYTEA NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_used_at TEXT);

-- +goose Down
DROP TABLE secrets;
