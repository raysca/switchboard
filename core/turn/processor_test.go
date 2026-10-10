package turn

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	when time.Time
	c    chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool          { return true }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{when: c.now.Add(d), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.timers[:0]
	for _, t := range c.timers {
		if !t.when.After(c.now) {
			t.c <- c.now
		} else {
			keep = append(keep, t)
		}
	}
	c.timers = keep
}

// stt stands in for the STT provider: it sees raw VAD/audio and forwards.
type fakeSTT struct {
	core.PassthroughProcessor
}

type harness struct {
	t     *testing.T
	clk   *fakeClock
	sess  *core.Session
	stt   *fakeSTT
	sink  *core.PassthroughProcessor
	asst  *core.AssistantContextProcessor
	proc  *Processor
	turns int
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{t: t, clk: &fakeClock{now: time.Unix(1000, 0)}, stt: &fakeSTT{}, sink: &core.PassthroughProcessor{}}
	proc, err := NewProcessor(cfg, ProcessorOptions{Clock: h.clk})
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
	h.sess, err = core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.sess.Close() })
	return h
}

// botSpeaks makes the assistant context hold an active reply and tells the
// processor the bot is audible.
func (h *harness) botSpeaks() {
	_ = h.sess.Post(&core.UserMessageFrame{Turn: "bot-1", Gen: 1, Timestamp: time.Now()})
	_ = h.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStarted, Turn: "bot-1", Gen: 1}, core.Upstream, h.sess.TailIndex(), 0)
	h.settle()
}

// settle lets the run loop drain what was posted.
func (h *harness) settle() { time.Sleep(30 * time.Millisecond) }

// run advances fake time in 20ms ticks until cond holds or total elapses.
func (h *harness) run(total time.Duration, cond func() bool) bool {
	for el := time.Duration(0); el < total; el += 20 * time.Millisecond {
		if cond() {
			return true
		}
		h.clk.Advance(20 * time.Millisecond)
		time.Sleep(3 * time.Millisecond)
	}
	return cond()
}

func (h *harness) events(kind core.TurnEventKind) []*core.TurnEventFrame {
	var out []*core.TurnEventFrame
	for _, f := range h.sink.GetFrames() {
		if ev, ok := f.(*core.TurnEventFrame); ok && ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (h *harness) count(typ core.FrameType) (n int) {
	for _, f := range h.stt.GetFrames() {
		if f.Type() == typ {
			n++
		}
	}
	return
}

func (h *harness) audio() { _ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 320)}) }

func TestProcessorVADBargeInAndSilenceEnd(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.botSpeaks()

	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.settle()
	if h.asst.InterruptionCount.Load() != 0 || len(h.events(core.TurnEventStarted)) != 0 {
		t.Fatal("raw VAD start must not barge in; the manager debounce decides")
	}
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("no TurnEventStarted")
	}
	h.settle()
	if h.asst.InterruptionCount.Load() != 1 || h.sess.CurrentGeneration() != 2 {
		t.Fatalf("barge-in: count=%d gen=%d", h.asst.InterruptionCount.Load(), h.sess.CurrentGeneration())
	}
	if got := h.count(core.FrameInterruption); got != 0 {
		t.Fatalf("barge-in InterruptionFrame reached STT %d times; it would cancel the open stream", got)
	}
	if got := h.events(core.TurnEventStarted)[0]; got.Turn != "turn-1-1" {
		t.Fatalf("turn id %q", got.Turn)
	}
	// STT is told to move its open stream to the new generation...
	var rebind *core.TurnEventFrame
	for _, f := range h.stt.GetFrames() {
		if ev, ok := f.(*core.TurnEventFrame); ok && ev.Kind == core.TurnEventStarted {
			rebind = ev
		}
	}
	if rebind == nil || rebind.Turn != "turn-1-1" || rebind.Gen != 2 {
		t.Fatalf("STT rebind %+v", rebind)
	}
	// ...so the rest of the interrupting utterance survives the fence (the
	// old-generation stream's output would be discarded).
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "stale"}, core.Downstream, 1, 1)
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 2, Text: "stop and tell me the time", Final: true}, core.Downstream, 1, 2)
	h.settle()
	var texts []string
	flushed := false
	for _, f := range h.sink.GetFrames() {
		switch v := f.(type) {
		case *core.TranscriptCandidateFrame:
			texts = append(texts, v.Text+"@"+string(v.Turn))
		case *core.FlushFrame:
			flushed = true
		}
	}
	if len(texts) != 1 || texts[0] != "stop and tell me the time@turn-1-1" {
		t.Fatalf("candidates after barge-in: %v", texts)
	}
	if !flushed {
		t.Fatal("barge-in did not flush bot output downstream")
	}

	// Silence fallback ends the turn after the raw VAD stop.
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 2})
	h.settle()
	if len(h.events(core.TurnEventCompleted)) != 0 {
		t.Fatal("turn completed before the silence timeout")
	}
	if !h.run(3*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("no TurnEventCompleted")
	}
	if h.events(core.TurnEventCompleted)[0].Reason != core.TurnReasonSilence {
		t.Fatal("expected silence reason")
	}
	if err := h.sess.Close(); err != nil || h.sess.LeakedTasks() != 0 {
		t.Fatalf("teardown err=%v leaked=%d", err, h.sess.LeakedTasks())
	}
}

