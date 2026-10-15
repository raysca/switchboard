package server

import (
	"bytes"
	"context"
	"fmt"
	"govoice/server/flow"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/transport/ws"
	"govoice/vad"
)

// pacerTransport feeds the fake transport's output through the real ws pacer,
// so unit-id rules the fake ignores (session-monotonic ids) fail the session.
type pacerTransport struct {
	*coretest.TransportOutput
	w *ws.PacedWriter
}

func (p *pacerTransport) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if oaf, ok := f.(*core.OutputAudioFrame); ok && dir == core.Downstream {
		if err := p.w.Enqueue(oaf); err != nil {
			return err
		}
	}
	return p.TransportOutput.ProcessFrame(ctx, f, dir, e)
}

func TestIdleConfigValidation(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Storage.Dialect = "sqlite"
		return cfg
	}
	cases := map[string]func(*Config){
		"negative user":       func(c *Config) { c.Idle.UserTimeout = -time.Second },
		"negative bot":        func(c *Config) { c.Idle.BotTimeout = -time.Second },
		"no prompts":          func(c *Config) { c.Idle.UserTimeout, c.Idle.MaxPrompts = time.Second, 0 },
		"bad mode":            func(c *Config) { c.Idle.PromptMode = "shout" },
		"negative bot stuck":  func(c *Config) { c.Idle.BotMaxStuck = -1 },
		"dtmf inside timeout": func(c *Config) { c.Idle.UserTimeout, c.DTMF.Aggregate = time.Second, true },
	}
	for name, mod := range cases {
		cfg := base()
		mod(cfg)
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "idle.") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	cfg := base()
	cfg.Idle.UserTimeout, cfg.Idle.BotTimeout = 15*time.Second, 30*time.Second
	if err := ValidateConfig(cfg); err != nil && strings.Contains(err.Error(), "idle.") {
		t.Fatal(err)
	}
}

func idleTestPipeline(t *testing.T, cfg *Config) *core.Pipeline {
	t.Helper()
	return idleTestPipelineFor(t, cfg, CallSpec{})
}

func idleTestPipelineFor(t *testing.T, cfg *Config, call CallSpec) *core.Pipeline {
	t.Helper()
	reg := NewRegistry()
	for _, k := range []string{"google.stt", "google.llm", "google.tts"} {
		reg.Register(k, func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	call.STT, call.TTS, call.LLM = StageSpec{Provider: "google"}, StageSpec{Provider: "google"}, StageSpec{Provider: "google"}
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
	if err != nil {
		t.Fatal(err)
	}
	return pipe
}

func TestIdleSwitchedOffAddsNoProcessor(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Idle.UserTimeout = 0 // on by default; switched off it adds nothing
	pipe := idleTestPipeline(t, cfg)
	if pipe.Len() != 11 { // turn processor (manager on by default) and the async trigger (every call)
		t.Fatalf("pipeline has %d processors, want 11", pipe.Len())
	}
	for _, p := range pipe.Processors() {
		if _, ok := p.(*core.IdleController); ok {
			t.Fatal("idle controller built while disabled")
		}
	}
}

func TestIdleEnabledSitsBeforeTheModel(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Idle.UserTimeout = 15 * time.Second
	procs := idleTestPipeline(t, cfg).Processors()
	if len(procs) != 12 {
		t.Fatalf("pipeline has %d processors, want 12", len(procs))
	}
	if _, ok := procs[5].(*core.IdleController); !ok {
		t.Fatalf("processor 5 is %T, want the idle controller before the LLM", procs[5])
	}
}

// stallLLM never answers a user message containing "stall" (a hung provider)
// and answers anything else; it drops its work on an interruption.
type stallLLM struct {
	sess      core.SessionController
	idx       int
	mu        sync.Mutex
	cancel    context.CancelFunc
	cancelled atomic.Int32
}

func (l *stallLLM) Attach(s core.SessionController, i int) error { l.sess, l.idx = s, i; return nil }

func (l *stallLLM) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch v := f.(type) {
	case *core.UserMessageFrame:
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
		if strings.Contains(v.Content, "stall") {
			return nil
		}
		return l.sess.Go(func(context.Context) {
			gen := v.Gen
			_ = l.sess.Reenter(&core.LLMTextFrame{Text: "Sure thing.", Turn: v.Turn, Gen: gen}, core.Downstream, 0, gen)
		})
	case *core.InterruptionFrame:
		l.cancelled.Add(1)
	}
	return e.PushDirection(f, dir)
}

type idleE2E struct {
	sess    *core.Session
	llm     *stallLLM
	ended   chan string
	mu      sync.Mutex
	units   []string
	seen    map[[2]any]bool
	seenErr map[*core.ErrorFrame]bool
	errs    int
}

func (r *idleE2E) spoken() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.units...)
}

