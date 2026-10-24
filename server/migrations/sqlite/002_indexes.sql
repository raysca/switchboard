-- +goose Up
CREATE INDEX idx_sessions_agent_version ON sessions(agent_version_id);
CREATE INDEX idx_turns_session ON turns(session_id);
CREATE INDEX idx_trace_events_session_time ON trace_events(session_id,occurred_at);
CREATE INDEX idx_mcp_discoveries_server ON mcp_discoveries(server_id);
CREATE INDEX idx_eval_jobs_claim ON eval_jobs(status,run_after);
CREATE INDEX idx_eval_jobs_run ON eval_jobs(run_id);
CREATE INDEX idx_eval_results_run ON eval_results(run_id);
CREATE INDEX idx_eval_runs_agent ON eval_runs(agent_id,ended_at);
