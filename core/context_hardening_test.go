package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

type captureEmitter struct{ frames []Frame }

type fixedGenerationSession struct{ gen uint64 }

func (s *fixedGenerationSession) ID() uint64                { return 1 }
func (s *fixedGenerationSession) Context() context.Context  { return context.Background() }
func (s *fixedGenerationSession) CurrentGeneration() uint64 { return s.gen }
func (s *fixedGenerationSession) Go(fn func(context.Context)) error {
	go fn(context.Background())
	return nil
}
func (s *fixedGenerationSession) Reenter(Frame, Direction, int, uint64) error { return nil }
func (s *fixedGenerationSession) IncrementGeneration() uint64                 { s.gen++; return s.gen }

func (e *captureEmitter) Push(f Frame) error { e.frames = append(e.frames, f); return nil }
func (e *captureEmitter) PushDirection(f Frame, _ Direction) error {
	e.frames = append(e.frames, f)
	return nil
}

func TestContextCopiesMutableInput(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 8, MaxTokens: 100})
	meta := map[string]any{"labels": []string{"safe"}}
	if err := ctx.AddMessage(Message{Role: RoleUser, Content: "hello", TurnID: "t", Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	meta["labels"].([]string)[0] = "mutated"
	got := ctx.Messages()
	got[0].Metadata["labels"].([]string)[0] = "also-mutated"
	if ctx.Messages()[0].Metadata["labels"].([]string)[0] != "safe" {
		t.Fatal("context did not isolate mutable message metadata")
	}
}

func TestContextCopiesAllAcceptedMutableMetadata(t *testing.T) {
	raw := json.RawMessage(`{"safe":true}`)
	labels := map[string]string{"a": "safe"}
	scores := []float64{1, 2}
	ctx := NewConversationContext(ContextConfig{MaxMessages: 8, MaxTokens: 100})
	if err := ctx.AddMessage(Message{Role: RoleUser, Content: "x", TurnID: "t", Metadata: map[string]any{"raw": raw, "labels": labels, "scores": scores}}); err != nil {
		t.Fatal(err)
	}
	raw[2] = 'X'
	labels["a"] = "bad"
	scores[0] = 9
	got := ctx.Messages()[0].Metadata
	if string(got["raw"].(json.RawMessage)) != `{"safe":true}` || got["labels"].(map[string]string)["a"] != "safe" || got["scores"].([]float64)[0] != 1 {
		t.Fatalf("aliased metadata: %#v", got)
	}
	if err := ctx.AddMessage(Message{Role: RoleUser, Content: "bad", TurnID: "x", Metadata: map[string]any{"unsupported": make(chan int)}}); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("unsupported metadata = %v", err)
	}
}

