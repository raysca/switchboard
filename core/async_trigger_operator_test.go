package core_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"govoice/core"
)

func operatorNote(o *orderLog, name string, run bool) core.AsyncToolResult {
	return core.AsyncToolResult{
		Call:     core.ToolCall{ID: name, Name: "context_note"},
		NoTurn:   !run,
		Operator: true,
		Inject:   func() error { o.add(name); return nil },
	}
}

// Operator items have their own cap and are refused when it is full: a burst of notes can
// never evict (and so force through mid-reply) a pending handoff flip or tool result.
func TestAsyncTriggerOperatorBurstNeverForcesAPendingItem(t *testing.T) {
	r := newTriggerRig(t, func(c *core.AsyncTriggerConfig) { c.MaxQueue = 2 })
	o := &orderLog{}
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	r.deliverItem(o, "flip", false) // a handoff flip waiting for idle
	if err := r.trig.DeliverOperator(operatorNote(o, "n1", false), operatorNote(o, "n2", false)); err != nil {
		t.Fatal(err)
	}
	if err := r.trig.DeliverOperator(operatorNote(o, "n3", false)); !errors.Is(err, core.ErrAsyncQueueFull) {
		t.Fatalf("a note past the operator cap: err=%v, want ErrAsyncQueueFull", err)
	}
	if got := o.list(); len(got) != 0 {
		t.Fatalf("injected while the caller speaks: %v", got)
	}
	if err := r.sess.Post(&core.UserStoppedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch", func() bool { return len(o.list()) == 3 })
	if got := o.list(); !slices.Equal(got, []string{"flip", "n1", "n2"}) {
		t.Fatalf("order = %v", got)
	}
}

// A note with run posted while the caller is speaking rides the caller's own turn: it is in
// the history before that turn reaches the model, and no second reply follows.
func TestAsyncTriggerCallerTurnCarriesPendingNotes(t *testing.T) {
	r := newTriggerRig(t, nil)
	o := &orderLog{}
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := r.trig.DeliverOperator(operatorNote(o, "note", true), core.AsyncToolResult{Call: core.ToolCall{Name: "run_llm"}, Operator: true, Inject: func() error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Post(&core.UserStoppedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Post(&core.UserMessageFrame{Turn: "caller-1", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the caller's turn", func() bool { return r.llm.count() == 1 })
	if got := o.list(); !slices.Equal(got, []string{"note"}) {
		t.Fatalf("note not injected with the caller's turn: %v", got)
	}
	never(t, "a second reply", func() bool { return r.llm.count() != 1 || r.trig.TriggeredCount.Load() != 0 })
}

// Items still queued when the call closes are counted as dropped; a delivery to a closed call
// reports it instead of being accepted silently.
func TestAsyncTriggerCountsItemsDroppedAtClose(t *testing.T) {
	r := newTriggerRig(t, nil)
	o := &orderLog{}
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := r.trig.DeliverOperator(operatorNote(o, "a", false), operatorNote(o, "b", false)); err != nil {
		t.Fatal(err)
	}
	_ = r.sess.Close()
	waitFor(t, "the drops", func() bool { return r.trig.DroppedCount.Load() == 2 })
	if err := r.trig.DeliverOperator(operatorNote(o, "late", false)); !errors.Is(err, core.ErrAsyncClosed) {
		t.Fatalf("delivery after close: err=%v, want ErrAsyncClosed", err)
	}
}

// A live session that briefly refuses new tasks (its task budget is full) must not close the
// trigger: the delivery is refused as retryable, and the trigger works once tasks free up.
func TestAsyncTriggerSurvivesATransientTaskBudget(t *testing.T) {
	r := newTriggerRig(t, nil)
	o := &orderLog{}
	release := make(chan struct{})
	held := 0
	for {
		err := r.sess.Go(func(ctx context.Context) {
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		if errors.Is(err, core.ErrTooManyTasks) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		held++
	}
	if held == 0 {
		t.Fatal("could not fill the task budget")
	}
	if err := r.trig.DeliverOperator(operatorNote(o, "busy", true)); !errors.Is(err, core.ErrAsyncBusy) {
		t.Fatalf("delivery with the task budget full: err=%v, want ErrAsyncBusy", err)
	}
	r.deliverItem(o, "tool-while-busy", false) // dropped and counted, not fatal
	close(release)
	time.Sleep(50 * time.Millisecond)
	if err := r.trig.DeliverOperator(operatorNote(o, "later", true)); err != nil {
		t.Fatalf("delivery after tasks freed up: %v (the trigger must not have closed)", err)
	}
	waitFor(t, "the later note", func() bool { return slices.Contains(o.list(), "later") })
	waitFor(t, "its reply", func() bool { return r.llm.count() == 1 })
}
