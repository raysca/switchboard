package turn

import (
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		InitialSilenceTimeout: 5 * time.Second,
		SilenceTimeout:        time.Second,
		MaxTurnDuration:       10 * time.Second,
		StartDebounce:         100 * time.Millisecond,
		StopDebounce:          100 * time.Millisecond,
		MinimumSpeechDuration: 200 * time.Millisecond,
		SemanticMaxAge:        time.Second,
		MaxRetiredTurns:       4,
	}
}

func manager(t *testing.T, detector TurnDetector) *Manager {
	t.Helper()
	m, err := NewManager(testConfig(), detector)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t0); err != nil {
		t.Fatal(err)
	}
	return m
}

func observation(at time.Time, id core.TurnID, gen core.Generation, speaking bool) Observation {
	return Observation{At: at, TurnID: id, Generation: gen, Speaking: speaking}
}

func eventFrames(frames []core.Frame) []*core.TurnEventFrame {
	out := make([]*core.TurnEventFrame, 0, len(frames))
	for _, frame := range frames {
		if event, ok := frame.(*core.TurnEventFrame); ok {
			out = append(out, event)
		}
	}
	return out
}

func TestTurnManagerRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig()
	cfg.SilenceTimeout = -time.Second
	if _, err := NewManager(cfg, SilenceDetector{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative silence timeout: got %v", err)
	}
	cfg = testConfig()
	cfg.MaxRetiredTurns = 0
	if _, err := NewManager(cfg, SilenceDetector{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero retired-turn capacity: got %v", err)
	}
}

func TestNoSpeechTimeoutResetsOnActivity(t *testing.T) {
	m := manager(t, SilenceDetector{})
	frames, err := m.Observe(Observation{At: t0.Add(4 * time.Second), Activity: true})
	if err != nil || len(eventFrames(frames)) != 0 {
		t.Fatalf("activity should reset initial silence: frames=%v err=%v", frames, err)
	}
	frames, err = m.Advance(t0.Add(8 * time.Second))
	if err != nil || len(eventFrames(frames)) != 0 {
		t.Fatalf("timeout fired before reset deadline: frames=%v err=%v", frames, err)
	}
	frames, err = m.Advance(t0.Add(9 * time.Second))
	events := eventFrames(frames)
	if err != nil || len(events) != 1 || events[0].Kind != core.TurnEventNoSpeechTimeout || events[0].At != t0.Add(9*time.Second) {
		t.Fatalf("expected one timestamped initial no-speech timeout: events=%+v err=%v", events, err)
	}
	frames, _ = m.Advance(t0.Add(20 * time.Second))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("initial no-speech timeout repeated")
	}
}

func TestTurnDetectorCompletesOnceAfterMidCallSilence(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "turn-a", 1, true))
	started, err := m.Advance(t0.Add(250 * time.Millisecond))
	if err != nil || len(eventFrames(started)) != 1 || eventFrames(started)[0].Kind != core.TurnEventStarted {
		t.Fatalf("speech start not debounced: events=%+v err=%v", eventFrames(started), err)
	}
	_, _ = m.Observe(observation(t0.Add(time.Second), "turn-a", 1, false))
	frames, err := m.Advance(t0.Add(2200 * time.Millisecond))
	events := eventFrames(frames)
	completed := findEvent(events, core.TurnEventCompleted)
	if err != nil || len(events) != 2 || findEvent(events, core.TurnEventStopped) == nil || completed == nil || completed.Turn != core.TurnID("turn-a") || completed.Gen != 1 || completed.Reason != core.TurnReasonSilence || completed.At != t0.Add(2200*time.Millisecond) || completed.HasConfidence {
		t.Fatalf("unexpected completion: events=%+v err=%v", events, err)
	}
	frames, _ = m.Advance(t0.Add(4 * time.Second))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("completion repeated")
	}
}

func TestTurnManagerDebouncesAndRejectsFalseStart(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "false", 1, true))
	frames, _ := m.Observe(observation(t0.Add(80*time.Millisecond), "false", 1, false))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("short VAD burst emitted a turn event")
	}
	frames, _ = m.Advance(t0.Add(time.Second))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("false start completed after its source stopped")
	}
}