func (r *idleE2E) errors() int { r.mu.Lock(); defer r.mu.Unlock(); return r.errs }

// newIdleE2E runs the real BuildCallPipeline with a fake STT/TTS/transport. The
// fake transport reports no speaking state, so the test plays the transport's
// part: the first audio of a reply starts it, the turn-final audio stops it.
func newIdleE2E(t *testing.T, mod func(*Config)) *idleE2E {
	t.Helper()
	r := &idleE2E{llm: &stallLLM{}, ended: make(chan string, 4), seen: map[[2]any]bool{}, seenErr: map[*core.ErrorFrame]bool{}}
	cfg := DefaultConfig(ProfileDevelopment)
	mod(cfg)
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return passSTTStage{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 2}), nil
	})
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return r.llm, nil })
	call := CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, MaxMessages: 20, MaxTokens: 4000}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	w := ws.NewPacedWriter(ws.PacerConfig{EnablePacing: false}, func(context.Context, []byte) error { return nil })
	t.Cleanup(func() { w.Close() })
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &pacerTransport{coretest.NewTransportOutput(coretest.TransportConfig{}), w})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pipe.Processors() {
		if ic, ok := p.(*core.IdleController); ok {
			ic.SetOnEnd(func(reason string) { r.ended <- reason })
		}
	}
	if r.sess, err = core.NewSession(9, pipe, core.DefaultResourceBudget()); err != nil {
		t.Fatal(err)
	}
	var speaking atomic.Bool
	up := func(s core.SpeakingState) {
		_ = r.sess.Reenter(&core.SpeakingStateFrame{State: s, Reason: core.SpeakingStopDrained}, core.Upstream, r.sess.TailIndex(), 0)
	}
	r.sess.OnFrame = func(f core.Frame, dir core.Direction) {
		switch v := f.(type) {
		case *core.SynthesisUnitFrame:
			r.mu.Lock() // a unit passes several hops: record each once
			if k := [2]any{v.Turn, v.UnitID}; !r.seen[k] {
				r.seen[k] = true
				r.units = append(r.units, v.Text)
			}
			r.mu.Unlock()
		case *core.ErrorFrame:
			r.mu.Lock() // the frame is seen again at the edge past the transport
			if !r.seenErr[v] {
				r.seenErr[v] = true
				r.errs++
			}
			r.mu.Unlock()
		case *core.OutputAudioFrame:
			if dir == core.Downstream && speaking.CompareAndSwap(false, true) {
				up(core.SpeakingStarted)
			}
			if v.TurnFinal && speaking.CompareAndSwap(true, false) {
				up(core.SpeakingStopped)
			}
		}
	}
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.sess.Close()
		if n := r.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return r
}

func (r *idleE2E) say(t *testing.T, turn, text string) {
	t.Helper()
	gen := r.sess.CurrentGeneration()
	if err := r.sess.Reenter(&core.FinalTranscriptionFrame{Text: text, Turn: turn, Gen: gen, Committed: true, Timestamp: time.Now()}, core.Downstream, CallUserContextIndex, gen); err != nil {
		t.Fatal(err)
	}
}

func idleWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// A model that never answers: the client gets an error, the generation is
// cancelled, a fallback phrase is spoken, and the next turn works.
func TestCallPipelineIdleStuckModelRecovers(t *testing.T) {
	r := newIdleE2E(t, func(c *Config) {
		c.Idle.BotTimeout, c.Idle.BotFallback = 150*time.Millisecond, "Sorry, give me a moment."
	})
	g0 := r.sess.CurrentGeneration()
	r.say(t, "t1", "please stall")
	idleWait(t, "error to the client", func() bool { return r.errors() > 0 })
	idleWait(t, "generation bumped", func() bool { return r.sess.CurrentGeneration() > g0 })
	idleWait(t, "generation cancelled", func() bool { return r.llm.cancelled.Load() > 0 })
	idleWait(t, "fallback phrase", func() bool { return strings.Contains(strings.Join(r.spoken(), "|"), "give me a moment") })
	r.say(t, "t2", "hello")
	idleWait(t, "answer to the next turn", func() bool { return strings.Contains(strings.Join(r.spoken(), "|"), "Sure thing.") })
	time.Sleep(400 * time.Millisecond)   // answered: the watchdog stays quiet
	if err := r.sess.Err(); err != nil { // e.g. the pacer refusing the reply after the idle phrase
		t.Fatalf("session failed: %v", err)
	}
	if n := r.errors(); n != 1 {
		t.Fatalf("errors = %d, want 1", n)
	}
	select {
	case reason := <-r.ended:
		t.Fatalf("call ended: %s", reason)
	default:
	}
}

