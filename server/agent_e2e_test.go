package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"govoice/core"
	"govoice/server/agent"
	"govoice/transport/ws"
)

// End to end with the echo provider: a real
// server, a stored routed flow, and a WebSocket client that sends text turns.
// The echo LLM labels each reply with its step's prompt and calls the step's
// transfer tool on "transfer to <step>", so the whole path runs: transfer tool,
// on-demand step build, flip, old-step close, timeline, traces and metrics.

const e2eMgmtKey = "management-test-key-32-bytes-long-and-separate"

// stageProbe wraps the fake stage factories: it counts builds and closes per
// step (LLM by prompt, TTS by voice), can fail or hold a step's build.
type stageProbe struct {
	mu            sync.Mutex
	built         map[string]int
	closed        map[string]int
	units         map[string]int      // synthesis units each TTS voice was asked to speak
	said          map[string][]string // the text of those units, by voice
	tools         map[string][]string // tool calls and results each stage saw ("call:<name>", "result:<status>")
	failTTS       string              // a TTS voice whose build fails
	holdTTS       string              // a TTS voice whose build waits for gate
	failAfterHold bool                // the held build fails once released (a provider that times out)
	gate          chan struct{}       // closed to let the held build finish
	entered       chan struct{}       // closed when the held build starts
	once          sync.Once
}

func (p *stageProbe) count(m map[string]int, k string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return m[k]
}
func (p *stageProbe) builds(k string) int { return p.count(p.built, k) }
func (p *stageProbe) closes(k string) int { return p.count(p.closed, k) }
func (p *stageProbe) spoke(k string) int  { return p.count(p.units, k) }

// sayings is everything a TTS voice was asked to speak, in order.
func (p *stageProbe) sayings(voice string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.said[voice]...)
}

// toolFrames is the tool calls and results a stage saw, in order.
func (p *stageProbe) toolFrames(k string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tools[k]...)
}

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

