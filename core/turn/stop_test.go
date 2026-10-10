package turn

import (
	"testing"
	"time"

	"govoice/core"
)

type fakeAnalyzer struct {
	res   AnalyzerResult
	calls int
}

func (f *fakeAnalyzer) Analyze(DetectionInput) AnalyzerResult { f.calls++; return f.res }

func TestStopStrategyDecisionTable(t *testing.T) {
	an := &fakeAnalyzer{res: AnalyzerResult{Complete: true, Confidence: 0.9}}
	incomplete := &fakeAnalyzer{}
	cases := []struct {
		name string
		s    UserTurnStopStrategy
		in   DetectionInput
		want core.TurnReason // None: no decision
	}{
		{"timeout below", SpeechTimeout(0), DetectionInput{SilenceFor: 699 * time.Millisecond, SilenceAfter: 700 * time.Millisecond}, core.TurnReasonNone},
		{"timeout at default", SpeechTimeout(0), DetectionInput{SilenceFor: 700 * time.Millisecond, SilenceAfter: 700 * time.Millisecond}, core.TurnReasonSilence},
		{"timeout override", SpeechTimeout(300 * time.Millisecond), DetectionInput{SilenceFor: 300 * time.Millisecond, SilenceAfter: time.Second}, core.TurnReasonSilence},
		{"analyzer complete", AnalyzerStop(an, 3*time.Second), DetectionInput{SilenceFor: 100 * time.Millisecond}, core.TurnReasonSemantic},
		{"analyzer incomplete waits", AnalyzerStop(incomplete, 3*time.Second), DetectionInput{SilenceFor: 2 * time.Second}, core.TurnReasonNone},
		{"analyzer fallback", AnalyzerStop(incomplete, 3*time.Second), DetectionInput{SilenceFor: 3 * time.Second}, core.TurnReasonSilence},
		{"eager final sentence", EagerStop(200 * time.Millisecond), DetectionInput{Text: "Book it.", TextFinal: true, SilenceFor: 200 * time.Millisecond}, core.TurnReasonSilence},
		{"eager within grace", EagerStop(200 * time.Millisecond), DetectionInput{Text: "Book it.", TextFinal: true, SilenceFor: 199 * time.Millisecond}, core.TurnReasonNone},
		{"eager interim", EagerStop(0), DetectionInput{Text: "Book it.", SilenceFor: time.Second}, core.TurnReasonNone},
		{"eager no sentence end", EagerStop(0), DetectionInput{Text: "Book it and", TextFinal: true, SilenceFor: time.Second}, core.TurnReasonNone},
		{"eager quoted cjk", EagerStop(0), DetectionInput{Text: "好的。」", TextFinal: true}, core.TurnReasonSilence},
		{"external none", ExternalStop(), DetectionInput{SilenceFor: time.Hour}, core.TurnReasonNone},
		{"external signal", ExternalStop(), DetectionInput{ExternalStop: true}, core.TurnReasonSilence},
		{"analyzer default fallback", AnalyzerStop(incomplete, 0), DetectionInput{SilenceFor: time.Second, SilenceAfter: time.Second}, core.TurnReasonSilence},
	}
	for _, c := range cases {
		d, ok := c.s.Detect(c.in)
		if (c.want != core.TurnReasonNone) != ok || (ok && d.Reason != c.want) {
			t.Errorf("%s: got %+v ok=%v want %v", c.name, d, ok, c.want)
		}
	}
	if d, _ := AnalyzerStop(an, 0).Detect(DetectionInput{}); !d.HasConfidence || d.Confidence != 0.9 {
		t.Fatalf("analyzer confidence not carried: %+v", d)
	}
}