func TestCallPipelineIdleStuckModelEndsCallWhenRepeated(t *testing.T) {
	r := newIdleE2E(t, func(c *Config) { c.Idle.BotTimeout, c.Idle.BotMaxStuck = 100*time.Millisecond, 2 })
	g0 := r.sess.CurrentGeneration()
	r.say(t, "t1", "stall one")
	idleWait(t, "first error", func() bool { return r.errors() >= 1 })
	// The cancel's generation bump must land first, or t2 is stamped stale and fenced.
	idleWait(t, "generation bumped", func() bool { return r.sess.CurrentGeneration() > g0 })
	r.say(t, "t2", "stall two")
	select {
	case reason := <-r.ended:
		if reason != core.IdleReasonBot {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not end")
	}
}

// Silence after the bot spoke: two prompts, a goodbye, then the hang-up.
func TestCallPipelineIdleUserSilenceEndsCall(t *testing.T) {
	r := newIdleE2E(t, func(c *Config) {
		c.Idle.UserTimeout, c.Idle.MaxPrompts = 200*time.Millisecond, 2
		c.Idle.PromptMode, c.Idle.Prompt, c.Idle.Goodbye = "say", "Still there?", "Goodbye."
	})
	r.say(t, "t1", "hello")
	idleWait(t, "greeting", func() bool { return len(r.spoken()) >= 1 })
	start := time.Now()
	select {
	case reason := <-r.ended:
		if reason != core.IdleReasonUser {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("call did not end; spoken %v", r.spoken())
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("ended after %v, before two prompts and a final timeout", d)
	}
	// Suffix: the clock also runs from call start, so a very slow runner may
	// prompt once before "hello" lands (that prompt is then reset by it).
	if got := strings.Join(r.spoken(), "|"); !strings.HasSuffix(got, "Sure thing.|Still there?|Still there?|Goodbye.") {
		t.Fatalf("spoken = %q", got)
	}
}

// A caller who connects and never speaks is prompted and hung up over the real
// WebSocket path, with a final 1000 close (not the 1001 "reconnect elsewhere"
// of a drain) and the user_idle reason in a non-fatal RTVI error.
func TestIdleSilentCallerIsHungUpWithNormalClose(t *testing.T) {
	s, _, dial := drainFixture(t, time.Second)
	s.cfg.Idle.UserTimeout, s.cfg.Idle.MaxPrompts, s.cfg.Idle.PromptMode, s.cfg.Idle.Goodbye = 100*time.Millisecond, 1, "say", "" // no spoken goodbye: the drain fixture plays no audio to finish
	conn, _ := dial(true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var err error
	saw := false
	for {
		var data []byte
		if _, data, err = conn.Read(ctx); err != nil {
			break
		}
		saw = saw || (bytes.Contains(data, []byte(core.IdleReasonUser)) && bytes.Contains(data, []byte(`"fatal":false`)))
	}
	if got := websocket.CloseStatus(err); got != websocket.StatusNormalClosure || !saw {
		t.Fatalf("close status %d (%v), reason seen %v", got, err, saw)
	}
	idleWait(t, "hang-up counted", func() bool { return s.metrics.UserIdleHangupsTotal.Load() == 1 })
}

// idle speech re-enters at the TTS stage; the DTMF detector and input filter inserted at the
// front of the pipeline after the idle controller was built must shift that index.
func TestIdleSpeakIndexFollowsFrontInsertions(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"none":   func(*Config) {},
		"dtmf":   func(c *Config) { c.DTMF.Detect = true },
		"filter": func(c *Config) { c.Audio.InputFilters = []string{"denoise"} },
		"both":   func(c *Config) { c.DTMF.Detect, c.Audio.InputFilters = true, []string{"denoise"} },
	} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Idle.UserTimeout = 15 * time.Second
		mod(cfg)
		reg := NewRegistry()
		tts := &coretest.PassthroughProcessor{}
		reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
		reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
		reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return tts, nil })
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
		call := CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}}
		pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var idle *core.IdleController
		for _, p := range pipe.Processors() {
			if c, ok := p.(*core.IdleController); ok {
				idle = c
			}
		}
		if idle == nil {
			t.Fatalf("%s: no idle controller", name)
		}
		got := pipe.At(idle.SpeakIndex())
		// the TTS stage may be wrapped (failover/unit reporting); it must at least not be the
		// assistant-context stage or earlier: the stage at the index must follow the idle stage and the LLM.
		if idle.SpeakIndex() < 0 || got == nil {
			t.Fatalf("%s: speak index %d invalid", name, idle.SpeakIndex())
		}
		if got != core.Processor(tts) && !strings.Contains(fmt.Sprintf("%T", got), "TTS") {
			t.Errorf("%s: speak index %d points at %T, want the TTS stage", name, idle.SpeakIndex(), got)
		}
	}
}