func TestTurnManagerStopDebounceDoesNotCompleteBeforeStableStop(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "stable", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "stable", 1, false))
	frames, err := m.Advance(t0.Add(time.Second + 99*time.Millisecond))
	if err != nil || len(eventFrames(frames)) != 0 {
		t.Fatalf("stop debounce fired early: events=%+v err=%v", eventFrames(frames), err)
	}
	frames, err = m.Advance(t0.Add(time.Second + 100*time.Millisecond))
	if err != nil || findEvent(eventFrames(frames), core.TurnEventStopped) == nil || findEvent(eventFrames(frames), core.TurnEventCompleted) != nil {
		t.Fatalf("stable stop should begin, but not finish, silence timing: events=%+v err=%v", eventFrames(frames), err)
	}
}

func TestTurnManagerContinuousIdleSnapshotsPreserveSilenceOrigin(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "idle-stream", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "idle-stream", 1, false))
	var stopped, completed int
	for at := t0.Add(time.Second + 100*time.Millisecond); !at.After(t0.Add(3 * time.Second)); at = at.Add(100 * time.Millisecond) {
		frames, err := m.Observe(observation(at, "idle-stream", 1, false))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range eventFrames(frames) {
			switch event.Kind {
			case core.TurnEventStopped:
				stopped++
			case core.TurnEventCompleted:
				completed++
			}
		}
	}
	if stopped != 1 || completed != 1 {
		t.Fatalf("continuous idle must preserve one stop and one silence completion; stopped=%d completed=%d", stopped, completed)
	}
}

func TestTurnManagerMinimumSpeechDuration(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "short", 1, true))
	_, _ = m.Observe(observation(t0.Add(150*time.Millisecond), "short", 1, true))
	frames, _ := m.Observe(observation(t0.Add(150*time.Millisecond), "short", 1, false))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("speech shorter than minimum was accepted")
	}
}

func TestTurnManagerMaximumActiveDuration(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "long", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	frames, err := m.Advance(t0.Add(11 * time.Second))
	events := eventFrames(frames)
	if err != nil || len(events) != 1 || events[0].Kind != core.TurnEventCompleted || events[0].Reason != core.TurnReasonMaxDuration {
		t.Fatalf("active duration timeout missing: events=%+v err=%v", events, err)
	}
}

func TestTurnManagerIgnoresDuplicateLateAndStaleObservations(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "one", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "one", 1, false))
	frames, _ := m.Advance(t0.Add(2200 * time.Millisecond))
	if findEvent(eventFrames(frames), core.TurnEventCompleted) == nil {
		t.Fatal("expected first completion")
	}
	frames, err := m.Observe(observation(t0.Add(1500*time.Millisecond), "one", 1, true))
	if err != nil || len(eventFrames(frames)) != 0 {
		t.Fatalf("late old-turn observation changed decision: events=%+v err=%v", eventFrames(frames), err)
	}
	_, _ = m.Observe(observation(t0.Add(3*time.Second), "two", 2, true))
	frames, _ = m.Observe(observation(t0.Add(4*time.Second), "one", 1, false))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("stale generation affected the new turn")
	}
}

func TestTurnManagerStaleTurnCannotStopCurrentTurn(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "current", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	frames, err := m.Observe(observation(t0.Add(time.Second), "old", 1, false))
	if err != nil || len(eventFrames(frames)) != 0 {
		t.Fatalf("stale turn observation changed current turn: events=%+v err=%v", eventFrames(frames), err)
	}
	frames, _ = m.Advance(t0.Add(2 * time.Second))
	if findEvent(eventFrames(frames), core.TurnEventCompleted) != nil {
		t.Fatal("stale turn unexpectedly started the silence timer")
	}
}

