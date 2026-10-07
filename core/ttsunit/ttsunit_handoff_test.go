package ttsunit

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

// A refused background task must not leave a hole in the play order: the lost
// unit gets one ErrorFrame and the next unit is still spoken.
func TestRefusedHandoffDoesNotWedgeLaterUnits(t *testing.T) {
	release := make(chan struct{})
	r := New(Config{Provider: "fake", Model: "m", SampleRate: 24000, Classify: classify,
		Attempt: func(ctx context.Context, u *Unit, _ int) error {
			if u.Frame.UnitID == 1 {
				<-release
			}
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

	h.say(t, 1, "one", false)
	h.say(t, 2, "two", false) // budget full: refused
	waitFor(t, "error for unit 2", func() bool { return len(of[*core.ErrorFrame](h)) == 1 })
	close(release)
	waitFor(t, "unit 1", func() bool { return len(of[*core.TTSStoppedFrame](h)) == 1 })
	h.say(t, 3, "three", true)
	waitFor(t, "unit 3 (not wedged)", func() bool { return len(of[*core.TTSStoppedFrame](h)) == 2 })
	time.Sleep(10 * time.Millisecond)
	if e := of[*core.ErrorFrame](h); len(e) != 1 || e[0].UnitID != 2 {
		t.Fatalf("errors: %+v", e)
	}
	for _, a := range of[*core.OutputAudioFrame](h) {
		if a.UnitID == 2 {
			t.Fatal("refused unit was spoken")
		}
	}
}

// Under TTSFailover the provider's emitter swallows, so a refused unit's error
// must be re-entered: otherwise the wrapper waits on it until the watchdog.
// Unit 2 is started from unit 1's own task (its final frame), so with a budget
// of one task it is always refused.
func TestRefusedHandoffUnderFailover(t *testing.T) {
	release := make(chan struct{})
	cand := func() *Runner {
		return New(Config{Provider: "fake", Model: "m", SampleRate: 24000, Classify: classify,
			Attempt: func(ctx context.Context, u *Unit, _ int) error {
				if u.Frame.UnitID == 1 {
					<-release
				}
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

	h.say(t, 1, "one", false)
	h.say(t, 2, "two", false)
	time.Sleep(20 * time.Millisecond)
	close(release)
	waitFor(t, "error for unit 2 (not the watchdog)", func() bool { return len(of[*core.ErrorFrame](h)) == 1 })
	time.Sleep(20 * time.Millisecond) // unit 1's task has returned
	h.say(t, 3, "three", true)
	waitFor(t, "unit 3", func() bool {
		a := of[*core.OutputAudioFrame](h)
		return len(a) > 0 && a[len(a)-1].UnitID == 3 && a[len(a)-1].IsFinal
	})
	if e := of[*core.ErrorFrame](h); len(e) != 1 || e[0].UnitID != 2 {
		t.Fatalf("errors: %+v", e)
	}
	for _, a := range of[*core.OutputAudioFrame](h) {
		if a.UnitID == 2 {
			t.Fatal("refused unit was spoken")
		}
	}
}
