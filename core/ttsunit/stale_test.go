package ttsunit

import (
	"context"
	"errors"
	"testing"
	"time"

	"govoice/core"
)

// A unit without a generation (gen 0, idle prompts) cannot be fenced by a
// generation check: only the unit's done channel stops it after a barge-in
// or Close that does not bump the generation.
func TestUnitStopsOnCancelWithoutGenerationBump(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(h *harness) core.Frame // returns a frame to wait for in the sink, or nil
	}{
		{"interruption", func(h *harness) core.Frame {
			_ = h.sess.Post(&core.InterruptionFrame{})
			return &core.InterruptionFrame{}
		}},
		{"close", func(h *harness) core.Frame { _ = h.r.Close(); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrote := make(chan struct{})
			proceed := make(chan struct{})
			writeErr := make(chan error, 1)
			h := start(t, func(ctx context.Context, u *Unit, _ int) error {
				for i := 0; i < 20; i++ {
					if err := u.Write(make([]byte, 1920)); err != nil {
						writeErr <- err
						return err
					}
					if i == 0 {
						close(wrote)
						<-proceed
					} else {
						time.Sleep(2 * time.Millisecond)
					}
				}
				writeErr <- nil
				return u.Finish()
			})
			if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 0, UnitID: 1, Text: "idle", IsFinal: true}); err != nil {
				t.Fatal(err)
			}
			<-wrote
			waitFor(t, "first audio", func() bool { return len(of[*core.OutputAudioFrame](h)) >= 1 })
			wantFrame := tc.stop(h)
			if wantFrame != nil {
				waitFor(t, "interruption processed", func() bool { return len(of[*core.InterruptionFrame](h)) >= 1 })
			}
			before := len(of[*core.OutputAudioFrame](h))
			close(proceed)
			select {
			case err := <-writeErr:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Write after cancel = %v, want context.Canceled", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Write never failed after cancel")
			}
			time.Sleep(10 * time.Millisecond)
			if n := len(of[*core.OutputAudioFrame](h)); n != before {
				t.Fatalf("audio after cancel: %d -> %d frames", before, n)
			}
			if s, m := of[*core.TTSStoppedFrame](h), of[*core.UsageMetricsFrame](h); len(s) != 0 || len(m) != 0 {
				t.Fatalf("closing frames after cancel: stopped=%d usage=%d", len(s), len(m))
			}
		})
	}
}

// done is not closed by a normal end of unit: nothing is truncated.
func TestUnitNormalEndIsNotTruncated(t *testing.T) {
	var werr error
	h := start(t, func(ctx context.Context, u *Unit, _ int) error {
		for i := 0; i < 5; i++ {
			if werr = u.Write(make([]byte, 960)); werr != nil {
				return werr
			}
		}
		return u.Finish()
	})
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 0, UnitID: 1, Text: "x", IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stop", stopped(h))
	total := 0
	for _, a := range of[*core.OutputAudioFrame](h) {
		total += len(a.Samples)
	}
	if werr != nil || total != 5*960 || len(of[*core.UsageMetricsFrame](h)) != 1 {
		t.Fatalf("werr=%v total=%d usage=%d", werr, total, len(of[*core.UsageMetricsFrame](h)))
	}
}
