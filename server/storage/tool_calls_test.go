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
			{ID: NewID(), SessionID: sess, TurnID: turn.ID, RemoteName: "lookup_order", SchemaHash: "h", StartedAt: at, EndedAt: at.Add(120 * time.Millisecond), Status: "succeeded"},
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
			got[1].TurnID != "" || !got[1].EndedAt.IsZero() || got[1].Status != "cancelled" {
			t.Fatalf("rows %+v", got)
		}
	})
}