func TestStopChainFirstDecisiveWins(t *testing.T) {
	chain := StopChain(nil, ExternalStop(), EagerStop(0), SpeechTimeout(0))
	d, ok := chain.Detect(DetectionInput{Text: "ok.", TextFinal: true, SilenceFor: 10, SilenceAfter: 10})
	if !ok || d.Reason != core.TurnReasonSilence {
		t.Fatal("chain should stop")
	}
	if _, ok := StopChain().Detect(DetectionInput{SilenceFor: time.Hour, SilenceAfter: 1}); ok {
		t.Fatal("empty chain never stops")
	}
	an := &fakeAnalyzer{res: AnalyzerResult{Complete: true, Confidence: 1}}
	StopChain(SpeechTimeout(0), AnalyzerStop(an, 0)).Detect(DetectionInput{SilenceFor: 5, SilenceAfter: 5})
	if an.calls != 0 {
		t.Fatal("later strategy evaluated after an earlier one decided")
	}
}

// openTurn starts a turn and ends its speech; the silence clock starts at t0+300ms.
func openTurn(t *testing.T, m *Manager) {
	t.Helper()
	for _, at := range []time.Duration{0, 250 * time.Millisecond} {
		if _, err := m.Observe(observation(t0.Add(at), "t1", 1, true)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Observe(observation(t0.Add(300*time.Millisecond), "t1", 1, false)); err != nil {
		t.Fatal(err)
	}
}

func completed(frames []core.Frame) *core.TurnEventFrame {
	for _, e := range eventFrames(frames) {
		if e.Kind == core.TurnEventCompleted {
			return e
		}
	}
	return nil
}

func TestManagerAnalyzerIncompleteWaitsForFallback(t *testing.T) {
	an := &fakeAnalyzer{}
	m := manager(t, AnalyzerStop(an, 3*time.Second))
	openTurn(t, m)
	// Silence is 2.9s: well past SilenceTimeout (1s) but the analyzer holds the turn.
	if f, _ := m.Advance(t0.Add(3200 * time.Millisecond)); completed(f) != nil {
		t.Fatal("completed while analyzer said incomplete")
	}
	if an.calls == 0 {
		t.Fatal("analyzer not consulted")
	}
	f, _ := m.Advance(t0.Add(3300 * time.Millisecond))
	if e := completed(f); e == nil || e.Reason != core.TurnReasonSilence {
		t.Fatalf("fallback: %+v", eventFrames(f))
	}
}

func TestManagerAnalyzerComplete(t *testing.T) {
	m := manager(t, AnalyzerStop(&fakeAnalyzer{res: AnalyzerResult{Complete: true, Confidence: 0.8}}, 3*time.Second))
	openTurn(t, m)
	f, _ := m.Advance(t0.Add(400 * time.Millisecond))
	if e := completed(f); e == nil || e.Reason != core.TurnReasonSemantic || !e.HasConfidence || e.Confidence != 0.8 {
		t.Fatalf("got %+v", eventFrames(f))
	}
}

func TestManagerEagerGraceRetraction(t *testing.T) {
	m := manager(t, StopChain(EagerStop(200*time.Millisecond), SpeechTimeout(0)))
	openTurn(t, m)
	obs := observation(t0.Add(350*time.Millisecond), "t1", 1, false)
	obs.Transcript, obs.TranscriptFinal = "Book a table.", true
	m.Observe(obs)
	// User resumes inside the grace window: nothing was emitted, silence resets.
	if f, _ := m.Observe(observation(t0.Add(450*time.Millisecond), "t1", 1, true)); completed(f) != nil {
		t.Fatal("completed inside grace window")
	}
	m.Observe(observation(t0.Add(600*time.Millisecond), "t1", 1, false))
	// Interim extension replaces the final: no early end even after the grace.
	obs = observation(t0.Add(650*time.Millisecond), "t1", 1, false)
	obs.Transcript = "Book a table for"
	m.Observe(obs)
	if f, _ := m.Advance(t0.Add(900 * time.Millisecond)); completed(f) != nil {
		t.Fatal("early end on a non-final transcript")
	}
	obs = observation(t0.Add(950*time.Millisecond), "t1", 1, false)
	obs.Transcript, obs.TranscriptFinal = "Book a table for two.", true
	f, _ := m.Observe(obs) // 350ms of silence: past grace, far below the 1s timeout
	if e := completed(f); e == nil {
		t.Fatal("eager end did not fire after grace")
	}
}

func TestManagerExternalStop(t *testing.T) {
	m := manager(t, StopChain(ExternalStop()))
	openTurn(t, m)
	if f, _ := m.Advance(t0.Add(5 * time.Second)); completed(f) != nil {
		t.Fatal("external strategy ended the turn on silence")
	}
	obs := observation(t0.Add(6*time.Second), "t1", 1, false)
	obs.ExternalStop = true
	f, _ := m.Observe(obs)
	if completed(f) == nil {
		t.Fatal("external signal did not end the turn")
	}
	// A signal with no open turn is not latched into the next one.
	obs = observation(t0.Add(7*time.Second), "t1", 1, false)
	obs.ExternalStop = true
	m.Observe(obs)
	openNext := func(at time.Duration) { m.Observe(observation(t0.Add(at), "t2", 1, true)) }
	openNext(8 * time.Second)
	openNext(8300 * time.Millisecond)
	if f, _ := m.Advance(t0.Add(9 * time.Second)); completed(f) != nil {
		t.Fatal("stale external signal ended the next turn")
	}
}

func TestManagerExternalStopWhileSpeaking(t *testing.T) {
	m := manager(t, ExternalStop())
	m.Observe(observation(t0, "t1", 1, true))
	m.Observe(observation(t0.Add(250*time.Millisecond), "t1", 1, true))
	obs := observation(t0.Add(400*time.Millisecond), "t1", 1, true)
	obs.ExternalStop = true
	if f, _ := m.Observe(obs); completed(f) == nil {
		t.Fatal("external stop must end the turn even while VAD still speaks")
	}
}

func TestProcessorEagerStopThroughSession(t *testing.T) {
	cfg := DefaultConfig()
	h := newStopHarness(t, cfg, StopChain(EagerStop(100*time.Millisecond), SpeechTimeout(0)))
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.settle()
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "what time is it?", Final: true}, core.Downstream, 1, 1)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	start := h.clk.Now()
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("no completion")
	}
	if got := h.clk.Now().Sub(start); got >= cfg.SilenceTimeout {
		t.Fatalf("eager end took %v, not earlier than the %v silence timeout", got, cfg.SilenceTimeout)
	}
	if err := h.sess.Close(); err != nil || h.sess.LeakedTasks() != 0 {
		t.Fatalf("close %v leaked %d", err, h.sess.LeakedTasks())
	}
}

