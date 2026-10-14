package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type SessionRecord struct {
	ID, FlowID, FlowVersionID, ConfigHash, Status, ProviderStackJSON, SafeSummary string
	RuntimeSnapshotJSON                                                           string
	// Draft marks a test call on an unpublished config: FlowVersionID is the version it was based on
	// and ConfigHash is the draft's.
	Draft              bool
	RuntimeTools       []SelectedTool
	StartedAt, EndedAt time.Time
	TurnCount          int
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
	for _, id := range []string{v.ID, v.FlowID, v.FlowVersionID} {
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
		query := "SELECT current_version_id,enabled,archived FROM agents WHERE id=?"
		if r.s.dialect == Postgres {
			query += " FOR UPDATE"
		}
		var current sql.NullString
		var enabled, archived bool
		if err := tx.QueryRowContext(ctx, r.s.bind(query), v.FlowID).Scan(&current, &enabled, &archived); err != nil {
			return err
		}
		if !enabled || archived {
			return errors.New("flow unavailable for new sessions")
		}
		if !current.Valid || current.String != v.FlowVersionID {
			return errors.New("flow version is no longer current")
		}
		runtimeTools := append([]SelectedTool(nil), v.RuntimeTools...)
		selectedRows, err := tx.QueryContext(ctx, r.s.bind("SELECT mcp_server_id,remote_name,schema_hash,required FROM agent_mcp_tools WHERE agent_version_id=?"), v.FlowVersionID)
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
				return errors.New("runtime MCP tool does not match flow version selection")
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
		_, err = tx.ExecContext(ctx, r.s.bind("INSERT INTO sessions(id,agent_id,agent_version_id,config_hash,started_at,ended_at,status,provider_stack_json,turn_count,safe_summary,runtime_snapshot_json,draft) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)"), v.ID, v.FlowID, v.FlowVersionID, v.ConfigHash, utc(v.StartedAt), nullTime(v.EndedAt), v.Status, string(canonical), v.TurnCount, v.SafeSummary, string(runtimeCanonical), boolValue(r.s.dialect, v.Draft))
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
			var status string
			if err := tx.QueryRowContext(ctx, r.s.bind("SELECT status FROM sessions WHERE id=?"), id).Scan(&status); err != nil {
				return err
			}
			if status != "ended" {
				return sql.ErrNoRows
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return r.s.MCP().FinalizePendingDeletes(ctx)
}

// orphanNote is set on a session ended by EndOrphans, unless it already has a summary.
const orphanNote = "Ended when the server restarted."

// EndOrphans ends every active or draining session that started before `before`: its process is
// gone, so nothing will ever end it. The end time is the last finished turn, else the start (never
// "now": that would show a phantom call that lasted until the restart). It returns how many it
// ended, and retries the MCP deletes those sessions were holding back.
func (r SessionRepository) EndOrphans(ctx context.Context, before time.Time) (int, error) {
	res, err := r.s.db.ExecContext(ctx, r.s.bind(`UPDATE sessions SET
			status='ended',
			ended_at=COALESCE((SELECT MAX(t.ended_at) FROM turns t WHERE t.session_id=sessions.id), started_at),
			safe_summary=CASE WHEN safe_summary='' THEN ? ELSE safe_summary END
			WHERE status IN ('active','draining') AND started_at < ?`), orphanNote, utc(before))
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// The sessions are ended whatever happens next: report the count with a finalize failure.
	return int(affected), r.s.MCP().FinalizePendingDeletes(ctx)
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
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO turns(id,session_id,ordinal,started_at,ended_at,transcript,assistant_text,latencies_json,interrupted,tools_json) VALUES(?,?,?,?,?,?,?,?,?,?)"), v.ID, v.SessionID, v.Ordinal, utc(v.StartedAt), nullTime(v.EndedAt), nullable(v.Transcript), nullable(v.AssistantText), string(latencyJSON), boolValue(r.s.dialect, v.Interrupted), string(toolsJSON)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, r.s.bind("UPDATE sessions SET turn_count=turn_count+1 WHERE id=?"), v.SessionID)
		return err
	})
}

type SessionFilter struct {
	FlowID string
	Status string
	Limit  int
	Offset int
}

