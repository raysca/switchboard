-- +goose Up
CREATE TABLE flows (id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', enabled BOOLEAN NOT NULL DEFAULT TRUE, archived BOOLEAN NOT NULL DEFAULT FALSE, current_version_id TEXT, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
CREATE TABLE flow_versions (id TEXT PRIMARY KEY, flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE RESTRICT, version INTEGER NOT NULL, schema_version INTEGER NOT NULL, config_json TEXT NOT NULL, config_hash TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE(flow_id,version));
CREATE TABLE mcp_servers (id TEXT PRIMARY KEY, name TEXT NOT NULL, endpoint TEXT NOT NULL, transport TEXT NOT NULL, auth_env TEXT NOT NULL DEFAULT '', enabled BOOLEAN NOT NULL, validation_state TEXT NOT NULL, latest_discovery_id TEXT, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
CREATE TABLE mcp_discoveries (id TEXT PRIMARY KEY, server_id TEXT NOT NULL, discovery_hash TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL);
CREATE TABLE mcp_tools (discovery_id TEXT NOT NULL REFERENCES mcp_discoveries(id) ON DELETE RESTRICT, remote_name TEXT NOT NULL, schema_json TEXT NOT NULL, schema_hash TEXT NOT NULL, PRIMARY KEY(discovery_id,remote_name));
CREATE TABLE flow_mcp_tools (flow_version_id TEXT NOT NULL REFERENCES flow_versions(id) ON DELETE RESTRICT, mcp_server_id TEXT NOT NULL, remote_name TEXT NOT NULL, schema_hash TEXT NOT NULL, required BOOLEAN NOT NULL, PRIMARY KEY(flow_version_id,mcp_server_id,remote_name));
CREATE TABLE sessions (id TEXT PRIMARY KEY, flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE RESTRICT, flow_version_id TEXT NOT NULL REFERENCES flow_versions(id) ON DELETE RESTRICT, config_hash TEXT NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, status TEXT NOT NULL, provider_stack_json TEXT NOT NULL, turn_count INTEGER NOT NULL, safe_summary TEXT NOT NULL);
CREATE TABLE turns (id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT, ordinal INTEGER NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, transcript TEXT, assistant_text TEXT, latencies_json TEXT NOT NULL, interrupted BOOLEAN NOT NULL, tools_json TEXT NOT NULL, UNIQUE(session_id,ordinal));
CREATE TABLE trace_spans (id TEXT PRIMARY KEY, session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT, turn_id TEXT REFERENCES turns(id) ON DELETE RESTRICT, parent_id TEXT REFERENCES trace_spans(id) ON DELETE RESTRICT, name TEXT NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, attributes_json TEXT NOT NULL);
CREATE TABLE trace_events (id TEXT PRIMARY KEY, span_id TEXT REFERENCES trace_spans(id) ON DELETE RESTRICT, session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT, turn_id TEXT REFERENCES turns(id) ON DELETE RESTRICT, name TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL, attributes_json TEXT NOT NULL);
CREATE TABLE tool_calls (id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT, turn_id TEXT REFERENCES turns(id) ON DELETE RESTRICT, mcp_server_id TEXT, remote_name TEXT NOT NULL, schema_hash TEXT NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, status TEXT NOT NULL, arguments_summary TEXT NOT NULL, result_summary TEXT NOT NULL);
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'immutable storage row'; END; $$;
-- +goose StatementEnd
CREATE TRIGGER flow_versions_immutable_update BEFORE UPDATE ON flow_versions FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_discoveries_immutable_update BEFORE UPDATE ON mcp_discoveries FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_tools_immutable_update BEFORE UPDATE ON mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER flow_versions_immutable_delete BEFORE DELETE ON flow_versions FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_discoveries_immutable_delete BEFORE DELETE ON mcp_discoveries FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_tools_immutable_delete BEFORE DELETE ON mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER flow_mcp_tools_immutable_update BEFORE UPDATE ON flow_mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER flow_mcp_tools_immutable_delete BEFORE DELETE ON flow_mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
-- +goose StatementBegin
CREATE FUNCTION reject_current_mcp_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF EXISTS (SELECT 1 FROM flows a JOIN flow_mcp_tools t ON t.flow_version_id=a.current_version_id WHERE t.mcp_server_id=OLD.id) THEN RAISE EXCEPTION 'MCP server referenced by current flow'; END IF; RETURN OLD; END; $$;
-- +goose StatementEnd
CREATE TRIGGER mcp_servers_referenced_delete BEFORE DELETE ON mcp_servers FOR EACH ROW EXECUTE FUNCTION reject_current_mcp_delete();
-- +goose StatementBegin
CREATE FUNCTION check_flow_current_version() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.current_version_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM flow_versions v WHERE v.id=NEW.current_version_id AND v.flow_id=NEW.id) THEN RAISE EXCEPTION 'current version must belong to flow'; END IF; RETURN NEW; END; $$;
-- +goose StatementEnd
CREATE TRIGGER flows_current_version_valid BEFORE INSERT OR UPDATE OF current_version_id ON flows FOR EACH ROW EXECUTE FUNCTION check_flow_current_version();
-- +goose StatementBegin
CREATE FUNCTION check_mcp_latest() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.latest_discovery_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM mcp_discoveries d WHERE d.id=NEW.latest_discovery_id AND d.server_id=NEW.id) THEN RAISE EXCEPTION 'latest discovery must belong to MCP server'; END IF; RETURN NEW; END; $$;
-- +goose StatementEnd
CREATE TRIGGER mcp_servers_latest_valid BEFORE INSERT OR UPDATE OF latest_discovery_id ON mcp_servers FOR EACH ROW EXECUTE FUNCTION check_mcp_latest();
-- +goose StatementBegin
CREATE FUNCTION check_session_provenance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NOT EXISTS (SELECT 1 FROM flow_versions v WHERE v.id=NEW.flow_version_id AND v.flow_id=NEW.flow_id AND v.config_hash=NEW.config_hash) THEN RAISE EXCEPTION 'session provenance mismatch'; END IF; RETURN NEW; END; $$;
-- +goose StatementEnd
CREATE TRIGGER sessions_provenance_valid BEFORE INSERT OR UPDATE OF flow_id,flow_version_id,config_hash ON sessions FOR EACH ROW EXECUTE FUNCTION check_session_provenance();
CREATE TRIGGER sessions_provenance_immutable BEFORE UPDATE OF flow_id,flow_version_id,config_hash ON sessions FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
