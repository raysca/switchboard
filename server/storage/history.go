package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type SessionRecord struct {
	ID, AgentID, AgentVersionID, ConfigHash, Status, ProviderStackJSON, SafeSummary string
	RuntimeSnapshotJSON                                                             string
	RuntimeTools                                                                    []SelectedTool
	StartedAt, EndedAt                                                              time.Time
	TurnCount                                                                       int
}
type TurnRecord struct {
	ID, SessionID, Transcript, AssistantText, LatenciesJSON, ToolsJSON string
	Ordinal                                                            int
	StartedAt, EndedAt                                                 time.Time
	Interrupted                                                        bool
}
type SessionRepository struct{ s *Store }

func (s *Store) Sessions() SessionRepository { return SessionRepository{s} }
func (r SessionRepository) Create(ctx context.Context, v SessionRecord) error {
	for _, id := range []string{v.ID, v.AgentID, v.AgentVersionID} {
		if err := validID(id); err != nil {
			return err
		}
	}
	stack := v.ProviderStackJSON
	if stack == "" {
		stack = "{}"
	}
	canonical, err := CanonicalJSON([]byte(stack))
	if err != nil {
		return err
	}
	runtimeSnapshot := v.RuntimeSnapshotJSON
	if runtimeSnapshot == "" {
		runtimeSnapshot = "{}"
	}
	runtimeCanonical, err := CanonicalJSON([]byte(runtimeSnapshot))
	if err != nil {
		return err
	}
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		query := "SELECT current_version_id FROM agents WHERE id=?"
		if r.s.dialect == Postgres {
			query += " FOR UPDATE"
		}
		var current sql.NullString
		if err := tx.QueryRowContext(ctx, r.s.bind(query), v.AgentID).Scan(&current); err != nil {
			return err
		}
		if !current.Valid || current.String != v.AgentVersionID {
			return errors.New("agent version is no longer current")
		}
		runtimeTools := append([]SelectedTool(nil), v.RuntimeTools...)
		selectedRows, err := tx.QueryContext(ctx, r.s.bind("SELECT mcp_server_id,remote_name,schema_hash,required FROM agent_mcp_tools WHERE agent_version_id=?"), v.AgentVersionID)
		if err != nil {
			return err
		}
		selected := map[string]SelectedTool{}
		for selectedRows.Next() {
			var tool SelectedTool
			if err := selectedRows.Scan(&tool.ServerID, &tool.RemoteName, &tool.SchemaHash, &tool.Required); err != nil {
				selectedRows.Close()
				return err
			}
			selected[tool.ServerID+"\x00"+tool.RemoteName] = tool
		}
		if err := selectedRows.Err(); err != nil {
			selectedRows.Close()
			return err
		}
		selectedRows.Close()
		if v.RuntimeTools == nil {
			for _, tool := range selected {
				runtimeTools = append(runtimeTools, tool)
			}
		}
		included := map[string]bool{}
		for _, tool := range runtimeTools {
			key := tool.ServerID + "\x00" + tool.RemoteName
			pinned, exists := selected[key]
			if !exists || pinned.SchemaHash != tool.SchemaHash || pinned.Required != tool.Required {
				return errors.New("runtime MCP tool does not match agent version selection")
			}
			included[key] = true
			query := "SELECT s.enabled,s.pending_delete,mt.schema_hash FROM mcp_servers s LEFT JOIN mcp_tools mt ON mt.discovery_id=s.latest_discovery_id AND mt.remote_name=? WHERE s.id=?"
			if r.s.dialect == Postgres {
				query += " FOR SHARE OF s"
			}
			var enabled, pending bool
			var schemaHash sql.NullString
			if err := tx.QueryRowContext(ctx, r.s.bind(query), tool.RemoteName, tool.ServerID).Scan(&enabled, &pending, &schemaHash); err != nil {
				return err
			}
			if !enabled || pending {
				return errors.New("MCP dependency is unavailable for new sessions")
			}
			if !schemaHash.Valid || schemaHash.String != tool.SchemaHash {
				return errors.New("MCP schema changed before session start")
			}
		}
		for key, tool := range selected {
			if tool.Required && !included[key] {
				return errors.New("required MCP tool was not captured for session")
			}
		}
		_, err = tx.ExecContext(ctx, r.s.bind("INSERT INTO sessions(id,agent_id,agent_version_id,config_hash,started_at,ended_at,status,provider_stack_json,turn_count,safe_summary,runtime_snapshot_json) VALUES(?,?,?,?,?,?,?,?,?,?,?)"), v.ID, v.AgentID, v.AgentVersionID, v.ConfigHash, utc(v.StartedAt), nullTime(v.EndedAt), v.Status, string(canonical), v.TurnCount, v.SafeSummary, string(runtimeCanonical))
		if err != nil {
			return err
		}
		for _, tool := range runtimeTools {
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO session_mcp_tools(session_id,mcp_server_id,remote_name,schema_hash,required) VALUES(?,?,?,?,?)"), v.ID, tool.ServerID, tool.RemoteName, tool.SchemaHash, boolValue(r.s.dialect, tool.Required)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r SessionRepository) End(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, r.s.bind("UPDATE sessions SET status='ended',ended_at=? WHERE id=? AND status IN ('active','draining')"), utc(time.Now()), id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return err
	}
	return r.s.MCP().FinalizePendingDeletes(ctx)
}
func (r SessionRepository) AddTurn(ctx context.Context, v TurnRecord) error {
	for _, id := range []string{v.ID, v.SessionID} {
		if err := validID(id); err != nil {
			return err
		}
	}
	latency := v.LatenciesJSON
	if latency == "" {
		latency = "{}"
	}
	tools := v.ToolsJSON
	if tools == "" {
		tools = "[]"
	}
	latencyJSON, err := CanonicalJSON([]byte(latency))
	if err != nil {
		return err
	}
	toolsJSON, err := CanonicalJSON([]byte(tools))
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO turns(id,session_id,ordinal,started_at,ended_at,transcript,assistant_text,latencies_json,interrupted,tools_json) VALUES(?,?,?,?,?,?,?,?,?,?)"), v.ID, v.SessionID, v.Ordinal, utc(v.StartedAt), nullTime(v.EndedAt), nullable(v.Transcript), nullable(v.AssistantText), string(latencyJSON), boolValue(r.s.dialect, v.Interrupted), string(toolsJSON))
	return err
}
func (r SessionRepository) Get(ctx context.Context, id string) (SessionRecord, error) {
	var v SessionRecord
	var started string
	var ended sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,agent_id,agent_version_id,config_hash,started_at,ended_at,status,provider_stack_json,turn_count,safe_summary,runtime_snapshot_json FROM sessions WHERE id=?"), id).Scan(&v.ID, &v.AgentID, &v.AgentVersionID, &v.ConfigHash, &started, &ended, &v.Status, &v.ProviderStackJSON, &v.TurnCount, &v.SafeSummary, &v.RuntimeSnapshotJSON)
	if err != nil {
		return v, err
	}
	if v.StartedAt, err = parseTime(started); err != nil {
		return v, err
	}
	if ended.Valid {
		if v.EndedAt, err = parseTime(ended.String); err != nil {
			return v, err
		}
	}
	return v, nil
}
func (r SessionRepository) Turn(ctx context.Context, id string) (TurnRecord, error) {
	var v TurnRecord
	var started string
	var ended, transcript, assistant sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,session_id,ordinal,started_at,ended_at,transcript,assistant_text,latencies_json,interrupted,tools_json FROM turns WHERE id=?"), id).Scan(&v.ID, &v.SessionID, &v.Ordinal, &started, &ended, &transcript, &assistant, &v.LatenciesJSON, &v.Interrupted, &v.ToolsJSON)
	if err != nil {
		return v, err
	}
	if v.StartedAt, err = parseTime(started); err != nil {
		return v, err
	}
	if ended.Valid {
		if v.EndedAt, err = parseTime(ended.String); err != nil {
			return v, err
		}
	}
	v.Transcript = transcript.String
	v.AssistantText = assistant.String
	return v, nil
}

