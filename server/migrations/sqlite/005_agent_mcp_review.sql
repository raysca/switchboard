-- +goose Up
ALTER TABLE agent_versions ADD COLUMN mcp_review_required INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE agent_versions DROP COLUMN mcp_review_required;
