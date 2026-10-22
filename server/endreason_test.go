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
		EndReasonAgent:      storage.EndedByAgent,
		"something_new":     storage.EndCallerHungUp,
		"":                  storage.EndCallerHungUp,
	} {
		if got := reasonFromEnd(in); got != want {
			t.Errorf("reasonFromEnd(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"peer_unresponsive":    storage.EndConnectionLost,
		"session_start_failed": storage.EndSetupFailed,
		"":                     "",
		"unknown":              "",
	} {
		if got := reasonFromTransport(in); got != want {
			t.Errorf("reasonFromTransport(%q) = %q, want %q", in, got, want)
		}
	}
}

// testClock is a hand-driven clock for sttOutage.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func sttDown(turn string) *core.ErrorFrame {
	return &core.ErrorFrame{Err: errors.Join(core.ErrSTTUnavailable, errors.New("refused")), Turn: core.TurnID(turn)}
}

func TestSTTOutageEndsTheCallOnlyAfterTwoFailuresAWindowApartWithNoTranscriptBetween(t *testing.T) {
	clk := &testClock{t: time.Unix(1000, 0)}
	o := sttOutage{window: 5 * time.Second, now: clk.now}
	other := &core.ErrorFrame{Err: errors.New("tts failed")}
	if o.observe(sttDown("turn-1-1")) || o.observe(other) {
		t.Fatal("a lone failure (or another error) must not end the call")
	}
	clk.advance(5 * time.Second)
	if !o.observe(sttDown("turn-1-2")) {
		t.Fatal("the second failure a full window later must end the call")
	}
	if o.observe(sttDown("turn-1-3")) {
		t.Fatal("the end is signalled once")
	}

	o = sttOutage{window: 5 * time.Second, now: clk.now}
	o.observe(sttDown("a"))
	o.observe(&core.TranscriptCandidateFrame{Text: "hi"})
	clk.advance(10 * time.Second)
	if o.observe(sttDown("b")) {
		t.Fatal("a transcript between failures resets the count")
	}
	o.observe(&core.UserMessageFrame{})
	clk.advance(10 * time.Second)
	if o.observe(sttDown("c")) {
		t.Fatal("a user message resets the count")
	}
}

// STT keys errors by VAD segment; the turn processor merges segments into one turn. A pause inside
// an utterance (two segments within one no-transcript window) is one lost turn, not two.
func TestSTTOutageSegmentsInsideOneWindowAreOneLostTurn(t *testing.T) {
	clk := &testClock{t: time.Unix(1000, 0)}
	o := sttOutage{window: 5 * time.Second, now: clk.now}
	if o.observe(sttDown("turn-1-1")) {
		t.Fatal("first segment failure ended the call")
	}
	clk.advance(1200 * time.Millisecond) // the caller paused, a second VAD segment
	if o.observe(sttDown("turn-1-2")) {
		t.Fatal("a second segment inside the window hung the call up")
	}
	clk.advance(4 * time.Second) // 5.2s after the first counted failure
	if !o.observe(sttDown("turn-1-3")) {
		t.Fatal("a failure a full window after the first must end the call")
	}
}

// While a provider's breaker is open every call's dial fails fast on each segment; none of the
// calls may hang up on its second segment.
func TestSTTOutageOpenBreakerDoesNotHangUpCallsOnTheirSecondSegment(t *testing.T) {
	clk := &testClock{t: time.Unix(1000, 0)}
	for i := 0; i < 50; i++ {
		o := sttOutage{window: 5 * time.Second, now: clk.now}
		o.observe(sttDown("turn-1-1"))
		clk.advance(300 * time.Millisecond)
		if o.observe(sttDown("turn-1-2")) {
			t.Fatalf("call %d hung up on its second segment", i)
		}
	}
}

// An unbound dial (no turn) is not a lost turn.
func TestSTTOutageIgnoresUnboundDials(t *testing.T) {
	clk := &testClock{t: time.Unix(1000, 0)}
	o := sttOutage{window: time.Second, now: clk.now}
	unbound := func() *core.ErrorFrame { return &core.ErrorFrame{Err: core.ErrSTTUnavailable} }
	o.observe(unbound())
	clk.advance(2 * time.Second)
	if o.observe(unbound()) || o.observe(sttDown("turn-1-1")) {
		t.Fatal("unbound dial failures counted as lost turns")
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
		clk     = &testClock{t: time.Unix(1000, 0)}
		outage  = sttOutage{window: 5 * time.Second}
		ends    int
		changes int
	)
	outage.now = clk.now
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
	clk.advance(6 * time.Second)
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
