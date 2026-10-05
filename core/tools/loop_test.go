package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	core "govoice/core"
)

type loopTraceRecorder struct{ events []core.TraceEvent }
type loopTraceSpan struct{}

func (r *loopTraceRecorder) StartSpan(_ context.Context, event core.TraceEvent) core.Span {
	r.events = append(r.events, event)
	return loopTraceSpan{}
}
func (loopTraceSpan) SetAttribute(string, any) {}
func (loopTraceSpan) AddEvent(core.TraceEvent) {}
func (loopTraceSpan) End(error)                {}

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
	cfg.MaxResultBytes = 20
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
	if err != nil || got.Results[0].ErrorCategory != "stale_generation" {
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
