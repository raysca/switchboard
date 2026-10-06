package core

import (
	"testing"
	"time"
)

func TestSpeakingTrackerStartsFromWrittenAudioAndStopsOnce(t *testing.T) {
	tracker := NewSpeakingTracker()

	started, ok := tracker.ObserveWrite("turn-1", 1, OutputInterruptible)
	if !ok || started.State != SpeakingStarted || started.Turn != "turn-1" || started.Gen != 1 {
		t.Fatalf("unexpected start transition: %#v, ok=%v", started, ok)
	}
	if _, ok := tracker.ObserveWrite("turn-1", 1, OutputInterruptible); ok {
		t.Fatal("a second write in the same turn must not restart speaking")
	}

	stopped, ok := tracker.StopThrough(1, SpeakingStopDrained)
	if !ok || stopped.State != SpeakingStopped || stopped.Reason != SpeakingStopDrained {
		t.Fatalf("unexpected stop transition: %#v, ok=%v", stopped, ok)
	}
	if _, ok := tracker.StopThrough(1, SpeakingStopFlushed); ok {
		t.Fatal("speaking must stop exactly once")
	}
}

func TestSpeakingTrackerFlushPreservesNewGenerationAndUninterruptibleOutput(t *testing.T) {
	tracker := NewSpeakingTracker()
	if _, ok := tracker.ObserveWrite("critical", 1, OutputUninterruptible); !ok {
		t.Fatal("expected critical output to start speaking")
	}
	if _, ok := tracker.StopThrough(1, SpeakingStopFlushed); ok {
		t.Fatal("flush must not stop explicitly uninterruptible output")
	}
	if _, ok := tracker.Stop(SpeakingStopDrained); !ok {
		t.Fatal("final drain must stop uninterruptible output")
	}

	if _, ok := tracker.ObserveWrite("turn-2", 2, OutputInterruptible); !ok {
		t.Fatal("expected new generation to start speaking")
	}
	if _, ok := tracker.StopThrough(1, SpeakingStopFlushed); ok {
		t.Fatal("an old flush must not stop newer-generation speaking")
	}
	if !tracker.IsSpeaking() {
		t.Fatal("new generation should remain speaking")
	}
}

func TestSpeakingTrackerLateDrainCannotStopDifferentOutput(t *testing.T) {
	tracker := NewSpeakingTracker()
	if _, ok := tracker.ObserveWrite("turn-2", 2, OutputInterruptible); !ok {
		t.Fatal("expected start")
	}
	if _, ok := tracker.StopOutput("turn-1", 1, SpeakingStopDrained); ok {
		t.Fatal("late drain stopped newer output")
	}
	if !tracker.IsSpeaking() {
		t.Fatal("newer output should remain speaking")
	}
	if _, ok := tracker.StopOutput("turn-2", 2, SpeakingStopDrained); !ok {
		t.Fatal("matching drain did not stop output")
	}
}