func (s *probedStage) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if u, ok := f.(*core.SynthesisUnitFrame); ok {
		s.probe.mu.Lock()
		s.probe.units[s.key]++
		s.probe.said[s.key] = append(s.probe.said[s.key], u.Text)
		s.probe.mu.Unlock()
	}
	switch v := f.(type) {
	case *core.ToolCallFrame:
		s.probe.mu.Lock()
		s.probe.tools[s.key] = append(s.probe.tools[s.key], "call:"+v.Call.Name)
		s.probe.mu.Unlock()
	case *core.ToolResultFrame:
		s.probe.mu.Lock()
		s.probe.tools[s.key] = append(s.probe.tools[s.key], "result:"+string(v.Result.Status))
		s.probe.mu.Unlock()
	}
	return s.Processor.ProcessFrame(ctx, f, dir, e)
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
		orig := s.registry.factories["echo."+stage]
		s.registry.Register("echo."+stage, func(cfg *Config, spec StageSpec) (core.Processor, error) {
			key := spec.SystemPrompt
			if stage == "tts" {
				key = spec.Voice
				if key == p.failTTS && key != "" {
					return nil, errors.New("tts provider down")
				}
				if key == p.holdTTS && key != "" {
					p.once.Do(func() { close(p.entered) })
					<-p.gate
					if p.failAfterHold {
						return nil, errors.New("tts provider timed out")
					}
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
	return &stageProbe{built: map[string]int{}, closed: map[string]int{}, units: map[string]int{}, said: map[string][]string{}, tools: map[string][]string{}, gate: make(chan struct{}), entered: make(chan struct{})}
}

type agentRigE2E struct {
	t       *testing.T
	s       *Server
	ts      *httptest.Server
	probe   *stageProbe
	agentID string
}

func agentConfigJSON(salesPrompt, salesVoice string, cue ...string) json.RawMessage {
	tail := ""
	if len(cue) > 0 {
		tail = `,"handoff_cue":` + cue[0]
	}
	stage := func(extra string) string { return `"provider":"echo"` + extra }
	return json.RawMessage(`{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":{"stt":{` + stage(`,"model":"s"`) + `},"llm":{` + stage(`,"model":"m","system_prompt":"FRONT"`) + `},"tts":{` + stage(`,"voice":"frontvoice"`) + `}}},` +
		`{"id":"sales","name":"Sales","config":{"llm":{` + stage(`,"model":"m","system_prompt":"`+salesPrompt+`"`) + `},"tts":{` + stage(`,"voice":"`+salesVoice+`"`) + `}}}],` +
		`"handoffs":[{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."}]` + tail + `}`)
}

func newAgentRigE2E(t *testing.T, probe *stageProbe, mods ...func(*Config)) *agentRigE2E {
	t.Helper()
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", e2eMgmtKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Providers.Echo.Mimic, cfg.Providers.Echo.LabelReplies = "instant", true
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	for _, m := range mods {
		m(cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	probe.install(s)
	ts := httptest.NewServer(s.wsServer)
	t.Cleanup(ts.Close)
	// Runs first (LIFO), after each call's own cleanup closed its connection: waits for the
	// calls to end and releases the pinned MCP sessions, VAD and management plane.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return &agentRigE2E{t: t, s: s, ts: ts, probe: probe}
}

func (r *agentRigE2E) createHandoffAgent(salesVoice string, cue ...string) string {
	a, err := r.s.agents.Create(r.t.Context(), agent.CreateRequest{Name: "Dealership", Config: agentConfigJSON("SALES", salesVoice, cue...)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		r.t.Fatalf("graph flow: %+v err=%v", a.Readiness, err)
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
	fn        []fnEvent // llm-function-call-* messages, in arrival order
	wire      []string  // every RTVI message ("msg:<type>") and bot audio frame ("audio"), in arrival order
	audio     []audioArrival
	seq       int
	done      chan struct{} // closed when the server ends the connection
}

// fnEvent is one llm-function-call-* message the client received.
type fnEvent struct {
	Type        string         `json:"type"`
	Data        map[string]any `json:"data"`
	Raw         string         `json:"-"` // the whole message, as sent
	TextsBefore int            `json:"-"` // bot-llm-text messages received before it
}

func (c *e2eCall) fnEvents() []fnEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.fn)
}

// audioArrival is one bot audio frame on the wire. The fake TTS sends silence,
// so a frame with a high peak is the route cue.
type audioArrival struct {
	at   time.Time
	peak int
	dur  time.Duration
}

func (r *agentRigE2E) dial(agentID string) *e2eCall {
	r.t.Helper()
	return r.dialToken(r.voiceToken(agentID))
}

// voiceToken issues a voice token for flowID, as an integration's backend does.
func (r *agentRigE2E) voiceToken(agentID string) string {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodPost, "/api/v1/voice-tokens", e2eMgmtKey, `{"principal":"alice","agent_id":"`+agentID+`"}`)
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Data.Token
}

// dialToken connects with an already issued voice token (a draft token, for one).
func (r *agentRigE2E) dialToken(token string) *e2eCall {
	r.t.Helper()
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.t.Context(), 30*time.Second)
	r.t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.ts.URL, "http")+"?token="+token, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.CloseNow() })
	c := &e2eCall{t: r.t, conn: conn, ctx: ctx, done: make(chan struct{})}
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
	defer close(c.done)
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			switch m := f.(type) {
			case *ws.WireMessageFrame:
				var head struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(m.Data), &head)
				c.mu.Lock()
				c.wire = append(c.wire, "msg:"+head.Type)
				c.mu.Unlock()
				if strings.Contains(m.Data, "bot-llm-text") {
					c.mu.Lock()
					c.texts = append(c.texts, m.Data)
					c.mu.Unlock()
				}
				if strings.Contains(m.Data, `"type":"llm-function-call-`) {
					ev := fnEvent{Raw: m.Data}
					if json.Unmarshal([]byte(m.Data), &ev) == nil {
						c.mu.Lock()
						ev.TextsBefore = len(c.texts)
						c.fn = append(c.fn, ev)
						c.mu.Unlock()
					}
				}
			case *core.InputAudioFrame: // server-to-client audio decodes as an audio frame
				peak := 0
				for i := 0; i+1 < len(m.Samples); i += 2 {
					v := int(int16(binary.LittleEndian.Uint16(m.Samples[i:])))
					peak = max(peak, v, -v)
				}
				c.mu.Lock()
				c.audio = append(c.audio, audioArrival{at: time.Now(), peak: peak, dur: m.Duration})
				c.wire = append(c.wire, "audio")
				c.mu.Unlock()
			}
		}
	}
}

// cueFrames counts the bot audio frames that carry sound (the cue; speech is silent in the fake).
func (c *e2eCall) cueFrames() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, a := range c.audio {
		if a.peak > 2000 {
			n++
		}
	}
	return n
}

