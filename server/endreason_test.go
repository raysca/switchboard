package server

import (
	"errors"
	"fmt"
	"govoice/core/coretest"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/storage"
)

func TestTheEndReasonHolderDefaultsToAHangUpAndTheFirstWriterWins(t *testing.T) {
	var e endReason
	if got := e.get(); got != storage.EndCallerHungUp {
		t.Fatalf("default %q", got)
	}
	e.set(storage.EndSilentCaller)
	e.set(storage.EndTimeLimit)
	if got := e.get(); got != storage.EndSilentCaller {
		t.Fatalf("first writer must win, got %q", got)
	}
	var nilHolder *endReason
	nilHolder.set(storage.EndError) // a call built without a holder (tests) must not panic
	if got := nilHolder.get(); got != storage.EndCallerHungUp {
		t.Fatalf("nil holder: %q", got)
	}
}

func TestTheEndReasonHolderIsRaceFreeWithManyWriters(t *testing.T) {
	var e endReason
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				e.set(storage.EndTimeLimit)
			} else {
				e.set(storage.EndServerShutdown)
			}
			_ = e.get()
		}(i)
	}
	wg.Wait()
	if got := e.get(); got != storage.EndTimeLimit && got != storage.EndServerShutdown {
		t.Fatalf("%q", got)
	}
}

func TestEndCauses(t *testing.T) {
	for in, want := range map[string]string{
		core.IdleReasonUser: storage.EndSilentCaller,
		core.IdleReasonBot:  storage.EndAgentStuck,
		EndReasonFlow:       storage.EndedByFlow,
		"something_new":     storage.EndCallerHungUp,
		"":                  storage.EndCallerHungUp,
	} {
		if got := reasonFromEnd(in); got != want {
			t.Errorf("reasonFromEnd(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"peer_unresponsive":    storage.EndConnectionLost,
		"connection_lifetime":  storage.EndTimeLimit,
		"session_start_failed": storage.EndSetupFailed,
		"":                     "",
		"unknown":              "",
	} {
		if got := reasonFromTransport(in); got != want {
			t.Errorf("reasonFromTransport(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSTTOutageEndsTheCallOnlyAfterTwoFailuresWithNoTranscriptBetween(t *testing.T) {
	turn := 0
	down := func() *core.ErrorFrame { // each call is the next turn's failure
		turn++
		return &core.ErrorFrame{Err: errors.Join(core.ErrSTTUnavailable, errors.New("refused")), Turn: core.TurnID(fmt.Sprintf("turn-%d", turn))}
	}
	other := &core.ErrorFrame{Err: errors.New("tts failed")}

	var o sttOutage
	if o.observe(down()) || o.observe(other) {
		t.Fatal("a lone failure (or another error) must not end the call")
	}
	if !o.observe(down()) {
		t.Fatal("the second consecutive failure must end the call")
	}
	if o.observe(down()) {
		t.Fatal("the end is signalled once")
	}

	o = sttOutage{}
	o.observe(down())
	o.observe(&core.TranscriptCandidateFrame{Text: "hi"})
	if o.observe(down()) {
		t.Fatal("a transcript between failures resets the count")
	}
	o.observe(&core.UserMessageFrame{})
	if o.observe(down()) {
		t.Fatal("a user message resets the count")
	}
}

// An unbound dial (no turn) is not a lost turn, and one turn's failures count once
// however many frames report them.
func TestSTTOutageCountsLogicalTurns(t *testing.T) {
	unbound := func() *core.ErrorFrame { return &core.ErrorFrame{Err: core.ErrSTTUnavailable} }
	turnFail := func(turn string) *core.ErrorFrame {
		return &core.ErrorFrame{Err: core.ErrSTTUnavailable, Turn: core.TurnID(turn)}
	}
	var o sttOutage
	if o.observe(unbound()) || o.observe(turnFail("turn-1")) {
		t.Fatal("an unbound pre-onset dial failure plus the turn's own failure is one lost turn")
	}
	if o.observe(turnFail("turn-1")) {
		t.Fatal("another frame of the same turn must not count")
	}
	if !o.observe(turnFail("turn-2")) {
		t.Fatal("a second turn ends the call")
	}
}

// One ErrorFrame crosses every hop of the pipeline and OnFrame sees it at each, so it must
// count once: the call ends on the second failure, not the first.
func TestSTTOutageCountsAFrameOncePerPipelineNotPerHop(t *testing.T) {
	pipe, err := core.NewPipeline(&coretest.PassthroughProcessor{}, &coretest.PassthroughProcessor{}, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(77, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu      sync.Mutex
		outage  sttOutage
		ends    int
		changes int
	)
	sess.OnFrame = func(f core.Frame, _ core.Direction) {
		mu.Lock()
		defer mu.Unlock()
		changes++
		if outage.observe(f) {
			ends++
		}
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	gen := sess.CurrentGeneration()
	fail := func(n int) {
		_ = sess.Reenter(&core.ErrorFrame{Err: core.ErrSTTUnavailable, Turn: core.TurnID(fmt.Sprint(n)), Gen: core.Generation(gen)}, core.Downstream, 1, gen)
	}
	settle := func(want int) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
			mu.Lock()
			n := changes
			mu.Unlock()
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("OnFrame saw %d frames, want %d", n, want)
			}
		}
	}
	fail(1)
	settle(2) // indexes 1 and 2 of the 3-hop pipeline (and the sink)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if ends != 0 {
		mu.Unlock()
		t.Fatalf("one failed dial ended the call (%d hops counted it)", changes)
	}
	seen := changes
	mu.Unlock()
	fail(2) // a second turn
	settle(seen + 2)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if ends != 1 {
		t.Fatalf("ends = %d after the second failure, want 1", ends)
	}
}
