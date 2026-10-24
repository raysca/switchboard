-- +goose Up
-- held: a call the retention job must not touch (a legal or operational hold); hold_reason is operator text and goes with the call.
ALTER TABLE sessions ADD COLUMN held INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN hold_reason TEXT NOT NULL DEFAULT '';
-- erasing by caller looks calls up by their label
CREATE INDEX sessions_caller ON sessions(caller);
-- retention_audit: holds, releases and erasures: the action, the subject (a session id, or a hash of a caller label), counts, the request. Never content.
CREATE TABLE retention_audit (id TEXT PRIMARY KEY, at TEXT NOT NULL, action TEXT NOT NULL, subject TEXT NOT NULL, detail TEXT NOT NULL, request_id TEXT NOT NULL);
CREATE INDEX retention_audit_at ON retention_audit(at);
-- erasing a call finds its rows by span, turn and session; without these each erasure scans whole tables while it holds the write lock
CREATE INDEX idx_trace_events_span ON trace_events(span_id);
CREATE INDEX idx_trace_events_turn ON trace_events(turn_id);
CREATE INDEX idx_trace_spans_session ON trace_spans(session_id);
CREATE INDEX idx_trace_spans_turn ON trace_spans(turn_id);
CREATE INDEX idx_tool_calls_session ON tool_calls(session_id);

-- +goose Down
DROP INDEX idx_tool_calls_session;
DROP INDEX idx_trace_spans_turn;
DROP INDEX idx_trace_spans_session;
DROP INDEX idx_trace_events_turn;
DROP INDEX idx_trace_events_span;
DROP INDEX retention_audit_at;
DROP TABLE retention_audit;
DROP INDEX sessions_caller;
ALTER TABLE sessions DROP COLUMN hold_reason;
ALTER TABLE sessions DROP COLUMN held;