func (c *e2eCall) say(text string) {
	c.seq++
	c.send(fmt.Sprintf(`{"id":"t%d","label":"rtvi-ai","type":"send-text","data":{"content":%q}}`, c.seq, text))
}

// replyCount is the number of bot reply fragments so far.
func (c *e2eCall) replyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.texts)
}

// replies counts how often want occurs in what the bot has said so far. The model streams a reply
// word by word, so a phrase spans fragments: the fragments are joined first.
func (c *e2eCall) replies(want string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var said strings.Builder
	for _, t := range c.texts {
		var m struct {
			Data struct {
				Text string `json:"text"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(t), &m) == nil {
			said.WriteString(m.Data.Text)
		}
	}
	return strings.Count(said.String(), want)
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

func (r *agentRigE2E) timeline() []TimelineEvent { return r.s.timelines.GetOrCreate(1).Events() }

func (r *agentRigE2E) has(typ TimelineEventType, details string) bool {
	for _, e := range r.timeline() {
		if e.Type == typ && (details == "" || e.Details == details) {
			return true
		}
	}
	return false
}

func (r *agentRigE2E) spanNames(sessionID string) map[string]bool {
	spans, err := r.s.mgmt.Load().store.Traces().Spans(r.t.Context(), sessionID, "")
	if err != nil {
		r.t.Fatal(err)
	}
	names := map[string]bool{}
	for _, sp := range spans {
		names[sp.Name] = true
		// The designer replays the path from these attributes; the trace allowlists
		// drop unknown keys, so they must really be persisted.
		if sp.Name == "handoff.active" && (!strings.Contains(sp.AttributesJSON, `"from_step":"front"`) || !strings.Contains(sp.AttributesJSON, `"to_step":"sales"`)) {
			r.t.Fatalf("handoff.active attributes = %s", sp.AttributesJSON)
		}
	}
	return names
}

// spanAttributes is the stored attributes JSON of the first span called name ("" when none).
func (r *agentRigE2E) spanAttributes(sessionID, name string) string {
	spans, err := r.s.mgmt.Load().store.Traces().Spans(r.t.Context(), sessionID, "")
	if err != nil {
		r.t.Fatal(err)
	}
	for _, sp := range spans {
		if sp.Name == name {
			return sp.AttributesJSON
		}
	}
	return ""
}

func (r *agentRigE2E) activeSessions() int { return activeSessionCount(r.t, r.s, r.agentID) }

// The whole path with a healthy second step.
func TestAgentE2EHandoffMovesTheCallAndClosesTheLeftStep(t *testing.T) {
	probe := newProbe()
	r := newAgentRigE2E(t, probe)
	c := r.dial(r.createHandoffAgent("salesvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("hello")
	c.waitReplies("[FRONT", 1)
	if probe.builds("SALES") != 0 || probe.builds("salesvoice") != 0 {
		t.Fatal("only the entry step may exist before the route")
	}

	c.say("please transfer to sales")
	c.waitReplies("Connecting you.", 1)
	c.waitReplies("[SALES", 1) // the new step greets once the call is idle
	if probe.builds("SALES") != 1 || probe.builds("salesvoice") != 1 {
		t.Fatalf("sales must be built exactly once: llm=%d tts=%d", probe.builds("SALES"), probe.builds("salesvoice"))
	}
	eventually(t, "front step closed", func() bool { return probe.closes("FRONT") == 1 && probe.closes("frontvoice") == 1 })
	if probe.closes("SALES") != 0 || probe.closes("salesvoice") != 0 {
		t.Fatal("the active step must stay open")
	}

	front := c.replies("[FRONT")
	c.say("what do you sell")
	c.waitReplies("[SALES", 2)
	if c.replies("[FRONT") != front {
		t.Fatal("the front step answered after the route")
	}

	if !r.has(EventHandoff, "front->sales") || !r.has(EventStepActive, "sales") || !r.has(EventStepClosed, "front") {
		t.Fatalf("timeline = %+v", r.timeline())
	}
	steps := map[string]bool{}
	for _, e := range r.timeline() {
		if e.Type == EventStepActive || e.Type == EventHandoff {
			steps[e.Step] = true
		}
	}
	if r.s.metrics.handoff.total.Load() != 1 || r.s.metrics.handoff.failures.Load() != 0 {
		t.Fatalf("metrics: routes=%d failures=%d", r.s.metrics.handoff.total.Load(), r.s.metrics.handoff.failures.Load())
	}
	eventually(t, "route trace spans", func() bool {
		n := r.spanNames(c.sessionID)
		return n["handoff.preparing"] && n["handoff.active"]
	})

	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, "sales step closed with the call", func() bool { return probe.closes("SALES") == 1 && probe.closes("salesvoice") == 1 })
}

// The second step cannot be built: the caller stays with the first.
func TestAgentE2EFailedStepBuildKeepsTheCallGoing(t *testing.T) {
	probe := newProbe()
	probe.failTTS = "brokenvoice"
	r := newAgentRigE2E(t, probe)
	c := r.dial(r.createHandoffAgent("brokenvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("transfer to sales")
	c.waitReplies("Connecting you.", 1)
	eventually(t, "handoff_failed", func() bool { return r.has(EventHandoffFailed, "sales") })
	if r.s.metrics.handoff.failures.Load() != 1 || r.s.metrics.handoff.total.Load() != 0 {
		t.Fatalf("metrics: routes=%d failures=%d", r.s.metrics.handoff.total.Load(), r.s.metrics.handoff.failures.Load())
	}
	for _, e := range r.timeline() {
		if e.Type == EventHandoffFailed && e.Error == "" {
			t.Fatal("the failure must say why")
		}
	}

	c.say("are you still there")
	c.waitReplies("[FRONT", 2)
	if c.replies("[SALES") != 0 || r.activeSessions() != 1 {
		t.Fatal("the call must stay with the front step")
	}
	if probe.closes("FRONT") != 0 || probe.closes("frontvoice") != 0 {
		t.Fatal("the front step must stay open")
	}
	if probe.closes("SALES") != probe.builds("SALES") {
		t.Fatalf("a step whose build failed half-way must not leak its LLM: built=%d closed=%d", probe.builds("SALES"), probe.closes("SALES"))
	}
}

// The caller talks while the second step is still being built: the first step
// answers, then the call moves over and the new step greets.
func TestAgentE2ECallerSpeaksWhileTheStepIsBeingPrepared(t *testing.T) {
	probe := newProbe()
	probe.holdTTS = "slowvoice"
	r := newAgentRigE2E(t, probe)
	c := r.dial(r.createHandoffAgent("slowvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("transfer to sales")
	c.waitReplies("Connecting you.", 1)
	<-probe.entered // the sales build is running and held
	c.say("one more question")
	c.waitReplies("[FRONT", 2) // not ready yet: the first step answers
	if c.replies("[SALES") != 0 {
		t.Fatal("sales must not answer before it is built")
	}
	if !r.has(EventStepPreparing, "sales") {
		t.Fatal("the designer needs the preparing event")
	}

	close(probe.gate)
	c.waitReplies("[SALES", 1)
	eventually(t, "front closed after the flip", func() bool { return probe.closes("FRONT") == 1 })
	if r.s.metrics.handoff.total.Load() != 1 {
		t.Fatal("exactly one route")
	}
}

// The caller hangs up while the next step is being built: when the build
// finishes, its stages are closed and nothing keeps running.
func TestAgentE2EHangUpWhilePreparingClosesTheLateStep(t *testing.T) {
	probe := newProbe()
	probe.holdTTS = "slowvoice"
	r := newAgentRigE2E(t, probe)
	baseline := runtime.NumGoroutine()
	c := r.dial(r.createHandoffAgent("slowvoice"))
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

// Republishing the flow mid-call does not change the call in progress.
func TestAgentE2ERepublishMidCallKeepsTheCallsAgent(t *testing.T) {
	probe := newProbe()
	r := newAgentRigE2E(t, probe)
	c := r.dial(r.createHandoffAgent("salesvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("hello")
	c.waitReplies("[FRONT", 1)

	v2 := agentConfigJSON("SALESV2", "salesvoice")
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

// A plain (flat) flow goes through the same server unchanged.
func TestAgentE2EFlatAgentHasNoHandoffs(t *testing.T) {
	probe := newProbe()
	r := newAgentRigE2E(t, probe)
	a, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Flat", Config: json.RawMessage(
		`{"stt":{"provider":"echo"},"llm":{"provider":"echo","model":"m","system_prompt":"FLAT"},"tts":{"provider":"echo"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	r.agentID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("transfer to sales")
	c.waitReplies("[FLAT", 1)
	if c.replies("Connecting you.") != 0 {
		t.Fatal("a flat flow has no transfer tool")
	}
	for _, e := range r.timeline() {
		switch e.Type {
		case EventHandoff, EventStepActive, EventStepPreparing, EventHandoffFailed:
			t.Fatalf("a flat flow produced a route event: %+v", e)
		}
	}
	if r.s.metrics.handoff.total.Load() != 0 {
		t.Fatal("no route metrics for a flat flow")
	}
}

// ---- Router cue, end to end -------------------------------------------------
// A real server with audio.cues configured, a routed flow with a handoff_cue and a
// WebSocket client that decodes the bot's audio frames: the cue is a tone on the
// wire (the fake TTS sends silence), so its start, its length and its stop are
// observable bytes, not just timeline events.

// cueWAV is a 0.4 s 440 Hz tone, PCM16 mono 24 kHz (the transport's player rate).
func cueWAV(t *testing.T) string {
	t.Helper()
	const rate, n = 24000, 9600
	pcm := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(12000*math.Sin(2*math.Pi*440*float64(i)/rate))))
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, struct {
		Size             uint32
		Format, Channels uint16
		Rate, ByteRate   uint32
		Align, Bits      uint16
	}{16, 1, 1, rate, rate * 2, 2, 16})
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	path := filepath.Join(t.TempDir(), "chime.wav")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withCue(t *testing.T) func(*Config) {
	path := cueWAV(t)
	return func(c *Config) {
		gain, loop := 0.0, true
		c.Audio.Cues = map[string]CueConfig{"chime": {File: path, GainDB: &gain, Loop: &loop}}
	}
}

func (r *agentRigE2E) cueStopReason() string {
	for _, e := range r.timeline() {
		if e.Type == EventCueStop {
			return e.Details
		}
	}
	return ""
}

// The cue fills the gap while the next step is being built, then stops with a
// short fade when the new step speaks.
func TestAgentE2ECuePlaysInTheHandoffGapAndStopsWhenSalesSpeaks(t *testing.T) {
	probe := newProbe()
	probe.holdTTS = "slowvoice" // the sales build takes as long as the test wants
	r := newAgentRigE2E(t, probe, withCue(t))
	c := r.dial(r.createHandoffAgent("slowvoice", `{"name":"chime","after_ms":50}`))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("hello")
	c.waitReplies("[FRONT", 1)
	time.Sleep(300 * time.Millisecond)
	if c.cueFrames() != 0 || r.has(EventCueStart, "") {
		t.Fatal("a cue must not play outside a route")
	}

	c.say("transfer to sales")
	c.waitReplies("Connecting you.", 1)
	<-probe.entered
	eventually(t, "cue on the wire", func() bool { return r.has(EventCueStart, "") && c.cueFrames() >= 5 })
	playing := c.cueFrames()
	time.Sleep(200 * time.Millisecond)
	if c.cueFrames() <= playing {
		t.Fatal("the cue must keep playing while the next step is being built")
	}

	close(probe.gate) // sales is built; it greets
	c.waitReplies("[SALES", 1)
	eventually(t, "cue stopped by the new step's first audio", func() bool { return r.cueStopReason() == "first_audio" })

	time.Sleep(400 * time.Millisecond) // past the 30 ms fade
	stopped := c.cueFrames()
	time.Sleep(300 * time.Millisecond)
	if c.cueFrames() != stopped {
		t.Fatal("the cue kept playing after the sales step spoke")
	}
	// In the gap the cue is paced audio, not a burst: 20 ms frames about 20 ms apart. (Its 30 ms
	// fade runs under the new step's first speech frames, which are not 20 ms.)
	c.mu.Lock()
	defer c.mu.Unlock()
	var gaps []time.Duration
	var prev time.Time
	for _, a := range c.audio {
		if a.peak > 2000 && a.dur == 20*time.Millisecond {
			if !prev.IsZero() {
				gaps = append(gaps, a.at.Sub(prev))
			}
			prev = a.at
		}
	}
	if len(gaps) < 5 {
		t.Fatalf("only %d paced cue frames in the gap", len(gaps)+1)
	}
	slices.Sort(gaps)
	if med := gaps[len(gaps)/2]; med < 10*time.Millisecond || med > 40*time.Millisecond {
		t.Fatalf("cue frames arrive %v apart, want about 20 ms (paced, not a burst)", med)
	}
}

// A route quicker than after_ms stays silent: nothing on the wire, no cue event.
func TestAgentE2EQuickHandoffPlaysNoCue(t *testing.T) {
	probe := newProbe()
	r := newAgentRigE2E(t, probe, withCue(t))
	c := r.dial(r.createHandoffAgent("salesvoice", `{"name":"chime","after_ms":8000}`))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("transfer to sales")
	c.waitReplies("[SALES", 1)
	time.Sleep(500 * time.Millisecond)
	if c.cueFrames() != 0 || r.has(EventCueStart, "") {
		t.Fatalf("a quick route must be silent (cue frames=%d)", c.cueFrames())
	}
}

// The next step cannot be built while the cue plays: the cue ends, the caller
// keeps the first step, and no sound is left behind.
func TestAgentE2ECueStopsWhenTheHandoffFails(t *testing.T) {
	probe := newProbe()
	probe.holdTTS, probe.failAfterHold = "slowvoice", true
	r := newAgentRigE2E(t, probe, withCue(t))
	c := r.dial(r.createHandoffAgent("slowvoice", `{"name":"chime","after_ms":50}`))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("transfer to sales")
	c.waitReplies("Connecting you.", 1)
	<-probe.entered
	eventually(t, "cue on the wire", func() bool { return c.cueFrames() >= 3 })

	close(probe.gate) // the build now fails
	eventually(t, "route failed", func() bool { return r.has(EventHandoffFailed, "sales") })
	eventually(t, "cue stopped because the route failed", func() bool { return r.cueStopReason() == "failed" })
	time.Sleep(400 * time.Millisecond)
	stopped := c.cueFrames()
	time.Sleep(300 * time.Millisecond)
	if c.cueFrames() != stopped {
		t.Fatal("the cue kept playing after the route failed")
	}

	c.say("are you still there")
	c.waitReplies("[FRONT", 2) // the caller is still with the front step
}

// A flow published with a sound the server had, on a server whose sounds changed since (a restart with a
// smaller catalogue), still hands off, silently: publish refuses an unknown name, a running call never does.
func TestAgentE2EUnknownCueNameStillHandsOff(t *testing.T) {
	probe := newProbe()
	r := newAgentRigE2E(t, probe, withCue(t))
	agentID := r.createHandoffAgent("salesvoice", `{"name":"chime","after_ms":50}`)
	delete(r.s.cues, "chime")
	c := r.dial(agentID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("transfer to sales")
	c.waitReplies("[SALES", 1)
	if c.cueFrames() != 0 || r.has(EventCueStart, "") {
		t.Fatal("an unknown cue must play nothing")
	}
}

// send_dtmf is a call tool: after a route the new step can still press keys.
func TestAgentE2ESendDTMFWorksAfterAHandoff(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(cfg *Config) { cfg.DTMF.SendTool = true })
	c := r.dial(r.createHandoffAgent("salesvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("please transfer to sales")
	c.waitReplies("[SALES", 1)
	c.say(`call tool send_dtmf {"digits":"1"}`)
	c.waitReplies("Tool send_dtmf returned", 1)
	if c.replies("Tool send_dtmf failed") != 0 {
		t.Fatal("send_dtmf failed after the route")
	}
}

// endCallFlowJSON is a front -> sales flow; frontEnds and salesEnds give that step the end_call tool.
func endCallAgentJSON(frontEnds, salesEnds bool) json.RawMessage {
	stt := `"stt":{"provider":"echo","model":"s"}`
	return json.RawMessage(`{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","end_call":` + strconv.FormatBool(frontEnds) + `,"config":{` + stt + `,"llm":{"provider":"echo","model":"m","system_prompt":"FRONT"},"tts":{"provider":"echo","voice":"frontvoice"}}},` +
		`{"id":"sales","name":"Sales","end_call":` + strconv.FormatBool(salesEnds) + `,"config":{"llm":{"provider":"echo","model":"m","system_prompt":"SALES"},"tts":{"provider":"echo","voice":"salesvoice"}}}],` +
		`"handoffs":[{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."}]}`)
}

func TestAgentE2EEndCallEndsTheCallAfterTheGoodbye(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	a, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Closer", Config: endCallAgentJSON(true, false)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", a.Readiness, err)
	}
	r.agentID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("hello")
	c.waitReplies("[FRONT", 1)
	if r.activeSessions() != 1 {
		t.Fatal("the call must stay up until end_call")
	}
	before := c.replyCount()
	c.say("please end the call")
	eventually(t, "the goodbye after end_call", func() bool { return c.replies("Connecting you.") >= 1 && c.replyCount() > before })
	eventually(t, "the call ended by the flow", func() bool { return r.has(EventDisconnect, "terminated: "+EndReasonAgent) })
	eventually(t, "session closed", func() bool { return r.activeSessions() == 0 })
	eventually(t, "call.end span", func() bool { return r.spanNames(c.sessionID)["call.end"] })
	// The trace says why and from which step, not just that the call ended.
	attrs := r.spanAttributes(c.sessionID, "call.end")
	if !strings.Contains(attrs, `"reason":"`+EndReasonAgent+`"`) || !strings.Contains(attrs, `"from_step":"front"`) {
		t.Fatalf("call.end attributes = %s", attrs)
	}
}

// oneStepEndCallJSON is a single-step flow (no routes); ends gives the step the end_call tool.
func oneStepEndCallJSON(ends bool) json.RawMessage {
	return json.RawMessage(`{"start":"main","steps":[{"id":"main","name":"Main","end_call":` + strconv.FormatBool(ends) + `,"config":{` +
		`"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"MAIN"},"tts":{"provider":"echo","voice":"v"}}}],"handoffs":[]}`)
}

func TestAgentE2EAOneStepAgentCanEndTheCall(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	a, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "OneStepCloser", Config: oneStepEndCallJSON(true)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", a.Readiness, err)
	}
	r.agentID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("hello")
	c.waitReplies("[MAIN", 1)
	if r.activeSessions() != 1 {
		t.Fatal("the call must stay up until end_call")
	}
	before := c.replyCount()
	c.say("please end the call")
	eventually(t, "the goodbye after end_call", func() bool { return c.replyCount() > before })
	eventually(t, "the call ended by the flow", func() bool { return r.has(EventDisconnect, "terminated: "+EndReasonAgent) })
	eventually(t, "session closed", func() bool { return r.activeSessions() == 0 })

	// The same one-step flow without the flag has no end_call tool: the phrase is just echoed and the call stays.
	b, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "OneStepStays", Config: oneStepEndCallJSON(false)})
	if err != nil || b.Readiness.State != agent.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", b.Readiness, err)
	}
	r.agentID = b.ID
	d := r.dial(b.ID)
	eventually(t, "second session", func() bool { return r.activeSessions() == 1 })
	d.say("please end the call")
	d.waitReplies("[MAIN", 1)
	d.say("are you there")
	d.waitReplies("[MAIN", 2)
	if r.activeSessions() != 1 {
		t.Fatal("a step without end_call must not hang up")
	}
}