func TestFakeLLMEmitsWholeTurnCompletion(t *testing.T) {
	llm := NewFakeLLMProcessor(FakeLLMConfig{ResponseChunks: []string{"hello ", "world"}})
	sink := &PassthroughProcessor{}
	pipe, err := NewPipeline(llm, sink)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(1, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&UserMessageFrame{Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range sink.GetFrames() {
			if final, ok := frame.(*LLMTextFrame); ok {
				if final.Text != "hello world" || final.Turn != "turn-1" || final.Gen != 1 {
					t.Fatalf("unexpected completion: %#v", final)
				}
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("fake LLM did not emit a whole-turn completion frame")
}

func TestAssistantContextEmitsExplicitFinalMarkerForAlreadyStreamedUnit(t *testing.T) {
	contextStore := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	assistant := NewAssistantContextProcessor(contextStore, DefaultAssistantContextConfig())
	sink := &PassthroughProcessor{}
	pipe, err := NewPipeline(assistant, sink)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(2, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&UserMessageFrame{Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&LLMTextDeltaFrame{Delta: "Hello world.", Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&LLMTextFrame{Text: "Hello world.", Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var units []*SynthesisUnitFrame
		var marker *OutputTurnFinalFrame
		for _, frame := range sink.GetFrames() {
			if unit, ok := frame.(*SynthesisUnitFrame); ok {
				units = append(units, unit)
			}
			if final, ok := frame.(*OutputTurnFinalFrame); ok {
				marker = final
			}
		}
		if marker != nil {
			if len(units) != 1 || units[0].IsFinal {
				t.Fatalf("streamed synthesis units = %#v, want one unit-final without premature whole-turn finality", units)
			}
			if marker.Turn != "turn-1" || marker.Gen != 1 || marker.UnitID != units[0].UnitID {
				t.Fatalf("unexpected whole-turn marker: %#v", marker)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("assistant context did not emit an explicit whole-turn final marker")
}

func TestAssistantContextUnitIDsAreSessionMonotonicAcrossTurns(t *testing.T) {
	contextStore := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	assistant := NewAssistantContextProcessor(contextStore, DefaultAssistantContextConfig())
	sink := &PassthroughProcessor{}
	pipe, err := NewPipeline(assistant, sink)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(12, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for i, turn := range []string{"turn-1", "turn-2"} {
		if err := session.Post(&UserMessageFrame{Turn: turn, Gen: 1}); err != nil {
			t.Fatal(err)
		}
		text := []string{"First response.", "Second response."}[i]
		if err := session.Post(&LLMTextFrame{Text: text, Turn: turn, Gen: 1}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var ids []uint64
		for _, frame := range sink.GetFrames() {
			if unit, ok := frame.(*SynthesisUnitFrame); ok {
				ids = append(ids, unit.UnitID)
			}
		}
		if len(ids) == 2 {
			if ids[0] != 1 || ids[1] != 2 {
				t.Fatalf("unit IDs = %v, want globally monotonic [1 2]", ids)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("did not observe both synthesis units")
}

func TestPlaybackTracker_OrderedLifecycleAndTransitions(t *testing.T) {
	tracker := NewPlaybackTracker()

	// 1. Initial generating transition
	f, ok := tracker.ObserveGenerating("turn-1", 1)
	if !ok || f.State != PlaybackGenerating || f.Turn != "turn-1" || f.Gen != 1 {
		t.Fatalf("unexpected generating transition: %+v, ok=%v", f, ok)
	}
	if tracker.CurrentState() != PlaybackGenerating {
		t.Fatalf("expected state generating, got %s", tracker.CurrentState())
	}

	// 2. Audio buffered
	f, ok = tracker.ObserveBuffered("turn-1", 1, 101)
	if !ok || f.State != PlaybackBuffered || f.UnitID != 101 {
		t.Fatalf("unexpected buffered transition: %+v, ok=%v", f, ok)
	}
	// Redundant buffer in same turn is a no-op
	if _, ok := tracker.ObserveBuffered("turn-1", 1, 102); ok {
		t.Fatal("redundant buffer should not emit state transition")
	}

	// 3. Audio playing (first audio dispatched)
	f, ok = tracker.ObservePlaying("turn-1", 1, 101)
	if !ok || f.State != PlaybackPlaying || f.UnitID != 101 {
		t.Fatalf("unexpected playing transition: %+v, ok=%v", f, ok)
	}
	// Redundant play is a no-op
	if _, ok := tracker.ObservePlaying("turn-1", 1, 102); ok {
		t.Fatal("redundant play should not emit state transition")
	}

	// 4. Pacer queue drained
	f, ok = tracker.ObserveDrained("turn-1", 1)
	if !ok || f.State != PlaybackDrained {
		t.Fatalf("unexpected drained transition: %+v, ok=%v", f, ok)
	}

	// 5. Completed
	f, ok = tracker.ObserveCompleted("turn-1", 1)
	if !ok || f.State != PlaybackCompleted {
		t.Fatalf("unexpected completed transition: %+v, ok=%v", f, ok)
	}
	if tracker.CurrentState() != PlaybackCompleted {
		t.Fatalf("expected completed state, got %s", tracker.CurrentState())
	}

	// Stale transition after completion rejected
	if _, ok := tracker.ObservePlaying("turn-1", 1, 103); ok {
		t.Fatal("transition after completion should be rejected")
	}
}

func TestPlaybackTracker_InterruptionTransition(t *testing.T) {
	tracker := NewPlaybackTracker()
	_, _ = tracker.ObserveGenerating("turn-2", 1)
	_, _ = tracker.ObserveBuffered("turn-2", 1, 201)
	_, _ = tracker.ObservePlaying("turn-2", 1, 201)

	// Mid-stream interruption
	f, ok := tracker.ObserveInterrupted("turn-2", 1)
	if !ok || f.State != PlaybackInterrupted {
		t.Fatalf("unexpected interrupted transition: %+v, ok=%v", f, ok)
	}
	if tracker.CurrentState() != PlaybackInterrupted {
		t.Fatalf("expected interrupted state, got %s", tracker.CurrentState())
	}

	// Further buffer/playing for interrupted turn is rejected
	if _, ok := tracker.ObservePlaying("turn-2", 1, 202); ok {
		t.Fatal("playing after interruption should be rejected")
	}
	if _, ok := tracker.ObserveDrained("turn-2", 1); ok {
		t.Fatal("drain after interruption should be rejected")
	}

	// Next generation restarts cleanly
	f, ok = tracker.ObserveGenerating("turn-3", 2)
	if !ok || f.State != PlaybackGenerating || f.Turn != "turn-3" || f.Gen != 2 {
		t.Fatalf("unexpected restart transition: %+v, ok=%v", f, ok)
	}
}

func TestPlaybackTracker_EmptyOutputDirectCompletion(t *testing.T) {
	// A turn with empty bot output (e.g. tool execution turn or silent response)
	tracker := NewPlaybackTracker()
	_, _ = tracker.ObserveGenerating("turn-empty", 1)

	// No audio ever buffered or played; transitions straight to completed
	f, ok := tracker.ObserveCompleted("turn-empty", 1)
	if !ok || f.State != PlaybackCompleted {
		t.Fatalf("unexpected empty completion: %+v, ok=%v", f, ok)
	}
	if tracker.CurrentState() != PlaybackCompleted {
		t.Fatalf("expected completed, got %s", tracker.CurrentState())
	}
}