func TestTurnManagerHigherGenerationFalseSnapshotCancelsOldCandidate(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "candidate-a", 1, true))
	frames, err := m.Observe(observation(t0.Add(50*time.Millisecond), "candidate-b", 2, false))
	if err != nil || findEvent(eventFrames(frames), core.TurnEventStarted) != nil {
		t.Fatalf("false higher-generation observation emitted a start: events=%+v err=%v", eventFrames(frames), err)
	}
	frames, _ = m.Advance(t0.Add(250 * time.Millisecond))
	if started := findEvent(eventFrames(frames), core.TurnEventStarted); started != nil {
		t.Fatalf("old generation candidate survived rollover: %+v", started)
	}
	_, _ = m.Observe(observation(t0.Add(time.Second), "candidate-b", 2, true))
	frames, _ = m.Advance(t0.Add(1250 * time.Millisecond))
	started := findEvent(eventFrames(frames), core.TurnEventStarted)
	if started == nil || started.Turn != "candidate-b" || started.Gen != 2 {
		t.Fatalf("new generation candidate did not start: events=%+v", eventFrames(frames))
	}
}

func TestTurnManagerHigherGenerationAutomaticallyInterruptsActiveTurn(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "active-a", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	frames, err := m.Observe(observation(t0.Add(time.Second), "active-b", 2, true))
	interrupted := findEvent(eventFrames(frames), core.TurnEventInterrupted)
	if err != nil || interrupted == nil || interrupted.Turn != "active-a" || interrupted.Gen != 1 {
		t.Fatalf("generation advance did not interrupt old turn: events=%+v err=%v", eventFrames(frames), err)
	}
}

func TestTurnManagerRetiresAllTerminalTurnsWithinGeneration(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRetiredTurns = 3
	m, err := NewManager(cfg, SilenceDetector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t0); err != nil {
		t.Fatal(err)
	}
	completeTestTurn(t, m, "retired-a", 1, t0)
	_, _ = m.Observe(observation(t0.Add(3*time.Second), "turn-b", 1, true))
	_, _ = m.Advance(t0.Add(3250 * time.Millisecond))
	frames, err := m.Observe(observation(t0.Add(4*time.Second), "retired-a", 1, true))
	if err != nil || len(eventFrames(frames)) != 0 {
		t.Fatalf("retired A altered active B: events=%+v err=%v", eventFrames(frames), err)
	}
	_, _ = m.Observe(observation(t0.Add(5*time.Second), "turn-b", 1, false))
	frames, _ = m.Advance(t0.Add(6200 * time.Millisecond))
	if findEvent(eventFrames(frames), core.TurnEventCompleted) == nil {
		t.Fatal("turn B did not complete")
	}
	frames, err = m.Observe(observation(t0.Add(7*time.Second), "retired-a", 1, true))
	if err != nil || len(eventFrames(frames)) != 0 {
		t.Fatalf("late A revived after B completed: events=%+v err=%v", eventFrames(frames), err)
	}
	frames, _ = m.Advance(t0.Add(7250 * time.Millisecond))
	if findEvent(eventFrames(frames), core.TurnEventStarted) != nil {
		t.Fatal("retired turn A restarted from its old observation")
	}
}

func TestTurnManagerRetiredTurnCapacityFailsClosedAndGenerationClears(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRetiredTurns = 1
	m, err := NewManager(cfg, SilenceDetector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t0); err != nil {
		t.Fatal(err)
	}
	completeTestTurn(t, m, "only-slot", 1, t0)
	frames, err := m.Observe(observation(t0.Add(3*time.Second), "blocked", 1, true))
	if !errors.Is(err, ErrRetiredTurnLimit) || len(eventFrames(frames)) != 0 {
		t.Fatalf("full retirement ledger must reject new same-generation turn: events=%+v err=%v", eventFrames(frames), err)
	}
	_, err = m.Observe(observation(t0.Add(4*time.Second), "new-generation", 2, true))
	if err != nil {
		t.Fatalf("generation advance should clear retired IDs: %v", err)
	}
	frames, err = m.Advance(t0.Add(4250 * time.Millisecond))
	started := findEvent(eventFrames(frames), core.TurnEventStarted)
	if err != nil || started == nil || started.Turn != "new-generation" || started.Gen != 2 {
		t.Fatalf("new generation did not resume after ledger reset: events=%+v err=%v", eventFrames(frames), err)
	}
}