func newPTTHarness(t *testing.T) *harness {
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{ExternalStart()}
	return newStopHarness(t, cfg, StopChain(ExternalStop()))
}

func (h *harness) ptt(pressed bool) {
	_ = h.sess.Post(&core.PushToTalkFrame{Pressed: pressed})
	h.settle()
}

func TestPushToTalkIgnoresSpeechWithoutPress(t *testing.T) {
	h := newPTTHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) > 0 }) {
		t.Fatal("voice detection alone started a push-to-talk turn")
	}
}

func TestPushToTalkPressSpeakRelease(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("press + speech did not start a turn")
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	if h.run(3*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("a pause ended a held push-to-talk turn")
	}
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1}) // resumes while held: same turn
	h.settle()
	h.ptt(false)
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("release did not end the turn")
	}
	if n := len(h.events(core.TurnEventStarted)); n != 1 {
		t.Fatalf("%d turns started, want 1", n)
	}
	if err := h.sess.Close(); err != nil || h.sess.LeakedTasks() != 0 {
		t.Fatalf("close %v leaked %d", err, h.sess.LeakedTasks())
	}
}

func TestPushToTalkPressDuringSpeechStartsTurn(t *testing.T) {
	h := newPTTHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return false })
	h.ptt(true)
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("pressing while already speaking did not start the turn")
	}
}

func TestPushToTalkPressWithoutSpeechDoesNotLinger(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(true)
	h.ptt(false)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) > 0 }) {
		t.Fatal("an earlier press-and-release started a later unpressed turn")
	}
}

func TestPushToTalkReleaseWithNoTurnIsIgnored(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(false) // no open turn
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	if h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("a stale release ended the next turn")
	}
}

