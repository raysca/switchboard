package turn

import (
	"testing"
	"time"

	"govoice/core"
)

// A final for the first segment must not end the turn while a resumed segment has no final yet.
func TestGatedStopResumedSegmentWithoutTextHoldsTurn(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "I would like to book a table", true)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(100*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.run(400*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if h.run(700*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("ended at the resume window on the first segment's final; the resumed segment has no final yet")
	}
	h.candidate("turn-1-3", "for two at eight", true)
	if !h.run(500*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("the resumed segment's final did not end the turn")
	}
}

// A late final for an earlier segment after the last segment's final: every segment is final,
// so the turn ends at the window, not at the STT p99.
func TestGatedStopOutOfOrderFinalsEndAtWindow(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "I would like", false)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(100*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.settle()
	h.candidate("turn-1-3", "a coffee", true)
	h.candidate("turn-1-1", "I would like", true) // the earlier segment's final arrives late
	stopAt := h.clk.Now()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("turn never ended")
	}
	if got := h.clk.Now().Sub(stopAt); got > 700*time.Millisecond {
		t.Fatalf("ended %v after the VAD stop: waited for the STT p99 although every segment is final", got)
	}
}

// Through the real finalizer: the resumed segment's words are committed with the first's.
func TestCommitResumedSegmentKeepsAllWords(t *testing.T) {
	h := newCommitHarnessWith(t, DefaultConfig(), SpeechTimeout(SpeechTimeoutConfig{VADStop: 200 * time.Millisecond, STTP99: 1570 * time.Millisecond}))
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "I would like to book a table", Final: true}, core.Downstream, 1, 1)
	h.settle()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(100*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.run(400*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(1100*time.Millisecond, func() bool { return false })
	// The finalizer's grace runs on the wall clock: let it pass before the late final arrives.
	time.Sleep(500 * time.Millisecond)
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "for two at eight", Final: true}, core.Downstream, 1, 1)
	if !h.waitCommit("I would like to book a table for two at eight") {
		t.Fatalf("resumed words lost; commits=%q", h.commits())
	}
	time.Sleep(300 * time.Millisecond) // a second, partial commit would land by now
	if c := h.commits(); len(c) != 1 {
		t.Fatalf("commits=%q, want exactly one with all the words", c)
	}
}

// A rejected backchannel must stay rejected when a noise blip follows after the bot stops:
// re-observing old text must not re-run start strategies for a turn that never started.
func TestBackchannelThenNoiseBlipStartsNoTurn(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{MinWordsStart(3, true)}
	h := newHarness(t, cfg)
	h.botSpeaks()
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = h.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "uh-huh", Final: true})
	h.settle()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(200*time.Millisecond, func() bool { return false })
	_ = h.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStopped, Turn: "bot-1", Gen: 1}, core.Upstream, h.sess.TailIndex(), 0)
	h.settle()
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1}) // cough or breath: the STT emits nothing
	h.settle()
	h.run(100*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(3*time.Second, func() bool { return false })
	if n := len(h.events(core.TurnEventStarted)); n != 0 {
		t.Fatalf("backchannel admitted as a user turn by a noise blip (%d starts, %d interruptions)", n, h.asst.InterruptionCount.Load())
	}
}
