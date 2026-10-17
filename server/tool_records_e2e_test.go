package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/server/api"
	"govoice/server/storage"
)

// sessionTraces and sessionTurns read a stored call through the management API, as the console does.
func (r *flowRigE2E) sessionTraces(id string) api.SessionTracesDTO {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodGet, "/api/v1/sessions/"+id+"/traces", e2eMgmtKey, "")
	var resp struct {
		Data api.SessionTracesDTO `json:"data"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		r.t.Fatalf("traces: %d %s", w.Code, w.Body.String())
	}
	return resp.Data
}

func (r *flowRigE2E) sessionTurns(id string) []api.TurnDTO {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodGet, "/api/v1/sessions/"+id+"/turns", e2eMgmtKey, "")
	var resp struct {
		Data []api.TurnDTO `json:"data"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		r.t.Fatalf("turns: %d %s", w.Code, w.Body.String())
	}
	return resp.Data
}

func (r *flowRigE2E) waitTurns(id string, n int) {
	r.t.Helper()
	eventually(r.t, fmt.Sprintf("%d stored turns", n), func() bool {
		turns, _ := r.s.store.Sessions().Turns(context.Background(), id)
		return len(turns) >= n
	})
}

func (c *e2eCall) hangUp() { _ = c.conn.Close(websocket.StatusNormalClosure, "bye") }

func TestMCPCallToolCallsAreRecorded(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	r.waitTurns(c.sessionID, 1)
	c.barrier(t) // a late or duplicate row would be written by now
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)

	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool calls %+v", calls)
	}
	tc := calls[0]
	if tc.RemoteName != "lookup_order" || tc.MCPServerID != id || tc.SchemaHash != hashes["lookup_order"] || tc.Status != "succeeded" ||
		tc.EndedAt == nil || tc.EndedAt.Before(tc.StartedAt) || tc.TurnID == "" || tc.ArgumentsSummary != "" || tc.ResultSummary != "" {
		t.Fatalf("tool call %+v", tc)
	}
	var tools []api.TurnToolDTO
	for _, turn := range r.sessionTurns(c.sessionID) {
		if turn.ID == tc.TurnID {
			tools = turn.Tools
		}
	}
	if len(tools) != 1 || tools[0].Name != "lookup_order" || tools[0].Status != "succeeded" || tools[0].DurationMS < 0 {
		t.Fatalf("the call's turn lists %+v", tools)
	}
	attrs := r.spanAttributes(c.sessionID, "mcp.tool.execute")
	for _, want := range []string{`"mcp.server_id":"` + id + `"`, `"mcp.tool_name":"lookup_order"`, `"mcp.duration_ms":`} {
		if !strings.Contains(attrs, want) {
			t.Fatalf("mcp.tool.execute attributes %s lack %s", attrs, want)
		}
	}
}

func TestMCPCallFailedToolIsRecordedFailed(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("fail", r.ts.URL+"/fail/tools", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "tool_error").ID)
	c.say("call tool tool_error")
	c.waitReplies("Tool tool_error failed", 1)
	r.waitTurns(c.sessionID, 1)
	c.barrier(t) // a late or duplicate row would be written by now
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].RemoteName != "tool_error" || calls[0].MCPServerID != id || calls[0].Status != "failed" {
		t.Fatalf("tool calls %+v", calls)
	}
}

func TestMCPCallBargeInRecordsTheToolCancelled(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.MCP.Timeout = 10 * time.Second })
	session := r.liveSession()
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":4000}`)
	waitFnEvents(t, c, 2)
	bargeIn(t, session())
	eventually(t, "the server to see the call abandoned", func() bool { return r.lastCall("slow_lookup").Cancelled })
	assertFnEvents(t, c, []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":true,"tool_call_id":"#1"}`})
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].RemoteName != "slow_lookup" || calls[0].Status != "cancelled" || calls[0].EndedAt == nil {
		t.Fatalf("tool calls %+v", calls)
	}
}

func TestMCPCallAsyncToolIsRecordedWhenItEnds(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, []string{id + "/slow_lookup"}, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":300}`)
	assertFnEvents(t, c, []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}) // the background run ended
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].Status != "succeeded" || calls[0].EndedAt == nil || calls[0].TurnID == "" ||
		calls[0].EndedAt.Sub(calls[0].StartedAt) < 250*time.Millisecond {
		t.Fatalf("tool calls %+v (want the background run's outcome and duration, on its turn)", calls)
	}
}

func TestMCPCallParallelToolCallsAreEachRecorded(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.Tools.MaxParallel = 4 })
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order", "echo").ID)
	c.say(`call tool lookup_order {"order_id":"42"} then call tool echo {"text":"hi"}`)
	c.waitReplies("Tool echo returned", 1)
	r.waitTurns(c.sessionID, 1)
	c.barrier(t)
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool calls %+v", calls)
	}
	names := map[string]bool{}
	for _, tc := range calls {
		names[tc.RemoteName] = true
		if tc.Status != "succeeded" || tc.MCPServerID != id || tc.TurnID == "" || tc.EndedAt == nil {
			t.Fatalf("tool call %+v", tc)
		}
	}
	if !names["lookup_order"] || !names["echo"] {
		t.Fatalf("recorded %v", names)
	}
}

// The echo model only calls attached tools, so the model-invented name is posted into the live call's
// pipeline as the tool loop would report it (an unknown tool, failed): the real recorder, database and API.
func TestMCPCallModelInventedToolNameIsNeverStored(t *testing.T) {
	r := newMCPCallRig(t)
	session := r.liveSession()
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	r.waitTurns(c.sessionID, 1)
	sess := session()
	gen, at := core.Generation(sess.CurrentGeneration()), time.Now()
	for _, f := range []core.Frame{
		&core.ToolCallFrame{Call: core.ToolCall{ID: "invented-1", Name: "my_card_is_4111111111111111", TurnID: "turn-invented", Generation: gen}, Timestamp: at},
		&core.ToolResultFrame{Turn: "turn-invented", Gen: gen, Result: core.ToolResult{CallID: "invented-1", Status: core.ToolFailed, ErrorCategory: "unknown_tool", StartedAt: at, EndedAt: at.Add(time.Millisecond)}},
	} {
		if err := sess.Post(f); err != nil {
			t.Fatal(err)
		}
	}
	c.barrier(t)
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	traces := r.sessionTraces(c.sessionID)
	var invented *api.ToolCallDTO
	for i, tc := range traces.ToolCalls {
		if tc.RemoteName == "unknown_tool" {
			invented = &traces.ToolCalls[i]
		}
	}
	if len(traces.ToolCalls) != 2 || invented == nil || invented.Status != "failed" || invented.MCPServerID != "" {
		t.Fatalf("tool calls %+v", traces.ToolCalls)
	}
	b, _ := json.Marshal(traces)
	turns, _ := json.Marshal(r.sessionTurns(c.sessionID))
	if strings.Contains(string(b), "my_card_is") || strings.Contains(string(b), "4111") || strings.Contains(string(turns), "4111") {
		t.Fatalf("the model's tool name leaked: %s %s", b, turns)
	}
}
