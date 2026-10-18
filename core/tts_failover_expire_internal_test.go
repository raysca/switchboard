package core

import (
	"context"
	"testing"
	"time"
)

type nopTTS struct{}

func (nopTTS) ProcessFrame(context.Context, Frame, Direction, Emitter) error { return nil }

type expireGateFrame struct{}

func (expireGateFrame) Type() FrameType { return FrameInternal }

type expireGateSink struct{ entered, release chan struct{} }

func (g expireGateSink) ProcessFrame(_ context.Context, f Frame, d Direction, e Emitter) error {
	if _, ok := f.(expireGateFrame); ok {
		close(g.entered)
		<-g.release
		return nil
	}
	return e.PushDirection(f, d)
}

// A watchdog expiry that was already in flight when the retry branch advanced the unit must neither
// panic on the not-yet-assigned owner (-1) nor demote or skip a candidate.
func TestTTSExpireInFlightDuringRetryIsHarmless(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(u *ttsUnit, attemptBefore int) (int, int)
	}{
		{"attempt fence", func(u *ttsUnit, a int) (int, int) { return a, u.progress }},
		{"owner guard", func(u *ttsUnit, _ int) (int, int) { return u.attempt, u.progress }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := NewTTSFailover(TTSFailoverConfig{
				Candidates: []TTSCandidate{{Provider: "a", Proc: nopTTS{}}, {Provider: "b", Proc: nopTTS{}}, {Provider: "c", Proc: nopTTS{}}},
				Classify:   func(error) string { return "server_error" }, UnitTimeout: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			sink := expireGateSink{make(chan struct{}), make(chan struct{})}
			pipe, _ := NewPipeline(w, sink)
			sess, err := NewSession(903, pipe, DefaultResourceBudget())
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.Start(); err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			gen := sess.CurrentGeneration()
			if err := sess.Post(&SynthesisUnitFrame{Turn: "t1", Gen: gen, UnitID: 1, Text: "hi"}); err != nil {
				t.Fatal(err)
			}
			var u *ttsUnit
			for end := time.Now().Add(2 * time.Second); u == nil && time.Now().Before(end); time.Sleep(time.Millisecond) {
				w.mu.Lock()
				if w.cur != nil && w.cur.watchdog != nil {
					u = w.cur
				}
				w.mu.Unlock()
			}
			if u == nil {
				t.Fatal("unit never started")
			}
			if err := sess.Post(expireGateFrame{}); err != nil { // parks the run loop: the retry's ttsRun waits
				t.Fatal(err)
			}
			<-sink.entered
			w.mu.Lock()
			attemptBefore := u.attempt
			w.mu.Unlock()
			_ = w.fromCandidate(0, &ErrorFrame{Err: context.Canceled, UnitID: 1}, Downstream, w.index+1, gen)
			w.mu.Lock()
			attempt, progress := tc.args(u, attemptBefore)
			w.mu.Unlock()
			w.expire(u, attempt, progress) // panics on Candidates[-1] without the guards
			w.mu.Lock()
			pos, bad := u.pos, len(w.bad)
			w.mu.Unlock()
			close(sink.release)
			if pos != 1 || bad != 1 {
				t.Fatalf("pos=%d demoted=%d, want 1 and 1: the stale expiry moved the unit", pos, bad)
			}
		})
	}
}
