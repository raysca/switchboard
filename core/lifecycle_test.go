package core_test

import (
	"govoice/core"
	"govoice/core/coretest"

	"context"
	"sync"
	"testing"
	"time"
)

type observedMuteState struct{ event, applied bool }

func TestTurnLifecycleDuplicateAndFalseStart(t *testing.T) {
	var turn core.TurnLifecycle
	if turn.Complete(core.TurnID("one")) || turn.Interrupt(core.TurnID("one")) {
		t.Fatal("idle turn transitioned")
	}
	if !turn.Start(core.TurnID("one"), core.Generation(1)) {
		t.Fatal("first start rejected")
	}
	if turn.Start(core.TurnID("two"), core.Generation(1)) {
		t.Fatal("duplicate start accepted")
	}
	if !turn.Complete(core.TurnID("one")) || turn.Complete(core.TurnID("one")) || turn.Interrupt(core.TurnID("one")) {
		t.Fatal("completion was not one shot")
	}
	if turn.Start(core.TurnID("one"), core.Generation(2)) {
		t.Fatal("completed turn restarted after generation advance")
	}
	if !turn.Start(core.TurnID("two"), core.Generation(2)) {
		t.Fatal("next turn rejected")
	}
	if turn.Complete(core.TurnID("one")) || turn.Interrupt(core.TurnID("one")) {
		t.Fatal("late terminal affected next turn")
	}
	if !turn.Interrupt(core.TurnID("two")) || turn.Interrupt(core.TurnID("two")) || turn.Complete(core.TurnID("two")) {
		t.Fatal("interruption was not one shot")
	}
}

func TestMuteConcurrentAdmissionAndOrderedState(t *testing.T) {
	states := make(chan observedMuteState, 4)
	audio := make(chan byte, 4)
	pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		if f.Type() == core.FrameInputAudio {
			audio <- f.(*core.InputAudioFrame).Samples[0]
		}
		return e.PushDirection(f, d)
	}})
	s, _ := core.NewSession(11, pipe, core.DefaultResourceBudget())
	s.OnOutput = func(f core.Frame) {
		if v, ok := f.(*core.InputMutedStateFrame); ok {
			states <- observedMuteState{v.Muted, s.InputMuted()}
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _ = s.Post(&core.InputMuteFrame{}) }()
	go func() { defer wg.Done(); <-start; _ = s.Post(&core.InputUnmuteFrame{}) }()
	close(start)
	wg.Wait()
	// The last accepted control determines the admission state; a following
	// audio frame must agree with the final observable state.
	_ = s.Post(&core.InputAudioFrame{Samples: []byte{7}})
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
	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s, _ := core.NewSession(12, pipe, core.DefaultResourceBudget())
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _ = s.Post(&core.FlushFrame{Gen: 1}) }()
	}
	close(start)
	wg.Wait()
	if s.CurrentGeneration() != 2 {
		t.Fatalf("duplicate flush advanced generation to %d", s.CurrentGeneration())
	}
	seen := make(chan core.Frame, 4)
	s.OnOutput = func(f core.Frame) { seen <- f }
	_ = s.Post(&core.LLMTextDeltaFrame{Gen: 2, Delta: "new"})
	_ = s.Start()
	defer s.Close()
	for {
		select {
		case f := <-seen:
			if v, ok := f.(*core.LLMTextDeltaFrame); ok {
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
	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s, _ := core.NewSession(13, pipe, core.DefaultResourceBudget())
	seen := make(chan core.Frame, 8)
	s.OnOutput = func(f core.Frame) { seen <- f }
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("flush not observed")
	}
	_ = s.Post(&core.LLMTextDeltaFrame{Gen: 1, Delta: "late"})
	_ = s.Post(&core.OutputAudioFrame{Gen: 1, Samples: []byte{2}})
	_ = s.Post(&core.OutputAudioFrame{Gen: 1, Samples: []byte{1}})
	_ = s.Post(&core.LLMTextDeltaFrame{Gen: 2, Delta: "fresh"})
	select {
	case f := <-seen:
		v, ok := f.(*core.LLMTextDeltaFrame)
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
	ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 100})
	assistant := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	pipe, _ := core.NewPipeline(assistant)
	s, _ := core.NewSession(14, pipe, core.DefaultResourceBudget())
	seen := make(chan core.FrameType, 4)
	s.OnOutput = func(f core.Frame) { seen <- f.Type() }
	_ = s.Start()
	defer s.Close()
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Post(&core.UserMessageFrame{Turn: "B", Gen: 2, Content: "current"})
	awaitType(t, seen, core.FrameFlush)
	awaitType(t, seen, core.FrameUserMessage)
	_ = s.Post(&core.InterruptionFrame{Turn: "B", Gen: 1})
	_ = s.Post(&core.LLMTextDeltaFrame{Turn: "B", Gen: 2, Delta: "still current"})
	awaitType(t, seen, core.FrameLLMTextDelta)
	if s.CurrentGeneration() != 2 {
		t.Fatal("stale interruption advanced current generation")
	}
}