func (r SessionRepository) List(ctx context.Context, filter SessionFilter) ([]SessionRecord, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	query := "SELECT id,agent_id,agent_version_id,config_hash,started_at,ended_at,status,provider_stack_json,turn_count,safe_summary,runtime_snapshot_json,draft FROM sessions"
	var clauses []string
	var args []any
	if filter.FlowID != "" {
		clauses = append(clauses, "agent_id=?")
		args = append(args, filter.FlowID)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status=?")
		args = append(args, filter.Status)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SessionRecord, 0)
	for rows.Next() {
		var v SessionRecord
		var started string
		var ended sql.NullString
		if err := rows.Scan(&v.ID, &v.FlowID, &v.FlowVersionID, &v.ConfigHash, &started, &ended, &v.Status, &v.ProviderStackJSON, &v.TurnCount, &v.SafeSummary, &v.RuntimeSnapshotJSON, &v.Draft); err != nil {
			return nil, err
		}
		if v.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if ended.Valid {
			if v.EndedAt, err = parseTime(ended.String); err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r SessionRepository) Get(ctx context.Context, id string) (SessionRecord, error) {
	var v SessionRecord
	var started string
	var ended sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,agent_id,agent_version_id,config_hash,started_at,ended_at,status,provider_stack_json,turn_count,safe_summary,runtime_snapshot_json,draft FROM sessions WHERE id=?"), id).Scan(&v.ID, &v.FlowID, &v.FlowVersionID, &v.ConfigHash, &started, &ended, &v.Status, &v.ProviderStackJSON, &v.TurnCount, &v.SafeSummary, &v.RuntimeSnapshotJSON, &v.Draft)
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
	toolRows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT mcp_server_id,remote_name,schema_hash,required FROM session_mcp_tools WHERE session_id=?"), id)
	if err == nil {
		defer toolRows.Close()
		for toolRows.Next() {
			var t SelectedTool
			if err := toolRows.Scan(&t.ServerID, &t.RemoteName, &t.SchemaHash, &t.Required); err == nil {
				v.RuntimeTools = append(v.RuntimeTools, t)
			}
		}
	}
	return v, nil
}
func (r SessionRepository) Turns(ctx context.Context, sessionID string) ([]TurnRecord, error) {
	if err := validID(sessionID); err != nil {
		return nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT id,session_id,ordinal,started_at,ended_at,transcript,assistant_text,latencies_json,interrupted,tools_json FROM turns WHERE session_id=? ORDER BY ordinal ASC"), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TurnRecord, 0)
	for rows.Next() {
		var v TurnRecord
		var started string
		var ended, transcript, assistant sql.NullString
		if err := rows.Scan(&v.ID, &v.SessionID, &v.Ordinal, &started, &ended, &transcript, &assistant, &v.LatenciesJSON, &v.Interrupted, &v.ToolsJSON); err != nil {
			return nil, err
		}
		if v.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if ended.Valid {
			if v.EndedAt, err = parseTime(ended.String); err != nil {
				return nil, err
			}
		}
		v.Transcript = transcript.String
		v.AssistantText = assistant.String
		out = append(out, v)
	}
	return out, rows.Err()
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

func (r TraceRepository) EndSpan(ctx context.Context, id string, endedAt time.Time, attributesJSON string) error {
	if err := validID(id); err != nil {
		return err
	}
	attrs, err := metadata(attributesJSON)
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("UPDATE trace_spans SET ended_at=?, attributes_json=? WHERE id=?"), nullTime(endedAt), attrs, id)
	return err
}

func (r TraceRepository) Spans(ctx context.Context, sessionID, turnID string) ([]TraceSpan, error) {
	for _, id := range []string{sessionID, turnID} {
		if err := optionalID(id); err != nil {
			return nil, err
		}
	}
	query := "SELECT id,session_id,turn_id,parent_id,name,started_at,ended_at,attributes_json FROM trace_spans"
	var clauses []string
	var args []any
	if sessionID != "" {
		clauses = append(clauses, "session_id=?")
		args = append(args, sessionID)
	}
	if turnID != "" {
		clauses = append(clauses, "turn_id=?")
		args = append(args, turnID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY started_at ASC, id ASC"

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TraceSpan, 0)
	for rows.Next() {
		var v TraceSpan
		var sess, turn, parent, ended sql.NullString
		var started string
		if err := rows.Scan(&v.ID, &sess, &turn, &parent, &v.Name, &started, &ended, &v.AttributesJSON); err != nil {
			return nil, err
		}
		v.SessionID = sess.String
		v.TurnID = turn.String
		v.ParentID = parent.String
		if v.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if ended.Valid {
			if v.EndedAt, err = parseTime(ended.String); err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
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

func (r TraceRepository) Events(ctx context.Context, sessionID, turnID string) ([]TraceEvent, error) {
	for _, id := range []string{sessionID, turnID} {
		if err := optionalID(id); err != nil {
			return nil, err
		}
	}
	query := "SELECT id,span_id,session_id,turn_id,name,occurred_at,attributes_json FROM trace_events"
	var clauses []string
	var args []any
	if sessionID != "" {
		clauses = append(clauses, "session_id=?")
		args = append(args, sessionID)
	}
	if turnID != "" {
		clauses = append(clauses, "turn_id=?")
		args = append(args, turnID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY occurred_at ASC, id ASC"

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TraceEvent, 0)
	for rows.Next() {
		var v TraceEvent
		var span, sess, turn sql.NullString
		var occurred string
		if err := rows.Scan(&v.ID, &span, &sess, &turn, &v.Name, &occurred, &v.AttributesJSON); err != nil {
			return nil, err
		}
		v.SpanID = span.String
		v.SessionID = sess.String
		v.TurnID = turn.String
		if v.OccurredAt, err = parseTime(occurred); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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

func (r TraceRepository) ToolCalls(ctx context.Context, sessionID, turnID string) ([]ToolCallRecord, error) {
	for _, id := range []string{sessionID, turnID} {
		if err := optionalID(id); err != nil {
			return nil, err
		}
	}
	query := "SELECT id,session_id,turn_id,mcp_server_id,remote_name,schema_hash,started_at,ended_at,status,arguments_summary,result_summary FROM tool_calls"
	var clauses []string
	var args []any
	if sessionID != "" {
		clauses = append(clauses, "session_id=?")
		args = append(args, sessionID)
	}
	if turnID != "" {
		clauses = append(clauses, "turn_id=?")
		args = append(args, turnID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY started_at ASC, id ASC"

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ToolCallRecord, 0)
	for rows.Next() {
		var v ToolCallRecord
		var turn, mcpServer, ended sql.NullString
		var started string
		if err := rows.Scan(&v.ID, &v.SessionID, &turn, &mcpServer, &v.RemoteName, &v.SchemaHash, &started, &ended, &v.Status, &v.ArgumentsSummary, &v.ResultSummary); err != nil {
			return nil, err
		}
		v.TurnID = turn.String
		v.MCPServerID = mcpServer.String
		if v.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if ended.Valid {
			if v.EndedAt, err = parseTime(ended.String); err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func metadata(s string) (string, error) {
	if s == "" {
		s = "{}"
	}
	v, err := CanonicalJSON([]byte(s))
	return string(v), err
}