type TraceSpan struct {
	ID, SessionID, TurnID, ParentID, Name, AttributesJSON string
	StartedAt, EndedAt                                    time.Time
}
type TraceEvent struct {
	ID, SpanID, SessionID, TurnID, Name, AttributesJSON string
	OccurredAt                                          time.Time
}
type ToolCallRecord struct {
	ID, SessionID, TurnID, MCPServerID, RemoteName, SchemaHash, Status, ArgumentsSummary, ResultSummary string
	StartedAt, EndedAt                                                                                  time.Time
}
type TraceRepository struct{ s *Store }

func (s *Store) Traces() TraceRepository { return TraceRepository{s} }
func (r TraceRepository) AddSpan(ctx context.Context, v TraceSpan) error {
	if err := validID(v.ID); err != nil {
		return err
	}
	for _, id := range []string{v.SessionID, v.TurnID, v.ParentID} {
		if err := optionalID(id); err != nil {
			return err
		}
	}
	attrs, err := metadata(v.AttributesJSON)
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO trace_spans(id,session_id,turn_id,parent_id,name,started_at,ended_at,attributes_json) VALUES(?,?,?,?,?,?,?,?)"), v.ID, nullable(v.SessionID), nullable(v.TurnID), nullable(v.ParentID), v.Name, utc(v.StartedAt), nullTime(v.EndedAt), attrs)
	return err
}
func (r TraceRepository) AddEvent(ctx context.Context, v TraceEvent) error {
	if err := validID(v.ID); err != nil {
		return err
	}
	for _, id := range []string{v.SpanID, v.SessionID, v.TurnID} {
		if err := optionalID(id); err != nil {
			return err
		}
	}
	attrs, err := metadata(v.AttributesJSON)
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO trace_events(id,span_id,session_id,turn_id,name,occurred_at,attributes_json) VALUES(?,?,?,?,?,?,?)"), v.ID, nullable(v.SpanID), nullable(v.SessionID), nullable(v.TurnID), v.Name, utc(v.OccurredAt), attrs)
	return err
}
func (r TraceRepository) AddToolCall(ctx context.Context, v ToolCallRecord) error {
	for _, id := range []string{v.ID, v.SessionID} {
		if err := validID(id); err != nil {
			return err
		}
	}
	for _, id := range []string{v.TurnID, v.MCPServerID} {
		if err := optionalID(id); err != nil {
			return err
		}
	}
	if v.SessionID == "" {
		return errors.New("tool call session required")
	}
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO tool_calls(id,session_id,turn_id,mcp_server_id,remote_name,schema_hash,started_at,ended_at,status,arguments_summary,result_summary) VALUES(?,?,?,?,?,?,?,?,?,?,?)"), v.ID, v.SessionID, nullable(v.TurnID), nullable(v.MCPServerID), v.RemoteName, v.SchemaHash, utc(v.StartedAt), nullTime(v.EndedAt), v.Status, v.ArgumentsSummary, v.ResultSummary)
	return err
}
func metadata(s string) (string, error) {
	if s == "" {
		s = "{}"
	}
	v, err := CanonicalJSON([]byte(s))
	return string(v), err
}