func TestFlushInlineStaleInterruptionCannotCancelCurrentTurn(t *testing.T) {
	ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 100})
	assistant := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	injector := &testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		if f.Type() == core.FrameLLMTextDelta {
			if err := e.Push(&core.InterruptionFrame{Turn: "B", Gen: 1}); err != nil {
				return err
			}
		}
		return e.PushDirection(f, d)
	}}
	pipe, _ := core.NewPipeline(injector, assistant)
	s, _ := core.NewSession(17, pipe, core.DefaultResourceBudget())
	seen := make(chan core.FrameType, 8)
	s.OnOutput = func(f core.Frame) { seen <- f.Type() }
	_ = s.Start()
	defer s.Close()
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Post(&core.UserMessageFrame{Turn: "B", Gen: 2, Content: "current"})
	awaitType(t, seen, core.FrameFlush)
	awaitType(t, seen, core.FrameUserMessage)
	_ = s.Post(&core.LLMTextDeltaFrame{Turn: "B", Gen: 2, Delta: "still current"})
	awaitType(t, seen, core.FrameLLMTextDelta)
	if s.CurrentGeneration() != 2 {
		t.Fatal("inline stale interruption canceled B")
	}
}

func TestFlushConcurrentCompletionCannotCommitOldTurnTwice(t *testing.T) {
	ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 100})
	assistant := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	pipe, _ := core.NewPipeline(assistant)
	s, _ := core.NewSession(18, pipe, core.DefaultResourceBudget())
	seen := make(chan core.FrameType, 8)
	s.OnOutput = func(f core.Frame) { seen <- f.Type() }
	_ = s.Start()
	defer s.Close()
	_ = s.Post(&core.UserMessageFrame{Turn: "A", Gen: 1, Content: "question"})
	_ = s.Post(&core.LLMTextFrame{Turn: "A", Gen: 1, Text: "answer"})
	awaitType(t, seen, core.FrameUserMessage)
	awaitType(t, seen, core.FrameLLMText)
	awaitType(t, seen, core.FrameSynthesisUnit)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = s.Post(&core.WriteCompletionFrame{Turn: "A", Gen: 1, UnitID: 1, IsFinal: true})
	}()
	go func() { defer wg.Done(); <-start; _ = s.Post(&core.FlushFrame{Gen: 1}) }()
	close(start)
	wg.Wait()
	_ = s.Post(&core.WriteCompletionFrame{Turn: "A", Gen: 1, UnitID: 1, IsFinal: true})
	_ = s.Post(&core.UserMessageFrame{Turn: "B", Gen: 2, Content: "next"})
	// B reaching the edge proves all prior queued controls were processed.
	for {
		select {
		case kind := <-seen:
			if kind == core.FrameUserMessage {
				goto drained
			}
		case <-time.After(time.Second):
			t.Fatal("new turn did not complete")
		}
	}