func completeTestTurn(t *testing.T, m *Manager, id core.TurnID, generation core.Generation, start time.Time) {
	t.Helper()
	_, err := m.Observe(observation(start, id, generation, true))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Advance(start.Add(250 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Observe(observation(start.Add(time.Second), id, generation, false))
	if err != nil {
		t.Fatal(err)
	}
	frames, err := m.Advance(start.Add(2200 * time.Millisecond))
	if err != nil || findEvent(eventFrames(frames), core.TurnEventCompleted) == nil {
		t.Fatalf("turn %q did not complete: frames=%+v err=%v", id, eventFrames(frames), err)
	}
}

func TestTurnManagerMutePausesAndUnmuteDoesNotReplay(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "muted", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	err := m.SetMuted(true, t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := m.Observe(observation(t0.Add(30*time.Second), "muted", 1, false))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("muted input advanced turn state")
	}
	if err := m.SetMuted(false, t0.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	frames, _ = m.Advance(t0.Add(31*time.Second + 500*time.Millisecond))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("muted silence was replayed on unmute")
	}
}

func TestTurnManagerInterruptionAndGenerationFence(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "old", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	frames, err := m.Interrupt("old", 1, t0.Add(time.Second))
	if err != nil || len(eventFrames(frames)) != 1 || eventFrames(frames)[0].Kind != core.TurnEventInterrupted {
		t.Fatalf("interruption missing: events=%+v err=%v", eventFrames(frames), err)
	}
	frames, _ = m.Interrupt("old", 1, t0.Add(1100*time.Millisecond))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("duplicate interruption emitted")
	}
	_, _ = m.Observe(observation(t0.Add(2*time.Second), "new", 2, true))
	frames, _ = m.Observe(observation(t0.Add(3*time.Second), "old", 1, false))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("old turn interrupted or completed new generation")
	}
}

func TestTurnManagerRejectsRegressingCurrentTimestamp(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(Observation{At: t0.Add(time.Second), Activity: true})
	if _, err := m.Advance(t0.Add(time.Second - time.Nanosecond)); !errors.Is(err, ErrTimestampRegression) {
		t.Fatalf("regression should be rejected: %v", err)
	}
}

func TestTurnManagerResetIsolationAndCloseIdempotence(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "before", 1, true))
	if err := m.Reset(t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	frames, _ := m.Advance(t0.Add(4 * time.Second))
	if len(eventFrames(frames)) != 0 {
		t.Fatal("reset retained prior turn or timeout state")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal("close should be idempotent:", err)
	}
}

func TestTurnManagerRejectsOperationsAfterClose(t *testing.T) {
	m := manager(t, SilenceDetector{})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close: %v", err)
	}
	if _, err := m.Observe(observation(t0, "closed", 1, true)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Observe after Close: %v", err)
	}
	if _, err := m.Advance(t0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Advance after Close: %v", err)
	}
	if err := m.SetMuted(true, t0); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetMuted after Close: %v", err)
	}
	if _, err := m.Interrupt("closed", 1, t0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Interrupt after Close: %v", err)
	}
	if err := m.Reset(t0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reset after Close: %v", err)
	}
}

