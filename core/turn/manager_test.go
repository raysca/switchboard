package turn

import (
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
	if _, err := NewHybridDetector(SilenceDetector{}, time.Second, math.NaN()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NaN semantic threshold: got %v", err)
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

func TestHybridDetectorEarlyCompletionAndSafeSilenceFallback(t *testing.T) {
	hybrid, err := NewHybridDetector(SilenceDetector{}, 300*time.Millisecond, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	m := manager(t, hybrid)
	_, _ = m.Observe(observation(t0, "semantic", 1, true))
	_, _ = m.Advance(t0.Add(250 * time.Millisecond))
	ended := observation(t0.Add(time.Second), "semantic", 1, false)
	_, _ = m.Observe(ended)
	ended.Semantic = &SemanticEvidence{TurnID: "semantic", Generation: 1, At: t0.Add(1100 * time.Millisecond), Confidence: 0.95}
	_, _ = m.Observe(ended)
	frames, err := m.Advance(t0.Add(1400 * time.Millisecond))
	events := eventFrames(frames)
	completed := findEvent(events, core.TurnEventCompleted)
	if err != nil || completed == nil || completed.Reason != core.TurnReasonSemantic || !completed.HasConfidence || completed.Confidence != 0.95 {
		t.Fatalf("valid semantic evidence should complete early: events=%+v err=%v", events, err)
	}

	fallback := manager(t, hybrid)
	_, _ = fallback.Observe(observation(t0, "fallback", 1, true))
	_, _ = fallback.Advance(t0.Add(250 * time.Millisecond))
	_, _ = fallback.Observe(observation(t0.Add(time.Second), "fallback", 1, false))
	_, _ = fallback.Observe(Observation{At: t0.Add(1100 * time.Millisecond), TurnID: "fallback", Generation: 1, Semantic: &SemanticEvidence{TurnID: "fallback", Generation: 9, At: t0.Add(1100 * time.Millisecond), Confidence: 0.99}})
	frames, err = fallback.Advance(t0.Add(2200 * time.Millisecond))
	events = eventFrames(frames)
	completed = findEvent(events, core.TurnEventCompleted)
	if err != nil || completed == nil || completed.Reason != core.TurnReasonSilence || completed.HasConfidence {
		t.Fatalf("stale semantic evidence must safely fall back to silence: events=%+v err=%v", events, err)
	}
}

func TestHybridDetectorRejectsUnavailableInvalidStaleAndLowConfidenceEvidence(t *testing.T) {
	detector, err := NewHybridDetector(SilenceDetector{}, 300*time.Millisecond, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(2 * time.Second)
	base := DetectionInput{
		At: now, TurnStartedAt: t0, TurnID: "turn", Generation: 4,
		SilenceFor: 500 * time.Millisecond, SilenceAfter: time.Second, SemanticMaxAge: time.Second,
	}
	evidence := func(id core.TurnID, gen core.Generation, at time.Time, confidence float64) *SemanticEvidence {
		return &SemanticEvidence{TurnID: id, Generation: gen, At: at, Confidence: confidence}
	}
	cases := []struct {
		name string
		item *SemanticEvidence
	}{
		{name: "unavailable"},
		{name: "wrong turn", item: evidence("other", 4, now, 0.99)},
		{name: "stale generation", item: evidence("turn", 3, now, 0.99)},
		{name: "future timestamp", item: evidence("turn", 4, now.Add(time.Nanosecond), 0.99)},
		{name: "expired", item: evidence("turn", 4, now.Add(-2*time.Second), 0.99)},
		{name: "low confidence", item: evidence("turn", 4, now, 0.79)},
		{name: "invalid confidence", item: evidence("turn", 4, now, math.NaN())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			input.Semantic = tc.item
			if decision, done := detector.Detect(input); done {
				t.Fatalf("invalid evidence completed early: %+v", decision)
			}
		})
	}
	base.Semantic, base.SilenceFor = nil, time.Second
	decision, done := detector.Detect(base)
	if !done || decision.Reason != core.TurnReasonSilence || decision.HasConfidence {
		t.Fatalf("all unavailable/invalid evidence must preserve silence fallback: %+v done=%v", decision, done)
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

func TestTurnEventFrameCarriesTypedIdentity(t *testing.T) {
	var frame core.Frame = &core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: core.TurnID("typed"), Gen: core.Generation(7), At: t0}
	var generational core.GenerationalFrame = frame.(core.GenerationalFrame)
	if frame.Type() != core.FrameTurnEvent || generational.Generation() != 7 {
		t.Fatalf("unexpected framework event: type=%v generation=%d", frame.Type(), generational.Generation())
	}
}
