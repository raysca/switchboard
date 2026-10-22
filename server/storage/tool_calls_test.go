package storage

import (
	"context"
	"testing"
	"time"
)

// The turn recorder's write pattern, on SQLite and (TEST_POSTGRES_DSN) Postgres: a call joins a stored turn, an
// unattached call has no turn, and a call cannot join a turn row that does not exist.
func TestToolCallRowsFollowTheirTurn(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		sess := seedSession(t, s, time.Now().Add(-time.Minute), "active")
		turn := TurnRecord{ID: NewID(), SessionID: sess, Ordinal: 1, StartedAt: time.Now()}
		if err := s.Sessions().AddTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Millisecond)
		for _, tc := range []ToolCallRecord{
			{ID: NewID(), SessionID: sess, TurnID: turn.ID, RemoteName: "lookup_order", SchemaHash: "h", StartedAt: at, EndedAt: at.Add(120 * time.Millisecond), Status: "failed", ErrorCategory: "tool_error"},
			{ID: NewID(), SessionID: sess, RemoteName: "transfer_to_sales", StartedAt: at.Add(time.Second), Status: "cancelled"},
		} {
			if err := s.Traces().AddToolCall(ctx, tc); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Traces().AddToolCall(ctx, ToolCallRecord{ID: NewID(), SessionID: sess, TurnID: NewID(), RemoteName: "x", StartedAt: at, Status: "failed"}); err == nil {
			t.Fatal("a tool call joined a turn row that does not exist")
		}
		got, err := s.Traces().ToolCalls(ctx, sess, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].TurnID != turn.ID || got[0].EndedAt.Sub(got[0].StartedAt) != 120*time.Millisecond ||
			got[0].ErrorCategory != "tool_error" || got[1].ErrorCategory != "" || got[1].TurnID != "" || !got[1].EndedAt.IsZero() || got[1].Status != "cancelled" {
			t.Fatalf("rows %+v", got)
		}
	})
}

func TestAddDiscoveryDoesNotDuplicateUnchangedTools(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		m := MCPServer{ID: NewID(), Name: "M", Endpoint: "https://m.example", Transport: "streamable-http", AuthEnv: "MCP_TOKEN_X", Enabled: true}
		if err := s.MCP().CreateServer(ctx, m); err != nil {
			t.Fatal(err)
		}
		tools := []MCPTool{{RemoteName: "a", SchemaJSON: []byte(`{"type":"object"}`)}}
		first, err := s.MCP().AddDiscovery(ctx, m.ID, tools)
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.MCP().AddDiscovery(ctx, m.ID, tools)
		if err != nil || again.ID != first.ID || len(again.Tools) != 1 {
			t.Fatalf("again=%+v err=%v", again, err)
		}
		count := func() (n int) {
			if err := s.db.QueryRowContext(ctx, s.bind("SELECT COUNT(*) FROM mcp_discoveries WHERE server_id=?"), m.ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := count(); n != 1 {
			t.Fatalf("discoveries=%d, want 1", n)
		}
		described, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "a", Description: "new words", SchemaJSON: []byte(`{"type":"object"}`)}})
		if err != nil || described.ID == first.ID || count() != 2 {
			t.Fatalf("description-only change: %+v err=%v", described, err)
		}
		if _, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "a", Description: "new words", SchemaJSON: []byte(`{"type":"object"}`)}}); err != nil || count() != 2 {
			t.Fatalf("identical described list duplicated: err=%v", err)
		}
		changed, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "a", SchemaJSON: []byte(`{"type":"string"}`)}})
		if err != nil || changed.ID == first.ID || count() != 3 {
			t.Fatalf("changed=%+v err=%v", changed, err)
		}
	})
}