func TestTurnDetectorMayReenterManagerControlsWithoutStaleCommit(t *testing.T) {
	cases := []struct {
		name         string
		action       func(*Manager, time.Time) ([]core.Frame, error)
		wantInt      bool
		wantComplete bool
	}{
		{name: "close", action: func(m *Manager, _ time.Time) ([]core.Frame, error) { return nil, m.Close() }},
		{name: "interrupt", wantInt: true, action: func(m *Manager, at time.Time) ([]core.Frame, error) {
			return m.Interrupt("reentrant", 1, at)
		}},
		{name: "mute", action: func(m *Manager, at time.Time) ([]core.Frame, error) { return nil, m.SetMuted(true, at) }},
		{name: "reset", action: func(m *Manager, at time.Time) ([]core.Frame, error) { return nil, m.Reset(at) }},
		{name: "observe", wantComplete: true, action: func(m *Manager, at time.Time) ([]core.Frame, error) {
			return m.Observe(observation(at, "reentrant", 1, false))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *Manager
			callbackResult := make(chan struct {
				frames []core.Frame
				err    error
			}, 1)
			detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
				frames, callbackErr := tc.action(m, input.At)
				callbackResult <- struct {
					frames []core.Frame
					err    error
				}{frames: frames, err: callbackErr}
				return DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: 0.95, HasConfidence: true}, true
			})
			var err error
			m, err = NewManager(testConfig(), detector)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Start(t0); err != nil {
				t.Fatal(err)
			}
			_, _ = m.Observe(observation(t0, "reentrant", 1, true))
			_, _ = m.Advance(t0.Add(250 * time.Millisecond))
			_, _ = m.Observe(observation(t0.Add(time.Second), "reentrant", 1, false))
			type result struct {
				frames []core.Frame
				err    error
			}
			done := make(chan result, 1)
			go func() {
				frames, callErr := m.Advance(t0.Add(2200 * time.Millisecond))
				done <- result{frames: frames, err: callErr}
			}()
			var outer result
			select {
			case got := <-done:
				outer = got
			case <-time.After(300 * time.Millisecond):
				t.Fatal("detector control reentry deadlocked on manager state lock")
			}
			select {
			case nested := <-callbackResult:
				if nested.err != nil {
					t.Fatalf("reentrant control failed: %v", nested.err)
				}
				if tc.wantInt && findEvent(eventFrames(nested.frames), core.TurnEventInterrupted) == nil {
					t.Fatal("reentrant Interrupt did not emit its event")
				}
			case <-time.After(time.Second):
				t.Fatal("detector was not called")
			}
			completed := findEvent(eventFrames(outer.frames), core.TurnEventCompleted)
			if outer.err != nil || (completed != nil) != tc.wantComplete {
				t.Fatalf("unexpected completion after reentrant %s: events=%+v err=%v", tc.name, eventFrames(outer.frames), outer.err)
			}
			if tc.name == "close" {
				if _, err := m.Advance(t0.Add(2300 * time.Millisecond)); !errors.Is(err, ErrClosed) {
					t.Fatalf("Advance after reentrant Close: %v", err)
				}
			}
		})
	}
}

func TestTurnManagerResetClearsPendingDetectionSnapshot(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return (SilenceDetector{}).Detect(input)
	})
	m := manager(t, detector)
	_, _ = m.Observe(observation(t0, "old-a", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "old-a", 1, false))
	oldDone := make(chan []core.Frame, 1)
	go func() {
		frames, _ := m.Advance(t0.Add(2200 * time.Millisecond))
		oldDone <- frames
	}()
	<-entered
	if err := m.Reset(t0); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Observe(observation(t0, "new-b", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "new-b", 1, false))
	newFrames, err := m.Advance(t0.Add(1100 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	oldFrames := <-oldDone
	all := append(eventFrames(oldFrames), eventFrames(newFrames)...)
	if findEvent(all, core.TurnEventCompleted) != nil {
		t.Fatalf("post-reset turn completed from stale pending time: %+v", all)
	}
	for _, event := range all {
		if event.At.After(t0.Add(1100 * time.Millisecond)) {
			t.Fatalf("event timestamp exceeds caller advancement: %+v", event)
		}
	}
}

func TestTurnManagerBoundsAdversarialDetectorCoalescing(t *testing.T) {
	var m *Manager
	calls := 0
	sawInvalidEvidence := false
	detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
		calls++
		if input.Semantic != nil {
			sawInvalidEvidence = true
		}
		if calls > 4 {
			panic("unbounded detector drain")
		}
		next := input.At.Add(time.Nanosecond)
		_, _ = m.Observe(Observation{
			At: next, TurnID: input.TurnID, Generation: input.Generation,
			Semantic: &SemanticEvidence{TurnID: input.TurnID, Generation: input.Generation, At: next, Confidence: math.NaN()},
		})
		return DetectionDecision{}, false
	})
	var err error
	m, err = NewManager(testConfig(), detector)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Start(t0)
	_, _ = m.Observe(observation(t0, "bounded", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "bounded", 1, false))
	frames, err := m.Advance(t0.Add(2200 * time.Millisecond))
	completed := findEvent(eventFrames(frames), core.TurnEventCompleted)
	if err != nil || completed == nil || completed.Reason != core.TurnReasonSilence || calls != 2 {
		t.Fatalf("bounded fallback failed: events=%+v calls=%d err=%v", eventFrames(frames), calls, err)
	}
	if sawInvalidEvidence {
		t.Fatal("invalid semantic evidence reached detector snapshot")
	}
}

