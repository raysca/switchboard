-- +goose Up
ALTER TABLE mcp_servers ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_servers ADD COLUMN last_connected_at TEXT;
ALTER TABLE mcp_servers ADD COLUMN last_discovered_at TEXT;
ALTER TABLE mcp_tools ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE mcp_tools DROP COLUMN description;
ALTER TABLE mcp_servers DROP COLUMN last_discovered_at;
ALTER TABLE mcp_servers DROP COLUMN last_connected_at;
ALTER TABLE mcp_servers DROP COLUMN description;
