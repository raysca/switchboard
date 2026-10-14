-- +goose Up
ALTER TABLE sessions ADD COLUMN draft BOOLEAN NOT NULL DEFAULT FALSE;
-- A draft test call runs an unpublished config on top of a version: its row names that version, and
-- carries the draft's hash, so only the version/flow match is required for drafts.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION check_session_provenance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NOT EXISTS (SELECT 1 FROM flow_versions v WHERE v.id=NEW.flow_version_id AND v.flow_id=NEW.flow_id AND (NEW.draft OR v.config_hash=NEW.config_hash)) THEN RAISE EXCEPTION 'session provenance mismatch'; END IF; RETURN NEW; END; $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION check_session_provenance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NOT EXISTS (SELECT 1 FROM flow_versions v WHERE v.id=NEW.flow_version_id AND v.flow_id=NEW.flow_id AND v.config_hash=NEW.config_hash) THEN RAISE EXCEPTION 'session provenance mismatch'; END IF; RETURN NEW; END; $$;
-- +goose StatementEnd
ALTER TABLE sessions DROP COLUMN draft;