func TestTurnManagerConcurrentMuteInvalidatesBlockedDetector(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	detector := detectorFunc(func(DetectionInput) (DetectionDecision, bool) {
		close(entered)
		<-release
		return DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: 0.9, HasConfidence: true}, true
	})
	m := manager(t, detector)
	_, _ = m.Observe(observation(t0, "blocked", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "blocked", 1, false))
	type result struct {
		frames []core.Frame
		err    error
	}
	done := make(chan result, 1)
	go func() {
		frames, err := m.Advance(t0.Add(2200 * time.Millisecond))
		done <- result{frames: frames, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("detector was not entered")
	}
	muteDone := make(chan error, 1)
	go func() { muteDone <- m.SetMuted(true, t0.Add(2200*time.Millisecond)) }()
	select {
	case err := <-muteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		close(release)
		<-done
		t.Fatal("concurrent state update blocked behind detector")
	}
	close(release)
	got := <-done
	if got.err != nil || findEvent(eventFrames(got.frames), core.TurnEventCompleted) != nil {
		t.Fatalf("blocked stale decision committed after mute: events=%+v err=%v", eventFrames(got.frames), got.err)
	}
}

func TestTurnDetectorReentrantAdvanceDoesNotWaitOnPluginCallback(t *testing.T) {
	var m *Manager
	calls := 0
	detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
		calls++
		if calls == 1 {
			if _, err := m.Advance(input.At.Add(100 * time.Millisecond)); err != nil {
				t.Errorf("reentrant Advance: %v", err)
			}
			return DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: 0.9, HasConfidence: true}, true
		}
		return DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: 0.9, HasConfidence: true}, true
	})
	var err error
	m, err = NewManager(testConfig(), detector)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t0); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Observe(observation(t0, "reentrant-advance", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "reentrant-advance", 1, false))

	done := make(chan struct {
		frames []core.Frame
		err    error
	}, 1)
	go func() {
		frames, callErr := m.Advance(t0.Add(2200 * time.Millisecond))
		done <- struct {
			frames []core.Frame
			err    error
		}{frames: frames, err: callErr}
	}()
	select {
	case result := <-done:
		if result.err != nil || findEvent(eventFrames(result.frames), core.TurnEventCompleted) == nil || calls != 2 {
			t.Fatalf("reentrant Advance was not drained: events=%+v calls=%d err=%v", eventFrames(result.frames), calls, result.err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("reentrant Advance waited for the detector callback")
	}
}

func TestTurnManagerOverlappingAdvancesDrainNewestEvaluation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	detector := detectorFunc(func(DetectionInput) (DetectionDecision, bool) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: 0.9, HasConfidence: true}, true
	})
	m := manager(t, detector)
	_, _ = m.Observe(observation(t0, "overlap", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "overlap", 1, false))
	type result struct {
		frames []core.Frame
		err    error
	}
	firstDone := make(chan result, 1)
	go func() {
		frames, err := m.Advance(t0.Add(2200 * time.Millisecond))
		firstDone <- result{frames: frames, err: err}
	}()
	<-entered
	secondFrames, secondErr := m.Advance(t0.Add(2300 * time.Millisecond))
	close(release)
	first := <-firstDone
	events := append(eventFrames(first.frames), eventFrames(secondFrames)...)
	if first.err != nil || secondErr != nil || calls != 2 || findEvent(events, core.TurnEventStopped) == nil {
		t.Fatalf("overlap did not preserve valid stop and drain newest input: events=%+v calls=%d errors=%v/%v", events, calls, first.err, secondErr)
	}
	completed := 0
	for _, event := range events {
		if event.Kind == core.TurnEventCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("two overlapping advances emitted %d completions, want one: %+v", completed, events)
	}
}

