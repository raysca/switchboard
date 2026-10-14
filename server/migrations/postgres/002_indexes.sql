-- +goose Up
CREATE INDEX idx_sessions_flow_version ON sessions(flow_version_id);
CREATE INDEX idx_turns_session ON turns(session_id);
CREATE INDEX idx_trace_events_session_time ON trace_events(session_id,occurred_at);
CREATE INDEX idx_mcp_discoveries_server ON mcp_discoveries(server_id);
