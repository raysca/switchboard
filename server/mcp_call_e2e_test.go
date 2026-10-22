package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/turn"
	"govoice/server/flow"
	"govoice/server/mcp/mcptest"
	"govoice/transport/ws"
)

// mcpCallRig is the graph rig (echo provider, real WebSocket calls) plus local test
// MCP servers. The echo LLM calls an attached MCP tool when a user turn says
// "call tool <name> {args}" and replies with what came back.
type mcpCallRig struct {
	*flowRigE2E
	mcp    *mcptest.Server
	ts     *httptest.Server
	refuse string
}

func newMCPCallRig(t *testing.T, mods ...func(*Config)) *mcpCallRig {
	t.Helper()
	// Registered before the flow rig so it closes after the rig's Shutdown: the calls' DELETEs
	// still reach the MCP server and end its sessions.
	mcp := mcptest.New(mcptest.Options{Token: "tok-1"})
	ts := httptest.NewServer(mcp.Handler())
	t.Cleanup(ts.Close)
	g := newFlowRigE2E(t, newProbe(), append([]func(*Config){func(c *Config) {
		c.MCP.AllowLocalHTTP = true
		c.MCP.Timeout = 800 * time.Millisecond
	}}, mods...)...)
	vars := testProviderVars(t)
	vars["MCP_TOKEN_TEST"] = "tok-1"
	g.s.env = mapEnv(vars)
	r := &mcpCallRig{flowRigE2E: g, mcp: mcp, ts: ts}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.refuse = free.Addr().String()
	_ = free.Close()
	return r
}

// server registers and validates an MCP server; it returns its id and each tool's schema hash.
func (r *mcpCallRig) server(name, endpoint, authEnv string) (string, map[string]string) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "endpoint": endpoint, "auth_env": authEnv, "enabled": true})
	w := apiRequest(r.s, http.MethodPost, "/api/v1/mcp-servers", e2eMgmtKey, string(body))
	var created struct{ Data struct{ ID string } }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.ID == "" {
		r.t.Fatalf("create %s: %d %s", name, w.Code, w.Body.String())
	}
	id := created.Data.ID
	apiRequest(r.s, http.MethodPost, "/api/v1/mcp-servers/"+id+"/validate", e2eMgmtKey, "")
	return r.discovered(id)
}

// discovered returns a validated server's id and each discovered tool's schema hash.
func (r *mcpCallRig) discovered(id string) (string, map[string]string) {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodGet, "/api/v1/mcp-servers/"+id, e2eMgmtKey, "")
	var detail struct {
		Data struct {
			Discovery *struct {
				Tools []struct {
					RemoteName string `json:"remote_name"`
					SchemaHash string `json:"schema_hash"`
				}
			}
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &detail)
	hashes := map[string]string{}
	if detail.Data.Discovery != nil {
		for _, t := range detail.Data.Discovery.Tools {
			hashes[t.RemoteName] = t.SchemaHash
		}
	}
	return id, hashes
}

// flowWith creates a plain echo flow with the given tools of one server attached.
func (r *mcpCallRig) flowWith(serverID string, hashes map[string]string, required bool, async []string, tools ...string) flow.Flow {
	r.t.Helper()
	return r.flowWithExtra("", serverID, hashes, required, async, tools...)
}

// flowWithExtra is flowWith with more top-level config members (a JSON fragment starting with a comma).
func (r *mcpCallRig) flowWithExtra(extra, serverID string, hashes map[string]string, required bool, async []string, tools ...string) flow.Flow {
	r.t.Helper()
	sel := make([]flow.ToolSelection, 0, len(tools))
	for _, name := range tools {
		hash := hashes[name]
		if hash == "" {
			hash = strings.Repeat("0", 64) // a server that never answered discovery
		}
		sel = append(sel, flow.ToolSelection{ServerID: serverID, RemoteName: name, SchemaHash: hash, Required: required})
	}
	asyncJSON, _ := json.Marshal(async)
	cfg := `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"P"},"tts":{"provider":"echo","voice":"v"},"async_tools":` + string(asyncJSON) + extra + `}`
	a, err := r.s.flows.Create(r.t.Context(), flow.CreateRequest{Name: "MCP " + strings.Join(tools, "+"), Config: json.RawMessage(cfg), MCPTools: sel})
	if err != nil {
		r.t.Fatalf("flow: %v", err)
	}
	r.flowID = a.ID
	return a
}

func (r *mcpCallRig) lastCall(tool string) mcptest.Record {
	recs := r.mcp.Records()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Tool == tool {
			return recs[i]
		}
	}
	return mcptest.Record{}
}

