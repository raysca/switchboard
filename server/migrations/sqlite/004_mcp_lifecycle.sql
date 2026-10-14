-- +goose Up
ALTER TABLE mcp_servers ADD COLUMN pending_delete INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN runtime_snapshot_json TEXT NOT NULL DEFAULT '{}';
CREATE TABLE session_mcp_tools (session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT, mcp_server_id TEXT NOT NULL, remote_name TEXT NOT NULL, schema_hash TEXT NOT NULL, required INTEGER NOT NULL, PRIMARY KEY(session_id,mcp_server_id,remote_name));
DROP TRIGGER mcp_servers_referenced_delete;
CREATE TRIGGER mcp_servers_referenced_delete BEFORE DELETE ON mcp_servers
WHEN EXISTS (SELECT 1 FROM flows a JOIN flow_mcp_tools t ON t.flow_version_id=a.current_version_id WHERE t.mcp_server_id=OLD.id)
 OR EXISTS (SELECT 1 FROM sessions s JOIN session_mcp_tools t ON t.session_id=s.id WHERE t.mcp_server_id=OLD.id AND s.status IN ('active','draining'))
BEGIN SELECT RAISE(ABORT, 'MCP server referenced by current flow or active session'); END;

-- +goose Down
DROP TRIGGER mcp_servers_referenced_delete;
CREATE TRIGGER mcp_servers_referenced_delete BEFORE DELETE ON mcp_servers
WHEN EXISTS (SELECT 1 FROM flows a JOIN flow_mcp_tools t ON t.flow_version_id=a.current_version_id WHERE t.mcp_server_id=OLD.id)
BEGIN SELECT RAISE(ABORT, 'MCP server referenced by current flow'); END;
DROP TABLE session_mcp_tools;
ALTER TABLE sessions DROP COLUMN runtime_snapshot_json;
ALTER TABLE mcp_servers DROP COLUMN pending_delete;
