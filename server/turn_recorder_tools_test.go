package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/server/agent"
	"govoice/server/storage"
)

func TestTurnRecorderStoresToolCallsWithTheirTurn(t *testing.T) {
	var mu sync.Mutex
	var turns []storage.TurnRecord
	var calls []storage.ToolCallRecord
	snap := agent.MCPToolSnapshot{ServerID: "srv-1", RemoteName: "lookup_order", SchemaHash: "hash-1"}
	r := newTurnRecorder(func(_ context.Context, row storage.TurnRecord) error {
		mu.Lock()
		defer mu.Unlock()
		turns = append(turns, row)
		return nil
	}, func(_ context.Context, tc storage.ToolCallRecord) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, tc)
		return nil
	}, func(name string) (agent.MCPToolSnapshot, bool) { return snap, name == "mcp__lookup" }, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())
	tail := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(r, tail)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(100, 0)
	call := func(id, name, turn string) *core.ToolCallFrame {
		return &core.ToolCallFrame{Call: core.ToolCall{ID: id, Name: name, SessionID: "1", TurnID: core.TurnID(turn), Generation: 1}, Timestamp: at}
	}
	result := func(id, turn string, st core.ToolStatus) *core.ToolResultFrame {
		return &core.ToolResultFrame{Turn: core.TurnID(turn), Gen: 1, Result: core.ToolResult{CallID: id, Status: st, StartedAt: at, EndedAt: at.Add(120 * time.Millisecond)}}
	}
	for _, f := range []core.Frame{
		&core.UserMessageFrame{Turn: "t1", Content: "Where is my order?", Timestamp: at},
		call("c1", "mcp__lookup", "t1"),
		result("c1", "t1", core.ToolSucceeded),
		call("c2", "transfer_to_sales", "t1"),
		&core.AssistantMessageFrame{Turn: "t1", Content: "Shipped.", Timestamp: at.Add(time.Second)},
		&core.ToolResultFrame{Turn: "t1", Gen: 1, Result: core.ToolResult{CallID: "c2", Status: core.ToolFailed, ErrorCategory: "tool_error", StartedAt: at, EndedAt: at}}, // settled after its turn's row (barge-in, an async tool)
		result("c3", "t1", core.ToolFailed), // never announced: its generation was stale
		call("c4", "mcp__lookup", "t2"),     // still running when the call ends
		&core.LLMTextFrame{Text: "marker", Turn: "t9", Gen: 1},
	} {
		if err := session.Post(f); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every frame through the recorder", func() bool {
		for _, f := range tail.GetFrames() {
			if _, ok := f.(*core.LLMTextFrame); ok {
				return true
			}
		}
		return false
	})
	_ = session.Close()
	r.close(t.Context())

	mu.Lock()
	defer mu.Unlock()
	if len(turns) != 1 || turns[0].AssistantText != "Shipped." {
		t.Fatalf("turns %+v", turns)
	}
	row := turns[0].ID
	if len(calls) != 3 {
		t.Fatalf("tool rows %+v, want 3 (c1, c2, c4; never c3)", calls)
	}
	c1, c2, c4 := calls[0], calls[1], calls[2]
	if c1.SessionID != "sess" || c1.TurnID != row || c1.MCPServerID != "srv-1" || c1.RemoteName != "lookup_order" || c1.SchemaHash != "hash-1" ||
		c1.Status != "succeeded" || c1.EndedAt.Sub(c1.StartedAt) != 120*time.Millisecond {
		t.Fatalf("c1 %+v", c1)
	}
	if c2.TurnID != row || c2.RemoteName != "transfer_to_sales" || c2.MCPServerID != "" || c2.Status != "failed" || c2.ErrorCategory != "tool_error" {
		t.Fatalf("c2 %+v", c2)
	}
	if c4.TurnID != "" || c4.RemoteName != "lookup_order" || c4.Status != "cancelled" || !c4.EndedAt.IsZero() {
		t.Fatalf("c4 %+v", c4)
	}
}

// The loop announces a name it has no definition for as "unknown_tool"; the record keeps the failure category, not a name.
// A defined tool whose executor itself answers unknown_tool (the dtmf tool with nowhere to send) keeps its own name.
func TestTurnRecorderKeepsADefinedToolNameWhoseExecutorSaysUnknownTool(t *testing.T) {
	for name, tc := range map[string]struct{ announced, category, want string }{
		"invented": {"unknown_tool", "invalid_tool_name", "invalid_tool_name"},
		"defined":  {"send_dtmf", "unknown_tool", "send_dtmf"},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []storage.ToolCallRecord
			r := newTurnRecorder(func(context.Context, storage.TurnRecord) error { return nil }, func(_ context.Context, rec storage.ToolCallRecord) error {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, rec)
				return nil
			}, func(string) (agent.MCPToolSnapshot, bool) { return agent.MCPToolSnapshot{}, false }, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())
			tail := &coretest.PassthroughProcessor{}
			pipe, _ := core.NewPipeline(r, tail)
			session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
			if err := session.Start(); err != nil {
				t.Fatal(err)
			}
			for _, f := range []core.Frame{
				&core.ToolCallFrame{Call: core.ToolCall{ID: "c1", Name: tc.announced, SessionID: "1", TurnID: "t1", Generation: 1}, Timestamp: time.Unix(100, 0)},
				&core.ToolResultFrame{Turn: "t1", Gen: 1, Result: core.ToolResult{CallID: "c1", Status: core.ToolFailed, ErrorCategory: tc.category}},
				&core.LLMTextFrame{Text: "marker", Turn: "t9", Gen: 1},
			} {
				if err := session.Post(f); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, "every frame through the recorder", func() bool { return len(tail.GetFrames()) == 3 })
			_ = session.Close()
			r.close(t.Context())
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != 1 || calls[0].RemoteName != tc.want {
				t.Fatalf("stored %+v, want remote_name %q", calls, tc.want)
			}
		})
	}
}

// Audio passes at 50 frames/s per call: the recorder must not take its lock for it.
func TestTurnRecorderPassesAudioWithoutItsLock(t *testing.T) {
	r := newTurnRecorder(func(context.Context, storage.TurnRecord) error { return nil }, func(context.Context, storage.ToolCallRecord) error { return nil },
		func(string) (agent.MCPToolSnapshot, bool) { return agent.MCPToolSnapshot{}, false }, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())
	tail := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(r, tail)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock() // held for the whole test: only a frame that needs recorder state may wait
	released := false
	release := func() {
		if !released {
			released = true
			r.mu.Unlock()
		}
	}
	defer release()
	if err := session.Post(&core.OutputAudioFrame{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the audio frame to pass the recorder while its lock is held", func() bool { return len(tail.GetFrames()) == 1 })
	release()
	_ = session.Close()
	r.close(t.Context())
}
