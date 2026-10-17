package core_test

import (
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// Once the loop announced a call (its generation was current at claim time), barge-in between the claim
// and delivery must not drop either frame: the executor may already be running. The loop itself refuses to
// announce a call that is stale at claim time (TestToolLoopReportsNothingForAStaleGeneration).
func TestToolFramesSurviveTheGenerationFence(t *testing.T) {
	tail := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(tail)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	stale := sess.CurrentGeneration()
	now := sess.IncrementGeneration()
	call := core.ToolCall{ID: "c1", Name: "x", SessionID: "1", TurnID: "t1", Generation: core.Generation(stale)}
	if err := sess.Reenter(&core.ToolCallFrame{Call: call}, core.Downstream, 0, stale); err != nil {
		t.Fatal(err)
	}
	if err := sess.Reenter(&core.ToolResultFrame{Turn: "t1", Gen: core.Generation(stale), Result: core.ToolResult{CallID: "c1", Status: core.ToolCancelled}}, core.Downstream, 0, stale); err != nil {
		t.Fatal(err)
	}
	if err := sess.Reenter(&core.LLMTextFrame{Text: "marker", Turn: "t2", Gen: now}, core.Downstream, 0, now); err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		calls, results = 0, 0
		marker := false
		for _, f := range tail.GetFrames() {
			switch f.(type) {
			case *core.ToolCallFrame:
				calls++
			case *core.ToolResultFrame:
				results++
			case *core.LLMTextFrame:
				marker = true
			}
		}
		if marker {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the marker never arrived")
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("call frames delivered %d, results %d (want 1 and 1)", calls, results)
	}
}