func TestTurnManagerReentrantEpochChangeDiscardsPreCallbackFrames(t *testing.T) {
	cases := []struct {
		name   string
		action func(*Manager, time.Time) ([]core.Frame, error)
	}{
		{name: "reset", action: func(m *Manager, at time.Time) ([]core.Frame, error) { return nil, m.Reset(at) }},
		{name: "close", action: func(m *Manager, _ time.Time) ([]core.Frame, error) { return nil, m.Close() }},
		{name: "interrupt", action: func(m *Manager, at time.Time) ([]core.Frame, error) { return m.Interrupt("epoch", 1, at) }},
		{name: "generation", action: func(m *Manager, at time.Time) ([]core.Frame, error) {
			return m.Observe(observation(at, "next", 2, false))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *Manager
			var nested []core.Frame
			detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
				var err error
				nested, err = tc.action(m, input.At)
				if err != nil {
					t.Errorf("reentrant %s: %v", tc.name, err)
				}
				return DetectionDecision{}, false
			})
			m, _ = NewManager(testConfig(), detector)
			_ = m.Start(t0)
			_, _ = m.Observe(observation(t0, "epoch", 1, true))
			_, _ = m.Advance(t0.Add(250 * time.Millisecond))
			_, _ = m.Observe(observation(t0.Add(time.Second), "epoch", 1, false))
			frames, err := m.Advance(t0.Add(1100 * time.Millisecond))
			if err != nil || len(eventFrames(frames)) != 0 {
				t.Fatalf("stale pre-callback frames escaped after %s: events=%+v err=%v", tc.name, eventFrames(frames), err)
			}
			if tc.name == "interrupt" || tc.name == "generation" {
				events := eventFrames(nested)
				if len(events) != 1 || events[0].Kind != core.TurnEventInterrupted {
					t.Fatalf("interrupt ordering/duplication: %+v", events)
				}
			}
		})
	}
}

func TestTurnDetectorReceivesDefensiveSemanticEvidenceCopy(t *testing.T) {
	var m *Manager
	seen := false
	detector := detectorFunc(func(input DetectionInput) (DetectionDecision, bool) {
		if input.Semantic != nil {
			input.Semantic.Confidence = 0
			seen = true
		}
		return DetectionDecision{}, false
	})
	var err error
	m, err = NewManager(testConfig(), detector)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t0); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Observe(observation(t0, "evidence-copy", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "evidence-copy", 1, false))
	evidence := &SemanticEvidence{TurnID: "evidence-copy", Generation: 1, At: t0.Add(1100 * time.Millisecond), Confidence: 0.95}
	_, _ = m.Observe(Observation{At: t0.Add(1100 * time.Millisecond), TurnID: "evidence-copy", Generation: 1, Semantic: evidence})
	_, err = m.Advance(t0.Add(1200 * time.Millisecond))
	if err != nil || !seen || evidence.Confidence != 0.95 {
		t.Fatalf("detector input was not isolated: seen=%v source confidence=%v err=%v", seen, evidence.Confidence, err)
	}
}

