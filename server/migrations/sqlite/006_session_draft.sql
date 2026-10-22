-- +goose Up
ALTER TABLE sessions ADD COLUMN draft INTEGER NOT NULL DEFAULT 0;
-- A draft test call runs an unpublished config on top of a version: its row names that version, and
-- carries the draft's hash, so only the version/agent match is required for drafts.
DROP TRIGGER sessions_provenance_valid;
CREATE TRIGGER sessions_provenance_valid BEFORE INSERT ON sessions WHEN NOT EXISTS (SELECT 1 FROM agent_versions v WHERE v.id=NEW.agent_version_id AND v.agent_id=NEW.agent_id AND (NEW.draft=1 OR v.config_hash=NEW.config_hash)) BEGIN SELECT RAISE(ABORT, 'session provenance mismatch'); END;

-- +goose Down
DROP TRIGGER sessions_provenance_valid;
CREATE TRIGGER sessions_provenance_valid BEFORE INSERT ON sessions WHEN NOT EXISTS (SELECT 1 FROM agent_versions v WHERE v.id=NEW.agent_version_id AND v.agent_id=NEW.agent_id AND v.config_hash=NEW.config_hash) BEGIN SELECT RAISE(ABORT, 'session provenance mismatch'); END;
ALTER TABLE sessions DROP COLUMN draft;
