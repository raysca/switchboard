package core

import (
	"context"
	"testing"
	"time"
)

func TestControlEnvelopeContract(t *testing.T) {
	var _ Frame = (*InputMuteFrame)(nil)
	var _ Frame = (*InputUnmuteFrame)(nil)
	var _ Frame = (*InputMutedStateFrame)(nil)
	var _ Frame = (*RuntimeSettingsUpdate)(nil)
	_ = ControlEnvelope{Session: SessionID("session"), Turn: TurnID("turn"), Generation: Generation(1), Frame: &InputMuteFrame{}}
}

func TestTurnLifecycleDuplicateAndFalseStart(t *testing.T) {
	var turn TurnLifecycle
	if turn.Complete() || turn.Interrupt() {
		t.Fatal("idle turn transitioned")
	}
	if !turn.Start(TurnID("one"), Generation(1)) {
		t.Fatal("first start rejected")
	}
	if turn.Start(TurnID("two"), Generation(1)) {
		t.Fatal("duplicate start accepted")
	}
	if !turn.Complete() || turn.Complete() || turn.Interrupt() {
		t.Fatal("completion was not one shot")
	}
	if turn.Start(TurnID("one"), Generation(2)) {
		t.Fatal("completed turn restarted after generation advance")
	}
	if !turn.Start(TurnID("two"), Generation(2)) {
		t.Fatal("next turn rejected")
	}
	if !turn.Interrupt() || turn.Interrupt() || turn.Complete() {
		t.Fatal("interruption was not one shot")
	}
}

func TestMuteUnmuteKeepsSessionAndSuppressesAudio(t *testing.T) {
	seen := make(chan FrameType, 8)
	pipe, _ := NewPipeline(&testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		if f.Type() == FrameInputAudio {
			seen <- f.Type()
		}
		return e.PushDirection(f, d)
	}})
	s, _ := NewSession(1, pipe, DefaultResourceBudget())
	s.OnOutput = func(f Frame) { seen <- f.Type() }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Post(&InputMuteFrame{}); err != nil {
		t.Fatal(err)
	}
	awaitType(t, seen, FrameInputMutedState)
	if !s.InputMuted() {
		t.Fatal("mute state absent")
	}
	_ = s.Post(&InputAudioFrame{Samples: []byte{1}})
	_ = s.Post(&InputUnmuteFrame{})
	awaitType(t, seen, FrameInputMutedState)
	if s.InputMuted() {
		t.Fatal("unmute failed")
	}
	_ = s.Post(&InputAudioFrame{Samples: []byte{2}})
	awaitType(t, seen, FrameInputAudio)
	awaitType(t, seen, FrameInputAudio)
	if s.State() != StateRunning {
		t.Fatal("session restarted or stopped")
	}
}

func TestFlushQueuedMediaPreservesControl(t *testing.T) {
	seen := make(chan FrameType, 8)
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(1, pipe, DefaultResourceBudget())
	s.OnOutput = func(f Frame) { seen <- f.Type() }
	_ = s.Post(&LLMTextDeltaFrame{Gen: 1, Delta: "stale"})
	_ = s.Post(&TTSAudioFrame{Gen: 1, Audio: []byte{1}})
	_ = s.Post(&OutputAudioFrame{Gen: 1, Samples: []byte{1}})
	_ = s.Post(&FlushFrame{Gen: 1})
	_ = s.Post(&ErrorFrame{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	awaitType(t, seen, FrameFlush)
	awaitType(t, seen, FrameError)
	if s.CurrentGeneration() != 2 {
		t.Fatal("flush failed to fence queued media")
	}
	select {
	case f := <-seen:
		t.Fatalf("stale frame delivered: %v", f)
	case <-time.After(20 * time.Millisecond):
	}
	_ = s.Post(&LLMTextDeltaFrame{Gen: 1, Delta: "late"})
	_ = s.Post(&LLMTextDeltaFrame{Gen: 2, Delta: "fresh"})
	awaitType(t, seen, FrameLLMTextDelta)
}

func TestFlushPreservesUninterruptibleAndPartialContext(t *testing.T) {
	seen := make(chan FrameType, 8)
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(2, pipe, DefaultResourceBudget())
	s.OnOutput = func(f Frame) { seen <- f.Type() }
	_ = s.Post(&OutputAudioFrame{Gen: 1, Disposition: OutputUninterruptible})
	_ = s.Post(&AssistantMessageFrame{Gen: 1, Partial: true, Content: "delivered"})
	_ = s.Post(&FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	awaitType(t, seen, FrameFlush)
	awaitType(t, seen, FrameOutputAudio)
	awaitType(t, seen, FrameAssistantMessage)
}

func TestFlushTransportPreservesUninterruptibleAudio(t *testing.T) {
	transport := NewFakeTransportOutput(FakeTransportConfig{})
	pipe, _ := NewPipeline(transport)
	s, _ := NewSession(3, pipe, DefaultResourceBudget())
	acks := make(chan FrameType, 1)
	s.OnUpstream = func(f Frame) {
		if f.Type() == FrameFlushProcessed {
			acks <- f.Type()
		}
	}
	_ = s.Post(&OutputAudioFrame{Gen: 1, Disposition: OutputUninterruptible, Samples: []byte{1}})
	_ = s.Post(&FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	awaitType(t, acks, FrameFlushProcessed)
	if len(transport.GetWrittenFrames()) != 1 {
		t.Fatal("uninterruptible transport audio discarded")
	}
}

func awaitType(t *testing.T, ch <-chan FrameType, want FrameType) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("got %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for %v", want)
	}
}