func TestContextRejectsInvalidJSONAndNonFiniteMetadataAtomically(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 8, MaxTokens: 100})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "safe", TurnID: "t"})
	before := ctx.Messages()
	for _, metadata := range []map[string]any{{"raw": json.RawMessage(`{"broken":`)}, {"nan": math.NaN()}, {"inf": []float64{1, math.Inf(1)}}, {"nested": []any{map[string]any{"bad": math.Inf(-1)}}}} {
		if err := ctx.AddMessage(Message{Role: RoleUser, Content: "bad", TurnID: "x", Metadata: metadata}); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("metadata %#v = %v", metadata, err)
		}
		if !reflect.DeepEqual(ctx.Messages(), before) {
			t.Fatal("invalid metadata changed context")
		}
	}
	if err := ctx.AddMessage(Message{Role: RoleUser, Content: "finite", TurnID: "f", Metadata: map[string]any{"score": 1.25, "samples": []float64{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(ctx.Messages()); err != nil {
		t.Fatalf("accepted metadata does not marshal: %v", err)
	}
}

func TestContextToolOrderingCorrelationAndReplay(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	if err := ctx.AddMessage(Message{Role: RoleUser, Content: "weather", TurnID: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := seedToolCall(ctx, "t", 1, "call-1", "weather", []byte(`{"city":"LON"}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := seedToolResult(ctx, "t", 1, "call-1", []byte(`{"temp":12}`), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := seedToolResult(ctx, "t", 1, "call-1", []byte(`{"temp":12}`), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	msgs := ctx.Messages()
	if len(msgs) != 3 || msgs[1].Role != RoleAssistant || msgs[1].ToolCallID != "call-1" || msgs[2].Role != RoleTool || msgs[2].ToolCallID != "call-1" {
		t.Fatalf("bad tool exchange: %#v", msgs)
	}
	if err := seedToolResult(ctx, "t", 1, "missing", nil, "bad", time.Now()); !errors.Is(err, ErrUnmatchedToolResult) {
		t.Fatalf("unmatched result error = %v", err)
	}
}

func TestContextRejectsDirectToolMessagesAndConflictingResultStatus(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	directCall := Message{Role: RoleAssistant, Kind: MessageToolCall, TurnID: "t", Generation: 1, ToolCallID: "c", ToolName: "lookup", Content: `{}`}
	if err := ctx.AddMessage(directCall); !errors.Is(err, ErrInvalidToolMessage) {
		t.Fatalf("direct tool call = %v", err)
	}
	direct := Message{Role: RoleTool, Kind: MessageToolResult, TurnID: "t", Generation: 1, ToolCallID: "c", Content: "x"}
	if err := ctx.AddMessage(direct); !errors.Is(err, ErrInvalidToolMessage) {
		t.Fatalf("direct tool result = %v", err)
	}
	if err := seedToolCall(ctx, "t", 1, "c", "lookup", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ctx.AddToolResultStatus("t", 1, "c", ToolFailed, nil, "timeout", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ctx.AddToolResultStatus("t", 1, "c", ToolCancelled, nil, "timeout", time.Now()); !errors.Is(err, ErrToolCorrelation) {
		t.Fatalf("status conflict = %v", err)
	}
	if err := ctx.AddToolResultStatus("t", 1, "c", ToolFailed, []byte(`{"partial":true}`), "timeout", time.Now()); !errors.Is(err, ErrToolCorrelation) {
		t.Fatalf("result payload conflict = %v", err)
	}
	if ctx.Messages()[1].ToolStatus != ToolFailed {
		t.Fatal("tool status was not retained")
	}
}

func TestContextClaimToolCallElectsOneExecutorAndReturnsCompletedReplay(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	call := ToolCall{ID: "once", Name: "lookup", Arguments: json.RawMessage(`{"q":1}`), TurnID: "turn", Generation: 1}
	const workers = 12
	claims := make(chan bool, workers)
	errs := make(chan error, workers)
	for range workers {
		go func() {
			_, claimed, err := ctx.ClaimToolCall(call, time.Now())
			claims <- claimed
			errs <- err
		}()
	}
	claimCount := 0
	for range workers {
		err := <-errs
		if err != nil && !errors.Is(err, ErrToolCallPending) {
			t.Fatal(err)
		}
		if <-claims {
			claimCount++
		}
	}
	if claimCount != 1 {
		t.Fatalf("executor claim count = %d, want 1", claimCount)
	}
	result := ToolResult{CallID: call.ID, Status: ToolSucceeded, Result: json.RawMessage(`{"ok":true}`)}
	if err := ctx.AddToolResultStatus("turn", 1, call.ID, result.Status, result.Result, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	replay, claimed, err := ctx.ClaimToolCall(call, time.Now())
	if err != nil || claimed {
		t.Fatalf("completed replay claim = %v, %v", claimed, err)
	}
	if replay.CallID != result.CallID || replay.Status != result.Status || string(replay.Result) != string(result.Result) {
		t.Fatalf("replayed result = %#v", replay)
	}
	call.Arguments = json.RawMessage(`{"q":2}`)
	if _, _, err := ctx.ClaimToolCall(call, time.Now()); !errors.Is(err, ErrToolCorrelation) {
		t.Fatalf("conflicting replay = %v", err)
	}
}

func TestContextToolReplayFenceSurvivesMessageEviction(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 4, MaxTokens: 1000, MaxToolIdentities: 4})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "one", TurnID: "t1"})
	_ = seedToolCall(ctx, "t1", 1, "c1", "lookup", []byte(`{}`), time.Now())
	_ = ctx.AddToolResultStatus("t1", 1, "c1", ToolSucceeded, []byte(`{"ok":true}`), "", time.Now())
	_ = ctx.AddMessage(Message{Role: RoleAssistant, Content: "done", TurnID: "t1"})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "two", TurnID: "t2"})
	if ctx.EvictionCount() != 1 {
		t.Fatalf("turn one was not evicted: %#v", ctx.Messages())
	}
	before := ctx.Messages()
	if err := seedToolCall(ctx, "t1", 1, "c1", "lookup", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ctx.AddToolResultStatus("t1", 1, "c1", ToolSucceeded, []byte(`{"ok":true}`), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ctx.Messages(), before) {
		t.Fatal("exact replay after eviction re-entered history")
	}
	if err := seedToolCall(ctx, "t2", 2, "c1", "lookup", []byte(`{}`), time.Now()); !errors.Is(err, ErrToolCorrelation) {
		t.Fatalf("identity reuse = %v", err)
	}
	bounded := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000, MaxToolIdentities: 1})
	if err := seedToolCall(bounded, "t", 1, "one", "lookup", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	// The ledger is a FIFO window: a full ledger forgets the oldest call
	// instead of failing every later tool call.
	if err := seedToolCall(bounded, "t", 1, "two", "lookup", []byte(`{}`), time.Now()); err != nil {
		t.Fatalf("full ledger rejected new call: %v", err)
	}
	if err := seedToolResult(bounded, "t", 1, "two", []byte(`{}`), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(bounded.toolLedger) != 1 || len(bounded.toolOrder) != 1 {
		t.Fatalf("ledger not bounded: %d/%d", len(bounded.toolLedger), len(bounded.toolOrder))
	}
}

func TestNewPipelineRejectsNilProcessor(t *testing.T) {
	if _, err := NewPipeline(&testFnProcessor{}, nil); err == nil {
		t.Fatal("nil processor accepted")
	}
}

// failingEmitter fails pushes of one frame type.
type failingEmitter struct {
	on  FrameType
	err error
}

func (e failingEmitter) Push(f Frame) error { return e.PushDirection(f, Downstream) }
func (e failingEmitter) PushDirection(f Frame, _ Direction) error {
	if f.Type() == e.on {
		return e.err
	}
	return nil
}

func TestUserContextReturnsPushErrors(t *testing.T) {
	p := NewUserContextProcessor(NewConversationContext(ContextConfig{}), UserContextConfig{})
	_ = p.Attach(&fixedGenerationSession{gen: 1}, 0)
	want := errors.New("push failed")
	for i, on := range []FrameType{FrameInterruption, FrameFinalTranscription} {
		turn := string(rune('a' + i))
		err := p.ProcessFrame(context.Background(), &FinalTranscriptionFrame{Text: turn, Turn: turn, Gen: 1, Committed: true, Timestamp: time.Now()}, Downstream, failingEmitter{on, want})
		if !errors.Is(err, want) {
			t.Fatalf("push of %v: err = %v", on, err)
		}
	}
}

func TestBargeInStillForwardsUserStartedSpeaking(t *testing.T) {
	p := NewAssistantContextProcessor(NewConversationContext(ContextConfig{}), DefaultAssistantContextConfig())
	_ = p.Attach(&fixedGenerationSession{gen: 1}, 0)
	e := &captureEmitter{}
	_ = p.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "a", Gen: 1}, Downstream, e)
	speech := &UserStartedSpeakingFrame{Gen: 1}
	if err := p.ProcessFrame(context.Background(), speech, Downstream, e); err != nil {
		t.Fatal(err)
	}
	if p.InterruptionCount.Load() != 1 || e.frames[len(e.frames)-1] != Frame(speech) {
		t.Fatalf("interruptions=%d frames=%#v", p.InterruptionCount.Load(), e.frames)
	}
}

func TestFlushLateAckDoesNotSilenceNextBarrier(t *testing.T) {
	p := NewAssistantContextProcessor(NewConversationContext(ContextConfig{}), AssistantContextConfig{FlushTimeout: 20 * time.Millisecond})
	_ = p.Attach(&fixedGenerationSession{gen: 1}, 0)
	e := &captureEmitter{}
	bargeIn := func(turn string, gen uint64) {
		_ = p.ProcessFrame(context.Background(), &UserMessageFrame{Turn: turn, Gen: gen}, Downstream, e)
		_ = p.ProcessFrame(context.Background(), &InterruptionFrame{}, Downstream, e)
	}
	bargeIn("a", 1)
	time.Sleep(60 * time.Millisecond)
	// The ack for barrier 1 arrives after it already timed out.
	_ = p.ProcessFrame(context.Background(), &FlushProcessedFrame{Gen: 1}, Upstream, e)
	bargeIn("b", 2)
	time.Sleep(60 * time.Millisecond)
	if got := p.FlushTimeouts.Load(); got != 2 {
		t.Fatalf("flush timeouts = %d, want 2 (late ack silenced the next barrier)", got)
	}
}

func TestContextEvictsUnansweredTurns(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 3})
	for _, turn := range []string{"t0", "t1", "t2", "t3", "t4", "t5"} {
		// Interrupted or failed turns never get an assistant reply.
		if err := ctx.AddMessage(Message{Role: RoleUser, Content: "hi", TurnID: turn}); err != nil {
			t.Fatalf("turn %s: %v", turn, err)
		}
	}
	if got := ctx.Messages(); len(got) != 3 || got[2].TurnID != "t5" {
		t.Fatalf("messages = %#v", got)
	}
}

func TestContextPendingToolCallCannotCompleteAfterCallEviction(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 3, MaxTokens: 1000})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "one", TurnID: "t1"})
	_ = seedToolCall(ctx, "t1", 1, "c", "lookup", []byte(`{}`), time.Now())
	_ = ctx.AddMessage(Message{Role: RoleAssistant, Content: "done", TurnID: "t1"})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "two", TurnID: "t2"})
	before := ctx.Messages()
	if err := ctx.AddToolResultStatus("t1", 1, "c", ToolSucceeded, []byte(`{"ok":true}`), "", time.Now()); !errors.Is(err, ErrUnmatchedToolResult) {
		t.Fatalf("retired pending call result = %v", err)
	}
	if !reflect.DeepEqual(ctx.Messages(), before) {
		t.Fatal("retired pending result mutated context")
	}
}

func TestContextToolResultTransactionRejectsSelfEviction(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 4, MaxTokens: 1000})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "one", TurnID: "t1"})
	_ = seedToolCall(ctx, "t1", 1, "c", "lookup", []byte(`{}`), time.Now())
	_ = ctx.AddMessage(Message{Role: RoleAssistant, Content: "done", TurnID: "t1"})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "two", TurnID: "t2"})
	before := ctx.Messages()
	beforeEvictions := ctx.EvictionCount()
	if err := ctx.AddToolResultStatus("t1", 1, "c", ToolSucceeded, []byte(`{"ok":true}`), "", time.Now()); !errors.Is(err, ErrContextLimitExceeded) {
		t.Fatalf("self-evicted result = %v", err)
	}
	if !reflect.DeepEqual(ctx.Messages(), before) || ctx.EvictionCount() != beforeEvictions {
		t.Fatal("self-evicted result transaction was not rolled back")
	}
	// The failed first result must not become an accepted completed replay.
	if err := ctx.AddToolResultStatus("t1", 1, "c", ToolSucceeded, []byte(`{"ok":true}`), "", time.Now()); !errors.Is(err, ErrContextLimitExceeded) {
		t.Fatalf("failed result leaked into ledger: %v", err)
	}
}

func TestContextToolCallTransactionRejectsSelfEviction(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 3, MaxTokens: 1000, MaxToolIdentities: 1})
	for _, message := range []Message{
		{Role: RoleUser, Content: "one", TurnID: "t1"},
		{Role: RoleAssistant, Content: "done", TurnID: "t1"},
		{Role: RoleUser, Content: "two", TurnID: "t2"},
	} {
		if err := ctx.AddMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	before := ctx.Messages()
	beforeEvictions := ctx.EvictionCount()
	for attempt := 0; attempt < 2; attempt++ {
		if err := seedToolCall(ctx, "t1", 1, "c", "lookup", []byte(`{}`), time.Now()); !errors.Is(err, ErrContextLimitExceeded) {
			t.Fatalf("self-evicted call attempt %d = %v", attempt, err)
		}
		if !reflect.DeepEqual(ctx.Messages(), before) || ctx.EvictionCount() != beforeEvictions {
			t.Fatalf("self-evicted call attempt %d changed context or eviction accounting", attempt)
		}
	}
	// A failed call must neither reserve its ID nor consume the sole ledger slot.
	if err := seedToolCall(ctx, "t2", 2, "c", "lookup", []byte(`{}`), time.Now()); err != nil {
		t.Fatalf("failed call retained phantom identity or capacity: %v", err)
	}
	if err := seedToolResult(ctx, "t2", 2, "c", []byte(`{"ok":true}`), "", time.Now()); err != nil {
		t.Fatalf("retained call could not accept result: %v", err)
	}
	if got := ctx.Messages(); len(got) != 3 || got[1].Kind != MessageToolCall || got[2].Kind != MessageToolResult {
		t.Fatalf("retained exchange = %#v", got)
	}
}

func TestContextPendingToolResultCommitsWhileCallRetained(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 4, MaxTokens: 1000})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "one", TurnID: "t1"})
	_ = seedToolCall(ctx, "t1", 1, "c", "lookup", []byte(`{}`), time.Now())
	if err := ctx.AddToolResultStatus("t1", 1, "c", ToolSucceeded, []byte(`{"ok":true}`), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := ctx.Messages(); len(got) != 3 || got[2].Kind != MessageToolResult {
		t.Fatalf("retained result = %#v", got)
	}
}

func TestAssistantContextWaitsForEveryDeliveryAndCommitsOnce(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	p := NewAssistantContextProcessor(store, AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
	e := &captureEmitter{}
	for _, f := range []Frame{
		&UserMessageFrame{Turn: "t", Gen: 1},
		&LLMTextFrame{Turn: "t", Gen: 1, Text: "One. Two."},
		&WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 2, IsFinal: true},
	} {
		if err := p.ProcessFrame(context.Background(), f, Downstream, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.Messages()) != 0 {
		t.Fatal("final-unit acknowledgement committed before earlier audio")
	}
	if err := p.ProcessFrame(context.Background(), &WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 1, IsFinal: true}, Downstream, e); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessFrame(context.Background(), &WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 2, IsFinal: true}, Downstream, e); err != nil {
		t.Fatal(err)
	}
	msgs := store.Messages()
	if len(msgs) != 1 || msgs[0].Content != "One. Two." || msgs[0].Partial {
		t.Fatalf("unexpected commit: %#v", msgs)
	}
}

func TestAssistantContextInterruptionDropsUndeliveredAndLateFinality(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	p := NewAssistantContextProcessor(store, DefaultAssistantContextConfig())
	e := &captureEmitter{}
	_ = p.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t", Gen: 2}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &LLMTextDeltaFrame{Turn: "t", Gen: 2, Delta: "not delivered"}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &InterruptionFrame{Gen: 2}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &LLMTextFrame{Turn: "t", Gen: 2, Text: "not delivered"}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &WriteCompletionFrame{Turn: "t", Gen: 2, UnitID: 1, IsFinal: true}, Downstream, e)
	if len(store.Messages()) != 0 {
		t.Fatalf("cancelled output entered context: %#v", store.Messages())
	}
	for _, f := range e.frames {
		if unit, ok := f.(*SynthesisUnitFrame); ok && unit.Turn == "" {
			t.Fatalf("cancelled turn resumed synthesis: %#v", unit)
		}
	}
}

func TestAssistantContextLateFinalityWaitsForMiddleDelivery(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	p := NewAssistantContextProcessor(store, AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
	e := &captureEmitter{}
	for _, f := range []Frame{&UserMessageFrame{Turn: "t", Gen: 1}, &LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: "One. Two. Three."}, &WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 1, IsFinal: true}, &WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 3, IsFinal: true}, &LLMTextFrame{Turn: "t", Gen: 1, Text: "One. Two. Three."}} {
		if err := p.ProcessFrame(context.Background(), f, Downstream, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.Messages()) != 0 {
		t.Fatalf("late finality skipped middle barrier: %#v", store.Messages())
	}
	if err := p.ProcessFrame(context.Background(), &WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 2, IsFinal: true}, Downstream, e); err != nil {
		t.Fatal(err)
	}
	if got := store.Messages(); len(got) != 1 || got[0].Content != "One. Two. Three." {
		t.Fatalf("commit = %#v", got)
	}
}

func TestContextFailedBudgetTransactionRestoresEvictionAccounting(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 4, MaxTokens: 20})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "old", TurnID: "old"})
	_ = ctx.AddMessage(Message{Role: RoleAssistant, Content: "ok", TurnID: "old"})
	before := ctx.Messages()
	for i := 0; i < 2; i++ {
		err := ctx.AddMessage(Message{Role: RoleUser, Content: "this current user message is far too large for the configured budget", TurnID: "new"})
		if !errors.Is(err, ErrContextLimitExceeded) {
			t.Fatalf("attempt %d = %v", i, err)
		}
		if ctx.EvictionCount() != 0 || len(ctx.Messages()) != len(before) {
			t.Fatal("failed transaction leaked eviction state")
		}
	}
}

func TestContextFailedSystemReplacementRestoresEvictionAccounting(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxTokens: 30})
	if err := ctx.SetSystemInstructionChecked("s", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "old", TurnID: "old"})
	_ = ctx.AddMessage(Message{Role: RoleAssistant, Content: "ok", TurnID: "old"})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "active", TurnID: "new"})
	before := ctx.Messages()
	for i := 0; i < 2; i++ {
		if err := ctx.SetSystemInstructionChecked("this replacement system instruction cannot fit even after the older completed turn is tentatively evicted", time.Now()); !errors.Is(err, ErrContextLimitExceeded) {
			t.Fatalf("replacement = %v", err)
		}
		if ctx.EvictionCount() != 0 {
			t.Fatal("failed system transaction leaked eviction state")
		}
		if after := ctx.Messages(); !reflect.DeepEqual(after, before) {
			t.Fatalf("system rollback = %#v", after)
		}
	}
}

// A partial reply that cannot be recorded is skipped without failing the
// interruption (M1-05), and no commit frame is announced for it.
func TestInterruptedAssistantBudgetFailureEmitsNoCommit(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxTokens: 8})
	if err := store.AddMessage(Message{Role: RoleUser, Content: "x", TurnID: "t"}); err != nil {
		t.Fatal(err)
	}
	p := NewAssistantContextProcessor(store, AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
	e := &captureEmitter{}
	_ = p.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t", Gen: 1}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: "This delivered response is too large."}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 1, IsFinal: true}, Downstream, e)
	err := p.ProcessFrame(context.Background(), &InterruptionFrame{Turn: "t", Gen: 1}, Downstream, e)
	if err != nil {
		t.Fatalf("interrupt error = %v", err)
	}
	for _, f := range e.frames {
		if _, ok := f.(*AssistantMessageFrame); ok {
			t.Fatal("emitted assistant commit absent from history")
		}
	}
}

func TestContextToolResultFailsClosedAtBudget(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 0, MaxTokens: 20})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "x", TurnID: "t"})
	_ = seedToolCall(ctx, "t", 1, "c", "lookup", []byte(`{}`), time.Now())
	before := ctx.Messages()
	if err := seedToolResult(ctx, "t", 1, "c", []byte(`{"very_large_result":"this cannot fit"}`), "", time.Now()); !errors.Is(err, ErrContextLimitExceeded) {
		t.Fatalf("budget error = %v", err)
	}
	if len(ctx.Messages()) != len(before) {
		t.Fatal("failed result changed context")
	}
}

// frameLog records frames passing through a pipeline position.
type frameLog struct {
	mu     sync.Mutex
	frames []Frame
}

func (l *frameLog) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	l.mu.Lock()
	l.frames = append(l.frames, f)
	l.mu.Unlock()
	return e.PushDirection(f, dir)
}

func (l *frameLog) userMessage(turn string) *UserMessageFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.frames {
		if um, ok := f.(*UserMessageFrame); ok && um.Turn == turn {
			return um
		}
	}
	return nil
}

func (l *frameLog) flushes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, f := range l.frames {
		if f.Type() == FrameFlush {
			n++
		}
	}
	return n
}

// A typed turn has no speech start, so the new committed user turn itself must
// interrupt a reply that is still playing: advance the generation, flush queued
// audio, and send the new turn to the LLM on the new generation.
func TestCommittedUserTurnInterruptsActiveReply(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 20, MaxTokens: 4000})
	user := NewUserContextProcessor(store, UserContextConfig{RequireCommittedTranscript: true})
	toLLM := &frameLog{}
	asst := NewAssistantContextProcessor(store, DefaultAssistantContextConfig())
	toTTS := &frameLog{}
	pipe, _ := NewPipeline(user, toLLM, asst, toTTS)
	s, _ := NewSession(904, pipe, DefaultResourceBudget())
	_ = s.Start()
	defer s.Close()

	gen := s.CurrentGeneration()
	_ = s.Post(&FinalTranscriptionFrame{Text: "Tell me a long story", Turn: "A", Gen: gen, Timestamp: time.Now(), Committed: true})
	_ = s.Post(&LLMTextDeltaFrame{Turn: "A", Gen: gen, Delta: "Once upon a time"})
	time.Sleep(20 * time.Millisecond)
	if asst.InterruptionCount.Load() != 0 {
		t.Fatal("the first turn interrupted nothing, yet the barrier ran")
	}

	_ = s.Post(&FinalTranscriptionFrame{Text: "Stop, what time is it?", Turn: "B", Gen: gen, Timestamp: time.Now(), Committed: true})
	time.Sleep(20 * time.Millisecond)

	if got := asst.InterruptionCount.Load(); got != 1 {
		t.Fatalf("interruptions = %d, want 1", got)
	}
	if s.CurrentGeneration() != gen+1 {
		t.Fatalf("generation = %d, want %d", s.CurrentGeneration(), gen+1)
	}
	if toTTS.flushes() == 0 {
		t.Fatal("queued audio of the interrupted reply was not flushed")
	}
	um := toLLM.userMessage("B")
	if um == nil || um.Gen != gen+1 {
		t.Fatalf("turn B reached the LLM as %#v, want generation %d", um, gen+1)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("session failed: %v", err)
	}
}

// seedToolCall and seedToolResult put a tool exchange into history through the
// production path (ClaimToolCall, AddToolResultStatus) for tests that need one.
func seedToolCall(c *ConversationContext, turn string, gen uint64, id, name string, args []byte, at time.Time) error {
	_, _, err := c.ClaimToolCall(ToolCall{TurnID: TurnID(turn), Generation: Generation(gen), ID: id, Name: name, Arguments: args}, at)
	return err
}

func seedToolResult(c *ConversationContext, turn string, gen uint64, id string, result []byte, safeErr string, at time.Time) error {
	status := ToolSucceeded
	if safeErr != "" {
		status = ToolFailed
	}
	return c.AddToolResultStatus(turn, gen, id, status, result, safeErr, at)
}
