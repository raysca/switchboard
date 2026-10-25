-- +goose Up
-- secret_audit: who-changed-what for stored secrets: the action, the secret's name and the request that did it. Never a value.
CREATE TABLE secret_audit (id TEXT PRIMARY KEY, at TEXT NOT NULL, action TEXT NOT NULL, name TEXT NOT NULL, request_id TEXT NOT NULL);
CREATE INDEX secret_audit_at ON secret_audit(at);

-- +goose Down
DROP INDEX secret_audit_at;
DROP TABLE secret_audit;
