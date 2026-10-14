-- +goose Up
ALTER TABLE flow_versions ADD COLUMN mcp_review_required BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE flow_versions DROP COLUMN mcp_review_required;