drained:
	count := 0
	for _, m := range ctx.Messages() {
		if m.Role == core.RoleAssistant && m.TurnID == "A" {
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
	pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		if f.Type() == core.FrameFlushProcessed {
			return e.Push(&core.InputMuteFrame{})
		}
		return e.PushDirection(f, d)
	}})
	s, _ := core.NewSession(15, pipe, core.DefaultResourceBudget())
	states := make(chan bool, 3)
	s.OnOutput = func(f core.Frame) {
		if v, ok := f.(*core.InputMutedStateFrame); ok {
			states <- v.Muted
		}
	}
	_ = s.Start()
	defer s.Close()
	_ = s.Reenter(&core.InputUnmuteFrame{}, core.Downstream, 0, 0)
	_ = s.Post(&core.FlushProcessedFrame{})
	// Reentry and inline emission must both commit and publish the state they
	// report, in order: unmute (re-entered) then mute (inline).
	for i, want := range []bool{false, true} {
		select {
		case got := <-states:
			if got != want {
				t.Fatalf("state %d = %v, want %v", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("state %d missing", i)
		}
	}
	if !s.InputMuted() {
		t.Fatal("inline mute did not commit")
	}
}

func TestMuteQueuedAndInlineControlsKeepAdmissionAndStateOrdered(t *testing.T) {
	for _, tc := range []struct {
		name           string
		queued, inline core.Frame
		first, final   bool
	}{
		{"queued mute then inline unmute", &core.InputMuteFrame{}, &core.InputUnmuteFrame{}, true, false},
		{"queued unmute then inline mute", &core.InputUnmuteFrame{}, &core.InputMuteFrame{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			inlineDone := make(chan struct{})
			audio := make(chan struct{}, 1)
			pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
				if f.Type() == core.FrameFlushProcessed {
					close(entered)
					<-release
					err := e.Push(tc.inline)
					close(inlineDone)
					return err
				}
				if f.Type() == core.FrameInputAudio {
					audio <- struct{}{}
				}
				return e.PushDirection(f, d)
			}})
			s, _ := core.NewSession(20, pipe, core.DefaultResourceBudget())
			states := make(chan observedMuteState, 2)
			s.OnOutput = func(f core.Frame) {
				if v, ok := f.(*core.InputMutedStateFrame); ok {
					states <- observedMuteState{v.Muted, s.InputMuted()}
				}
			}
			_ = s.Start()
			defer s.Close()
			_ = s.Post(&core.FlushProcessedFrame{})
			<-entered
			if err := s.Post(tc.queued); err != nil {
				t.Fatal(err)
			}
			close(release)
			<-inlineDone
			_ = s.Post(&core.InputAudioFrame{Samples: []byte{1}})
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
	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s, _ := core.NewSession(21, pipe, core.DefaultResourceBudget())
	if err := s.Post(&core.FlushFrame{}); err != core.ErrInvalidControlGeneration {
		t.Fatalf("zero target: %v", err)
	}
	if s.CurrentGeneration() != 1 {
		t.Fatal("zero target advanced generation")
	}
	if err := s.Reenter(&core.FlushFrame{Gen: 2}, core.Downstream, 0, 1); err != core.ErrInvalidControlGeneration {
		t.Fatalf("conflicting target: %v", err)
	}
	if s.CurrentGeneration() != 1 {
		t.Fatal("conflicting target advanced generation")
	}
	seen := make(chan core.Frame, 1)
	s.OnOutput = func(f core.Frame) { seen <- f }
	_ = s.Start()
	defer s.Close()
	select {
	case f := <-seen:
		t.Fatalf("rejected flush published: %T", f)
	case <-time.After(20 * time.Millisecond):
	}
	if err := s.Post(&core.FlushFrame{Gen: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-seen:
		v, ok := f.(*core.FlushFrame)
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
	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s, _ := core.NewSession(16, pipe, core.DefaultResourceBudget())
	if err := s.Reenter(&core.FlushFrame{Gen: 1}, core.Downstream, 0, 1); err != nil {
		t.Fatal(err)
	}
	if s.CurrentGeneration() != 2 {
		t.Fatal("reentered flush did not commit fence")
	}
	_ = s.Start()
	defer s.Close()
}

func TestControlInlineStaleFlushIsNotPublished(t *testing.T) {
	pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		if f.Type() == core.FrameInputMutedState {
			if err := e.Push(&core.FlushFrame{Gen: 1}); err != nil {
				return err
			}
		}
		return e.PushDirection(f, d)
	}})
	s, _ := core.NewSession(19, pipe, core.DefaultResourceBudget())
	seen := make(chan core.FrameType, 2)
	s.OnOutput = func(f core.Frame) { seen <- f.Type() }
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	awaitType(t, seen, core.FrameFlush)
	_ = s.Post(&core.InputMutedStateFrame{})
	awaitType(t, seen, core.FrameInputMutedState)
	select {
	case kind := <-seen:
		t.Fatalf("stale inline control published: %v", kind)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLegacyFrameNumbersStable(t *testing.T) {
	if core.FrameInternal != 821 || core.FrameWriteCompletion != 822 {
		t.Fatalf("legacy frame values changed: %d %d", core.FrameInternal, core.FrameWriteCompletion)
	}
}

func TestMuteUnmuteKeepsSessionAndSuppressesAudio(t *testing.T) {
	seen := make(chan core.FrameType, 8)
	pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		if f.Type() == core.FrameInputAudio {
			seen <- f.Type()
		}
		return e.PushDirection(f, d)
	}})
	s, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	s.OnOutput = func(f core.Frame) { seen <- f.Type() }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Post(&core.InputMuteFrame{}); err != nil {
		t.Fatal(err)
	}
	awaitType(t, seen, core.FrameInputMutedState)
	if !s.InputMuted() {
		t.Fatal("mute state absent")
	}
	_ = s.Post(&core.InputAudioFrame{Samples: []byte{1}})
	_ = s.Post(&core.InputUnmuteFrame{})
	awaitType(t, seen, core.FrameInputMutedState)
	if s.InputMuted() {
		t.Fatal("unmute failed")
	}
	_ = s.Post(&core.InputAudioFrame{Samples: []byte{2}})
	awaitType(t, seen, core.FrameInputAudio)
	awaitType(t, seen, core.FrameInputAudio)
	if s.State() != core.StateRunning {
		t.Fatal("session restarted or stopped")
	}
}