func TestMCPCallOK(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	a := r.flowWith(id, hashes, true, nil, "lookup_order")
	if a.Readiness.State != flow.ReadinessReady {
		t.Fatalf("readiness %+v", a.Readiness)
	}
	c := r.dial(a.ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	if got := r.lastCall("lookup_order"); got.Auth != "match" || string(got.Arguments) != `{"order_id":"42"}` {
		t.Fatalf("server saw %+v", got)
	}
	eventually(t, "mcp.tool.execute span", func() bool { return r.spanNames(c.sessionID)["mcp.tool.execute"] })
}

func TestMCPCallFailures(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("fail", r.ts.URL+"/fail/tools", "")
	for _, tool := range []string{"tool_error", "protocol_error", "drop", "too_big"} {
		t.Run(tool, func(t *testing.T) {
			c := r.dial(r.flowWith(id, hashes, false, nil, tool).ID)
			c.say("call tool " + tool)
			c.waitReplies("Tool "+tool+" failed", 1)
			// The call goes on after the failure. Paced: a text turn sent the instant a
			// reply arrives can be lost (tool-independent; tracked separately).
			time.Sleep(time.Second)
			c.say("hello after " + tool)
			c.waitReplies("hello after "+tool, 1)
		})
	}
}

func TestMCPCallSlow(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":200}`)
	c.waitReplies("Tool slow_lookup returned", 1)

	began := time.Now()
	c.say(`call tool slow_lookup {"delay_ms":2000}`)
	c.waitReplies("Tool slow_lookup failed", 1)
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Fatalf("over-time tool answered after %v (timeout 800ms)", took)
	}
	eventually(t, "the server to see the call abandoned", func() bool { return r.lastCall("slow_lookup").Cancelled })
	c.say("still there")
	c.waitReplies("still there", 1)
}

// Barge-in is the caller speaking: VAD start plus transcribed words reach the turn
// processor, a user turn starts, and the assistant's turn (with its tool call) is
// interrupted. The MCP timeout is long here, so only the interruption can end the call.
func TestMCPCallBargeInCancelsSlowTool(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.MCP.Timeout = 10 * time.Second })
	var live atomic.Pointer[ws.Connection]
	onText := r.s.wsServer.OnText
	r.s.wsServer.OnText = func(conn *ws.Connection, text string) error {
		live.Store(conn)
		return onText(conn, text)
	}
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":4000}`)
	eventually(t, "the tool call to start", func() bool { return r.lastCall("slow_lookup").Tool != "" })

	sess := live.Load().Session()
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
	began := time.Now()
	// What the VAD and the STT send when the caller says "wait, never mind".
	if err := sess.Reenter(&core.UserStartedSpeakingFrame{Seq: 99, Gen: gen, Timestamp: time.Now()}, core.Downstream, at, gen); err != nil {
		t.Fatal(err)
	}
	if err := sess.Reenter(&core.TranscriptCandidateFrame{Text: "wait never mind", Turn: core.TurnID(fmt.Sprintf("turn-%d-99", gen)), Gen: core.Generation(gen), Final: true, EndOfUtterance: true, Timestamp: time.Now()}, core.Downstream, at, gen); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the slow call to be abandoned", func() bool { return r.lastCall("slow_lookup").Cancelled })
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Fatalf("barge-in took %v to cancel the tool (the MCP timeout is 10s)", took)
	}
	time.Sleep(4200*time.Millisecond - time.Since(began)) // past the tool's own delay
	if c.replies("Tool slow_lookup returned") != 0 {
		t.Fatal("a late tool result was spoken after the barge-in")
	}
}

func TestMCPCallUnreachable(t *testing.T) {
	r := newMCPCallRig(t)
	// Tools are attached while the server answers; then it goes away.
	gone := httptest.NewServer(mcptest.New(mcptest.Options{}).Handler())
	id, hashes := r.server("gone", gone.URL+"/open", "")
	a := r.flowWith(id, hashes, false, nil, "lookup_order")
	gone.Close()
	c := r.dial(a.ID)
	began := time.Now()
	c.say(`call tool lookup_order {"order_id":"1"}`)
	c.waitReplies("Tool lookup_order failed", 1)
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Fatalf("unreachable tool failed after %v (timeout 800ms)", took)
	}
	c.say("hello")
	eventually(t, "the call to go on", func() bool { return c.replies("hello") > 0 })
}

