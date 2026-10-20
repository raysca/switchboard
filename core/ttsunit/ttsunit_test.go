package ttsunit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
)

var errFlaky = errors.New("flaky")

func classify(err error) resilience.Verdict {
	return resilience.Verdict{Retryable: errors.Is(err, errFlaky)}
}

type harness struct {
	r    *Runner
	sess *core.Session
	sink *coretest.PassthroughProcessor
}

func start(t *testing.T, attempt func(ctx context.Context, u *Unit, n int) error) *harness {
	t.Helper()
	r := New(Config{Provider: "fake", Model: "m", SampleRate: 24000, Classify: classify,
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
	h := &harness{r, sess, sink}
	t.Cleanup(func() {
		sess.Close()
		if n := sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return h
}

func (h *harness) say(t *testing.T, id uint64, text string, final bool) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: id, Text: text, IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

func of[T core.Frame](h *harness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for until := time.Now().Add(3 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func stopped(h *harness) func() bool {
	return func() bool { return len(of[*core.TTSStoppedFrame](h)) > 0 }
}

// Odd-sized writes come out as 960-byte (20 ms) frames plus a held final;
// timings ride on the next frame; only the last frame is IsFinal/TurnFinal.
func TestFramingAndTimings(t *testing.T) {
	h := start(t, func(ctx context.Context, u *Unit, _ int) error {
		u.Timings = append(u.Timings, core.WordTiming{Word: "a"})
		u.Write(make([]byte, 1500))
		u.Write(make([]byte, 1501))
		u.Timings = append(u.Timings, core.WordTiming{Word: "late"})
		return u.Finish()
	})
	h.say(t, 1, "hi", true)
	waitFor(t, "stop", stopped(h))
	a := of[*core.OutputAudioFrame](h)
	sizes := []int{960, 960, 960, 120} // 3001 bytes: the lone trailing byte is dropped
	if len(a) != len(sizes) {
		t.Fatalf("frames = %d, want %d", len(a), len(sizes))
	}
	for i, f := range a {
		last := i == len(a)-1
		if len(f.Samples) != sizes[i] || f.Seq != uint64(i+1) || f.IsFinal != last || f.TurnFinal != last {
			t.Fatalf("frame %d: len=%d seq=%d final=%v turnFinal=%v", i, len(f.Samples), f.Seq, f.IsFinal, f.TurnFinal)
		}
	}
	if len(a[0].WordTimings) != 1 || a[0].WordTimings[0].Word != "a" || len(a[3].WordTimings) != 1 || a[3].WordTimings[0].Word != "late" {
		t.Fatalf("timings: first=%v last=%v", a[0].WordTimings, a[3].WordTimings)
	}
	if u := of[*core.UsageMetricsFrame](h); len(u) != 1 || *u[0].Characters != 2 || *u[0].AudioDurationMs != 62 {
		t.Fatalf("usage = %+v", u)
	}
}

// A non-final unit never sets TurnFinal; empty text still closes the unit.
func TestEmptyTextFinalMarker(t *testing.T) {
	var calls atomic.Int32
	h := start(t, func(context.Context, *Unit, int) error { calls.Add(1); return nil })
	h.say(t, 1, "  ", false)
	waitFor(t, "stop", stopped(h))
	a := of[*core.OutputAudioFrame](h)
	if calls.Load() != 0 || len(a) != 1 || !a[0].IsFinal || a[0].TurnFinal || len(a[0].Samples) != 0 {
		t.Fatalf("calls=%d frames=%+v", calls.Load(), a)
	}
}

// A later unit that finishes first is still played after the earlier one.
func TestUnitsPlayInOrder(t *testing.T) {
	release := make(chan struct{})
	h := start(t, func(ctx context.Context, u *Unit, _ int) error {
		if u.Frame.UnitID == 1 {
			<-release
		}
		u.Write(make([]byte, 960))
		return u.Finish()
	})
	h.say(t, 1, "one", false)
	h.say(t, 2, "two", true)
	time.Sleep(20 * time.Millisecond)
	close(release)
	waitFor(t, "both", func() bool { return len(of[*core.TTSStoppedFrame](h)) == 2 })
	a := of[*core.OutputAudioFrame](h)
	if len(a) != 2 || a[0].UnitID != 1 || a[1].UnitID != 2 || a[0].TurnFinal || !a[1].TurnFinal {
		t.Fatalf("order: %+v", a)
	}
}

// Before audio, a retryable error is retried with clean state and usage is
// reported once; after audio, it is not retried, and the error precedes the
// one closing frame.
func TestRetryOnlyBeforeAudio(t *testing.T) {
	var n atomic.Int32
	h := start(t, func(ctx context.Context, u *Unit, attempt int) error {
		n.Add(1)
		if attempt == 0 {
			u.Write(make([]byte, 500)) // held back: not committed
			u.Timings = append(u.Timings, core.WordTiming{Word: "stale"})
			return errFlaky
		}
		u.Write(make([]byte, 2000))
		return errFlaky
	})
	h.say(t, 7, "hi", true)
	waitFor(t, "stop", stopped(h))
	if n.Load() != 2 || h.r.RetriesTotal.Load() != 1 || h.r.ErrorsTotal.Load() != 1 {
		t.Fatalf("attempts=%d retries=%d errors=%d", n.Load(), h.r.RetriesTotal.Load(), h.r.ErrorsTotal.Load())
	}
	errAt, finalAt, finals := -1, -1, 0
	for i, f := range h.sink.GetFrames() {
		switch f := f.(type) {
		case *core.ErrorFrame:
			if f.UnitID != 7 {
				t.Fatalf("error UnitID = %d", f.UnitID)
			}
			errAt = i
		case *core.OutputAudioFrame:
			if len(f.WordTimings) != 0 {
				t.Fatalf("timings from the failed attempt leaked: %v", f.WordTimings)
			}
			if f.IsFinal {
				finalAt, finals = i, finals+1
			}
		}
	}
	if errAt < 0 || finals != 1 || errAt > finalAt {
		t.Fatalf("error at %d, final at %d (%d finals)", errAt, finalAt, finals)
	}
	if a := of[*core.OutputAudioFrame](h); len(a) != 3 || len(a[2].Samples) != 80 { // 2000 = 960+960+80
		t.Fatalf("frames: %d", len(a))
	}
}

// Interruption mid-attempt and Close during retry backoff both stop the unit
// with no further frames and no leaked goroutine.
func TestCancelStopsUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(h *harness)
	}{
		{"interruption", func(h *harness) { h.sess.Post(&core.InterruptionFrame{}) }},
		{"close", func(h *harness) { h.r.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{}, 4)
			h := start(t, func(ctx context.Context, u *Unit, attempt int) error {
				entered <- struct{}{}
				if attempt == 0 {
					u.Write(make([]byte, 100))
					return errFlaky // close lands in backoff or the next attempt
				}
				<-ctx.Done()
				return ctx.Err()
			})
			h.say(t, 1, "one", false)
			h.say(t, 2, "two", true)
			<-entered
			tc.stop(h)
			time.Sleep(30 * time.Millisecond)
			if a, e := of[*core.OutputAudioFrame](h), of[*core.ErrorFrame](h); len(a) != 0 || len(e) != 0 {
				t.Fatalf("frames after cancel: audio=%d errors=%d", len(a), len(e))
			}
			if tc.name == "close" {
				h.say(t, 3, "late", true) // refused after Close
				time.Sleep(10 * time.Millisecond)
				if len(of[*core.TTSStartedFrame](h)) != 0 {
					t.Fatal("unit admitted after Close")
				}
			}
		})
	}
}

// A blank final unit ends the reply without an Attempt: EndReply releases the
// provider's reply session; so does an interruption.
func TestRunner_EndReplyHook(t *testing.T) {
	var ends atomic.Int32
	h := start(t, func(context.Context, *Unit, int) error { return nil })
	h.r.cfg.EndReply = func() { ends.Add(1) }
	h.say(t, 1, " ", true)
	waitFor(t, "blank final unit", func() bool { return ends.Load() == 1 })
	_ = h.sess.Post(&core.InterruptionFrame{})
	waitFor(t, "interruption", func() bool { return ends.Load() == 2 })
}

// The unit's text rides on its final frame only, so the transport can announce it once the
// unit's last audio is written.
func TestFinalFrameCarriesTheUnitText(t *testing.T) {
	h := start(t, func(ctx context.Context, u *Unit, _ int) error {
		u.Write(make([]byte, 2500))
		return u.Finish()
	})
	h.say(t, 1, "Hello there.", false)
	waitFor(t, "stop", stopped(h))
	a := of[*core.OutputAudioFrame](h)
	if len(a) < 2 {
		t.Fatalf("frames = %d", len(a))
	}
	for i, f := range a {
		want := ""
		if i == len(a)-1 {
			want = "Hello there."
		}
		if f.Text != want {
			t.Fatalf("frame %d Text = %q, want %q", i, f.Text, want)
		}
	}
}

// A unit with no audio still ends with a marker; it carries the text of a non-blank unit only.
func TestEmptyAudioMarkerText(t *testing.T) {
	h := start(t, func(ctx context.Context, u *Unit, _ int) error { return u.Finish() })
	h.say(t, 1, "Hi.", false)
	h.say(t, 2, "  ", true)
	waitFor(t, "both stops", func() bool { return len(of[*core.TTSStoppedFrame](h)) == 2 })
	a := of[*core.OutputAudioFrame](h)
	if len(a) != 2 || a[0].Text != "Hi." || a[1].Text != "" {
		t.Fatalf("frames = %+v", a)
	}
}

// A provider error after audio started closes the unit for playback to drain, but the unit was
// not fully spoken, so its text is never announced.
func TestFailedUnitSendsNoText(t *testing.T) {
	h := start(t, func(ctx context.Context, u *Unit, _ int) error {
		u.Write(make([]byte, 2000))
		return errFlaky
	})
	h.say(t, 1, "Hello there.", true)
	waitFor(t, "stop", stopped(h))
	a := of[*core.OutputAudioFrame](h)
	if len(a) == 0 || !a[len(a)-1].IsFinal {
		t.Fatalf("frames = %+v", a)
	}
	for i, f := range a {
		if f.Text != "" {
			t.Fatalf("frame %d of a failed unit carries Text %q", i, f.Text)
		}
	}
}