// end_call must hang up even when the server's own quiet-caller handling is off (the idle controller is forced on).
func TestAgentE2EOneStepEndCallWithIdleOff(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), serverIdle(0, ""))
	a, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "OneStepIdleOff", Config: oneStepEndCallJSON(true)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", a.Readiness, err)
	}
	r.agentID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("please end the call")
	eventually(t, "the call ended by the flow", func() bool { return r.has(EventDisconnect, "terminated: "+EndReasonAgent) })
	eventually(t, "session closed", func() bool { return r.activeSessions() == 0 })
}

// Only sales has end_call; the caller is on front, which lacks the tool, so the phrase is just echoed.
func TestAgentE2EStepWithoutEndCallKeepsTheCall(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	a, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "SalesCloses", Config: endCallAgentJSON(false, true)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", a.Readiness, err)
	}
	r.agentID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("please end the call")
	c.waitReplies("[FRONT", 1)
	// The call still answers a second turn, so the first did not hang it up.
	c.say("are you there")
	c.waitReplies("[FRONT", 2)
	if r.activeSessions() != 1 || r.has(EventDisconnect, "terminated: "+EndReasonAgent) {
		t.Fatal("a step without end_call must not hang up")
	}
}

// A token minted with a configured token.max_lifetime carries that lifetime and is honoured by the live /ws
// path (the call connects and talks); a lifetime above the 5m ceiling is refused when the server is built.
func TestAgentE2EConfiguredTokenLifetimeIsHonouredByTheLiveConnect(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(c *Config) { c.Token.MaxLifetime = 3 * time.Minute })
	tok := r.voiceToken(r.createHandoffAgent("salesvoice"))
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(tok, claims); err != nil {
		t.Fatal(err)
	}
	exp, _ := claims["exp"].(float64)
	iat, _ := claims["iat"].(float64)
	if exp-iat != 180 {
		t.Fatalf("exp-iat = %v s, want 180", exp-iat)
	}
	c := r.dialToken(tok)
	c.say("hello")
	c.waitReplies("[FRONT", 1)

	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Token.MaxLifetime = 5*time.Minute + time.Second
	if _, err := NewServer(cfg); err == nil || !strings.Contains(err.Error(), "token.max_lifetime") {
		t.Fatalf("over-limit config built a server: %v", err)
	}
}

