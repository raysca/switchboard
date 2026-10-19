package ttsunit

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
)

// startAhead is start with a look-ahead window.
func startAhead(t *testing.T, ahead int, attempt func(ctx context.Context, u *Unit, n int) error) *harness {
	t.Helper()
	r := New(Config{Provider: "fake", Model: "m", SampleRate: 24000, Classify: classify, LookAhead: ahead,
		Policy:  resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		Attempt: attempt})
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(r, sink)
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
	t.Cleanup(func() {
		sess.Close()
		if n := sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return &harness{r, sess, sink}
}

func (h *harness) sayGen(t *testing.T, id, gen uint64, text string) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: gen, UnitID: id, Text: text, IsFinal: true}); err != nil {
		t.Fatal(err)
	}
}

// startedSet records which unit texts have entered Attempt.
type startedSet struct {
	mu sync.Mutex
	m  map[string]int
}

func (s *startedSet) note(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]int{}
	}
	s.m[text]++
}

func (s *startedSet) has(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[text] > 0
}

func unitIDs(h *harness) (ids []uint64) {
	for _, a := range of[*core.OutputAudioFrame](h) {
		if len(ids) == 0 || ids[len(ids)-1] != a.UnitID {
			ids = append(ids, a.UnitID)
		}
	}
	return
}

func stoppedN(h *harness, n int) func() bool {
	return func() bool { return len(of[*core.TTSStoppedFrame](h)) >= n }
}

func TestRunnerLookAheadSynthesizesAheadInOrder(t *testing.T) {
	release := make(chan struct{})
	var seen startedSet
	h := startAhead(t, 2, func(ctx context.Context, u *Unit, _ int) error {
		seen.note(u.Frame.Text)
		if u.Frame.Text == "u1" {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_ = u.Write(make([]byte, 1920))
		return u.Finish()
	})
	for i, txt := range []string{"u1", "u2", "u3", "u4"} {
		h.say(t, uint64(i+1), txt, false)
	}
	waitFor(t, "u2 and u3 ahead", func() bool { return seen.has("u2") && seen.has("u3") })
	time.Sleep(50 * time.Millisecond) // give u4 a chance to start wrongly
	if seen.has("u4") {
		t.Fatal("u4 attempted beyond the look-ahead window")
	}
	if a := of[*core.OutputAudioFrame](h); len(a) != 0 || len(of[*core.TTSStartedFrame](h)) != 0 {
		t.Fatalf("audio emitted while u1 is pending: %d frames", len(a))
	}
	close(release)
	waitFor(t, "four units", stoppedN(h, 4))
	if got := unitIDs(h); len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 3 || got[3] != 4 {
		t.Fatalf("emission order = %v", got)
	}
	for _, a := range of[*core.OutputAudioFrame](h) {
		if len(a.Samples) == 0 && !a.IsFinal {
			t.Fatal("empty non-final frame")
		}
	}
}

func TestRunnerLookAheadStaleUnitKeepsOrder(t *testing.T) {
	release := make(chan struct{})
	var seen startedSet
	h := startAhead(t, 2, func(ctx context.Context, u *Unit, _ int) error {
		seen.note(u.Frame.Text)
		if u.Frame.Text == "u1" {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_ = u.Write(make([]byte, 1920))
		return u.Finish()
	})
	c := h.sess.CurrentGeneration()
	h.sayGen(t, 1, c+1, "u1")
	h.sayGen(t, 2, c, "u2") // goes stale once the generation moves on
	h.sayGen(t, 3, c+1, "u3")
	waitFor(t, "u2 and u3 ahead", func() bool { return seen.has("u2") && seen.has("u3") })
	h.sess.IncrementGeneration()
	close(release)
	waitFor(t, "u1 and u3", stoppedN(h, 2))
	time.Sleep(20 * time.Millisecond)
	if got := unitIDs(h); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("emission order = %v, want [1 3]", got)
	}
	if n := h.r.StaleTotal.Load(); n != 1 {
		t.Fatalf("StaleTotal = %d, want 1", n)
	}
}

func TestRunnerLookAheadRetriesBeforeItsTurn(t *testing.T) {
	release := make(chan struct{})
	var tries atomic.Int32
	h := startAhead(t, 1, func(ctx context.Context, u *Unit, attempt int) error {
		if u.Frame.Text == "u1" {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return u.Finish()
		}
		tries.Add(1)
		if attempt == 0 {
			_ = u.Write(bytes.Repeat([]byte{0xAA}, 2000))
			return errFlaky
		}
		_ = u.Write(bytes.Repeat([]byte{0xBB}, 1920))
		return u.Finish()
	})
	h.say(t, 1, "u1", false)
	h.say(t, 2, "u2", true)
	waitFor(t, "u2 retried while u1 pends", func() bool { return tries.Load() == 2 })
	if a := of[*core.OutputAudioFrame](h); len(a) != 0 {
		t.Fatalf("audio emitted before its turn: %d frames", len(a))
	}
	close(release)
	waitFor(t, "both units", stoppedN(h, 2))
	var total int
	for _, a := range of[*core.OutputAudioFrame](h) {
		if a.UnitID != 2 {
			continue
		}
		total += len(a.Samples)
		if bytes.IndexByte(a.Samples, 0xAA) >= 0 {
			t.Fatal("audio of the failed attempt was emitted")
		}
	}
	if total != 1920 {
		t.Fatalf("unit 2 audio = %d bytes, want 1920 (attempt 1 only)", total)
	}
	if n := h.r.RetriesTotal.Load(); n != 1 {
		t.Fatalf("RetriesTotal = %d, want 1", n)
	}
	if e := of[*core.ErrorFrame](h); len(e) != 0 {
		t.Fatalf("error frames: %v", e)
	}
}

func TestRunnerLookAheadFailedUnitDoesNotWedgeLaterOnes(t *testing.T) {
	h := startAhead(t, 2, func(ctx context.Context, u *Unit, _ int) error {
		if u.Frame.Text == "bad" {
			return context.DeadlineExceeded // not retryable: fails at once
		}
		_ = u.Write(make([]byte, 1920))
		return u.Finish()
	})
	h.say(t, 1, "ok1", false)
	h.say(t, 2, "bad", false)
	h.say(t, 3, "ok3", true)
	waitFor(t, "two units", stoppedN(h, 2))
	if got := unitIDs(h); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("emission order = %v, want [1 3]", got)
	}
	if e := of[*core.ErrorFrame](h); len(e) != 1 || e[0].UnitID != 2 || e[0].Fatal {
		t.Fatalf("error frames = %+v, want one non-fatal for unit 2", e)
	}
}

