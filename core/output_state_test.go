package core_test

import (
	"govoice/core"
	"govoice/core/coretest"

	"testing"
	"time"
)

func TestSpeakingTrackerStartsFromWrittenAudioAndStopsOnce(t *testing.T) {
	tracker := core.NewSpeakingTracker()

	started, ok := tracker.ObserveWrite("turn-1", 1, core.OutputInterruptible)
	if !ok || started.State != core.SpeakingStarted || started.Turn != "turn-1" || started.Gen != 1 {
		t.Fatalf("unexpected start transition: %#v, ok=%v", started, ok)
	}
	if _, ok := tracker.ObserveWrite("turn-1", 1, core.OutputInterruptible); ok {
		t.Fatal("a second write in the same turn must not restart speaking")
	}

	stopped, ok := tracker.StopThrough(1, core.SpeakingStopDrained)
	if !ok || stopped.State != core.SpeakingStopped || stopped.Reason != core.SpeakingStopDrained {
		t.Fatalf("unexpected stop transition: %#v, ok=%v", stopped, ok)
	}
	if _, ok := tracker.StopThrough(1, core.SpeakingStopFlushed); ok {
		t.Fatal("speaking must stop exactly once")
	}
}

func TestSpeakingTrackerFlushPreservesNewGenerationAndUninterruptibleOutput(t *testing.T) {
	tracker := core.NewSpeakingTracker()
	if _, ok := tracker.ObserveWrite("critical", 1, core.OutputUninterruptible); !ok {
		t.Fatal("expected critical output to start speaking")
	}
	if _, ok := tracker.StopThrough(1, core.SpeakingStopFlushed); ok {
		t.Fatal("flush must not stop explicitly uninterruptible output")
	}
	if _, ok := tracker.Stop(core.SpeakingStopDrained); !ok {
		t.Fatal("final drain must stop uninterruptible output")
	}

	if _, ok := tracker.ObserveWrite("turn-2", 2, core.OutputInterruptible); !ok {
		t.Fatal("expected new generation to start speaking")
	}
	if _, ok := tracker.StopThrough(1, core.SpeakingStopFlushed); ok {
		t.Fatal("an old flush must not stop newer-generation speaking")
	}
}

func TestSpeakingTrackerLateDrainCannotStopDifferentOutput(t *testing.T) {
	tracker := core.NewSpeakingTracker()
	if _, ok := tracker.ObserveWrite("turn-2", 2, core.OutputInterruptible); !ok {
		t.Fatal("expected start")
	}
	if _, ok := tracker.StopOutput("turn-1", 1, core.SpeakingStopDrained); ok {
		t.Fatal("late drain stopped newer output")
	}
	if _, ok := tracker.StopOutput("turn-2", 2, core.SpeakingStopDrained); !ok {
		t.Fatal("matching drain did not stop output")
	}
}

func TestFakeLLMEmitsWholeTurnCompletion(t *testing.T) {
	llm := coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"hello ", "world"}})
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(llm, sink)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range sink.GetFrames() {
			if final, ok := frame.(*core.LLMTextFrame); ok {
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
	contextStore := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	assistant := core.NewAssistantContextProcessor(contextStore, core.DefaultAssistantContextConfig())
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(assistant, sink)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(2, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&core.LLMTextDeltaFrame{Delta: "Hello world.", Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&core.LLMTextFrame{Text: "Hello world.", Turn: "turn-1", Gen: 1}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var units []*core.SynthesisUnitFrame
		var marker *core.OutputTurnFinalFrame
		for _, frame := range sink.GetFrames() {
			if unit, ok := frame.(*core.SynthesisUnitFrame); ok {
				units = append(units, unit)
			}
			if final, ok := frame.(*core.OutputTurnFinalFrame); ok {
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
	contextStore := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	assistant := core.NewAssistantContextProcessor(contextStore, core.DefaultAssistantContextConfig())
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(assistant, sink)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(12, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for i, turn := range []string{"turn-1", "turn-2"} {
		if err := session.Post(&core.UserMessageFrame{Turn: turn, Gen: 1}); err != nil {
			t.Fatal(err)
		}
		text := []string{"First response.", "Second response."}[i]
		if err := session.Post(&core.LLMTextFrame{Text: text, Turn: turn, Gen: 1}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var ids []uint64
		for _, frame := range sink.GetFrames() {
			if unit, ok := frame.(*core.SynthesisUnitFrame); ok {
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
