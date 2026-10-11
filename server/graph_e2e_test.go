//go:build fakeproviders

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"govoice/core"
	"govoice/server/agent"
	"govoice/server/credential"
	"govoice/transport/ws"
)

// End to end with the fake providers (go test -tags fakeproviders): a real
// server, a stored graph agent, and a WebSocket client that sends text turns.
// The fake LLM labels each reply with its node's prompt and calls the node's
// transfer tool on "transfer to <node>", so the whole path runs: transfer tool,
// on-demand node build, flip, old-node close, timeline, traces and metrics.

const e2eMgmtKey = "management-test-key-32-bytes-long-and-separate"

// stageProbe wraps the fake stage factories: it counts builds and closes per
// node (LLM by prompt, TTS by voice), can fail or hold a node's build.
type stageProbe struct {
	mu      sync.Mutex
	built   map[string]int
	closed  map[string]int
	failTTS string        // a TTS voice whose build fails
	holdTTS string        // a TTS voice whose build waits for gate
	gate    chan struct{} // closed to let the held build finish
	entered chan struct{} // closed when the held build starts
	once    sync.Once
}

func (p *stageProbe) count(m map[string]int, k string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return m[k]
}
func (p *stageProbe) builds(k string) int { return p.count(p.built, k) }
func (p *stageProbe) closes(k string) int { return p.count(p.closed, k) }

type probedStage struct {
	core.Processor
	key   string
	probe *stageProbe
}

func (s *probedStage) Attach(sc core.SessionController, i int) error {
	if a, ok := s.Processor.(core.Attacher); ok {
		return a.Attach(sc, i)
	}
	return nil
}

func (s *probedStage) Close() error {
	s.probe.mu.Lock()
	s.probe.closed[s.key]++
	s.probe.mu.Unlock()
	if c, ok := s.Processor.(core.Closer); ok {
		return c.Close()
	}
	return nil
}

func (p *stageProbe) install(s *Server) {
	for _, stage := range []string{"llm", "tts"} {
		orig := s.registry.factories["google."+stage]
		s.registry.Register("google."+stage, func(cfg *Config, spec StageSpec) (core.Processor, error) {
			key := spec.SystemPrompt
			if stage == "tts" {
				key = spec.Voice
				if key == p.failTTS && key != "" {
					return nil, errors.New("tts provider down")
				}
				if key == p.holdTTS && key != "" {
					p.once.Do(func() { close(p.entered) })
					<-p.gate
				}
			}
			proc, err := orig(cfg, spec)
			if err != nil {
				return nil, err
			}
			p.mu.Lock()
			p.built[key]++
			p.mu.Unlock()
			return &probedStage{Processor: proc, key: key, probe: p}, nil
		})
	}
}

func newProbe() *stageProbe {
	return &stageProbe{built: map[string]int{}, closed: map[string]int{}, gate: make(chan struct{}), entered: make(chan struct{})}
}

type graphRigE2E struct {
	t       *testing.T
	s       *Server
	ts      *httptest.Server
	probe   *stageProbe
	credID  string
	agentID string
}

func graphConfigJSON(cred, salesPrompt, salesVoice string) json.RawMessage {
	stage := func(extra string) string { return `"provider":"google","credential_id":"` + cred + `"` + extra }
	return json.RawMessage(`{"entry":"front","nodes":[` +
		`{"id":"front","name":"Front desk","config":{"stt":{` + stage(`,"model":"s"`) + `},"llm":{` + stage(`,"model":"m","system_prompt":"FRONT"`) + `},"tts":{` + stage(`,"voice":"frontvoice"`) + `}}},` +
		`{"id":"sales","name":"Sales","config":{"llm":{` + stage(`,"model":"m","system_prompt":"`+salesPrompt+`"`) + `},"tts":{` + stage(`,"voice":"`+salesVoice+`"`) + `}}}],` +
		`"edges":[{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."}]}`)
}

