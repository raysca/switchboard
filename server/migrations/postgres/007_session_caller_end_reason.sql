-- +goose Up
-- caller: the label the integration chose for the call (the voice token's principal); never an address.
-- end_reason: why a call ended, from a closed set; empty while it is active.
ALTER TABLE sessions ADD COLUMN caller TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN end_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN end_reason;
ALTER TABLE sessions DROP COLUMN caller;
