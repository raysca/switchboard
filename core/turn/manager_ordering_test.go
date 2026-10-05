package turn

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

// A queued due evaluation must survive controls which do not change its input,
// while real changes must be reflected in the next serialized callback.
func TestTurnManagerQueuedDetectionStateChanges(t *testing.T) {
	cases := []struct {
		name         string
		action       func(*Manager, time.Time) error
		wantErr      error
		wantComplete bool
		wantSemantic bool
	}{
		{name: "no-op mute", wantComplete: true, action: func(m *Manager, at time.Time) error { return m.SetMuted(false, at) }},
		{name: "later no-op mute", wantComplete: true, action: func(m *Manager, at time.Time) error { return m.SetMuted(false, at.Add(time.Second)) }},
		{name: "identity-free idle", wantComplete: true, action: func(m *Manager, at time.Time) error { _, err := m.Observe(Observation{At: at}); return err }},
		{name: "later identity-free activity", wantComplete: true, action: func(m *Manager, at time.Time) error {
			_, err := m.Observe(Observation{At: at.Add(time.Second), Activity: true})
			return err
		}},
		{name: "rejected speech", wantComplete: true, wantErr: ErrInvalidObservation, action: func(m *Manager, at time.Time) error {
			_, err := m.Observe(Observation{At: at, Speaking: true})
			return err
		}},
		{name: "wrong turn", wantComplete: true, action: func(m *Manager, at time.Time) error {
			_, err := m.Observe(observation(at, "other", 1, false))
			return err
		}},
		{name: "wrong interrupt", wantComplete: true, action: func(m *Manager, at time.Time) error { _, err := m.Interrupt("other", 1, at); return err }},
		{name: "duplicate advance", wantComplete: true, action: func(m *Manager, at time.Time) error { _, err := m.Advance(at); return err }},
		{name: "duplicate start", wantComplete: true, action: func(m *Manager, at time.Time) error { return m.Start(at) }},
		{name: "activity", wantComplete: true, action: func(m *Manager, at time.Time) error {
			_, err := m.Observe(Observation{At: at, TurnID: "queued", Generation: 1, Activity: true})
			return err
		}},
		{name: "semantic change", wantComplete: true, wantSemantic: true, action: func(m *Manager, at time.Time) error {
			_, err := m.Observe(Observation{At: at, TurnID: "queued", Generation: 1, Semantic: &SemanticEvidence{At: at, TurnID: "queued", Generation: 1, Confidence: .9}})
			return err
		}},
		{name: "speech resumes", action: func(m *Manager, at time.Time) error {
			_, err := m.Observe(observation(at, "queued", 1, true))
			return err
		}},
		{name: "mute", action: func(m *Manager, at time.Time) error { return m.SetMuted(true, at) }},
		{name: "mute then unmute", action: func(m *Manager, at time.Time) error {
			if err := m.SetMuted(true, at); err != nil {
				return err
			}
			return m.SetMuted(false, at)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var callbacks atomic.Int32
			var concurrent atomic.Bool
			var inputs []DetectionInput
			detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
				if callbacks.Add(1) != 1 {
					concurrent.Store(true)
				}
				defer callbacks.Add(-1)
				inputs = append(inputs, input)
				if len(inputs) == 1 {
					close(entered)
					<-release
				}
				return (SilenceDetector{}).Detect(input)
			})
			m := manager(t, detector)
			_, _ = m.Observe(observation(t0, "queued", 1, true))
			_, _ = m.Advance(t0.Add(250 * time.Millisecond))
			_, _ = m.Observe(observation(t0.Add(time.Second), "queued", 1, false))
			done := make(chan []core.Frame, 1)
			go func() { frames, _ := m.Advance(t0.Add(2200 * time.Millisecond)); done <- frames }()
			<-entered
			at := t0.Add(2300 * time.Millisecond)
			queued, queueErr := m.Advance(at)
			actionErr := tc.action(m, at)
			close(release)
			var frames []core.Frame
			select {
			case frames = <-done:
			case <-time.After(time.Second):
				t.Fatal("detector drain blocked")
			}
			if queueErr != nil || !errors.Is(actionErr, tc.wantErr) {
				t.Fatalf("queue=%v action=%v", queueErr, actionErr)
			}
			events := append(eventFrames(frames), eventFrames(queued)...)
			completed := findEvent(events, core.TurnEventCompleted)
			if (completed != nil) != tc.wantComplete {
				t.Fatalf("completion=%+v, want completion=%v; callbacks=%d", completed, tc.wantComplete, len(inputs))
			}
			if concurrent.Load() || len(inputs) > 2 {
				t.Fatalf("callbacks overlapped or exceeded bound: count=%d overlap=%v", len(inputs), concurrent.Load())
			}
			if tc.wantComplete {
				if len(inputs) != 2 || !completed.At.Equal(at) {
					t.Fatalf("queued input lost or altered: inputs=%+v completion=%+v", inputs, completed)
				}
				if (inputs[1].Semantic != nil) != tc.wantSemantic {
					t.Fatalf("queued callback used stale semantic state: %+v", inputs[1])
				}
			}
			if tc.name == "mute then unmute" && (len(inputs) != 2 || inputs[1].SilenceFor != 0) {
				t.Fatalf("unmute did not requeue fresh silence: %+v", inputs)
			}
		})
	}
}

func TestTurnManagerIdenticalNegativeDetectionIsReused(t *testing.T) {
	var m *Manager
	calls := 0
	detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
		calls++
		_, _ = m.Observe(Observation{At: input.At, TurnID: input.TurnID, Generation: input.Generation, Activity: true})
		return DetectionDecision{}, false
	})
	m = manager(t, detector)
	_, _ = m.Observe(observation(t0, "same", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "same", 1, false))
	frames, err := m.Advance(t0.Add(2200 * time.Millisecond))
	if err != nil || calls != 1 || findEvent(eventFrames(frames), core.TurnEventCompleted) != nil {
		t.Fatalf("identical negative result was reevaluated: calls=%d frames=%+v err=%v", calls, eventFrames(frames), err)
	}
}

func TestTurnManagerRejectedObservationPreservesTimestamp(t *testing.T) {
	for _, rejected := range []Observation{
		{At: t0.Add(5 * time.Second)},
		{At: t0.Add(5 * time.Second), Activity: true},
		{At: t0.Add(5 * time.Second), TurnID: "other", Generation: 1},
	} {
		m := manager(t, SilenceDetector{})
		_, _ = m.Observe(observation(t0, "current", 1, true))
		_, _ = m.Advance(t0.Add(250 * time.Millisecond))
		if _, err := m.Observe(rejected); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Advance(t0.Add(time.Second)); err != nil {
			t.Fatalf("rejected observation advanced timestamp: %+v: %v", rejected, err)
		}
	}
}
