package core_test

import (
	"errors"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// An expiry posted by a failover that was swapped out must not drive the stage that replaced it.
func TestSwappedOutFailoverExpiryDoesNotDriveReplacement(t *testing.T) {
	cfg := func(a, b *silent) core.LLMFailoverConfig {
		return core.LLMFailoverConfig{
			Candidates: []core.LLMCandidate{{Provider: "x0", Proc: a}, {Provider: "x1", Proc: b}},
			Classify: func(err error) string {
				return map[bool]string{true: "timeout"}[errors.Is(err, core.ErrFirstTokenTimeout)]
			},
			FirstTokenTimeout: 60 * time.Millisecond,
		}
	}
	a0, a1, b0, b1 := newSilent(), newSilent(), newSilent(), newSilent()
	A, _ := core.NewLLMFailover(cfg(a0, a1))
	B, _ := core.NewLLMFailover(cfg(b0, b1))
	swap := core.NewStageSwap(A)
	rec := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(swap, rec)
	sess, _ := core.NewSession(901, pipe, core.DefaultResourceBudget())
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.Post(&core.UserMessageFrame{Turn: "t1", Content: "hi", Gen: sess.CurrentGeneration(), Timestamp: time.Now()})
	deadline := time.Now().Add(time.Second)
	for a0.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := swap.Replace(B); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	t.Logf("a1.started=%d b0.stopped=%d b1.started=%d", a1.started.Load(), b0.stopped.Load(), b1.started.Load())
	if b1.started.Load() != 0 || b0.stopped.Load() != 0 {
		t.Fatalf("A's watchdog drove B: b1.started=%d b0.stopped=%d", b1.started.Load(), b0.stopped.Load())
	}
}