func TestProcessorMinWordsBackchannelThenFullPhrase(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{MinWordsStart(3, true)}
	h := newHarness(t, cfg)
	h.botSpeaks()

	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.audio()
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "uh-huh"})
	h.settle()
	h.run(500*time.Millisecond, func() bool { return false })
	if h.asst.InterruptionCount.Load() != 0 || len(h.events(core.TurnEventStarted)) != 0 {
		t.Fatal("backchannel must not barge in")
	}
	h.audio() // STT still receives audio although the turn has not started
	h.settle()
	if h.count(core.FrameInputAudio) != 2 {
		t.Fatalf("STT audio frames = %d, want 2", h.count(core.FrameInputAudio))
	}

	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "uh-huh wait stop that"})
	h.settle()
	h.run(200*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.settle()
	if h.asst.InterruptionCount.Load() != 1 {
		t.Fatalf("full phrase must barge in, count=%d", h.asst.InterruptionCount.Load())
	}
	// The finalizer sees one candidate re-keyed to the managed turn and the
	// current generation.
	var last *core.TranscriptCandidateFrame
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			last = c
		}
	}
	if last == nil || last.Text != "uh-huh wait stop that" || last.Turn != "turn-1-1" {
		t.Fatalf("candidate %+v", last)
	}
}

func TestProcessorUnstartedTurnIsInvalidated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{MinWordsStart(3, true)}
	h := newHarness(t, cfg)
	h.botSpeaks()
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "mm-hmm"})
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	if !h.run(3*time.Second, func() bool {
		for _, f := range h.sink.GetFrames() {
			if f.Type() == core.FrameTranscriptInvalidation {
				return true
			}
		}
		return false
	}) {
		t.Fatal("backchannel transcript was never retired")
	}
	if len(h.events(core.TurnEventStarted)) != 0 {
		t.Fatal("turn started")
	}
}

// A STT without interim results delivers its final 1-2 s after the VAD stop. A min_words
// turn must still be admitted by that late final instead of being dropped as "never admitted".
func TestProcessorLateFinalAdmitsMinWordsTurn(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{MinWordsStart(3, true)}
	h := newHarness(t, cfg)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.run(1500*time.Millisecond, func() bool { return false }) // past the 1 s expiry, no transcript yet
	for _, f := range h.sink.GetFrames() {
		if f.Type() == core.FrameTranscriptInvalidation {
			t.Fatal("turn dropped while its final transcript was still due")
		}
	}
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "what is two plus two", Final: true})
	if !h.run(3*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatalf("late final transcript lost: started=%d completed=%d", len(h.events(core.TurnEventStarted)), len(h.events(core.TurnEventCompleted)))
	}
}

func TestProcessorResumedSpeechJoinsSegments(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "I want", Final: true})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.run(200*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "a pizza", Final: true})
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	if !h.run(3*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("no completion")
	}
	if n := len(h.events(core.TurnEventStarted)); n != 1 {
		t.Fatalf("resume split the turn: %d starts", n)
	}
	var last *core.TranscriptCandidateFrame
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			last = c
		}
	}
	if last == nil || last.Text != "I want a pizza" || last.Turn != "turn-1-1" {
		t.Fatalf("candidate %+v", last)
	}
}

// A queued tick is fenced if the generation advances before it runs; the
// driver must replace it rather than wait on it forever.
func TestProcessorTickSurvivesGenerationBump(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.settle()
	h.proc.tickGen.Store(h.sess.CurrentGeneration()) // a tick queued at gen 1 ...
	h.sess.IncrementGeneration()                     // ... then discarded by the fence
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("tick driver stalled after a generation bump")
	}
}

// The min_words warning hook fires once per call, only when the STT's first
// candidate is already final (no interim results).
func TestProcessorFinalWithoutInterimsHook(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstFinal bool
		want       int32
	}{{"final first", true, 1}, {"interim first", false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig())
			var n atomic.Int32
			h.proc.onNoInterims = func() { n.Add(1) }
			for seq := uint64(1); seq <= 3; seq += 2 {
				id := core.TurnID(fmt.Sprintf("turn-1-%d", seq))
				_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: seq, Gen: 1})
				_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: id, Gen: 1, Text: "hello there", Final: tc.firstFinal})
				_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: id, Gen: 1, Text: "hello there", Final: true})
				_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: seq + 1, Gen: 1})
			}
			h.run(200*time.Millisecond, func() bool { return false })
			if got := n.Load(); got != tc.want {
				t.Fatalf("hook fired %d times, want %d", got, tc.want)
			}
		})
	}
}

