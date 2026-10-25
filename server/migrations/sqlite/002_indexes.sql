-- +goose Up
CREATE INDEX idx_sessions_agent_version ON sessions(agent_version_id);
CREATE INDEX idx_turns_session ON turns(session_id);
CREATE INDEX idx_trace_events_session_time ON trace_events(session_id,occurred_at);
CREATE INDEX idx_mcp_discoveries_server ON mcp_discoveries(server_id);
CREATE INDEX idx_eval_jobs_claim ON eval_jobs(status,run_after);
CREATE INDEX idx_eval_jobs_run ON eval_jobs(run_id);
CREATE INDEX idx_eval_results_run ON eval_results(run_id);
CREATE INDEX idx_eval_runs_agent ON eval_runs(agent_id,ended_at);
CREATE INDEX idx_eval_runs_agent_created ON eval_runs(agent_id,created_at);
CREATE INDEX idx_eval_runs_purge ON eval_runs(status,ended_at);
CREATE INDEX idx_eval_jobs_purge ON eval_jobs(status,updated_at);
CREATE INDEX idx_eval_results_created ON eval_results(created_at);
-- erasing a call blanks the reasons of its eval results
CREATE INDEX idx_eval_results_session ON eval_results(session_id);