func TestFlushQueuedMediaPreservesControl(t *testing.T) {
	seen := make(chan core.FrameType, 8)
	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	s.OnOutput = func(f core.Frame) { seen <- f.Type() }
	_ = s.Post(&core.LLMTextDeltaFrame{Gen: 1, Delta: "stale"})
	_ = s.Post(&core.OutputAudioFrame{Gen: 1, Samples: []byte{2}})
	_ = s.Post(&core.OutputAudioFrame{Gen: 1, Samples: []byte{1}})
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Post(&core.ErrorFrame{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	awaitType(t, seen, core.FrameFlush)
	awaitType(t, seen, core.FrameError)
	if s.CurrentGeneration() != 2 {
		t.Fatal("flush failed to fence queued media")
	}
	select {
	case f := <-seen:
		t.Fatalf("stale frame delivered: %v", f)
	case <-time.After(20 * time.Millisecond):
	}
	_ = s.Post(&core.LLMTextDeltaFrame{Gen: 1, Delta: "late"})
	_ = s.Post(&core.LLMTextDeltaFrame{Gen: 2, Delta: "fresh"})
	awaitType(t, seen, core.FrameLLMTextDelta)
}

func TestFlushPreservesUninterruptibleAndPartialContext(t *testing.T) {
	seen := make(chan core.FrameType, 8)
	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s, _ := core.NewSession(2, pipe, core.DefaultResourceBudget())
	s.OnOutput = func(f core.Frame) { seen <- f.Type() }
	_ = s.Post(&core.OutputAudioFrame{Gen: 1, Disposition: core.OutputUninterruptible})
	_ = s.Post(&core.AssistantMessageFrame{Gen: 1, Partial: true, Content: "delivered"})
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	awaitType(t, seen, core.FrameFlush)
	awaitType(t, seen, core.FrameOutputAudio)
	awaitType(t, seen, core.FrameAssistantMessage)
}

func TestFlushTransportPreservesUninterruptibleAudio(t *testing.T) {
	transport := coretest.NewTransportOutput(coretest.TransportConfig{})
	pipe, _ := core.NewPipeline(transport)
	s, _ := core.NewSession(3, pipe, core.DefaultResourceBudget())
	acks := make(chan core.FrameType, 1)
	s.OnUpstream = func(f core.Frame) {
		if f.Type() == core.FrameFlushProcessed {
			acks <- f.Type()
		}
	}
	_ = s.Post(&core.OutputAudioFrame{Gen: 1, Disposition: core.OutputUninterruptible, Samples: []byte{1}})
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Start()
	defer s.Close()
	awaitType(t, acks, core.FrameFlushProcessed)
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

func awaitType(t *testing.T, ch <-chan core.FrameType, want core.FrameType) {
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