// A stop strategy can end a turn before a non-streaming STT's final arrives
// (Smart Turn "complete" at the VAD stop; chirp_2 answers ~1 s later). If the
// user resumes before that final, it must join the new turn, not be dropped
// (live: "I would like to know ... what the capital of France is" lost its first half).
func TestProcessorPendingFinalJoinsNextTurn(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("first turn never completed")
	}
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 2 })
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "I would like to know", Final: true}) // late
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "the capital of France", Final: true})
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	if !h.run(3*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 2 }) {
		t.Fatal("second turn never completed")
	}
	var last *core.TranscriptCandidateFrame
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			last = c
		}
	}
	if last == nil || last.Text != "I would like to know the capital of France" || !last.Final {
		t.Fatalf("candidate %+v", last)
	}
}

// An STT end-of-utterance inside a managed turn must not commit the turn: the manager owns
// the boundary. Before, the finalizer committed the first half 200 ms after its EOU final and
// the resumed half, joined to the same turn, was dropped as already committed (live xAI S4:
// "I would like to know" + 1 s pause + "what the capital of France is." lost the second half).
func TestProcessorResumedSpeechAfterEOUCommitsWholeTurn(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	proc, err := NewProcessor(DefaultConfig(), ProcessorOptions{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	fin, _ := coretranscript.NewProcessor(coretranscript.DefaultConfig())
	fin.ManageTurns()
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(&core.PassthroughProcessor{}, proc, fin, sink)
	sess, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := &harness{t: t, clk: clk, sess: sess, sink: sink}
	finals := func() (out []string) {
		for _, f := range sink.GetFrames() {
			if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
				out = append(out, ft.Text)
			}
		}
		return out
	}
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	_ = sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "I would like to know", Final: true, EndOfUtterance: true, Timestamp: time.Now()})
	h.run(200*time.Millisecond, func() bool { return false })
	time.Sleep(300 * time.Millisecond) // past the finalizer's 200 ms grace (real time)
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	_ = sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "what the capital of France is", Final: true, EndOfUtterance: true, Timestamp: time.Now()})
	_ = sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	h.run(3*time.Second, func() bool { return len(finals()) > 0 && len(h.events(core.TurnEventCompleted)) == 1 })
	time.Sleep(400 * time.Millisecond)
	if got := finals(); len(got) != 1 || got[0] != "I would like to know what the capital of France is" {
		t.Fatalf("committed %q, want the joined turn once (started %d completed %d)", got, len(h.events(core.TurnEventStarted)), len(h.events(core.TurnEventCompleted)))
	}
}

func TestProcessor_VADSilenceOnsetEliminatesDoubleTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 500 * time.Millisecond
	h := newHarness(t, cfg)

	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: h.sess.CurrentGeneration()})
	h.settle()
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("no TurnEventStarted")
	}

	// User speaks for 300ms
	h.clk.Advance(300 * time.Millisecond)
	h.audio()

	// VAD accumulates 500ms of silence chunks
	h.clk.Advance(500 * time.Millisecond)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{
		Seq:             2,
		Gen:             h.sess.CurrentGeneration(),
		SilenceDuration: 500 * time.Millisecond,
	})
	_ = h.sess.Post(&core.TranscriptCandidateFrame{
		Turn:  "turn-1-1",
		Gen:   core.Generation(h.sess.CurrentGeneration()),
		Text:  "hello there",
		Final: true,
	})
	h.settle()

	// The turn should complete promptly without having to wait another full 500ms
	h.run(150*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	if len(h.events(core.TurnEventCompleted)) != 1 {
		t.Fatalf("turn should complete promptly when VAD already confirmed silence, got %d completed events", len(h.events(core.TurnEventCompleted)))
	}
}

func TestProcessor_SilenceDurationClampedToTurnStart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SilenceTimeout = 500 * time.Millisecond
	h := newHarness(t, cfg)

	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: h.sess.CurrentGeneration()})
	h.settle()
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("no TurnEventStarted")
	}
	startEv := h.events(core.TurnEventStarted)[0]

	// User stopped speaking with an artificially huge SilenceDuration (e.g. 10 hours)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{
		Seq:             2,
		Gen:             h.sess.CurrentGeneration(),
		SilenceDuration: 10 * time.Hour,
	})
	_ = h.sess.Post(&core.TranscriptCandidateFrame{
		Turn:  "turn-1-1",
		Gen:   core.Generation(h.sess.CurrentGeneration()),
		Text:  "quick word",
		Final: true,
	})
	h.settle()

	// Advance clock past silence timeout
	h.run(600*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	if len(h.events(core.TurnEventCompleted)) != 1 {
		t.Fatalf("turn should complete, got %d events", len(h.events(core.TurnEventCompleted)))
	}
	stopEv := h.events(core.TurnEventCompleted)[0]
	if stopEv.At.Before(startEv.At) {
		t.Fatalf("stop event At (%v) cannot precede start event At (%v)", stopEv.At, startEv.At)
	}
}
