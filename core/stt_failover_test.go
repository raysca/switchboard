package core_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/transcript"
)

var words = []string{"w0", "w1", "w2", "w3", "w4", "w5", "w6", "w7", "w8", "w9"}

type sttRig struct {
	t        *testing.T
	sess     *core.Session
	w        *core.STTFailover
	rec      *core.PassthroughProcessor
	events   chan core.DebugEvent
	mu       sync.Mutex
	switches []string
	dropped  atomic.Int64
	seq      uint64
	sent     []byte
}

func fakeSTT(name string, cfg core.FakeSTTConfig) *core.FakeSTTProcessor {
	cfg.Provider, cfg.Words = name, words
	return core.NewFakeSTTProcessor(cfg)
}

// newSTTRig builds [failover, (transcript finalizer), recorder].
func newSTTRig(t *testing.T, cfg core.STTFailoverConfig, finalizer bool) *sttRig {
	t.Helper()
	r := &sttRig{t: t, rec: &core.PassthroughProcessor{}, events: make(chan core.DebugEvent, 16)}
	cfg.OnFailover = func(from, to, class string) {
		r.mu.Lock()
		r.switches = append(r.switches, from+">"+to+":"+class)
		r.mu.Unlock()
	}
	cfg.OnDropped = func(n int) { r.dropped.Add(int64(n)) }
	if cfg.Classify == nil {
		cfg.Classify = func(error) string { return "server_error" }
	}
	var err error
	r.w, err = core.NewSTTFailover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	procs := []core.Processor{r.w}
	if finalizer {
		tp, err := transcript.NewProcessor(transcript.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		procs = append(procs, tp)
	}
	pipe, err := core.NewPipeline(append(procs, r.rec)...)
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(901, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.sess.Subscribe(16, func(e core.DebugEvent) {
		if e.Name == core.DebugSTTFailover {
			r.events <- e
		}
	}); err != nil {
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

func (r *sttRig) switched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.switches...)
}

// audio posts one frame carrying word w; the frame's remaining bytes are unique.
func (r *sttRig) audio(w int) {
	r.t.Helper()
	r.seq++
	b := bytes.Repeat([]byte{byte(r.seq)}, 64)
	b[0] = byte(w)
	r.sent = append(r.sent, b...)
	if err := r.sess.Post(&core.InputAudioFrame{Samples: b, Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1, Seq: r.seq}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sttRig) start() {
	r.t.Helper()
	r.seq++
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{Seq: r.seq, Gen: r.sess.CurrentGeneration()}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sttRig) stop() {
	r.t.Helper()
	if err := r.sess.Post(&core.UserStoppedSpeakingFrame{Gen: r.sess.CurrentGeneration()}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sttRig) waitFor(cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("condition not met; frames=%d switches=%v", r.rec.Count(), r.switched())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *sttRig) candidates() (out []*core.TranscriptCandidateFrame) {
	for _, f := range r.rec.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			out = append(out, c)
		}
	}
	return out
}

func (r *sttRig) errors() (n int) {
	for _, f := range r.rec.GetFrames() {
		if _, ok := f.(*core.ErrorFrame); ok {
			n++
		}
	}
	return n
}

func (r *sttRig) finals() (out []*core.TranscriptCandidateFrame) {
	for _, c := range r.candidates() {
		if c.Final {
			out = append(out, c)
		}
	}
	return out
}

func (r *sttRig) waitFinal(n int) {
	r.t.Helper()
	r.waitFor(func() bool { return len(r.finals()) >= n })
}

func (r *sttRig) waitActive(name string) {
	r.t.Helper()
	r.waitFor(func() bool { return r.w.Active() == name })
}

func cands(a, b core.STTCandidate) []core.STTCandidate { return []core.STTCandidate{a, b} }

const allWords = "w0 w1 w2 w3 w4"

func TestSTTFailoverMidUtteranceReplaysAndTranscribesOnce(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{FailAfter: 3})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	r.audio(0) // pre-speech padding: VAD reports the start late
	r.start()
	for _, w := range []int{1, 2, 3} {
		r.audio(w)
	}
	r.waitActive("b")
	r.audio(4)
	r.stop()
	r.waitFinal(1)

	if got := fallback.Received(); !bytes.Equal(got, r.sent) {
		t.Fatalf("fallback audio is not exactly the utterance once:\n got %d bytes\nwant %d bytes", len(got), len(r.sent))
	}
	finals := r.finals()
	if len(finals) != 1 || finals[0].Text != allWords || finals[0].Provider != "b" {
		t.Fatalf("finals = %+v", finals)
	}
	if r.errors() != 0 {
		t.Fatal("the failed candidate's error leaked downstream")
	}
	if got := r.switched(); len(got) != 1 || got[0] != "a>b:server_error" {
		t.Fatalf("switches = %v", got)
	}
	select {
	case ev := <-r.events:
		if a := ev.Attributes(); a["component"] != "a" || a["provider"] != "b" || a["reason"] != "server_error" {
			t.Fatalf("event attrs = %v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("no stt.failover debug event")
	}
}

func TestSTTFailoverStreamOpenFailureReplaysFromBuffer(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	r.audio(0)
	r.audio(1)
	r.start()
	r.waitFor(func() bool { return len(primary.Received()) == 2*64 })
	primary.Break(errors.New("dial: connection refused")) // the stream cannot be opened any more
	r.audio(2)
	r.audio(3)
	r.audio(4)
	r.stop()
	r.waitFinal(1)
	if got := fallback.Received(); !bytes.Equal(got, r.sent) {
		t.Fatalf("fallback did not get the buffered audio plus the rest: %d vs %d bytes", len(got), len(r.sent))
	}
	if f := r.finals(); len(f) != 1 || f[0].Text != allWords {
		t.Fatalf("finals = %+v", f)
	}
}

func TestSTTFailoverDropsAbandonedCandidateOutputAndInvalidation(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		primary := fakeSTT("a", core.FakeSTTConfig{FailAfter: 2, ZombieOnFail: true, InvalidateOnFail: invalidate})
		fallback := fakeSTT("b", core.FakeSTTConfig{})
		r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
		r.start()
		r.audio(0)
		r.audio(1)
		r.waitActive("b")
		r.audio(2)
		r.stop()
		r.waitFinal(1)
		time.Sleep(30 * time.Millisecond) // let any straggler land
		for _, c := range r.candidates() {
			if strings.Contains(c.Text, "ZOMBIE") {
				t.Fatalf("invalidate=%v: late output of the abandoned candidate got through: %+v", invalidate, c)
			}
		}
		for _, f := range r.rec.GetFrames() {
			switch f.(type) {
			case *core.ErrorFrame, *core.TranscriptInvalidationFrame:
				t.Fatalf("invalidate=%v: failure frame leaked: %T", invalidate, f)
			}
		}
		if f := r.finals(); len(f) != 1 || f[0].Text != "w0 w1 w2" {
			t.Fatalf("finals = %+v", f)
		}
	}
}

func TestSTTFailoverCommittedFinalOnce(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{FailAfter: 3})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, true)
	r.start()
	for _, w := range []int{0, 1, 2} {
		r.audio(w)
	}
	r.waitActive("b")
	r.audio(3)
	r.audio(4)
	r.stop()
	r.waitFor(func() bool {
		for _, f := range r.rec.GetFrames() {
			if ft, ok := f.(*core.FinalTranscriptionFrame); ok && ft.Committed {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, f := range r.rec.GetFrames() {
		if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
			n++
			if ft.Text != allWords {
				t.Fatalf("committed %q, want the whole utterance", ft.Text)
			}
		}
	}
	if n != 1 {
		t.Fatalf("committed %d finals, want 1", n)
	}
}

func TestSTTFailoverBufferIsBoundedAndOldestDropped(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{BufferBytes: 5 * 64, Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	for i := 0; i < 30; i++ {
		r.audio(i % 10)
	}
	r.waitFor(func() bool { return len(primary.Received()) == 30*64 })
	if got := r.w.Buffered(); got > 5*64 {
		t.Fatalf("buffer holds %d bytes, cap %d", got, 5*64)
	}
	if r.w.Dropped() != 0 || r.dropped.Load() != 0 {
		t.Fatalf("idle audio ageing out counted as loss: %d / %d", r.w.Dropped(), r.dropped.Load())
	}
	r.start() // a 10-frame utterance in a 5-frame buffer: its 5 pre-roll frames (all buffered audio fits in the 1 s pre-roll) plus 10 leave 10 out
	for i := 0; i < 10; i++ {
		r.audio(i)
	}
	r.waitFor(func() bool { return len(primary.Received()) == 40*64 })
	if r.w.Dropped() != 10*64 || r.dropped.Load() != 10*64 {
		t.Fatalf("utterance dropped = %d / counter %d, want %d", r.w.Dropped(), r.dropped.Load(), 10*64)
	}
}

func TestSTTFailoverLongUtteranceKeepsFinalizedPrefix(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{FinalEvery: 4, FailAfter: 6})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{BufferBytes: 3 * 64, Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	r.start()
	for w := 0; w < 6; w++ {
		r.audio(w)
	}
	r.waitActive("b")
	r.audio(6)
	r.audio(7)
	r.stop()
	r.waitFor(func() bool {
		c := r.candidates()
		return len(c) > 0 && c[len(c)-1].Provider == "b" && c[len(c)-1].EndOfUtterance
	})
	c := r.candidates()
	if last := c[len(c)-1]; last.Text != "w0 w1 w2 w3 w4 w5 w6 w7" {
		t.Fatalf("final = %q: the audio that fell out of the buffer lost its finalized text or got duplicated", last.Text)
	}
}

func TestSTTFailoverReturnsToPrimaryPerUtterance(t *testing.T) {
	var primaryOpen atomic.Bool
	primary := fakeSTT("a", core.FakeSTTConfig{})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(
		core.STTCandidate{Provider: "a", Proc: primary, Open: primaryOpen.Load},
		core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	utter := func(ws ...int) {
		r.audio(ws[0])
		r.start()
		for _, w := range ws[1:] {
			r.audio(w)
		}
		r.stop()
	}
	utter(0, 1)
	r.waitFinal(1)
	primaryOpen.Store(true) // breaker opens between utterances
	utter(2, 3)
	r.waitFinal(2)
	primaryOpen.Store(false) // cooldown elapsed, breaker allows again
	utter(4, 5)
	r.waitFinal(3)
	f := r.finals()
	if f[0].Provider != "a" || f[1].Provider != "b" || f[2].Provider != "a" {
		t.Fatalf("providers per utterance = %s %s %s, want a b a", f[0].Provider, f[1].Provider, f[2].Provider)
	}
	if f[1].Text != "w2 w3" || f[2].Text != "w4 w5" {
		t.Fatalf("pre-roll audio was lost at the switch: %q %q", f[1].Text, f[2].Text)
	}
	if got := r.switched(); len(got) != 2 || got[0] != "a>b:circuit_open" || got[1] != "b>a:recovered" {
		t.Fatalf("switches = %v", got)
	}
}

func TestSTTFailoverFailureBetweenUtterancesMovesLazily(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	primary.Break(errors.New("503"))
	r.audio(0) // idle: fails, the next candidate takes over and gets this frame
	r.start()
	r.audio(1)
	r.stop()
	r.waitFinal(1)
	if f := r.finals(); f[0].Provider != "b" || f[0].Text != "w0 w1" {
		t.Fatalf("final = %+v", f[0])
	}
	if !bytes.Equal(fallback.Received(), r.sent) {
		t.Fatal("fallback missed audio")
	}
}

func TestSTTFailoverBargeInRebindsAndSurvivesFailover(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{FailAfter: 3})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	g1 := r.sess.CurrentGeneration()
	r.start()
	r.audio(0)
	r.waitFor(func() bool { return len(primary.Received()) == 64 })
	g2 := r.sess.IncrementGeneration() // barge-in: the turn manager (after STT) rebinds the open stream, upstream
	if err := r.sess.Reenter(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: core.TurnID("turn-" + strconv.FormatUint(g1, 10) + "-1"), Gen: core.Generation(g2)}, core.Upstream, 0, g2); err != nil {
		t.Fatal(err)
	}
	r.audio(1)
	r.audio(2)
	r.waitActive("b")
	r.audio(3)
	r.stop()
	r.waitFinal(1)
	f := r.finals()[0]
	if f.Gen != core.Generation(g2) || f.Text != "w0 w1 w2 w3" {
		t.Fatalf("after failover the replayed rebinding was lost: gen=%d text=%q", f.Gen, f.Text)
	}
}

func TestSTTFailoverEveryCandidateFailingSurfacesOneError(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{FailAfter: 1})
	fallback := fakeSTT("b", core.FakeSTTConfig{FailAfter: 1})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	r.start()
	r.audio(0)
	r.waitFor(func() bool { return r.errors() >= 1 })
	time.Sleep(30 * time.Millisecond)
	if n := r.errors(); n != 1 {
		t.Fatalf("errors = %d, want exactly one", n)
	}
}

func TestSTTFailoverDisallowedClassPassesThrough(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{FailAfter: 1})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{
		Classify: func(error) string { return "client_error" }, Allow: func(c string) bool { return c != "client_error" },
		Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	r.start()
	r.audio(0)
	r.waitFor(func() bool { return r.errors() == 1 })
	if len(r.switched()) != 0 || r.w.Active() != "a" {
		t.Fatalf("failed over on a disallowed class: %v", r.switched())
	}
}

type nopEmitter struct{}

func (nopEmitter) Push(core.Frame) error                          { return nil }
func (nopEmitter) PushDirection(core.Frame, core.Direction) error { return nil }

type nopProc struct{}

func (nopProc) ProcessFrame(context.Context, core.Frame, core.Direction, core.Emitter) error {
	return nil
}

// BenchmarkSTTFailoverAudioFrame is the per-20 ms-frame cost of the wrapper in a
// long utterance (buffer full, evicting every frame).
func BenchmarkSTTFailoverAudioFrame(b *testing.B) {
	w, err := core.NewSTTFailover(core.STTFailoverConfig{Candidates: []core.STTCandidate{{Provider: "a", Proc: nopProc{}}, {Provider: "b", Proc: nopProc{}}}})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	_ = w.ProcessFrame(ctx, &core.UserStartedSpeakingFrame{Seq: 1, Gen: 1}, core.Downstream, nopEmitter{})
	f := &core.InputAudioFrame{Samples: make([]byte, 640), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1}
	for i := 0; i < 2000; i++ { // fill past the 10 s cap
		_ = w.ProcessFrame(ctx, f, core.Downstream, nopEmitter{})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.ProcessFrame(ctx, f, core.Downstream, nopEmitter{})
	}
}

// The failed candidate already emitted a provider final for part of the
// utterance: the real finalizer must still commit the whole utterance, once.
func TestSTTFailoverEarlierProviderFinalCommitsWholeUtteranceOnce(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{FinalEvery: 2, FailAfter: 3})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, true)
	r.start()
	r.audio(0)
	r.audio(1) // primary final "w0 w1"
	r.waitFor(func() bool { return len(primary.Received()) == 2*64 })
	r.audio(2) // primary dies
	r.waitActive("b")
	r.audio(3)
	r.audio(4)
	r.stop()
	committed := func() (out []string) {
		for _, f := range r.rec.GetFrames() {
			if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
				out = append(out, ft.Text)
			}
		}
		return out
	}
	r.waitFor(func() bool { return len(committed()) > 0 })
	time.Sleep(300 * time.Millisecond)
	if got := committed(); len(got) != 1 || got[0] != allWords {
		t.Fatalf("committed %q, want [%q]", got, allWords)
	}
}

// Deepgram NET-0001: the primary's idle stream closed quietly after an
// utterance; the next audio cannot reopen it. The fallback takes over on that
// frame and the next utterance keeps its first words.
func TestSTTFailoverIdleCloseThenSpeechKeepsFirstWords(t *testing.T) {
	primary := fakeSTT("a", core.FakeSTTConfig{})
	fallback := fakeSTT("b", core.FakeSTTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	r.start()
	r.audio(0)
	r.stop()
	r.waitFinal(1)
	primary.Break(errors.New("dial: 503")) // idle close; the reopen fails
	r.sent = nil
	r.audio(1) // onset, before VAD reports speech
	r.audio(2)
	r.start()
	r.audio(3)
	r.stop()
	r.waitFinal(2)
	if f := r.finals()[1]; f.Provider != "b" || f.Text != "w1 w2 w3" {
		t.Fatalf("final = %q from %s, want the whole utterance from b", f.Text, f.Provider)
	}
	if !bytes.Equal(fallback.Received(), r.sent) {
		t.Fatal("fallback missed or duplicated audio")
	}
}
