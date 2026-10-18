package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/providers/echo"
	"govoice/providers/google"
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

// assertNoToolContentStored is the privacy pin: whatever the API shows, the stored tool_calls rows carry
// no argument or result content (the summary columns stay ”).
func (r *flowRigE2E) assertNoToolContentStored() {
	r.t.Helper()
	var rows, withContent int
	err := r.s.store.DB().QueryRowContext(context.Background(),
		`SELECT count(*), coalesce(sum(CASE WHEN arguments_summary <> '' OR result_summary <> '' THEN 1 ELSE 0 END), 0) FROM tool_calls`).Scan(&rows, &withContent)
	if err != nil || rows == 0 || withContent != 0 {
		r.t.Fatalf("tool_calls rows=%d with stored content=%d err=%v", rows, withContent, err)
	}
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
		tc.EndedAt == nil || !tc.EndedAt.After(tc.StartedAt) || tc.TurnID == "" {
		t.Fatalf("tool call %+v", tc)
	}
	r.assertNoToolContentStored()
	var tools []api.TurnToolDTO
	for _, turn := range r.sessionTurns(c.sessionID) {
		if turn.ID == tc.TurnID {
			tools = turn.Tools
		}
	}
	if len(tools) != 1 || tools[0].Name != "lookup_order" || tools[0].Status != "succeeded" || tools[0].DurationMS != tc.EndedAt.Sub(tc.StartedAt).Milliseconds() {
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
	r.assertNoToolContentStored()
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
	r.assertNoToolContentStored()
}

func TestMCPCallAsyncToolIsRecordedWhenItEnds(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, []string{id + "/slow_lookup"}, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":300}`)
	assertFnEvents(t, c, []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}) // the background run ended
	r.waitTurns(c.sessionID, 1)
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].Status != "succeeded" || calls[0].EndedAt == nil || calls[0].TurnID == "" ||
		calls[0].EndedAt.Sub(calls[0].StartedAt) < 250*time.Millisecond {
		t.Fatalf("tool calls %+v (want the background run's outcome and duration, on its turn)", calls)
	}
	r.assertNoToolContentStored()
	for _, turn := range r.sessionTurns(c.sessionID) {
		if turn.ID == calls[0].TurnID && (len(turn.Tools) != 1 || turn.Tools[0].DurationMS < 250) {
			t.Fatalf("the turn lists %+v, want the background run's duration (>= 250 ms)", turn.Tools)
		}
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
	r.assertNoToolContentStored()
}

// Recorder, database and API plumbing only: the real loop's rewrite is guarded by TestMCPCallRealLoopInventedToolNameIsNeverStored.
// The echo model only calls attached tools, so an unknown tool is posted into the live call's
// pipeline as the tool loop announces it (the placeholder name, failed): the real recorder, database and API.
func TestMCPCallPlaceholderIsStoredAsCategory(t *testing.T) {
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
		&core.ToolCallFrame{Call: core.ToolCall{ID: "invented-1", Name: "unknown_tool", TurnID: "turn-invented", Generation: gen}, Timestamp: at},
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
	if strings.Contains(string(b), "my_card_is") || strings.Contains(string(turns), "my_card_is") {
		t.Fatalf("the model's tool name leaked: %s %s", b, turns)
	}
}

// inventingLLM answers the user's "invent" with calls to tools that do not exist, whose names hold the caller's words
// or are not valid names; everything else goes to the echo model.
type inventingLLM struct {
	*echo.LLMClient
	done atomic.Bool
}

var inventedNames = []string{"my_card_is_4111111111111111", "my card is 4111 1111 1111 1111", "ça_carte_是_4111", "my_card_" + strings.Repeat("x", 200)}

func (c *inventingLLM) GenerateStreamWithNativeTools(ctx context.Context, m, sp string, h []core.Message, n []google.GoogleNativeExchange, d []core.ToolDefinition, id core.ToolCall) (<-chan google.LLMStreamChunk, <-chan error, error) {
	if last := h[len(h)-1]; last.Role == core.RoleUser && strings.Contains(last.Content, "invent") && c.done.CompareAndSwap(false, true) {
		calls := make([]core.ToolCall, 0, len(inventedNames))
		for i, name := range inventedNames {
			calls = append(calls, core.ToolCall{ID: fmt.Sprintf("inv-%d", i+1), Name: name, Arguments: json.RawMessage(`{}`)})
		}
		ch, errs := make(chan google.LLMStreamChunk, 1), make(chan error)
		ch <- google.LLMStreamChunk{ToolCalls: calls}
		close(ch)
		close(errs)
		return ch, errs, nil
	}
	return c.LLMClient.GenerateStreamWithNativeTools(ctx, m, sp, h, n, d, id)
}

