-- +goose Up
-- held: a call the retention job must not touch (a legal or operational hold); hold_reason is operator text and goes with the call.
ALTER TABLE sessions ADD COLUMN held INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN hold_reason TEXT NOT NULL DEFAULT '';
-- erasing by caller looks calls up by their label
CREATE INDEX sessions_caller ON sessions(caller);
-- retention_audit: holds, releases and erasures: the action, the subject (a session id, or a hash of a caller label), counts, the request. Never content.
CREATE TABLE retention_audit (id TEXT PRIMARY KEY, at TEXT NOT NULL, action TEXT NOT NULL, subject TEXT NOT NULL, detail TEXT NOT NULL, request_id TEXT NOT NULL);
CREATE INDEX retention_audit_at ON retention_audit(at);

-- +goose Down
DROP INDEX retention_audit_at;
DROP TABLE retention_audit;
DROP INDEX sessions_caller;
ALTER TABLE sessions DROP COLUMN hold_reason;
ALTER TABLE sessions DROP COLUMN held;
