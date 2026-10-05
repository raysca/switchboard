package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "govoice/core"
)

type loopTraceRecorder struct {
	events []core.TraceEvent
	ended  []error
}
type loopTraceSpan struct{ recorder *loopTraceRecorder }

func (r *loopTraceRecorder) StartSpan(_ context.Context, event core.TraceEvent) core.Span {
	r.events = append(r.events, event)
	return loopTraceSpan{recorder: r}
}
func (loopTraceSpan) SetAttribute(string, any) {}
func (loopTraceSpan) AddEvent(core.TraceEvent) {}
func (s loopTraceSpan) End(err error)          { s.recorder.ended = append(s.recorder.ended, err) }

func testLoopConfig() ToolLoopConfig {
	return ToolLoopConfig{MaxRounds: 3, MaxCalls: 4, MaxArgumentBytes: 4096, MaxResultBytes: 1024, MaxJSONDepth: 8, DefaultCallTimeout: time.Second, RoundTimeout: 2 * time.Second}
}

func testCall(id, name, args string) ToolCall {
	return ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args), SessionID: "s", TurnID: "t", Generation: 1}
}

func newTestLoop(t *testing.T, exec ToolExecutor, defs ...ToolDefinition) (*ToolLoop, *core.ConversationContext) {
	t.Helper()
	for i := range defs {
		if len(defs[i].InputSchema) == 0 {
			defs[i].InputSchema = json.RawMessage("{\"type\":\"object\"}")
		}
	}
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 32, MaxTokens: 10000})
	loop, err := NewToolLoop(defs, exec, history, testLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	return loop, history
}

func TestToolLoopRejectsZeroOrInvalidConfig(t *testing.T) {
	for _, cfg := range []ToolLoopConfig{{}, func() ToolLoopConfig { c := testLoopConfig(); c.MaxCalls = 0; return c }(), func() ToolLoopConfig { c := testLoopConfig(); c.DefaultCallTimeout = -1; return c }(), func() ToolLoopConfig { c := testLoopConfig(); c.MaxResultBytes = 0; return c }(), func() ToolLoopConfig { c := testLoopConfig(); c.MaxArgumentBytes = 0; return c }()} {
		if _, err := NewToolLoop(nil, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }), core.NewConversationContext(core.ContextConfig{}), cfg); !errors.Is(err, ErrInvalidToolLoopConfig) {
			t.Fatalf("config %#v = %v", cfg, err)
		}
	}
}

func TestToolLoopRunsMultipleCallsSequentiallyInInputOrderAndContinues(t *testing.T) {
	var order []string
	loop, history := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		order = append(order, c.ID)
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{\"id\":\"" + c.ID + "\"}")}, nil
	}), ToolDefinition{Name: "lookup"})
	var continuations int
	result, err := loop.Run(context.Background(), []ToolCall{testCall("a", "lookup", "{}"), testCall("b", "lookup", "{}")}, func(_ context.Context, results []ToolResult) ([]ToolCall, bool, error) {
		continuations++
		if continuations == 1 {
			if len(history.Messages()) < 4 {
				t.Fatal("tool results must be inserted before continuation")
			}
			if len(results) != 2 || results[0].CallID != "a" || results[1].CallID != "b" {
				t.Fatalf("round results = %#v", results)
			}
			return []ToolCall{testCall("c", "lookup", "{}")}, false, nil
		}
		return nil, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"a", "b", "c"}) {
		t.Fatalf("execution order = %#v", order)
	}
	if result.Calls != 3 || result.Rounds != 2 || !result.Done {
		t.Fatalf("loop result = %#v", result)
	}
}