func newGraphRigE2E(t *testing.T, probe *stageProbe) *graphRigE2E {
	t.Helper()
	t.Setenv("GOVOICE_FAKE_LLM_LABEL", "1")
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	cred, err := s.credentials.Create(t.Context(), credential.CreateInput{Provider: "google", Secret: "test-google-api-key"})
	if err != nil {
		t.Fatal(err)
	}
	probe.install(s)
	ts := httptest.NewServer(s.wsServer)
	t.Cleanup(ts.Close)
	return &graphRigE2E{t: t, s: s, ts: ts, probe: probe, credID: cred.ID}
}

func (r *graphRigE2E) createGraphAgent(salesVoice string) string {
	a, err := r.s.agents.Create(r.t.Context(), agent.CreateRequest{Name: "Dealership", Config: graphConfigJSON(r.credID, "SALES", salesVoice)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		r.t.Fatalf("graph agent: %+v err=%v", a.Readiness, err)
	}
	r.agentID = a.ID
	return a.ID
}

// e2eCall is one WebSocket call that collects the bot's streamed reply text.
type e2eCall struct {
	t         *testing.T
	conn      *websocket.Conn
	ctx       context.Context
	sessionID string // the stored session (the token's jti)
	mu        sync.Mutex
	texts     []string
	seq       int
}

func (r *graphRigE2E) dial(agentID string) *e2eCall {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodPost, "/api/v1/voice-tokens", e2eMgmtKey, `{"principal":"alice","agent_id":"`+agentID+`"}`)
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(resp.Data.Token, claims); err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.t.Context(), 30*time.Second)
	r.t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.ts.URL, "http")+"?token="+resp.Data.Token, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.CloseNow() })
	c := &e2eCall{t: r.t, conn: conn, ctx: ctx}
	c.sessionID, _ = claims["jti"].(string)
	c.send(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	go c.read()
	return c
}

func (c *e2eCall) send(raw string) {
	frame, _ := ws.EncodeMessageFrame(raw)
	if err := c.conn.Write(c.ctx, websocket.MessageBinary, frame); err != nil {
		c.t.Fatal(err)
	}
}

func (c *e2eCall) read() {
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok && strings.Contains(m.Data, "bot-llm-text") {
				c.mu.Lock()
				c.texts = append(c.texts, m.Data)
				c.mu.Unlock()
			}
		}
	}
}

func (c *e2eCall) say(text string) {
	c.seq++
	c.send(fmt.Sprintf(`{"id":"t%d","label":"rtvi-ai","type":"send-text","data":{"content":%q}}`, c.seq, text))
}

// replies counts the bot reply fragments that contain want.
func (c *e2eCall) replies(want string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.texts {
		if strings.Contains(t, want) {
			n++
		}
	}
	return n
}

