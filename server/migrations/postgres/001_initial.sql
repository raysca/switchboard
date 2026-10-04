-- +goose Up
CREATE TABLE agents (id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', enabled BOOLEAN NOT NULL DEFAULT TRUE, archived BOOLEAN NOT NULL DEFAULT FALSE, current_version_id TEXT, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
CREATE TABLE agent_versions (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE RESTRICT, version INTEGER NOT NULL, schema_version INTEGER NOT NULL, config_json JSONB NOT NULL, config_hash TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE(agent_id,version));
CREATE TABLE provider_credentials (id TEXT PRIMARY KEY, provider TEXT NOT NULL, masked_suffix TEXT NOT NULL, validation_state TEXT NOT NULL, validated_at TIMESTAMPTZ, capabilities_json JSONB NOT NULL, ciphertext BYTEA NOT NULL, key_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
CREATE TABLE mcp_servers (id TEXT PRIMARY KEY, name TEXT NOT NULL, endpoint TEXT NOT NULL, transport TEXT NOT NULL, auth_credential_id TEXT REFERENCES provider_credentials(id) ON DELETE RESTRICT, enabled BOOLEAN NOT NULL, validation_state TEXT NOT NULL, latest_discovery_id TEXT, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
CREATE TABLE mcp_discoveries (id TEXT PRIMARY KEY, server_id TEXT NOT NULL, discovery_hash TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL);
CREATE TABLE mcp_tools (discovery_id TEXT NOT NULL REFERENCES mcp_discoveries(id) ON DELETE RESTRICT, remote_name TEXT NOT NULL, schema_json JSONB NOT NULL, schema_hash TEXT NOT NULL, PRIMARY KEY(discovery_id,remote_name));
CREATE TABLE agent_mcp_tools (agent_version_id TEXT NOT NULL REFERENCES agent_versions(id) ON DELETE RESTRICT, mcp_server_id TEXT NOT NULL, remote_name TEXT NOT NULL, schema_hash TEXT NOT NULL, required BOOLEAN NOT NULL, PRIMARY KEY(agent_version_id,mcp_server_id,remote_name));
CREATE TABLE sessions (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE RESTRICT, agent_version_id TEXT NOT NULL REFERENCES agent_versions(id) ON DELETE RESTRICT, config_hash TEXT NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, status TEXT NOT NULL, provider_stack_json JSONB NOT NULL, turn_count INTEGER NOT NULL, safe_summary TEXT NOT NULL);
CREATE TABLE turns (id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT, ordinal INTEGER NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, transcript TEXT, assistant_text TEXT, latencies_json JSONB NOT NULL, interrupted BOOLEAN NOT NULL, tools_json JSONB NOT NULL, UNIQUE(session_id,ordinal));
CREATE TABLE trace_spans (id TEXT PRIMARY KEY, session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT, turn_id TEXT REFERENCES turns(id) ON DELETE RESTRICT, parent_id TEXT REFERENCES trace_spans(id) ON DELETE RESTRICT, name TEXT NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, attributes_json JSONB NOT NULL);
CREATE TABLE trace_events (id TEXT PRIMARY KEY, span_id TEXT REFERENCES trace_spans(id) ON DELETE RESTRICT, session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT, turn_id TEXT REFERENCES turns(id) ON DELETE RESTRICT, name TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL, attributes_json JSONB NOT NULL);
CREATE TABLE tool_calls (id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT, turn_id TEXT REFERENCES turns(id) ON DELETE RESTRICT, mcp_server_id TEXT, remote_name TEXT NOT NULL, schema_hash TEXT NOT NULL, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ, status TEXT NOT NULL, arguments_summary TEXT NOT NULL, result_summary TEXT NOT NULL);
-- +goose StatementBegin
CREATE FUNCTION reject_immutable_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'immutable storage row'; END; $$;
-- +goose StatementEnd
CREATE TRIGGER agent_versions_immutable_update BEFORE UPDATE ON agent_versions FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_discoveries_immutable_update BEFORE UPDATE ON mcp_discoveries FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_tools_immutable_update BEFORE UPDATE ON mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER agent_versions_immutable_delete BEFORE DELETE ON agent_versions FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_discoveries_immutable_delete BEFORE DELETE ON mcp_discoveries FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER mcp_tools_immutable_delete BEFORE DELETE ON mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER agent_mcp_tools_immutable_update BEFORE UPDATE ON agent_mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
CREATE TRIGGER agent_mcp_tools_immutable_delete BEFORE DELETE ON agent_mcp_tools FOR EACH ROW EXECUTE FUNCTION reject_immutable_update();
-- +goose StatementBegin
CREATE FUNCTION reject_current_mcp_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF EXISTS (SELECT 1 FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=OLD.id) THEN RAISE EXCEPTION 'MCP server referenced by current agent'; END IF; RETURN OLD; END; $$;
-- +goose StatementEnd
CREATE TRIGGER mcp_servers_referenced_delete BEFORE DELETE ON mcp_servers FOR EACH ROW EXECUTE FUNCTION reject_current_mcp_delete();