func TestToolLoopUnknownMalformedAndDuplicateCallsAreDeterministic(t *testing.T) {
	var executions atomic.Int32
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		executions.Add(1)
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("null")}, nil
	}), ToolDefinition{Name: "valid"})
	got, err := loop.Run(context.Background(), []ToolCall{
		testCall("unknown", "missing", "{}"), testCall("bad-json", "valid", "{\"x\":"), testCall("first", "valid", "{}"), testCall("first", "valid", "{}"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executor calls = %d", executions.Load())
	}
	if len(got.Results) != 4 || got.Results[0].ErrorCategory != "unknown_tool" || got.Results[1].ErrorCategory != "invalid_arguments" || got.Results[2].CallID != "first" || got.Results[3].CallID != "first" {
		t.Fatalf("normalized results = %#v", got.Results)
	}
}

func TestToolLoopConflictingDuplicateAndMalformedIdentityFailClosed(t *testing.T) {
	var count atomic.Int32
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		count.Add(1)
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), ToolDefinition{Name: "x"})
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("same", "x", "{}"), testCall("same", "x", "{\"different\":true}")}, nil); !errors.Is(err, core.ErrToolCorrelation) {
		t.Fatalf("conflicting ID = %v", err)
	}
	if count.Load() != 1 {
		t.Fatalf("executor count = %d", count.Load())
	}
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("", "x", "{}")}, nil); !errors.Is(err, ErrMalformedToolCall) {
		t.Fatalf("empty ID = %v", err)
	}
}

func TestToolLoopNormalizesExecutorErrorPanicAndInvalidOrOversizedResult(t *testing.T) {
	cfg := testLoopConfig()
	cfg.MaxResultBytes = minimumToolResultBytes
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 32, MaxTokens: 10000})
	exec := ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		switch c.ID {
		case "error":
			return ToolResult{}, errors.New("secret detail")
		case "panic":
			panic("private panic")
		case "invalid":
			return ToolResult{Result: json.RawMessage("{broken")}, nil
		default:
			return ToolResult{Result: json.RawMessage("{\"long\":\"payload-that-is-too-long\"}")}, nil
		}
	})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, exec, history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loop.Run(context.Background(), []ToolCall{testCall("error", "x", "{}"), testCall("panic", "x", "{}"), testCall("invalid", "x", "{}"), testCall("large", "x", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"executor_error", "executor_panic", "invalid_result", "result_too_large"}
	for i := range want {
		if got.Results[i].ErrorCategory != want[i] || len(got.Results[i].Result) == 0 {
			t.Fatalf("result %d = %#v", i, got.Results[i])
		}
	}
	if string(got.Results[0].Result) == "secret detail" || string(got.Results[1].Result) == "private panic" {
		t.Fatal("unsafe executor detail leaked")
	}
}

func TestToolLoopEnforcesCallRoundAndTimeoutBounds(t *testing.T) {
	var executed atomic.Int32
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		executed.Add(1)
		if c.ID == "slow" {
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		}
		return ToolResult{Result: json.RawMessage("{}")}, nil
	}), ToolDefinition{Name: "x", Policy: ToolPolicy{Timeout: 15 * time.Millisecond}})
	result, err := loop.Run(context.Background(), []ToolCall{testCall("slow", "x", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Results[0].Status != ToolCancelled || result.Results[0].ErrorCategory != "timeout" {
		t.Fatalf("timeout result = %#v", result.Results[0])
	}

	loop, _ = newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{Result: json.RawMessage("{}")}, nil
	}), ToolDefinition{Name: "x"})
	_, err = loop.Run(context.Background(), []ToolCall{testCall("1", "x", "{}"), testCall("2", "x", "{}"), testCall("3", "x", "{}"), testCall("4", "x", "{}"), testCall("5", "x", "{}")}, nil)
	if !errors.Is(err, ErrMaxToolCalls) {
		t.Fatalf("call limit = %v", err)
	}
	_, err = loop.Run(context.Background(), []ToolCall{testCall("r1", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		return []ToolCall{testCall("r2", "x", "{}")}, false, nil
	})
	if !errors.Is(err, ErrMaxToolRounds) {
		t.Fatalf("round limit = %v", err)
	}
}

