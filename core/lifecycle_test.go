package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

type observedMuteState struct{ event, applied bool }

func TestTurnLifecycleDuplicateAndFalseStart(t *testing.T) {
	var turn TurnLifecycle
	if turn.Complete(TurnID("one")) || turn.Interrupt(TurnID("one")) {
		t.Fatal("idle turn transitioned")
	}
	if !turn.Start(TurnID("one"), Generation(1)) {
		t.Fatal("first start rejected")
	}
	if turn.Start(TurnID("two"), Generation(1)) {
		t.Fatal("duplicate start accepted")
	}
	if !turn.Complete(TurnID("one")) || turn.Complete(TurnID("one")) || turn.Interrupt(TurnID("one")) {
		t.Fatal("completion was not one shot")
	}
	if turn.Start(TurnID("one"), Generation(2)) {
		t.Fatal("completed turn restarted after generation advance")
	}
	if !turn.Start(TurnID("two"), Generation(2)) {
		t.Fatal("next turn rejected")
	}
	if turn.Complete(TurnID("one")) || turn.Interrupt(TurnID("one")) {
		t.Fatal("late terminal affected next turn")
	}
	if !turn.Interrupt(TurnID("two")) || turn.Interrupt(TurnID("two")) || turn.Complete(TurnID("two")) {
		t.Fatal("interruption was not one shot")
	}
}

func TestMuteConcurrentAdmissionAndOrderedState(t *testing.T) {
	states := make(chan observedMuteState, 4)
	audio := make(chan byte, 4)
	pipe, _ := NewPipeline(&testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		if f.Type() == FrameInputAudio {
			audio <- f.(*InputAudioFrame).Samples[0]
		}
		return e.PushDirection(f, d)
	}})
	s, _ := NewSession(11, pipe, DefaultResourceBudget())
	s.OnOutput = func(f Frame) {
		if v, ok := f.(*InputMutedStateFrame); ok {
			states <- observedMuteState{v.Muted, s.InputMuted()}
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _ = s.Post(&InputMuteFrame{}) }()
	go func() { defer wg.Done(); <-start; _ = s.Post(&InputUnmuteFrame{}) }()
	close(start)
	wg.Wait()
	// The last accepted control determines the admission state; a following
	// audio frame must agree with the final observable state.
	_ = s.Post(&InputAudioFrame{Samples: []byte{7}})
	_ = s.Start()
	defer s.Close()
	first := awaitState(t, states)
	second := awaitState(t, states)
	if first.event == second.event || first.event != first.applied || second.event != second.applied {
		t.Fatalf("state/event order inconsistent: %+v %+v", first, second)
	}
	select {
	case <-audio:
		if second.event {
			t.Fatal("muted audio reached processor")
		}
	case <-time.After(30 * time.Millisecond):
		if !second.event {
			t.Fatal("unmuted audio did not reach processor")
		}
	}
}

func awaitState(t *testing.T, ch <-chan observedMuteState) observedMuteState {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Second):
		t.Fatal("missing mute state")
		return observedMuteState{}
	}
}

func TestFlushConcurrentDuplicateDoesNotFenceNewGeneration(t *testing.T) {
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(12, pipe, DefaultResourceBudget())
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _ = s.Post(&FlushFrame{Gen: 1}) }()
	}
	close(start)
	wg.Wait()
	if s.CurrentGeneration() != 2 {
		t.Fatalf("duplicate flush advanced generation to %d", s.CurrentGeneration())
	}
	seen := make(chan Frame, 4)
	s.OnOutput = func(f Frame) { seen <- f }
	_ = s.Post(&LLMTextDeltaFrame{Gen: 2, Delta: "new"})
	_ = s.Start()
	defer s.Close()
	for {
		select {
		case f := <-seen:
			if v, ok := f.(*LLMTextDeltaFrame); ok {
				if v.Delta != "new" {
					t.Fatalf("unexpected text %q", v.Delta)
				}
				return
			}
		case <-time.After(time.Second):
			t.Fatal("new generation discarded")
		}
	}
}

