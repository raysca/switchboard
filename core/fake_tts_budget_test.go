package core_test

import (
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// A long reply (more sentences than the session's task budget) must not fail
// the session: the fake TTS queues its units like the real TTS stages do.
func TestFakeTTSLongReplyStaysWithinTaskBudget(t *testing.T) {
	tts := coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 1, FrameDelay: 5 * time.Millisecond})
	pipe, err := core.NewPipeline(tts, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	budget := core.DefaultResourceBudget()
	sess, err := core.NewSession(1, pipe, budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	n := budget.MaxAsyncTasks + 4
	gen := sess.CurrentGeneration()
	for i := 1; i <= n; i++ {
		if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: gen, UnitID: uint64(i), Text: "x", IsFinal: i == n}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for tts.Units.Load() < uint64(n) {
		if err := sess.Err(); err != nil {
			t.Fatalf("session failed after %d of %d units: %v", tts.Units.Load(), n, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("synthesized %d of %d units", tts.Units.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := sess.Err(); err != nil {
		t.Fatalf("session failed: %v", err)
	}
}