func TestToolLoopHonorsCancellationInterruptionAndStaleGeneration(t *testing.T) {
	var ran atomic.Int32
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		ran.Add(1)
		if c.ID == "block" {
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		}
		return ToolResult{Result: json.RawMessage("{}")}, nil
	}), ToolDefinition{Name: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := loop.Run(ctx, []ToolCall{testCall("block", "x", "{}"), testCall("after", "x", "{}")}, nil)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("calls after interruption = %d", ran.Load())
	}

	stale, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Fatal("stale call executed")
		return ToolResult{}, nil
	}), ToolDefinition{Name: "x"})
	stale.cfg.IsCurrent = func(ToolCall) bool { return false }
	got, err := stale.Run(context.Background(), []ToolCall{testCall("stale", "x", "{}")}, nil)
	if !errors.Is(err, ErrStaleToolCall) || got.Results[0].ErrorCategory != "stale_generation" {
		t.Fatalf("stale result = %#v, %v", got, err)
	}
}

func TestToolLoopExactReplayDoesNotExecuteTwiceAndCopiesResultBytes(t *testing.T) {
	var ran atomic.Int32
	loop, history := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		ran.Add(1)
		return ToolResult{Result: json.RawMessage("{\"ok\":true}")}, nil
	}), ToolDefinition{Name: "x"})
	first, err := loop.Run(context.Background(), []ToolCall{testCall("same", "x", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first.Results[0].Result[2] = 'X'
	secondLoop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		ran.Add(1)
		return ToolResult{Result: json.RawMessage("{\"wrong\":true}")}, nil
	}), history, testLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondLoop.Run(context.Background(), []ToolCall{testCall("same", "x", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || string(second.Results[0].Result) != "{\"ok\":true}" {
		t.Fatalf("replay=%#v executions=%d", second, ran.Load())
	}
	if got := history.Messages(); len(got) != 2 || got[1].Content != "{\"ok\":true}" {
		t.Fatalf("context result aliased: %#v", got)
	}
}

func TestToolLoopTraceCarriesNeutralCallCorrelationOnly(t *testing.T) {
	trace := &loopTraceRecorder{}
	cfg := testLoopConfig()
	cfg.Tracer = trace
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 8, MaxTokens: 1000})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage("{\"secret\":\"hidden\"}")}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	call := testCall("trace-id", "x", "{\"private\":true}")
	if _, err := loop.Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	if len(trace.events) != 1 {
		t.Fatalf("trace events = %d", len(trace.events))
	}
	event := trace.events[0]
	if event.ToolCallID != call.ID || event.SessionID != call.SessionID || event.TurnID != call.TurnID || event.Generation != call.Generation {
		t.Fatalf("trace correlation = %#v", event)
	}
	if len(event.Attributes()) != 0 {
		t.Fatalf("tool content leaked into trace: %#v", event.Attributes())
	}
}

func TestToolLoopRejectsOversizedArgumentsBeforeContextOrExecutor(t *testing.T) {
	cfg := testLoopConfig()
	cfg.MaxArgumentBytes = 4
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 16, MaxTokens: 10000})
	var executions atomic.Int32
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		executions.Add(1)
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("large-args", "x", "{\"a\":1}")}, nil); !errors.Is(err, ErrMalformedToolCall) {
		t.Fatalf("oversized arguments = %v", err)
	}
	if executions.Load() != 0 || len(history.Messages()) != 0 {
		t.Fatalf("rejected input executed or entered context: calls=%d messages=%#v", executions.Load(), history.Messages())
	}
}

func TestToolLoopCancellationBetweenRoundsPreventsContinuationCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var executions atomic.Int32
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		executions.Add(1)
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), ToolDefinition{Name: "x"})
	result, err := loop.Run(ctx, []ToolCall{testCall("first", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		cancel()
		return []ToolCall{testCall("never", "x", "{}")}, false, nil
	})
	if !errors.Is(err, context.Canceled) || executions.Load() != 1 || result.Rounds != 1 {
		t.Fatalf("between-round cancellation = %#v, %v", result, err)
	}
}

