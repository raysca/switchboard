package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/turn"
	"govoice/transport/ws"
)

// fnShape renders function-call messages as "<started|in-progress|stopped><data json>", each provider
// tool_call_id replaced by #1, #2… in order of first appearance, so a test asserts the exact sequence
// and payloads while the model picks the ids.
func fnShape(t *testing.T, evs []fnEvent) []string {
	t.Helper()
	ids := map[string]string{}
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		data := make(map[string]any, len(ev.Data))
		for k, v := range ev.Data {
			data[k] = v
		}
		if id, ok := data["tool_call_id"].(string); ok {
			if !strings.HasPrefix(id, "echo-call-") {
				t.Fatalf("tool_call_id %q is not the model's call id", id)
			}
			if ids[id] == "" {
				ids[id] = fmt.Sprintf("#%d", len(ids)+1)
			}
			data["tool_call_id"] = ids[id]
		}
		b, _ := json.Marshal(data)
		out = append(out, strings.TrimPrefix(ev.Type, "llm-function-call-")+string(b))
	}
	return out
}

func waitFnEvents(t *testing.T, c *e2eCall, n int) []fnEvent {
	t.Helper()
	eventually(t, fmt.Sprintf("%d function-call messages (have %d)", n, len(c.fnEvents())), func() bool { return len(c.fnEvents()) >= n })
	return c.fnEvents()
}

// assertFnEvents waits for len(want) function-call messages and checks them, then sends one more turn as an
// ordering barrier: once its answer arrived, everything the call emitted before it has too, so a late or
// duplicate event (a second stopped, a started after barge-in) shows. The list must still be exactly want.
func assertFnEvents(t *testing.T, c *e2eCall, want []string) []fnEvent {
	t.Helper()
	if got := fnShape(t, waitFnEvents(t, c, len(want))); !slices.Equal(got, want) {
		t.Fatalf("function-call messages %q, want %q", got, want)
	}
	c.barrier(t)
	evs := c.fnEvents()
	if got := fnShape(t, evs); !slices.Equal(got, want) {
		t.Fatalf("after the barrier turn: function-call messages %q, want exactly %q", got, want)
	}
	return evs
}

// barrier sends one more text turn and waits for the echo model's answer to it.
// ponytail: a text turn sent while the previous reply is still being committed can be lost (a known,
// tool-independent issue tracked separately), so it is resent, at most three times, 3 s apart.
func (c *e2eCall) barrier(t *testing.T) {
	t.Helper()
	for i := 0; i < 3; i++ {
		c.say("barrier")
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if c.replies("You said: barrier") > 0 {
				return
			}
		}
	}
	t.Fatal("the barrier turn was never answered")
}

// liveSession hooks the rig's WebSocket server so a test reaches the call's core session. Call it before dial.
func (r *mcpCallRig) liveSession() func() *core.Session {
	var live atomic.Pointer[ws.Connection]
	onText := r.s.wsServer.OnText
	r.s.wsServer.OnText = func(conn *ws.Connection, text string) error {
		live.Store(conn)
		return onText(conn, text)
	}
	return func() *core.Session { return live.Load().Session() }
}

// bargeIn is what the VAD and the STT send when the caller says "wait never mind" over the assistant.
func bargeIn(t *testing.T, sess *core.Session) {
	t.Helper()
	at := -1
	for i, p := range sess.Processors() {
		if _, ok := p.(*turn.Processor); ok {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("no turn processor in the call pipeline")
	}
	gen := sess.CurrentGeneration()
	if err := sess.Reenter(&core.UserStartedSpeakingFrame{Seq: 99, Gen: gen, Timestamp: time.Now()}, core.Downstream, at, gen); err != nil {
		t.Fatal(err)
	}
	if err := sess.Reenter(&core.TranscriptCandidateFrame{Text: "wait never mind", Turn: core.TurnID(fmt.Sprintf("turn-%d-99", gen)), Gen: core.Generation(gen), Final: true, EndOfUtterance: true, Timestamp: time.Now()}, core.Downstream, at, gen); err != nil {
		t.Fatal(err)
	}
}

func TestMCPCallToolEventsOnTheWire(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.agentWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	assertFnEvents(t, c, want)
}

func TestMCPCallFailedToolStopsUncancelled(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("fail", r.ts.URL+"/fail/tools", "")
	c := r.dial(r.agentWith(id, hashes, false, nil, "tool_error").ID)
	c.say("call tool tool_error")
	c.waitReplies("Tool tool_error failed", 1)
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	assertFnEvents(t, c, want)
}

func TestMCPCallParallelToolsEachGetTheirEvents(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.Tools.MaxParallel = 4 })
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.agentWith(id, hashes, true, nil, "lookup_order", "echo").ID)
	c.say(`call tool lookup_order {"order_id":"42"} then call tool echo {"text":"hi"}`)
	c.waitReplies("Tool echo returned", 1)
	want := []string{
		`started{}`, `in-progress{"tool_call_id":"#1"}`,
		`started{}`, `in-progress{"tool_call_id":"#2"}`,
		`stopped{"cancelled":false,"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#2"}`,
	}
	assertFnEvents(t, c, want)
	if r.lastCall("lookup_order").Tool == "" || r.lastCall("echo").Tool == "" {
		t.Fatal("the MCP server did not see both calls")
	}
}

func TestMCPCallBargeInSettlesTheAnnouncedCallAndNothingElse(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.MCP.Timeout = 10 * time.Second })
	session := r.liveSession()
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.agentWith(id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":4000}`)
	waitFnEvents(t, c, 2) // announced: started, in-progress
	// in-progress precedes the executor's MCP request: barge in only once the server has the call.
	eventually(t, "the MCP server to receive the call", func() bool { return r.lastCall("slow_lookup").Tool != "" })
	bargeIn(t, session())
	eventually(t, "the server to see the call abandoned", func() bool { return r.lastCall("slow_lookup").Cancelled })
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":true,"tool_call_id":"#1"}`}
	assertFnEvents(t, c, want)
}

func TestMCPCallAsyncToolStopsWhenItsBackgroundRunEnds(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.agentWith(id, hashes, false, []string{id + "/slow_lookup"}, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":1000}`)
	c.waitReplies("running", 1) // the acknowledgement is answered at once
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	evs := assertFnEvents(t, c, want)
	if evs[2].TextsBefore <= evs[1].TextsBefore {
		t.Fatal("the call was settled before the acknowledgement was answered (the ack must not stop it)")
	}
}

func TestMCPCallToolEventsCarryNoToolContent(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.Privacy.Redaction.Enabled = true })
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.agentWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"4111 1111 1111 1111"}`)
	c.waitReplies("shipped", 1)
	waitFnEvents(t, c, 3)
	c.barrier(t)
	evs := c.fnEvents()
	if len(evs) != 3 {
		t.Fatalf("%d function-call messages, want 3", len(evs))
	}
	for _, ev := range evs {
		for _, leak := range []string{"4111", "lookup_order", "function_name", "arguments", "result", "shipped"} {
			if strings.Contains(ev.Raw, leak) {
				t.Fatalf("%q reached the client: %s", leak, ev.Raw)
			}
		}
	}
}