func intp(v int) *int { return &v }

func TestAFlowsQuietCallerSettingsReplaceTheServersUserIdleOnly(t *testing.T) {
	server := IdleConfig{UserTimeout: 15 * time.Second, MaxPrompts: 5, Prompt: "Server prompt", PromptMode: "llm", Goodbye: "Server bye",
		BotTimeout: 40 * time.Second, BotMaxStuck: 4, BotFallback: "Server stuck"}
	if got := server.forCall(nil); got != server {
		t.Fatalf("no flow settings must leave the server's untouched: %+v", got)
	}
	got := server.forCall(&flow.IdleSettings{UserTimeoutMS: intp(8000)})
	want := IdleConfig{UserTimeout: 8 * time.Second, MaxPrompts: 2, Prompt: "Are you still there?", Goodbye: "I'll end the call now. Goodbye.",
		BotTimeout: 40 * time.Second, BotMaxStuck: 4, BotFallback: "Server stuck"}
	if got != want {
		t.Fatalf("a flow's idle is one unit with built-in defaults for what it leaves out:\n got %+v\nwant %+v", got, want)
	}
	got = server.forCall(&flow.IdleSettings{UserTimeoutMS: intp(5000), MaxPrompts: intp(3), Prompt: "Hello?", PromptMode: "say", Goodbye: "Bye."})
	if got.UserTimeout != 5*time.Second || got.MaxPrompts != 3 || got.Prompt != "Hello?" || got.PromptMode != "say" || got.Goodbye != "Bye." {
		t.Fatalf("flow members: %+v", got)
	}
	if off := server.forCall(&flow.IdleSettings{UserTimeoutMS: intp(0)}); off.UserTimeout != 0 || off.BotTimeout != 40*time.Second || !off.enabled() {
		t.Fatalf("0 turns user idle off but not the bot watchdog: %+v", off)
	}
	// A flow that sets only wording (no timeout) still has the built-in 15 s wait, not the server's.
	if w := (IdleConfig{UserTimeout: 60 * time.Second}).forCall(&flow.IdleSettings{Prompt: "Hello?"}); w.UserTimeout != 15*time.Second || w.Prompt != "Hello?" {
		t.Fatalf("members left out take the built-in defaults: %+v", w)
	}
}

func TestACallBuildsOrOmitsTheIdleControllerFromItsFlow(t *testing.T) {
	hasIdle := func(cfg *Config, call CallSpec) bool {
		for _, p := range idleTestPipelineFor(t, cfg, call).Processors() {
			if _, ok := p.(*core.IdleController); ok {
				return true
			}
		}
		return false
	}
	cfg := DefaultConfig(ProfileDevelopment)
	if !hasIdle(cfg, CallSpec{}) {
		t.Fatal("the built-in default must build the controller")
	}
	if hasIdle(cfg, CallSpec{Idle: &flow.IdleSettings{UserTimeoutMS: intp(0)}}) {
		t.Fatal("a flow that turns it off must not")
	}
	cfg.Idle.UserTimeout = 0
	if hasIdle(cfg, CallSpec{}) {
		t.Fatal("server off, flow silent: none")
	}
	if !hasIdle(cfg, CallSpec{Idle: &flow.IdleSettings{UserTimeoutMS: intp(4000)}}) {
		t.Fatal("a flow can turn it on when the server has it off")
	}
}
