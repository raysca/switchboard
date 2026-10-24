-- +goose Up
-- http_tools: the current name and definition hash of each HTTP tool. Definitions are immutable and kept by hash,
-- so an agent version that pinned an older one stays reviewable. An agent version pins a tool in agent_mcp_tools
-- with mcp_server_id "http:<tool id>".
CREATE TABLE http_tools (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, enabled INTEGER NOT NULL, current_hash TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE http_tool_definitions (tool_id TEXT NOT NULL REFERENCES http_tools(id) ON DELETE RESTRICT, hash TEXT NOT NULL, definition_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(tool_id, hash));
CREATE TRIGGER http_tool_definitions_immutable_update BEFORE UPDATE ON http_tool_definitions BEGIN SELECT RAISE(ABORT, 'immutable storage row'); END;

-- +goose Down
DROP TRIGGER http_tool_definitions_immutable_update;
DROP TABLE http_tool_definitions;
DROP TABLE http_tools;