func TestToolLoopRoundTimeoutStopsContinuation(t *testing.T) {
	cfg := testLoopConfig()
	cfg.RoundTimeout = 15 * time.Millisecond
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 16, MaxTokens: 10000})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Run(context.Background(), []ToolCall{testCall("first", "x", "{}")}, func(ctx context.Context, _ []ToolResult) ([]ToolCall, bool, error) {
		<-ctx.Done()
		return []ToolCall{testCall("never", "x", "{}")}, false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("round timeout = %v", err)
	}
}

func TestToolLoopResultInsertionFailureStopsContinuationAndFencesReplay(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 2, MaxTokens: 1000})
	if err := history.AddMessage(core.Message{Role: core.RoleUser, Content: "request", TurnID: "t"}); err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		executions.Add(1)
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{\"ok\":true}")}, nil
	}), history, testLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	continued := false
	call := testCall("result-full", "x", "{}")
	if _, err := loop.Run(context.Background(), []ToolCall{call}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) { continued = true; return nil, true, nil }); !errors.Is(err, core.ErrContextLimitExceeded) {
		t.Fatalf("result insertion = %v", err)
	}
	if continued || executions.Load() != 1 {
		t.Fatalf("continued=%v executions=%d", continued, executions.Load())
	}
	if _, err := loop.Run(context.Background(), []ToolCall{call}, nil); !errors.Is(err, core.ErrToolCallPending) {
		t.Fatalf("pending replay = %v", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("failed exchange re-executed %d times", executions.Load())
	}
}

func TestToolLoopRejectsUnsafeResultDepthAndContextInsertionFailure(t *testing.T) {
	cfg := testLoopConfig()
	cfg.MaxJSONDepth = 2
	ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 2, MaxTokens: 100})
	_ = ctx.AddMessage(core.Message{Role: core.RoleSystem, Content: "system", TurnID: "system"})
	_ = ctx.AddMessage(core.Message{Role: core.RoleUser, Content: "keep", TurnID: "t"})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Fatal("must not execute when call insert fails")
		return ToolResult{}, nil
	}), ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("new", "x", "{}")}, nil); !errors.Is(err, core.ErrContextLimitExceeded) {
		t.Fatalf("context insertion failure = %v", err)
	}

	ctx = core.NewConversationContext(core.ContextConfig{MaxMessages: 16, MaxTokens: 10000})
	loop, err = NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Result: json.RawMessage("{\"a\":{\"b\":{\"c\":1}}}")}, nil
	}), ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loop.Run(context.Background(), []ToolCall{testCall("deep", "x", "{}")}, nil)
	if err != nil || got.Results[0].ErrorCategory != "result_too_deep" {
		t.Fatalf("deep result = %#v, %v", got, err)
	}
}