func TestFlushLateTextTTSAndAudioAreDropped(t *testing.T) {
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(13, pipe, DefaultResourceBudget())
	seen := make(chan Frame, 8)
	s.OnOutput = func(f Frame) { seen <- f }
	_ = s.Post(&FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("flush not observed")
	}
	_ = s.Post(&LLMTextDeltaFrame{Gen: 1, Delta: "late"})
	_ = s.Post(&TTSAudioFrame{Gen: 1, Audio: []byte{1}})
	_ = s.Post(&OutputAudioFrame{Gen: 1, Samples: []byte{1}})
	_ = s.Post(&LLMTextDeltaFrame{Gen: 2, Delta: "fresh"})
	select {
	case f := <-seen:
		v, ok := f.(*LLMTextDeltaFrame)
		if !ok || v.Gen != 2 || v.Delta != "fresh" {
			t.Fatalf("stale output %T: %+v", f, f)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh text missing")
	}
	select {
	case f := <-seen:
		t.Fatalf("extra output %T", f)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestFlushStaleInterruptionCannotCancelCurrentTurn(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 100})
	assistant := NewAssistantContextProcessor(ctx, DefaultAssistantContextConfig())
	pipe, _ := NewPipeline(assistant)
	s, _ := NewSession(14, pipe, DefaultResourceBudget())
	seen := make(chan FrameType, 4)
	s.OnOutput = func(f Frame) { seen <- f.Type() }
	_ = s.Start()
	defer s.Close()
	_ = s.Post(&FlushFrame{Gen: 1})
	_ = s.Post(&UserMessageFrame{Turn: "B", Gen: 2, Content: "current"})
	awaitType(t, seen, FrameFlush)
	awaitType(t, seen, FrameUserMessage)
	_ = s.Post(&InterruptionFrame{Turn: "B", Gen: 1})
	_ = s.Post(&LLMTextDeltaFrame{Turn: "B", Gen: 2, Delta: "still current"})
	awaitType(t, seen, FrameLLMTextDelta)
	if s.CurrentGeneration() != 2 {
		t.Fatal("stale interruption advanced current generation")
	}
}

func TestFlushInlineStaleInterruptionCannotCancelCurrentTurn(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 100})
	assistant := NewAssistantContextProcessor(ctx, DefaultAssistantContextConfig())
	injector := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		if f.Type() == FrameLLMTextDelta {
			if err := e.Push(&InterruptionFrame{Turn: "B", Gen: 1}); err != nil {
				return err
			}
		}
		return e.PushDirection(f, d)
	}}
	pipe, _ := NewPipeline(injector, assistant)
	s, _ := NewSession(17, pipe, DefaultResourceBudget())
	seen := make(chan FrameType, 8)
	s.OnOutput = func(f Frame) { seen <- f.Type() }
	_ = s.Start()
	defer s.Close()
	_ = s.Post(&FlushFrame{Gen: 1})
	_ = s.Post(&UserMessageFrame{Turn: "B", Gen: 2, Content: "current"})
	awaitType(t, seen, FrameFlush)
	awaitType(t, seen, FrameUserMessage)
	_ = s.Post(&LLMTextDeltaFrame{Turn: "B", Gen: 2, Delta: "still current"})
	awaitType(t, seen, FrameLLMTextDelta)
	if s.CurrentGeneration() != 2 {
		t.Fatal("inline stale interruption canceled B")
	}
}

