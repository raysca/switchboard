package ttsunit

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

// holdOnlySlot fills a one-task budget with a foreign task so the next unit's
// worker cannot start; the returned func frees the slot.
func holdOnlySlot(t *testing.T, sess *core.Session) func() {
	t.Helper()
	hold := make(chan struct{})
	if err := sess.Go(func(context.Context) { <-hold }); err != nil {
		t.Fatal(err)
	}
	return func() { close(hold); time.Sleep(20 * time.Millisecond) }
}

// A refused background task must not leave a hole in the play order: the lost
// unit gets one ErrorFrame and the next unit is still spoken.
func TestRefusedHandoffDoesNotWedgeLaterUnits(t *testing.T) {
	r := New(Config{Provider: "fake", Model: "m", SampleRate: 24000, Classify: classify,
		Attempt: func(ctx context.Context, u *Unit, _ int) error {
			u.Write(make([]byte, 960))
			return u.Finish()
		}})
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(r, sink)
	b := core.DefaultResourceBudget()
	b.MaxAsyncTasks = 1
	sess, err := core.NewSession(1, pipe, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	h := &harness{r, sess, sink}
	defer sess.Close()

	free := holdOnlySlot(t, sess)
	h.say(t, 1, "one", false) // budget full: refused
	waitFor(t, "error for unit 1", func() bool { return len(of[*core.ErrorFrame](h)) == 1 })
	free()
	h.say(t, 2, "two", true)
	waitFor(t, "unit 2 (not wedged)", func() bool { return len(of[*core.TTSStoppedFrame](h)) == 1 })
	time.Sleep(10 * time.Millisecond)
	if e := of[*core.ErrorFrame](h); len(e) != 1 || e[0].UnitID != 1 {
		t.Fatalf("errors: %+v", e)
	}
	for _, a := range of[*core.OutputAudioFrame](h) {
		if a.UnitID == 1 {
			t.Fatal("refused unit was spoken")
		}
	}
}

// Under TTSFailover the provider's emitter swallows, so a refused unit's error
// must be re-entered: otherwise the wrapper waits on it until the watchdog.
func TestRefusedHandoffUnderFailover(t *testing.T) {
	cand := func() *Runner {
		return New(Config{Provider: "fake", Model: "m", SampleRate: 24000, Classify: classify,
			Attempt: func(ctx context.Context, u *Unit, _ int) error {
				u.Write(make([]byte, 960))
				return u.Finish()
			}})
	}
	fo, err := core.NewTTSFailover(core.TTSFailoverConfig{
		Candidates:  []core.TTSCandidate{{Provider: "a", Proc: cand()}, {Provider: "b", Proc: cand()}},
		Classify:    func(error) string { return "server_error" },
		UnitTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(fo, sink)
	b := core.DefaultResourceBudget()
	b.MaxAsyncTasks = 1
	sess, err := core.NewSession(1, pipe, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	h := &harness{nil, sess, sink}
	defer sess.Close()

	free := holdOnlySlot(t, sess)
	h.say(t, 1, "one", false)
	waitFor(t, "error for unit 1 (not the watchdog)", func() bool { return len(of[*core.ErrorFrame](h)) >= 1 })
	free()
	h.say(t, 2, "two", true)
	waitFor(t, "unit 2", func() bool {
		a := of[*core.OutputAudioFrame](h)
		return len(a) > 0 && a[len(a)-1].UnitID == 2 && a[len(a)-1].IsFinal
	})
	for _, a := range of[*core.OutputAudioFrame](h) {
		if a.UnitID == 1 {
			t.Fatal("refused unit was spoken")
		}
	}
}