func (c *e2eCall) waitReplies(want string, n int) {
	c.t.Helper()
	eventually(c.t, fmt.Sprintf("%d x %q (have %d)", n, want, c.replies(want)), func() bool { return c.replies(want) >= n })
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(8 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (r *graphRigE2E) timeline() []TimelineEvent { return r.s.timelines.GetOrCreate(1).Events() }

func (r *graphRigE2E) has(typ TimelineEventType, details string) bool {
	for _, e := range r.timeline() {
		if e.Type == typ && (details == "" || e.Details == details) {
			return true
		}
	}
	return false
}

func (r *graphRigE2E) spanNames(sessionID string) map[string]bool {
	spans, err := r.s.mgmt.store.Traces().Spans(r.t.Context(), sessionID, "")
	if err != nil {
		r.t.Fatal(err)
	}
	names := map[string]bool{}
	for _, sp := range spans {
		names[sp.Name] = true
	}
	return names
}

func (r *graphRigE2E) activeSessions() int { return activeSessionCount(r.t, r.s, r.agentID) }

// The whole path with a healthy second node.
func TestGraphE2EHandoffMovesTheCallAndClosesTheLeftNode(t *testing.T) {
	probe := newProbe()
	r := newGraphRigE2E(t, probe)
	c := r.dial(r.createGraphAgent("salesvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("hello")
	c.waitReplies("[FRONT", 1)
	if probe.builds("SALES") != 0 || probe.builds("salesvoice") != 0 {
		t.Fatal("only the entry node may exist before the handoff")
	}

	c.say("please transfer to sales")
	c.waitReplies("Connecting you.", 1)
	c.waitReplies("[SALES", 1) // the new agent greets once the call is idle
	if probe.builds("SALES") != 1 || probe.builds("salesvoice") != 1 {
		t.Fatalf("sales must be built exactly once: llm=%d tts=%d", probe.builds("SALES"), probe.builds("salesvoice"))
	}
	eventually(t, "front node closed", func() bool { return probe.closes("FRONT") == 1 && probe.closes("frontvoice") == 1 })
	if probe.closes("SALES") != 0 || probe.closes("salesvoice") != 0 {
		t.Fatal("the active node must stay open")
	}

	front := c.replies("[FRONT")
	c.say("what do you sell")
	c.waitReplies("[SALES", 2)
	if c.replies("[FRONT") != front {
		t.Fatal("the front node answered after the handoff")
	}

	if !r.has(EventHandoff, "front->sales") || !r.has(EventNodeActive, "sales") || !r.has(EventNodeClosed, "front") {
		t.Fatalf("timeline = %+v", r.timeline())
	}
	nodes := map[string]bool{}
	for _, e := range r.timeline() {
		if e.Type == EventNodeActive || e.Type == EventHandoff {
			nodes[e.Node] = true
		}
	}
	if r.s.metrics.handoff.total.Load() != 1 || r.s.metrics.handoff.failures.Load() != 0 {
		t.Fatalf("metrics: handoffs=%d failures=%d", r.s.metrics.handoff.total.Load(), r.s.metrics.handoff.failures.Load())
	}
	eventually(t, "handoff trace spans", func() bool {
		n := r.spanNames(c.sessionID)
		return n["handoff.preparing"] && n["handoff.active"]
	})

	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, "sales node closed with the call", func() bool { return probe.closes("SALES") == 1 && probe.closes("salesvoice") == 1 })
}

// The second node cannot be built: the caller stays with the first.
func TestGraphE2EFailedNodeBuildKeepsTheCallGoing(t *testing.T) {
	probe := newProbe()
	probe.failTTS = "brokenvoice"
	r := newGraphRigE2E(t, probe)
	c := r.dial(r.createGraphAgent("brokenvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("transfer to sales")
	c.waitReplies("Connecting you.", 1)
	eventually(t, "handoff_failed", func() bool { return r.has(EventHandoffFailed, "sales") })
	if r.s.metrics.handoff.failures.Load() != 1 || r.s.metrics.handoff.total.Load() != 0 {
		t.Fatalf("metrics: handoffs=%d failures=%d", r.s.metrics.handoff.total.Load(), r.s.metrics.handoff.failures.Load())
	}
	for _, e := range r.timeline() {
		if e.Type == EventHandoffFailed && e.Error == "" {
			t.Fatal("the failure must say why")
		}
	}

	c.say("are you still there")
	c.waitReplies("[FRONT", 2)
	if c.replies("[SALES") != 0 || r.activeSessions() != 1 {
		t.Fatal("the call must stay with the front node")
	}
	if probe.closes("FRONT") != 0 || probe.closes("frontvoice") != 0 {
		t.Fatal("the front node must stay open")
	}
	if probe.closes("SALES") != probe.builds("SALES") {
		t.Fatalf("a node whose build failed half-way must not leak its LLM: built=%d closed=%d", probe.builds("SALES"), probe.closes("SALES"))
	}
}

// The caller talks while the second node is still being built: the first node
// answers, then the call moves over and the new node greets.
func TestGraphE2ECallerSpeaksWhileTheNodeIsBeingPrepared(t *testing.T) {
	probe := newProbe()
	probe.holdTTS = "slowvoice"
	r := newGraphRigE2E(t, probe)
	c := r.dial(r.createGraphAgent("slowvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("transfer to sales")
	c.waitReplies("Connecting you.", 1)
	<-probe.entered // the sales build is running and held
	c.say("one more question")
	c.waitReplies("[FRONT", 2) // not ready yet: the first node answers
	if c.replies("[SALES") != 0 {
		t.Fatal("sales must not answer before it is built")
	}
	if !r.has(EventNodePreparing, "sales") {
		t.Fatal("the designer needs the preparing event")
	}

	close(probe.gate)
	c.waitReplies("[SALES", 1)
	eventually(t, "front closed after the flip", func() bool { return probe.closes("FRONT") == 1 })
	if r.s.metrics.handoff.total.Load() != 1 {
		t.Fatal("exactly one handoff")
	}
}

// The caller hangs up while the next node is being built: when the build
// finishes, its stages are closed and nothing keeps running.
func TestGraphE2EHangUpWhilePreparingClosesTheLateNode(t *testing.T) {
	probe := newProbe()
	probe.holdTTS = "slowvoice"
	r := newGraphRigE2E(t, probe)
	baseline := runtime.NumGoroutine()
	c := r.dial(r.createGraphAgent("slowvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("transfer to sales")
	<-probe.entered
	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, "session ended", func() bool { return r.activeSessions() == 0 })

	close(probe.gate) // the build now finishes into a closed call
	eventually(t, "late sales stages closed", func() bool {
		return probe.builds("SALES") == 1 && probe.closes("SALES") == 1 && probe.builds("slowvoice") == 1 && probe.closes("slowvoice") == 1
	})
	eventually(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= baseline+8 })
}

// Republishing the agent mid-call does not change the call in progress.
func TestGraphE2ERepublishMidCallKeepsTheCallsGraph(t *testing.T) {
	probe := newProbe()
	r := newGraphRigE2E(t, probe)
	c := r.dial(r.createGraphAgent("salesvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("hello")
	c.waitReplies("[FRONT", 1)

	v2 := graphConfigJSON(r.credID, "SALESV2", "salesvoice")
	if _, err := r.s.agents.Update(t.Context(), r.agentID, agent.UpdateRequest{Config: v2}); err != nil {
		t.Fatal(err)
	}
	c.say("transfer to sales")
	c.waitReplies("[SALES", 1)
	if c.replies("[SALESV2") != 0 || probe.builds("SALESV2") != 0 {
		t.Fatal("the call must keep the version it started on")
	}

	// A new call picks up the new version.
	c2 := r.dial(r.agentID)
	c2.say("transfer to sales")
	c2.waitReplies("[SALESV2", 1)
}

// A plain (flat) agent goes through the same server unchanged.
func TestGraphE2EFlatAgentHasNoHandoffs(t *testing.T) {
	probe := newProbe()
	r := newGraphRigE2E(t, probe)
	a, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Flat", Config: json.RawMessage(
		`{"stt":{"provider":"google","credential_id":"` + r.credID + `"},"llm":{"provider":"google","model":"m","system_prompt":"FLAT","credential_id":"` + r.credID + `"},"tts":{"provider":"google","credential_id":"` + r.credID + `"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	r.agentID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("transfer to sales")
	c.waitReplies("[FLAT", 1)
	if c.replies("Connecting you.") != 0 {
		t.Fatal("a flat agent has no transfer tool")
	}
	for _, e := range r.timeline() {
		switch e.Type {
		case EventHandoff, EventNodeActive, EventNodePreparing, EventHandoffFailed:
			t.Fatalf("a flat agent produced a handoff event: %+v", e)
		}
	}
	if r.s.metrics.handoff.total.Load() != 0 {
		t.Fatal("no handoff metrics for a flat agent")
	}
}