// The real google processor and tool loop against a model that invents tool names: the name never reaches the
// stored rows, the API, the stored spans or the client wire; each call is stored as unknown_tool.
func TestMCPCallRealLoopInventedToolNameIsNeverStored(t *testing.T) {
	r := newMCPCallRig(t)
	r.s.registry.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		llmCfg := googleLLMConfig(spec, r.s.registry.echoLLMBreaker)
		llmCfg.Provider = "echo"
		llmCfg.Client = &inventingLLM{LLMClient: echo.NewLLMClient(echoLLMOptions(cfg, spec))}
		return google.NewLLMProcessor(llmCfg)
	})
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	c.say("please invent")
	waitFnEvents(t, c, 3*len(inventedNames)) // started, in-progress, stopped for each
	c.waitReplies("Connecting you.", 1)
	r.waitTurns(c.sessionID, 1) // before the barrier turn, which would cut the reply off
	c.barrier(t)
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)

	traces := r.sessionTraces(c.sessionID)
	unknown := 0
	for _, tc := range traces.ToolCalls {
		// A well-formed undefined name is unknown_tool; a malformed one (spaces, non-ASCII, too long) invalid_tool_name.
		if (tc.RemoteName == "unknown_tool" || tc.RemoteName == "invalid_tool_name") && tc.Status == "failed" && tc.MCPServerID == "" {
			unknown++
		}
	}
	if unknown != len(inventedNames) || len(traces.ToolCalls) != len(inventedNames) {
		t.Fatalf("tool calls %+v, want %d failed rows stored by category", traces.ToolCalls, len(inventedNames))
	}
	b, _ := json.Marshal(traces)
	turns, _ := json.Marshal(r.sessionTurns(c.sessionID))
	wire := ""
	for _, e := range c.fnEvents() {
		wire += e.Raw
	}
	for _, seen := range []string{string(b), string(turns), wire} {
		for _, leak := range []string{"my_card", "my card", "carte", "xxxxxxxxxx"} {
			if strings.Contains(seen, leak) {
				t.Fatalf("the model's tool name leaked (%q): %.2000s", leak, seen)
			}
		}
	}
}

// A typed turn that arrives while the previous reply is still on its way supersedes it. The
// superseded turn keeps its row: the caller's words are history even when none of the reply was heard.
func TestCallRecordsATypedTurnLeavesTheSupersededTurnRow(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	long := "please tell me everything about the weather in every city of the world, starting with the biggest and ending with the smallest one you know"
	c.say(long)
	c.waitReplies("weather", 1) // the reply's text reached the client; its audio has not finished
	c.say("second")
	c.waitReplies("second", 1)
	r.waitTurns(c.sessionID, 2)
	c.barrier(t)
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)

	turns, err := r.s.store.Sessions().Turns(context.Background(), c.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Transcript != long || !turns[0].Interrupted || turns[1].Transcript != "second" || turns[1].Interrupted {
		t.Fatalf("stored turns %+v", turns)
	}
}

// The tool call a typed turn cuts off belongs to the turn row that typed turn leaves behind.
func TestCallRecordsATypedTurnLeavesTheToolCallAttached(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.MCP.Timeout = 10 * time.Second })
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":4000}`)
	eventually(t, "the tool call to start", func() bool { return r.lastCall("slow_lookup").Tool != "" })
	c.say("second")
	eventually(t, "the slow call to be abandoned", func() bool { return r.lastCall("slow_lookup").Cancelled })
	c.waitReplies("second", 1)
	r.waitTurns(c.sessionID, 2)
	c.barrier(t)
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)

	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].TurnID == "" {
		t.Fatalf("tool calls %+v", calls)
	}
	var tools []api.TurnToolDTO
	for _, turn := range r.sessionTurns(c.sessionID) {
		if turn.ID == calls[0].TurnID {
			tools = turn.Tools
		}
	}
	if len(tools) != 1 || tools[0].Name != "slow_lookup" {
		t.Fatalf("the cut-off turn lists %+v", tools)
	}
}
