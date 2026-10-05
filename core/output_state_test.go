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
