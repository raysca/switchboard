-- +goose Up
ALTER TABLE agent_versions ADD COLUMN mcp_review_required BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE agent_versions DROP COLUMN mcp_review_required;