func TestRunnerLookAheadInterruptionDropsHeldAudio(t *testing.T) {
	var seen startedSet
	var done2 atomic.Bool
	h := startAhead(t, 2, func(ctx context.Context, u *Unit, _ int) error {
		seen.note(u.Frame.Text)
		if u.Frame.Text == "u1" {
			<-ctx.Done()
			return ctx.Err()
		}
		_ = u.Write(make([]byte, 1920))
		err := u.Finish()
		done2.Store(true)
		return err
	})
	h.say(t, 1, "u1", false)
	h.say(t, 2, "u2", true)
	waitFor(t, "u2 held", done2.Load)
	_ = h.sess.Post(&core.InterruptionFrame{})
	time.Sleep(50 * time.Millisecond)
	if a, s := of[*core.OutputAudioFrame](h), of[*core.TTSStoppedFrame](h); len(a) != 0 || len(s) != 0 {
		t.Fatalf("held audio leaked after interruption: audio=%d stopped=%d", len(a), len(s))
	}
	// the window is free again
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t2", UnitID: 3, Text: "u3", IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "next unit plays", stoppedN(h, 1))
}

func TestRunnerLookAheadCloseWhileWaiting(t *testing.T) {
	var seen startedSet
	h := startAhead(t, 2, func(ctx context.Context, u *Unit, _ int) error {
		seen.note(u.Frame.Text)
		if u.Frame.Text == "u1" {
			<-ctx.Done()
			return ctx.Err()
		}
		_ = u.Write(make([]byte, 1920))
		return u.Finish()
	})
	h.say(t, 1, "u1", false)
	h.say(t, 2, "u2", false)
	h.say(t, 3, "u3", true)
	waitFor(t, "all ahead", func() bool { return seen.has("u2") && seen.has("u3") })
	begin := time.Now()
	_ = h.r.Close()
	h.sess.Close() // drains every task; the cleanup asserts none leaked
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
	if a := of[*core.OutputAudioFrame](h); len(a) != 0 {
		t.Fatalf("audio after Close: %d frames", len(a))
	}
}

// A failed held attempt that already called Finish must not leak its pending
// Finish into the retry.
func TestRunnerLookAheadRetryClearsPendingFinish(t *testing.T) {
	h := startAhead(t, 1, func(ctx context.Context, u *Unit, attempt int) error {
		if attempt == 0 {
			_ = u.Write(make([]byte, 1920))
			_ = u.Finish()
			return errFlaky
		}
		return u.Write(make([]byte, 1920)) // the retry never finishes the unit
	})
	h.say(t, 1, "u1", true)
	waitFor(t, "retry", func() bool { return h.r.RetriesTotal.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if s := of[*core.TTSStoppedFrame](h); len(s) != 0 {
		t.Fatalf("TTSStopped emitted from a discarded attempt's Finish")
	}
}

// A unit that fails fast while the unit before it is still playing reports its
// error only after that unit has stopped (units end in arrival order).
func TestRunnerLookAheadErrorWaitsForItsTurn(t *testing.T) {
	release := make(chan struct{})
	h := startAhead(t, 2, func(ctx context.Context, u *Unit, _ int) error {
		if u.Frame.Text == "u2" {
			return errors.New("boom")
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_ = u.Write(make([]byte, 1920))
		return u.Finish()
	})
	h.say(t, 1, "u1", false)
	h.say(t, 2, "u2", false)
	time.Sleep(100 * time.Millisecond) // u2 has failed by now
	if e := of[*core.ErrorFrame](h); len(e) != 0 {
		t.Fatalf("error emitted while u1 is pending: %v", e[0].Err)
	}
	close(release)
	waitFor(t, "u2 error", func() bool { return len(of[*core.ErrorFrame](h)) == 1 })
	stopped := -1
	for i, f := range h.sink.GetFrames() {
		switch v := f.(type) {
		case *core.TTSStoppedFrame:
			if v.UnitID == 1 {
				stopped = i
			}
		case *core.ErrorFrame:
			if stopped < 0 || v.UnitID != 2 {
				t.Fatalf("error for unit %d at %d, before unit 1 stopped (%d)", v.UnitID, i, stopped)
			}
		}
	}
}
