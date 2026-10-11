package core_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type orderLog struct {
	mu  sync.Mutex
	got []string
}

func (o *orderLog) add(s string) { o.mu.Lock(); o.got = append(o.got, s); o.mu.Unlock() }
func (o *orderLog) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.got)
}

func (r *triggerRig) deliverItem(o *orderLog, name string, noTurn bool) {
	r.trig.Deliver(core.AsyncToolResult{
		Call:   core.ToolCall{ID: name, Name: name},
		NoTurn: noTurn,
		Inject: func() error { o.add(name); return nil },
	})
}

// A note without run is injected when the call is idle and starts no model turn.
func TestAsyncTriggerNoTurnItemInjectsWithoutATurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	o := &orderLog{}
	r.deliverItem(o, "note", true)
	waitFor(t, "injection", func() bool { return len(o.list()) == 1 })
	never(t, "a model turn", func() bool { return r.llm.count() != 0 || r.trig.TriggeredCount.Load() != 0 })
}

// Items are injected in arrival order; one turn covers a batch that holds any turn-starting item.
func TestAsyncTriggerMixedBatchInjectsInOrderAndStartsOneTurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	o := &orderLog{}
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the run loop observe the speech
	r.deliverItem(o, "a", true)
	r.deliverItem(o, "b", false)
	r.deliverItem(o, "c", true)
	never(t, "injection while the user speaks", func() bool { return len(o.list()) != 0 })
	if err := r.sess.Post(&core.UserStoppedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch", func() bool { return len(o.list()) == 3 && r.llm.count() == 1 })
	never(t, "a second turn", func() bool { return r.llm.count() != 1 })
	if got := o.list(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("injection order = %v", got)
	}
}

// A failed injection (history rejected the note) is counted, and a batch with no successful
// turn-starting item starts no turn.
func TestAsyncTriggerFailedTurnItemStartsNoTurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	r.trig.Deliver(core.AsyncToolResult{Call: core.ToolCall{Name: "run_llm"}, Inject: func() error { return core.ErrContextLimitExceeded }})
	waitFor(t, "drop", func() bool { return r.trig.DroppedCount.Load() == 1 })
	never(t, "a model turn", func() bool { return r.llm.count() != 0 })
}

func TestAsyncTriggerReadyOnlyOnceAttached(t *testing.T) {
	if core.NewAsyncTrigger(core.AsyncTriggerConfig{}).Ready() {
		t.Fatal("a trigger that was never attached reports ready")
	}
	r := newTriggerRig(t, nil) // starts its session, which attaches the trigger
	if !r.trig.Ready() {
		t.Fatal("an attached trigger reports not ready")
	}
}