func TestPushToTalkBargesIn(t *testing.T) {
	h := newPTTHarness(t)
	h.botSpeaks()
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if !h.run(time.Second, func() bool { return h.asst.InterruptionCount.Load() == 1 }) {
		t.Fatal("press + speech while the bot spoke did not barge in")
	}
}

func newStopHarness(t *testing.T, cfg Config, det TurnDetector) *harness {
	t.Helper()
	h := &harness{t: t, clk: &fakeClock{now: time.Unix(1000, 0)}, stt: &fakeSTT{}, sink: &core.PassthroughProcessor{}}
	proc, err := NewProcessor(cfg, ProcessorOptions{Clock: h.clk, Detector: det})
	if err != nil {
		t.Fatal(err)
	}
	h.proc = proc
	acfg := core.DefaultAssistantContextConfig()
	acfg.BargeInOnTurnEvent = true
	h.asst = core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000}), acfg)
	pipe, err := core.NewPipeline(&h.stt.PassthroughProcessor, proc, h.asst, h.sink)
	if err != nil {
		t.Fatal(err)
	}
	if h.sess, err = core.NewSession(1, pipe, core.DefaultResourceBudget()); err != nil {
		t.Fatal(err)
	}
	if err := h.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.sess.Close() })
	return h
}

func (h *harness) candidates() (out []*core.TranscriptCandidateFrame) {
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			out = append(out, c)
		}
	}
	return out
}

// Speech that ended before the press is not part of the pressed turn, and a
// press with no speech does not start (or barge in) on its own.
func TestPushToTalkDropsUnpressedSpeechBeforePress(t *testing.T) {
	h := newPTTHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.settle()
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "side talk to a colleague", Final: true})
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	h.run(400*time.Millisecond, func() bool { return false })
	h.ptt(true)
	if h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) > 0 }) {
		t.Fatal("a press with no speech started a turn")
	}
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.settle()
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "what is the weather", Final: true})
	h.settle()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	h.settle()
	h.ptt(false)
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	c := h.candidates()
	if len(c) == 0 || c[len(c)-1].Text != "what is the weather" {
		t.Fatalf("pressed turn text = %+v, want only \"what is the weather\"", c)
	}
}

// A turn ended by release keeps its late final even when the user speaks
// again (unpressed) before that final arrives.
func TestPushToTalkReleasedTurnKeepsLateFinal(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	h.ptt(false)
	if !h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("release did not end the turn")
	}
	done := h.events(core.TurnEventCompleted)[0].Turn
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1}) // unpressed
	h.settle()
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "book a table for two", Final: true}, core.Downstream, 1, 1)
	h.settle()
	delivered := false
	for _, c := range h.candidates() {
		if c.Text == "book a table for two" {
			if c.Turn != done || !c.Final {
				t.Fatalf("late final delivered as %s final=%t, want the released turn %s, final", c.Turn, c.Final, done)
			}
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("late final of released turn %s was dropped; candidates=%+v", done, h.candidates())
	}
}

// Unpressed speech, a short pause, then a press during a second unpressed
// segment: only the segment being spoken at the press belongs to the turn.
func TestPushToTalkPressMidSecondSegmentDropsEarlierSegment(t *testing.T) {
	h := newPTTHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.settle()
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "side talk", Final: true})
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	h.run(300*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.settle()
	h.ptt(true)
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("press during speech did not start the turn")
	}
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "what is the weather", Final: true})
	h.settle()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	h.settle()
	h.ptt(false)
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	c := h.candidates()
	if len(c) == 0 || c[len(c)-1].Text != "what is the weather" {
		t.Fatalf("pressed turn text = %q, want only \"what is the weather\"", c[len(c)-1].Text)
	}
}

// Released mid-speech: the turn ends at the VAD stop, when the STT stream
// closes, so the final gets its normal window.
func TestPushToTalkReleaseWhileSpeakingEndsAtVADStop(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.ptt(false)
	if h.run(500*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("turn ended before the VAD stop")
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	if !h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("VAD stop after a release did not end the turn")
	}
}

// A release whose VAD stop never comes still ends the turn shortly after.
func TestPushToTalkReleaseWithoutVADStopIsCapped(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.ptt(false)
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("a release without a VAD stop never ended the turn")
	}
}
