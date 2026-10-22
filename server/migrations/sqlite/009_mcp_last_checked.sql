-- +goose Up
-- last_checked_at: when the last sync result was written, success or failure (last_connected_at only moves on a connection).
ALTER TABLE mcp_servers ADD COLUMN last_checked_at TEXT;

-- +goose Down
ALTER TABLE mcp_servers DROP COLUMN last_checked_at;
