package core_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
	"govoice/core/ttsunit"
)

// order records what leaves the TTS stage, in order.
type order struct {
	mu    sync.Mutex
	items []string
}

func (o *order) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch v := f.(type) {
	case *core.OutputAudioFrame:
		if len(v.Samples) > 0 {
			o.add("audio:" + v.Turn)
		}
	case *core.TTSStoppedFrame:
		o.add("stopped:" + v.Turn)
	}
	return e.PushDirection(f, dir)
}

func (o *order) add(s string) { o.mu.Lock(); o.items = append(o.items, s); o.mu.Unlock() }

func (o *order) has(s string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range o.items {
		if it == s {
			return true
		}
	}
	return false
}

// idleSayRig is the idle controller in front of a real assistant context, a TTS stage and a transport double.
type idleSayRig struct {
	*idleRig
	asst *core.AssistantContextProcessor
	out  *order
	sink *coretest.TransportOutput
}

func newIdleSayRig(t *testing.T, tts core.Processor, mod func(*core.IdleConfig)) *idleSayRig {
	t.Helper()
	r := &idleRig{t: t, clk: &fakeIdleClock{now: time.Unix(1000, 0)}, llm: &stageRec{keep: only[*core.UserMessageFrame](isInterrupt)},
		ended: make(chan string, 4), events: make(chan core.DebugEvent, 64), hist: core.NewConversationContext(core.ContextConfig{})}
	asst := core.NewAssistantContextProcessor(r.hist, core.AssistantContextConfig{})
	cfg := core.IdleConfig{UserTimeout: 15 * time.Second, MaxPrompts: 2, Say: true, UserPrompt: "are you still there?",
		History: r.hist, Clock: r.clk, Answering: asst.Active, Speak: asst.Say, OnEnd: func(reason string) { r.ended <- reason }}
	if mod != nil {
		mod(&cfg)
	}
	var err error
	if r.ctl, err = core.NewIdleController(cfg); err != nil {
		t.Fatal(err)
	}
	out, sink := &order{}, coretest.NewTransportOutput(coretest.TransportConfig{})
	pipe, err := core.NewPipeline(r.ctl, r.llm, asst, tts, out, sink)
	if err != nil {
		t.Fatal(err)
	}
	if r.sess, err = core.NewSession(1, pipe, core.DefaultResourceBudget()); err != nil {
		t.Fatal(err)
	}
	if err = r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.sess.Close()
		if n := r.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return &idleSayRig{idleRig: r, asst: asst, out: out, sink: sink}
}

// said waits for an assistant message with exactly text in the history.
func (r *idleSayRig) said(text string) {
	r.t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		for _, m := range r.hist.Messages() {
			if m.Role == core.RoleAssistant && m.Content == text {
				return
			}
		}
	}
	r.t.Fatalf("assistant never said %q; history %+v", text, r.hist.Messages())
}

func (r *idleSayRig) heard(text string) {
	r.t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		for _, f := range r.sink.GetWrittenFrames() {
			if strings.HasPrefix(f.Turn, "say-") {
				return
			}
		}
	}
	r.t.Fatalf("no audio of %q reached the sink", text)
}

func newUnitTTS() core.Processor {
	return ttsunit.New(ttsunit.Config{Provider: "fake", Model: "m", SampleRate: 24000,
		Policy:   resilience.Policy{MaxAttempts: 1},
		Classify: func(error) resilience.Verdict { return resilience.Verdict{} },
		Attempt: func(_ context.Context, u *ttsunit.Unit, _ int) error {
			if err := u.Write(make([]byte, 3*960)); err != nil {
				return err
			}
			return u.Finish()
		}})
}

var ttsStages = map[string]func() core.Processor{
	"coretest": func() core.Processor { return coretest.NewTTSProcessor(coretest.TTSConfig{}) },
	"ttsunit":  newUnitTTS,
}

func TestIdleSayPrompt_AppearsInHistory(t *testing.T) {
	for name, tts := range ttsStages {
		t.Run(name, func(t *testing.T) {
			r := newIdleSayRig(t, tts(), nil)
			r.clk.parkedAt(t, r.at(15*time.Second))
			r.clk.Advance(15 * time.Second)
			r.said("are you still there?")
			r.heard("are you still there?")
		})
	}
}

func TestIdleGoodbye_AppearsInHistory(t *testing.T) {
	for name, tts := range ttsStages {
		t.Run(name, func(t *testing.T) {
			r := newIdleSayRig(t, tts(), func(c *core.IdleConfig) { c.MaxPrompts = 1; c.Goodbye = "bye for now" })
			r.clk.parkedAt(t, r.at(15*time.Second))
			r.clk.Advance(15 * time.Second)
			r.said("are you still there?")
			r.clk.parkedAt(t, r.at(30*time.Second))
			r.clk.Advance(15 * time.Second)
			r.said("bye for now")
			r.heard("bye for now")
		})
	}
}

// The fallback is entered after the interruption it follows bumped the generation and passed the TTS
// stage: it is neither fenced by the bump nor cancelled by the stage's interruption handling.
func TestIdleBotFallback_AppearsInHistory(t *testing.T) {
	for name, tts := range ttsStages {
		t.Run(name, func(t *testing.T) {
			r := newIdleSayRig(t, tts(), func(c *core.IdleConfig) {
				c.UserTimeout, c.MaxPrompts = 0, 0
				c.BotTimeout, c.BotFallback = 30*time.Second, "sorry about that"
			})
			r.down(&core.UserMessageFrame{Turn: "t1", Content: "hi"})
			r.clk.parkedAt(t, r.at(30*time.Second))
			before := r.sess.CurrentGeneration()
			r.clk.Advance(30 * time.Second)
			r.said("sorry about that")
			r.heard("sorry about that")
			if after := r.sess.CurrentGeneration(); after <= before {
				t.Fatalf("the interruption did not bump the generation: %d -> %d", before, after)
			}
			for _, f := range r.sink.GetWrittenFrames() {
				if strings.HasPrefix(f.Turn, "say-") && f.Gen <= before {
					t.Fatalf("fallback audio carries generation %d, not above the interrupted turn's %d", f.Gen, before)
				}
			}
		})
	}
}

// The hang-up for user idle waits for the goodbye: its audio, its end (TTSStopped), then the transport's
// stop of speech; the goodbye is in the history by then.
func TestIdleGoodbyeThenHangup_Order(t *testing.T) {
	r := newIdleSayRig(t, newUnitTTS(), func(c *core.IdleConfig) { c.MaxPrompts = 1; c.Goodbye = "bye for now" })
	r.clk.parkedAt(t, r.at(15*time.Second))
	r.clk.Advance(15 * time.Second)
	r.said("are you still there?")
	r.clk.parkedAt(t, r.at(30*time.Second))
	r.clk.Advance(15 * time.Second)
	for end := time.Now().Add(3 * time.Second); !r.out.has("stopped:say-2") && time.Now().Before(end); time.Sleep(time.Millisecond) {
	}
	if !r.out.has("audio:say-2") || !r.out.has("stopped:say-2") {
		t.Fatalf("goodbye not delivered: %v", r.out.items)
	}
	r.noEnd() // delivered, but not yet played out
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); {
		select {
		case got := <-r.ended:
			if got != core.IdleReasonUser {
				t.Fatalf("ended with %q", got)
			}
			r.said("bye for now")
			return
		case <-time.After(5 * time.Millisecond):
			r.clk.Advance(100 * time.Millisecond) // the poll while the goodbye turn is still being closed out
		}
	}
	t.Fatal("call did not end after the goodbye played")
}
