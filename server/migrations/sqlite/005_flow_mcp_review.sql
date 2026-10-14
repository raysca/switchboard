-- +goose Up
ALTER TABLE flow_versions ADD COLUMN mcp_review_required INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE flow_versions DROP COLUMN mcp_review_required;
