-- +goose Up
ALTER TABLE mcp_servers ADD COLUMN pending_delete BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE sessions ADD COLUMN runtime_snapshot_json TEXT NOT NULL DEFAULT '{}';
CREATE TABLE session_mcp_tools (session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT, mcp_server_id TEXT NOT NULL, remote_name TEXT NOT NULL, schema_hash TEXT NOT NULL, required BOOLEAN NOT NULL, PRIMARY KEY(session_id,mcp_server_id,remote_name));
DROP TRIGGER mcp_servers_referenced_delete ON mcp_servers;
-- +goose StatementBegin
CREATE FUNCTION reject_referenced_mcp_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF EXISTS (SELECT 1 FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=OLD.id)
 OR EXISTS (SELECT 1 FROM sessions s JOIN session_mcp_tools t ON t.session_id=s.id WHERE t.mcp_server_id=OLD.id AND s.status IN ('active','draining')) THEN
  RAISE EXCEPTION 'MCP server referenced by current agent or active session';
 END IF;
 RETURN OLD;
END; $$;
-- +goose StatementEnd
CREATE TRIGGER mcp_servers_referenced_delete BEFORE DELETE ON mcp_servers FOR EACH ROW EXECUTE FUNCTION reject_referenced_mcp_delete();

-- +goose Down
DROP TRIGGER mcp_servers_referenced_delete ON mcp_servers;
DROP FUNCTION reject_referenced_mcp_delete();
CREATE TRIGGER mcp_servers_referenced_delete BEFORE DELETE ON mcp_servers FOR EACH ROW EXECUTE FUNCTION reject_current_mcp_delete();
DROP TABLE session_mcp_tools;
ALTER TABLE sessions DROP COLUMN runtime_snapshot_json;
ALTER TABLE mcp_servers DROP COLUMN pending_delete;