// R5b: a synchronous tool that runs past tool_cue.after_ms plays the flow's hold sound, which stops when the
// tool returns (before the reply is spoken). An async tool plays none, and an unknown cue is refused at publish.
func TestMCPCallAHoldSoundPlaysWhileASlowToolRuns(t *testing.T) {
	const holdCue = `,"audio":{"tool_cue":{"name":"chime","after_ms":300}}`
	slow := func(rig *mcpCallRig, async bool) *e2eCall {
		id, hashes := rig.server("slow", rig.ts.URL+"/slow", "")
		var asyncTools []string
		if async {
			asyncTools = []string{id + "/slow_lookup"}
		}
		return rig.dial(rig.flowWithExtra(holdCue, id, hashes, false, asyncTools, "slow_lookup").ID)
	}
	t.Run("a synchronous tool", func(t *testing.T) {
		r := newMCPCallRig(t, withCue(t), func(c *Config) { c.MCP.Timeout = 10 * time.Second })
		c := slow(r, false)
		c.say(`call tool slow_lookup {"delay_ms":1500}`)
		eventually(t, "the hold sound on the wire", func() bool { return c.cueFrames() >= 5 && r.has(EventCueStart, "tool") })
		c.waitReplies("Tool slow_lookup returned", 1)
		eventually(t, "the hold stopped because the tool returned", func() bool { return r.has(EventCueStop, "tool_done") })
		time.Sleep(400 * time.Millisecond) // past the fade
		stopped := c.cueFrames()
		time.Sleep(300 * time.Millisecond)
		if c.cueFrames() != stopped {
			t.Fatal("the hold sound kept playing after the tool returned")
		}
	})
	t.Run("an async tool", func(t *testing.T) {
		r := newMCPCallRig(t, withCue(t), func(c *Config) { c.MCP.Timeout = 10 * time.Second })
		c := slow(r, true)
		c.say(`call tool slow_lookup {"delay_ms":1500}`)
		time.Sleep(1200 * time.Millisecond)
		if c.cueFrames() != 0 || r.has(EventCueStart, "") {
			t.Fatalf("an async tool plays no hold sound (frames=%d)", c.cueFrames())
		}
	})
	t.Run("an unknown cue is refused at publish", func(t *testing.T) {
		r := newMCPCallRig(t, withCue(t))
		body := `{"name":"Nope","config":{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m"},"tts":{"provider":"echo","voice":"v"},"audio":{"tool_cue":{"name":"ding"}}}}`
		w := apiRequest(r.s, http.MethodPost, "/api/v1/flows", e2eMgmtKey, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "audio.tool_cue.name") {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	})
}

// Barge-in cancels the running tool, and the hold sound ends with it instead of playing on.
func TestMCPCallBargeInStopsTheHoldSound(t *testing.T) {
	r := newMCPCallRig(t, withCue(t), func(c *Config) { c.MCP.Timeout = 10 * time.Second })
	var live atomic.Pointer[ws.Connection]
	onText := r.s.wsServer.OnText
	r.s.wsServer.OnText = func(conn *ws.Connection, text string) error {
		live.Store(conn)
		return onText(conn, text)
	}
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWithExtra(`,"audio":{"tool_cue":{"name":"chime","after_ms":300}}`, id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":6000}`)
	eventually(t, "the hold sound", func() bool { return r.has(EventCueStart, "tool") && c.cueFrames() >= 5 })

	sess := live.Load().Session()
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
	eventually(t, "the tool to be abandoned", func() bool { return r.lastCall("slow_lookup").Cancelled })
	// The caller finishes speaking: from here only the tool's end can have stopped the sound (while they
	// speak the controller re-arms forever, so a missing toolDone would otherwise hide).
	now := sess.CurrentGeneration() // the barge-in bumped it: a frame at the old one would be dropped as stale
	if err := sess.Reenter(&core.UserStoppedSpeakingFrame{Seq: 99, Gen: now, Timestamp: time.Now()}, core.Downstream, at, now); err != nil {
		t.Fatal(err)
	}
	// The sound was paused by the caller's speech (barge_in) or stopped by the tool's end (tool_done).
	eventually(t, "the hold sound to stop", func() bool { return r.has(EventCueStop, "barge_in") || r.has(EventCueStop, "tool_done") })
	time.Sleep(400 * time.Millisecond) // past the fade
	stopped := c.cueFrames()
	time.Sleep(300*time.Millisecond + 400*time.Millisecond) // longer than after_ms: a restart would show
	if c.cueFrames() != stopped {
		t.Fatal("the hold sound kept playing after the barge-in")
	}
	// Once the caller stopped speaking, a tool still counted as running would restart it.
	starts := 0
	for _, e := range r.timeline() {
		if e.Type == EventCueStart {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("the hold sound started %d times: it must not restart after the tool was abandoned", starts)
	}
}

// An empty token variable counts as missing: flow readiness names it, and setting it clears it.
func TestFlowReadinessNamesAnEmptyMCPTokenVariable(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	a := r.flowWith(id, hashes, true, nil, "lookup_order")
	vars := testProviderVars(t)
	vars["MCP_TOKEN_TEST"] = ""
	r.s.env = mapEnv(vars)
	got, err := r.s.flows.Get(t.Context(), a.ID)
	if err != nil || got.Readiness.State != flow.ReadinessToolUnavailable || got.Readiness.ToolDependencies[0].State != "auth_env_missing" || len(got.Readiness.MissingEnv) != 1 || got.Readiness.MissingEnv[0] != "MCP_TOKEN_TEST" {
		t.Fatalf("readiness=%+v err=%v", got.Readiness, err)
	}
	vars["MCP_TOKEN_TEST"] = "tok-1"
	if got, err = r.s.flows.Get(t.Context(), a.ID); err != nil || got.Readiness.State != flow.ReadinessReady {
		t.Fatalf("readiness=%+v err=%v", got.Readiness, err)
	}
}
