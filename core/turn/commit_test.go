package turn

import (
	"testing"
	"time"

	"govoice/core"
	tr "govoice/core/transcript"
)

// newCommitHarness is a push-to-talk harness with the real transcript
// finalizer after the turn processor, so tests see committed transcripts.
func newCommitHarness(t *testing.T) *harness {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{ExternalStart()}
	return newCommitHarnessWith(t, cfg, StopChain(ExternalStop()))
}

// newCommitHarnessWith is the finalizer-backed harness for any start config and stop detector.
func newCommitHarnessWith(t *testing.T, cfg Config, det TurnDetector) *harness {
	t.Helper()
	h := &harness{t: t, clk: &fakeClock{now: time.Unix(1000, 0)}, stt: &fakeSTT{}, sink: &core.PassthroughProcessor{}}
	proc, err := NewProcessor(cfg, ProcessorOptions{Clock: h.clk, Detector: det})
	if err != nil {
		t.Fatal(err)
	}
	h.proc = proc
	fin, err := tr.NewProcessor(tr.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	fin.ManageTurns()
	acfg := core.DefaultAssistantContextConfig()
	acfg.BargeInOnTurnEvent = true
	h.asst = core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000}), acfg)
	pipe, err := core.NewPipeline(&h.stt.PassthroughProcessor, proc, fin, h.sink, h.asst)
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

func (h *harness) commits() (out []string) {
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.FinalTranscriptionFrame); ok && c.Committed {
			out = append(out, c.Text)
		}
	}
	return out
}

// waitCommit polls in real time: the finalizer's deadlines run on the wall clock.
func (h *harness) waitCommit(text string) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, c := range h.commits() {
			if c == text {
				return true
			}
		}
	}
	return false
}

// A released turn that spans two VAD segments, whose second segment's final
// arrives after new (unpressed) speech, still commits the whole utterance.
// (A single-segment turn's late final already carries the turn's own id.)
func TestCommitReleasedTurnLateFinal(t *testing.T) {
	h := newCommitHarness(t)
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "book a table", Final: true}, core.Downstream, 1, 1)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1}) // still held: same turn
	h.settle()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	h.settle()
	h.ptt(false)
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 5, Gen: 1}) // unpressed
	h.settle()
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "for two", Final: true}, core.Downstream, 1, 1)
	if !h.waitCommit("book a table for two") {
		t.Fatalf("released turn's late final not committed; commits=%q", h.commits())
	}
}

// Speech before the press is never committed; the pressed speech is.
func TestCommitOnlyPressedSpeech(t *testing.T) {
	h := newCommitHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.settle()
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "side talk", Final: true}, core.Downstream, 1, 1)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	h.run(300*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.settle()
	h.ptt(true)
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "what is the weather", Final: true}, core.Downstream, 1, 1)
	h.settle()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	h.settle()
	h.ptt(false)
	if !h.waitCommit("what is the weather") {
		t.Fatalf("pressed speech not committed; commits=%q", h.commits())
	}
	for _, c := range h.commits() {
		if c != "what is the weather" {
			t.Fatalf("committed %q, want only the pressed speech", c)
		}
	}
}