func TestFlushConcurrentCompletionCannotCommitOldTurnTwice(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 100})
	assistant := NewAssistantContextProcessor(ctx, DefaultAssistantContextConfig())
	pipe, _ := NewPipeline(assistant)
	s, _ := NewSession(18, pipe, DefaultResourceBudget())
	seen := make(chan FrameType, 8)
	s.OnOutput = func(f Frame) { seen <- f.Type() }
	_ = s.Start()
	defer s.Close()
	_ = s.Post(&UserMessageFrame{Turn: "A", Gen: 1, Content: "question"})
	_ = s.Post(&LLMTextFrame{Turn: "A", Gen: 1, Text: "answer"})
	awaitType(t, seen, FrameUserMessage)
	awaitType(t, seen, FrameLLMText)
	awaitType(t, seen, FrameSynthesisUnit)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = s.Post(&WriteCompletionFrame{Turn: "A", Gen: 1, UnitID: 1, IsFinal: true})
	}()
	go func() { defer wg.Done(); <-start; _ = s.Post(&FlushFrame{Gen: 1}) }()
	close(start)
	wg.Wait()
	_ = s.Post(&WriteCompletionFrame{Turn: "A", Gen: 1, UnitID: 1, IsFinal: true})
	_ = s.Post(&UserMessageFrame{Turn: "B", Gen: 2, Content: "next"})
	// B reaching the edge proves all prior queued controls were processed.
	for {
		select {
		case kind := <-seen:
			if kind == FrameUserMessage {
				goto drained
			}
		case <-time.After(time.Second):
			t.Fatal("new turn did not complete")
		}
	}
drained:
	count := 0
	for _, m := range ctx.Messages() {
		if m.Role == RoleAssistant && m.TurnID == "A" {
			count++
		}
	}
	if count > 1 {
		t.Fatalf("old turn committed %d times", count)
	}
	if s.CurrentGeneration() != 2 {
		t.Fatalf("flush generation = %d", s.CurrentGeneration())
	}
}

func TestControlReentryAndInlineCommit(t *testing.T) {
	pipe, _ := NewPipeline(&testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		if f.Type() == FrameStart {
			return e.Push(&InputMuteFrame{})
		}
		return e.PushDirection(f, d)
	}})
	s, _ := NewSession(15, pipe, DefaultResourceBudget())
	states := make(chan bool, 3)
	s.OnOutput = func(f Frame) {
		if v, ok := f.(*InputMutedStateFrame); ok {
			states <- v.Muted
		}
	}
	_ = s.Start()
	defer s.Close()
	_ = s.Reenter(&InputUnmuteFrame{}, Downstream, 0, 0)
	_ = s.Post(&StartFrame{})
	// Reentry and inline emission must both commit the state they report.
	select {
	case <-states:
	case <-time.After(time.Second):
		t.Fatal("reentry state missing")
	}
	select {
	case <-states:
	case <-time.After(time.Second):
		t.Fatal("inline state missing")
	}
	if !s.InputMuted() {
		t.Fatal("inline mute did not commit")
	}
}

func TestMuteQueuedAndInlineControlsKeepAdmissionAndStateOrdered(t *testing.T) {
	for _, tc := range []struct {
		name           string
		queued, inline Frame
		first, final   bool
	}{
		{"queued mute then inline unmute", &InputMuteFrame{}, &InputUnmuteFrame{}, true, false},
		{"queued unmute then inline mute", &InputUnmuteFrame{}, &InputMuteFrame{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			inlineDone := make(chan struct{})
			audio := make(chan struct{}, 1)
			pipe, _ := NewPipeline(&testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
				if f.Type() == FrameStart {
					close(entered)
					<-release
					err := e.Push(tc.inline)
					close(inlineDone)
					return err
				}
				if f.Type() == FrameInputAudio {
					audio <- struct{}{}
				}
				return e.PushDirection(f, d)
			}})
			s, _ := NewSession(20, pipe, DefaultResourceBudget())
			states := make(chan observedMuteState, 2)
			s.OnOutput = func(f Frame) {
				if v, ok := f.(*InputMutedStateFrame); ok {
					states <- observedMuteState{v.Muted, s.InputMuted()}
				}
			}
			_ = s.Start()
			defer s.Close()
			_ = s.Post(&StartFrame{})
			<-entered
			if err := s.Post(tc.queued); err != nil {
				t.Fatal(err)
			}
			close(release)
			<-inlineDone
			_ = s.Post(&InputAudioFrame{Samples: []byte{1}})
			first := awaitState(t, states)
			second := awaitState(t, states)
			if first.event != tc.first || first.applied != tc.first || second.event != tc.final || second.applied != tc.final {
				t.Fatalf("out of order state: %+v then %+v", first, second)
			}
			select {
			case <-audio:
				if tc.final {
					t.Fatal("audio leaked after mute")
				}
			case <-time.After(30 * time.Millisecond):
				if !tc.final {
					t.Fatal("audio discarded after unmute")
				}
			}
		})
	}
}