func TestToolLoopRejectsDeepArgumentsAndClearsSuccessErrorCategory(t *testing.T) {
	cfg := testLoopConfig()
	cfg.MaxJSONDepth = 1
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 16, MaxTokens: 10000})
	var executions atomic.Int32
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		executions.Add(1)
		if c.ID == "normal" {
			return ToolResult{Status: ToolSucceeded, ErrorCategory: "private", Result: json.RawMessage("{}")}, nil
		}
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loop.Run(context.Background(), []ToolCall{testCall("deep-args", "x", "{\"x\":{\"y\":true}}"), testCall("normal", "x", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 || got.Results[0].ErrorCategory != "arguments_too_deep" || got.Results[1].ErrorCategory != "" {
		t.Fatalf("arguments/success normalization = %#v, calls=%d", got.Results, executions.Load())
	}
}

func TestToolLoopGenerationChangeDuringExecutionFencesResultAndContinuation(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	continued := atomic.Bool{}
	loop, history := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		generation.Store(2)
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{\"private\":true}")}, nil
	}), ToolDefinition{Name: "x"})
	loop.cfg.IsCurrent = func(c ToolCall) bool { return uint64(c.Generation) == generation.Load() }
	got, err := loop.Run(context.Background(), []ToolCall{testCall("flip", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		continued.Store(true)
		return nil, true, nil
	})
	if !errors.Is(err, ErrStaleToolCall) || continued.Load() {
		t.Fatalf("stale execution = %#v err=%v continued=%v", got, err, continued.Load())
	}
	if len(got.Results) != 1 || got.Results[0].Status != ToolCancelled || got.Results[0].ErrorCategory != "stale_generation" || string(got.Results[0].Result) != "{\"error\":\"stale_generation\"}" {
		t.Fatalf("stale result = %#v", got.Results)
	}
	for _, message := range history.Messages() {
		if message.Kind == core.MessageToolResult && message.Content == "{\"private\":true}" {
			t.Fatal("stale executor payload entered context")
		}
	}
}

func TestToolLoopCompletedReplayIsFencedWhenGenerationTurnsStale(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	var executions atomic.Int32
	loop, history := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		executions.Add(1)
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{\"ok\":true}")}, nil
	}), ToolDefinition{Name: "x"})
	loop.cfg.IsCurrent = func(c ToolCall) bool { return uint64(c.Generation) == generation.Load() }
	call := testCall("replay-stale", "x", "{}")
	if _, err := loop.Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	generation.Store(2)
	got, err := loop.Run(context.Background(), []ToolCall{call}, nil)
	if !errors.Is(err, ErrStaleToolCall) || executions.Load() != 1 {
		t.Fatalf("stale replay = %#v err=%v executions=%d", got, err, executions.Load())
	}
	if len(got.Results) != 1 || got.Results[0].Status != ToolCancelled || got.Results[0].ErrorCategory != "stale_generation" {
		t.Fatalf("stale replay result = %#v", got.Results)
	}
	if len(history.Messages()) != 2 || history.Messages()[1].Content != "{\"ok\":true}" {
		t.Fatalf("replay changed committed history: %#v", history.Messages())
	}
}

func TestToolLoopTracerGetsSafeOutcomeClassification(t *testing.T) {
	const secret = "executor-private-sentinel"
	for _, tc := range []struct {
		name    string
		want    string
		exec    ToolExecutor
		ctx     func() (context.Context, func())
		timeout time.Duration
	}{
		{name: "success", exec: ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
			return ToolResult{Result: json.RawMessage("{}")}, nil
		})},
		{name: "executor_error", want: "executor_error", exec: ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, errors.New(secret) })},
		{name: "panic", want: "executor_panic", exec: ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) { panic(secret) })},
		{name: "timeout", want: "timeout", timeout: 5 * time.Millisecond, exec: ToolExecutorFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		})},
		{name: "cancelled", want: "cancelled", ctx: func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		}, exec: ToolExecutorFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &loopTraceRecorder{}
			cfg := testLoopConfig()
			cfg.Tracer = trace
			if tc.timeout > 0 {
				cfg.DefaultCallTimeout, cfg.RoundTimeout = tc.timeout, time.Second
			}
			history := core.NewConversationContext(core.ContextConfig{MaxMessages: 8, MaxTokens: 1000})
			loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, tc.exec, history, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.Background(), func() {}
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			if tc.name == "cancelled" {
				started := make(chan struct{})
				loop.executor = ToolExecutorFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
					close(started)
					<-ctx.Done()
					return ToolResult{}, ctx.Err()
				})
				done := make(chan struct{})
				go func() { defer close(done); _, _ = loop.Run(ctx, []ToolCall{testCall(tc.name, "x", "{}")}, nil) }()
				<-started
				cancel()
				<-done
			} else {
				got, runErr := loop.Run(ctx, []ToolCall{testCall(tc.name, "x", "{}")}, nil)
				if runErr != nil {
					t.Fatal(runErr)
				}
				if len(got.Results) != 1 {
					t.Fatalf("results = %#v", got.Results)
				}
			}
			if len(trace.ended) != 1 {
				t.Fatalf("span endings = %#v", trace.ended)
			}
			if tc.want == "" {
				if trace.ended[0] != nil {
					t.Fatalf("success trace outcome = %v", trace.ended[0])
				}
			} else if trace.ended[0] == nil || trace.ended[0].Error() != tc.want {
				t.Fatalf("trace outcome = %v, want safe category %q", trace.ended[0], tc.want)
			}
			if strings.Contains(fmt.Sprint(trace.ended), secret) {
				t.Fatalf("tracer saw secret: %#v", trace.ended)
			}
		})
	}
}

