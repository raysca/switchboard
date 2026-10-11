package turn

import (
	"testing"
	"time"

	"govoice/core"
)

// newGatedHarness uses the production stop with a 200ms VAD stop and Google's p99.
func newGatedHarness(t *testing.T) *harness {
	return newStopHarness(t, DefaultConfig(), SpeechTimeout(SpeechTimeoutConfig{VADStop: 200 * time.Millisecond, STTP99: 1570 * time.Millisecond}))
}

func (h *harness) candidate(seg, text string, final bool) {
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: core.TurnID(seg), Gen: 1, Text: text, Final: final}, core.Downstream, 1, 1)
	h.settle()
}

// A final that arrives before the window closes: the turn ends at VADStop + 600ms from onset.
func TestGatedStopEndsAtResumeWindow(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "what time is it", true)
	stopAt := h.clk.Now()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("turn never ended")
	}
	if got := h.clk.Now().Sub(stopAt); got < 580*time.Millisecond || got > 700*time.Millisecond {
		t.Fatalf("ended %v after the VAD stop, want about 600ms (VADStop+600ms from onset)", got)
	}
}

// Interim text only: the turn waits for the final (or p99), not the resume window.
func TestGatedStopWaitsForLateFinal(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "book a table", false)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if h.run(time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("ended on interim text before the final or p99")
	}
	h.candidate("turn-1-1", "book a table for two", true)
	if !h.run(500*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("the late final did not end the turn")
	}
}

// No transcript at all: the turn ends at the 5s fallback, not at MaxTurnDuration.
func TestGatedStopNoTranscriptFallback(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if h.run(4*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("ended before the no-transcript fallback")
	}
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("no-transcript turn never ended")
	}
}

// Speech resumes 300ms after a VAD stop that followed a final: still one turn.
func TestGatedStopResumeInsideWindowIsOneTurn(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "I would like", true)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(100*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.settle()
	h.candidate("turn-1-3", "a coffee", true)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	if s, c := len(h.events(core.TurnEventStarted)), len(h.events(core.TurnEventCompleted)); s != 1 || c != 1 {
		t.Fatalf("started=%d completed=%d, want one turn", s, c)
	}
}