func TestTurnManagerRejectsInvalidDetectorDecisionAndFallsBack(t *testing.T) {
	cases := []struct {
		name     string
		decision DetectionDecision
	}{
		{name: "unsupported reason", decision: DetectionDecision{Reason: core.TurnReasonMaxDuration}},
		{name: "nan confidence", decision: DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: math.NaN(), HasConfidence: true}},
		{name: "confidence above one", decision: DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: 1.1, HasConfidence: true}},
		{name: "confidence below zero", decision: DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: -0.1, HasConfidence: true}},
		{name: "nan hidden by absent flag", decision: DetectionDecision{Reason: core.TurnReasonSilence, Confidence: math.NaN()}},
		{name: "infinity hidden by absent flag", decision: DetectionDecision{Reason: core.TurnReasonSilence, Confidence: math.Inf(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := manager(t, detectorFunc(func(DetectionInput) (DetectionDecision, bool) { return tc.decision, true }))
			_, _ = m.Observe(observation(t0, "invalid-decision", 1, true))
			_, _ = m.Advance(t0.Add(250 * time.Millisecond))
			_, _ = m.Observe(observation(t0.Add(time.Second), "invalid-decision", 1, false))
			frames, err := m.Advance(t0.Add(1500 * time.Millisecond))
			if err != nil || findEvent(eventFrames(frames), core.TurnEventCompleted) != nil {
				t.Fatalf("invalid detector result ended turn early: events=%+v err=%v", eventFrames(frames), err)
			}
			frames, err = m.Advance(t0.Add(2200 * time.Millisecond))
			completed := findEvent(eventFrames(frames), core.TurnEventCompleted)
			if err != nil || completed == nil || completed.Reason != core.TurnReasonSilence {
				t.Fatalf("invalid suggestion did not fall back to silence: events=%+v err=%v", eventFrames(frames), err)
			}
			if completed.HasConfidence || completed.Confidence != 0 {
				t.Fatalf("absent confidence was not normalized: %+v", completed)
			}
			if _, err := json.Marshal(completed); err != nil {
				t.Fatalf("terminal event is not JSON-safe: %v", err)
			}
		})
	}
}

func TestTurnManagerDetectorPanicFallsBackToSilence(t *testing.T) {
	m := manager(t, detectorFunc(func(DetectionInput) (DetectionDecision, bool) { panic("detector failure") }))
	_, _ = m.Observe(observation(t0, "panic-detector", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "panic-detector", 1, false))
	frames, err := m.Advance(t0.Add(2200 * time.Millisecond))
	completed := findEvent(eventFrames(frames), core.TurnEventCompleted)
	if err != nil || completed == nil || completed.Reason != core.TurnReasonSilence {
		t.Fatalf("detector panic must not strand the turn: events=%+v err=%v", eventFrames(frames), err)
	}
}

func TestTurnManagerConcurrentCompletionIsOneShot(t *testing.T) {
	m := manager(t, SilenceDetector{})
	_, _ = m.Observe(observation(t0, "concurrent", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	_, _ = m.Observe(observation(t0.Add(time.Second), "concurrent", 1, false))
	const callers = 16
	results := make(chan []*core.TurnEventFrame, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames, _ := m.Advance(t0.Add(2200 * time.Millisecond))
			results <- eventFrames(frames)
		}()
	}
	wg.Wait()
	close(results)
	completed := 0
	for events := range results {
		if findEvent(events, core.TurnEventCompleted) != nil {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("concurrent advances emitted %d completions, want one", completed)
	}
}

func findEvent(events []*core.TurnEventFrame, kind core.TurnEventKind) *core.TurnEventFrame {
	for _, event := range events {
		if event.Kind == kind {
			return event
		}
	}
	return nil
}

type detectorFunc func(DetectionInput) (DetectionDecision, bool)

func (f detectorFunc) Detect(input DetectionInput) (DetectionDecision, bool) { return f(input) }

func TestTurnEventFrameCarriesTypedIdentity(t *testing.T) {
	var frame core.Frame = &core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: core.TurnID("typed"), Gen: core.Generation(7), At: t0}
	var generational core.GenerationalFrame = frame.(core.GenerationalFrame)
	if frame.Type() != core.FrameTurnEvent || generational.Generation() != 7 {
		t.Fatalf("unexpected framework event: type=%v generation=%d", frame.Type(), generational.Generation())
	}
}
