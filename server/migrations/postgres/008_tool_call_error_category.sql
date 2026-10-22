-- +goose Up
-- error_category: why a tool call failed, from a closed set; empty when it did not fail.
ALTER TABLE tool_calls ADD COLUMN error_category TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tool_calls DROP COLUMN error_category;