func TestToolLoopReplayFailsClosedWhenCurrentBoundsAreStricter(t *testing.T) {
	for _, tc := range []struct {
		name, payload      string
		maxBytes, maxDepth int
	}{
		{name: "bytes", payload: `{"value":"a payload comfortably longer than thirty bytes"}`, maxBytes: minimumToolResultBytes, maxDepth: 8},
		{name: "depth", payload: `{"a":{"b":{"c":1}}}`, maxBytes: 1024, maxDepth: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := core.NewConversationContext(core.ContextConfig{MaxMessages: 8, MaxTokens: 1000})
			var executions atomic.Int32
			exec := ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
				executions.Add(1)
				return ToolResult{Result: json.RawMessage(tc.payload)}, nil
			})
			defs := []ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}}
			firstCfg := testLoopConfig()
			first, err := NewToolLoop(defs, exec, history, firstCfg)
			if err != nil {
				t.Fatal(err)
			}
			call := testCall("bounded-replay-"+tc.name, "x", "{}")
			if _, err := first.Run(context.Background(), []ToolCall{call}, nil); err != nil {
				t.Fatal(err)
			}
			before := history.Messages()
			strictCfg := testLoopConfig()
			strictCfg.MaxResultBytes, strictCfg.MaxJSONDepth = tc.maxBytes, tc.maxDepth
			strict, err := NewToolLoop(defs, exec, history, strictCfg)
			if err != nil {
				t.Fatal(err)
			}
			continued := false
			got, err := strict.Run(context.Background(), []ToolCall{call}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) { continued = true; return nil, true, nil })
			if !errors.Is(err, ErrReplayResultOutOfBounds) || got.Done || continued {
				t.Fatalf("strict replay = %#v err=%v continued=%v", got, err, continued)
			}
			if executions.Load() != 1 {
				t.Fatalf("re-executed cached call: %d", executions.Load())
			}
			if !reflect.DeepEqual(history.Messages(), before) {
				t.Fatalf("replay rewrote committed history: before=%#v after=%#v", before, history.Messages())
			}
		})
	}
}

func TestToolLoopRechecksGenerationBeforeContinuation(t *testing.T) {
	var checks atomic.Int32
	continued := atomic.Bool{}
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{\"ok\":true}")}, nil
	}), ToolDefinition{Name: "x"})
	loop.cfg.IsCurrent = func(ToolCall) bool { return checks.Add(1) < 4 }
	got, err := loop.Run(context.Background(), []ToolCall{testCall("stales-late", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		continued.Store(true)
		return nil, true, nil
	})
	if !errors.Is(err, ErrStaleToolCall) || continued.Load() || got.Done || len(got.Results) != 1 || got.Results[0].ErrorCategory != "stale_generation" {
		t.Fatalf("late stale loop = %#v err=%v continued=%v checks=%d", got, err, continued.Load(), checks.Load())
	}
}