func TestFlushRejectsZeroOrConflictingTargetBeforeCommit(t *testing.T) {
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(21, pipe, DefaultResourceBudget())
	if err := s.Post(&FlushFrame{}); err != ErrInvalidControlGeneration {
		t.Fatalf("zero target: %v", err)
	}
	if s.CurrentGeneration() != 1 {
		t.Fatal("zero target advanced generation")
	}
	if err := s.Reenter(&FlushFrame{Gen: 2}, Downstream, 0, 1); err != ErrInvalidControlGeneration {
		t.Fatalf("conflicting target: %v", err)
	}
	if s.CurrentGeneration() != 1 {
		t.Fatal("conflicting target advanced generation")
	}
	seen := make(chan Frame, 1)
	s.OnOutput = func(f Frame) { seen <- f }
	_ = s.Start()
	defer s.Close()
	select {
	case f := <-seen:
		t.Fatalf("rejected flush published: %T", f)
	case <-time.After(20 * time.Millisecond):
	}
	if err := s.Post(&FlushFrame{Gen: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-seen:
		v, ok := f.(*FlushFrame)
		if !ok || v.Gen != 1 {
			t.Fatalf("wrong committed target: %T %+v", f, f)
		}
	case <-time.After(time.Second):
		t.Fatal("committed flush missing")
	}
	if s.CurrentGeneration() != 2 {
		t.Fatalf("one flush advanced to %d", s.CurrentGeneration())
	}
}

func TestControlReentryFlushCommitsFence(t *testing.T) {
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(16, pipe, DefaultResourceBudget())
	if err := s.Reenter(&FlushFrame{Gen: 1}, Downstream, 0, 1); err != nil {
		t.Fatal(err)
	}
	if s.CurrentGeneration() != 2 {
		t.Fatal("reentered flush did not commit fence")
	}
	_ = s.Start()
	defer s.Close()
}

func TestControlInlineStaleFlushIsNotPublished(t *testing.T) {
	pipe, _ := NewPipeline(&testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		if f.Type() == FrameStart {
			if err := e.Push(&FlushFrame{Gen: 1}); err != nil {
				return err
			}
		}
		return e.PushDirection(f, d)
	}})
	s, _ := NewSession(19, pipe, DefaultResourceBudget())
	seen := make(chan FrameType, 2)
	s.OnOutput = func(f Frame) { seen <- f.Type() }
	_ = s.Post(&FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	awaitType(t, seen, FrameFlush)
	_ = s.Post(&StartFrame{})
	awaitType(t, seen, FrameStart)
	select {
	case kind := <-seen:
		t.Fatalf("stale inline control published: %v", kind)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLegacyFrameNumbersStable(t *testing.T) {
	if FrameInternal != 821 || FrameWriteCompletion != 822 {
		t.Fatalf("legacy frame values changed: %d %d", FrameInternal, FrameWriteCompletion)
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
	deadline := time.After(time.Second)
	for len(transport.GetWrittenFrames()) != 1 {
		select {
		case <-deadline:
			t.Fatal("uninterruptible transport audio discarded")
		default:
			time.Sleep(time.Millisecond)
		}
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