// handoffBackgroundAgent is front -> sales -> support (and sales -> front when back is set); the given
// audio object, if any, goes on each step by id.
func (r *agentRigE2E) handoffBackgroundAgent(audio map[string]string, back bool) string {
	r.t.Helper()
	stage := func(prompt, voice, extra string) string {
		return `"llm":{"provider":"echo","model":"m","system_prompt":"` + prompt + `"},"tts":{"provider":"echo","voice":"` + voice + `"}` + extra
	}
	step := func(id, name, prompt string, first bool) string {
		cfg := stage(prompt, id+"voice", "")
		if first {
			cfg = `"stt":{"provider":"echo","model":"s"},` + cfg
		}
		if a := audio[id]; a != "" {
			cfg += `,"audio":` + a
		}
		return `{"id":"` + id + `","name":"` + name + `","config":{` + cfg + `}}`
	}
	handoffs := `{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."},` +
		`{"id":"e2","from":"sales","to":"support","condition":"The caller needs help."}`
	if back {
		handoffs += `,{"id":"e3","from":"sales","to":"front","condition":"The caller is done buying."}`
	}
	cfg := `{"start":"front","steps":[` + step("front", "Front desk", "FRONT", true) + `,` + step("sales", "Sales", "SALES", false) + `,` +
		step("support", "Support", "SUPPORT", false) + `],"handoffs":[` + handoffs + `]}`
	a, err := r.s.agents.Create(r.t.Context(), agent.CreateRequest{Name: "Beds", Config: json.RawMessage(cfg)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		r.t.Fatalf("background flow: %+v err=%v", a.Readiness, err)
	}
	r.agentID = a.ID
	return a.ID
}

// R5a: a step's background starts when the route to it arrives, and a step that names none keeps the
// sound that is playing.
func TestAgentE2EAStepsBackgroundStartsWhenTheHandoffArrives(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Audio.Backgrounds = map[string]CueConfig{"office": {File: constWAV(t, "office", 20000), GainDB: ptrF(0)}}
		c.Audio.AllowAgentBackground = true
	})
	c := r.dial(r.handoffBackgroundAgent(map[string]string{"sales": `{"background":{"name":"office"}}`}, false))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("hello")
	c.waitReplies("[FRONT", 1)
	time.Sleep(500 * time.Millisecond)
	if c.cueFrames() != 0 {
		t.Fatalf("the front step has no background: %d sound frames before the route", c.cueFrames())
	}
	c.say("transfer to sales")
	c.waitReplies("[SALES", 1)
	eventually(t, "the sales bed is heard", func() bool { return c.cueFrames() >= 5 })

	c.say("transfer to support") // names no background: the office bed keeps playing
	c.waitReplies("[SUPPORT", 1)
	time.Sleep(300 * time.Millisecond)
	before := c.cueFrames()
	time.Sleep(300 * time.Millisecond)
	if c.cueFrames() <= before+5 {
		t.Fatal("a step without a background must keep the current sound")
	}
}

// K7: a route back to the start step restores the bed the call began with, the server default included.
func TestAgentE2EAStepsBackgroundIsRestoredWhenAHandoffReturnsToTheStart(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Audio.Backgrounds = map[string]CueConfig{"soft": {File: constWAV(t, "soft", 6000), GainDB: ptrF(0)}}
		c.Audio.BackgroundDefault = "soft"
		c.Audio.AllowAgentBackground = true
	})
	c := r.dial(r.handoffBackgroundAgent(map[string]string{"sales": `{"background":{"name":""}}`}, true))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	eventually(t, "the server default plays on the start step", func() bool { return c.cueFrames() >= 5 })

	c.say("transfer to sales")
	c.waitReplies("[SALES", 1)
	time.Sleep(400 * time.Millisecond)
	silent := c.cueFrames()
	time.Sleep(300 * time.Millisecond)
	if c.cueFrames() != silent {
		t.Fatal("sales asked for no sound: the default must stop")
	}

	c.say("transfer to front desk")
	c.waitReplies("[FRONT", 1)
	eventually(t, "the start step's bed is back", func() bool { return c.cueFrames() > silent+5 })
}