func TestToolLoopResultByteMinimumCoversEveryFallback(t *testing.T) {
	for _, category := range []string{"invalid_tool_name", "invalid_arguments", "arguments_too_deep", "unknown_tool", "stale_generation", "timeout", "cancelled", "executor_error", "executor_panic", "invalid_status", "invalid_result", "result_too_large", "result_too_deep"} {
		payload := errorPayload(category)
		if len(payload) > minimumToolResultBytes || !json.Valid(payload) {
			t.Fatalf("fallback %s is not bounded JSON: %s", category, payload)
		}
	}
	for _, maxBytes := range []int{0, minimumToolResultBytes - 1} {
		cfg := testLoopConfig()
		cfg.MaxResultBytes = maxBytes
		if _, err := NewToolLoop(nil, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }), core.NewConversationContext(core.ContextConfig{}), cfg); !errors.Is(err, ErrInvalidToolLoopConfig) {
			t.Fatalf("MaxResultBytes=%d accepted: %v", maxBytes, err)
		}
	}
	cfg := testLoopConfig()
	cfg.MaxResultBytes = minimumToolResultBytes
	for _, mode := range []string{"error", "panic", "timeout", "oversized", "bad-args"} {
		history := core.NewConversationContext(core.ContextConfig{MaxMessages: 32, MaxTokens: 10000})
		loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}"), Policy: ToolPolicy{Timeout: 5 * time.Millisecond}}}, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
			switch c.ID {
			case "error":
				return ToolResult{}, errors.New("secret")
			case "panic":
				panic("private")
			case "timeout":
				<-ctx.Done()
				return ToolResult{}, ctx.Err()
			case "oversized":
				return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"too":"a payload that exceeds the configured thirty-byte bound"}`)}, nil
			default:
				return ToolResult{Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
			}
		}), history, cfg)
		if err != nil {
			t.Fatal(err)
		}
		args := "{}"
		if mode == "bad-args" {
			args = "{broken"
		}
		got, err := loop.Run(context.Background(), []ToolCall{testCall(mode, "x", args)}, nil)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if len(got.Results) != 1 || len(got.Results[0].Result) > minimumToolResultBytes || !json.Valid(got.Results[0].Result) {
			t.Fatalf("%s fallback is not bounded JSON: %#v", mode, got.Results)
		}
		if mode == "oversized" && got.Results[0].ErrorCategory != "result_too_large" {
			t.Fatalf("oversized category = %q", got.Results[0].ErrorCategory)
		}
	}
}

func TestToolLoopDoneIsFalseWhenCallOrRoundLimitErrors(t *testing.T) {
	cfg := testLoopConfig()
	cfg.MaxCalls = 1
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 32, MaxTokens: 10000})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loop.Run(context.Background(), []ToolCall{testCall("first", "x", "{}"), testCall("second", "x", "{}")}, nil)
	if !errors.Is(err, ErrMaxToolCalls) || got.Done {
		t.Fatalf("first-round limit = %#v %v", got, err)
	}

	cfg = testLoopConfig()
	cfg.MaxCalls = 1
	history = core.NewConversationContext(core.ContextConfig{MaxMessages: 32, MaxTokens: 10000})
	loop, err = NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err = loop.Run(context.Background(), []ToolCall{testCall("only", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		return []ToolCall{testCall("over", "x", "{}")}, false, nil
	})
	if !errors.Is(err, ErrMaxToolCalls) || got.Done {
		t.Fatalf("later-round call limit = %#v %v", got, err)
	}

	cfg = testLoopConfig()
	cfg.MaxRounds = 1
	history = core.NewConversationContext(core.ContextConfig{MaxMessages: 32, MaxTokens: 10000})
	loop, err = NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{}")}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err = loop.Run(context.Background(), []ToolCall{testCall("round1", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		return []ToolCall{testCall("round2", "x", "{}")}, false, nil
	})
	if !errors.Is(err, ErrMaxToolRounds) || got.Done {
		t.Fatalf("later-round limit = %#v %v", got, err)
	}
}
