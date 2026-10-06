package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "govoice/core"
)

type loopTraceRecorder struct {
	mu      sync.Mutex
	events  []core.TraceEvent
	ended   []error
	parents []core.Span
}
type loopTraceSpan struct{ recorder *loopTraceRecorder }

func (r *loopTraceRecorder) StartSpan(ctx context.Context, event core.TraceEvent) core.Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	r.parents = append(r.parents, core.SpanFromContext(ctx))
	return loopTraceSpan{recorder: r}
}

func TestToolLoopInheritsSessionTraceContextAndParent(t *testing.T) {
	r := &loopTraceRecorder{}
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	}), ToolDefinition{Name: "lookup"})
	defer loop.Close()
	parent := loopTraceSpan{recorder: r}
	ctx := core.ContextWithTraceContext(core.ContextWithSpan(context.Background(), parent), r, core.TracePolicy{})
	derived := loop.WithTraceContext(ctx)
	if _, err := derived.Run(ctx, []ToolCall{testCall("a", "lookup", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	_, ends := r.snapshot(t, 1)
	if len(ends) != 1 {
		t.Fatal("missing tool trace terminal")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.parents) != 1 || r.parents[0] != parent {
		t.Fatalf("tool parent=%v", r.parents)
	}
}
func (loopTraceSpan) SetAttribute(string, any) {}
func (s loopTraceSpan) AddEvent(event core.TraceEvent) {
	s.recorder.mu.Lock()
	defer s.recorder.mu.Unlock()
	s.recorder.events = append(s.recorder.events, event)
}
func (s loopTraceSpan) End(err error) {
	s.recorder.mu.Lock()
	defer s.recorder.mu.Unlock()
	s.recorder.ended = append(s.recorder.ended, err)
}

func (r *loopTraceRecorder) snapshot(t *testing.T, ends int) ([]core.TraceEvent, []error) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		events, ended := append([]core.TraceEvent(nil), r.events...), append([]error(nil), r.ended...)
		r.mu.Unlock()
		if len(ended) >= ends || time.Now().After(deadline) {
			return events, ended
		}
		time.Sleep(time.Millisecond)
	}
}

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

func TestToolLoopTraceRedactsOptInArgumentsWithoutRecordingResults(t *testing.T) {
	const sentinel = "tool-payload-secret-886"
	recorder := &loopTraceRecorder{}
	cfg := testLoopConfig()
	cfg.Tracer = recorder
	cfg.TracePolicy = core.TracePolicy{Content: core.TraceText, RedactToolContent: func(text string) string {
		return strings.ReplaceAll(text, sentinel, "[redacted]")
	}}
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 8, MaxTokens: 2048})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{CallID: "call-1", Status: ToolSucceeded, Result: json.RawMessage(`{"value":"` + sentinel + `"}`)}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("call-1", "lookup", `{"value":"`+sentinel+`"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	events, _ := recorder.snapshot(t, 1)
	var sawArguments bool
	for _, event := range events {
		attrs := event.Attributes()
		if value, ok := attrs["tool.arguments"].(string); ok {
			sawArguments = true
			if strings.Contains(value, sentinel) || !strings.Contains(value, "[redacted]") {
				t.Fatalf("tool args were not redacted: %q", value)
			}
		}
		if _, ok := attrs["tool.result"]; ok {
			t.Fatalf("tool result must not be traced before its normalized context commit: %#v", attrs)
		}
	}
	if !sawArguments {
		t.Fatalf("opt-in trace content missing: %#v", events)
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
	events, ended := trace.snapshot(t, 1)
	if len(events) != 1 {
		t.Fatalf("trace events = %d", len(events))
	}
	for i, event := range events {
		if event.ToolCallID != call.ID || event.SessionID != call.SessionID || event.TurnID != call.TurnID || event.Generation != call.Generation {
			t.Fatalf("trace correlation for event %d = %#v", i, event)
		}
		if _, ok := event.Attributes()["tool.arguments"]; ok {
			t.Fatalf("tool arguments leaked into event %d", i)
		}
		if _, ok := event.Attributes()["tool.result"]; ok {
			t.Fatalf("tool result leaked into event %d", i)
		}
	}
	if events[0].Name != "tool.execute" || len(ended) != 1 {
		t.Fatalf("tool trace order or terminal span invalid: events=%#v ends=%d", events, len(ended))
	}
}

func TestToolLoopTraceDropsRawInvalidResultAndExecutorError(t *testing.T) {
	const secret = "raw-trace-secret-395"
	trace := &loopTraceRecorder{}
	cfg := testLoopConfig()
	cfg.Tracer = trace
	cfg.TracePolicy = core.TracePolicy{Content: core.TraceText, RedactToolContent: func(s string) string { return s }}
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 8, MaxTokens: 1000})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutorFunc(func(_ context.Context, call ToolCall) (ToolResult, error) {
		if call.ID == "bad" {
			return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"value":"` + secret + `"`)}, nil
		}
		return ToolResult{}, errors.New(secret)
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bad", "error"} {
		if _, err := loop.Run(context.Background(), []ToolCall{testCall(id, "x", "{}")}, nil); err != nil {
			t.Fatal(err)
		}
	}
	events, ended := trace.snapshot(t, 2)
	for _, event := range events {
		if strings.Contains(fmt.Sprint(event.Attributes()), secret) {
			t.Fatalf("raw result leaked: %#v", event)
		}
	}
	for _, err := range ended {
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Fatalf("raw executor error leaked: %v", err)
		}
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

func TestToolLoopResultOverContextBudgetCommitsSmallError(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{MaxTokens: 100})
	_ = history.AddMessage(core.Message{Role: core.RoleUser, Content: "x", TurnID: "t"})
	loop, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage("{\"type\":\"object\"}")}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Result: json.RawMessage(`"` + strings.Repeat("a", 900) + `"`)}, nil
	}), history, testLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := loop.Run(context.Background(), []ToolCall{testCall("big", "x", "{}")}, nil)
	if err != nil || got.Results[0].ErrorCategory != "result_too_large" {
		t.Fatalf("oversized result = %#v, %v", got, err)
	}
	if msgs := history.Messages(); len(msgs) != 3 || msgs[2].Kind != core.MessageToolResult {
		t.Fatalf("call left without result: %#v", msgs)
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
			_, ended := trace.snapshot(t, 1)
			if len(ended) != 1 {
				t.Fatalf("span endings = %#v", ended)
			}
			if tc.want == "" {
				if ended[0] != nil {
					t.Fatalf("success trace outcome = %v", ended[0])
				}
			} else if ended[0] == nil || ended[0].Error() != tc.want {
				t.Fatalf("trace outcome = %v, want safe category %q", ended[0], tc.want)
			}
			if strings.Contains(fmt.Sprint(ended), secret) {
				t.Fatalf("tracer saw secret: %#v", ended)
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
	loop, history := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{\"ok\":true}")}, nil
	}), ToolDefinition{Name: "x"})
	loop.cfg.IsCurrent = func(ToolCall) bool { return checks.Add(1) < 4 }
	got, err := loop.Run(context.Background(), []ToolCall{testCall("stales-late", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		continued.Store(true)
		return nil, true, nil
	})
	if !errors.Is(err, ErrStaleToolCall) || continued.Load() || got.Done || len(got.Results) != 1 {
		t.Fatalf("late stale loop = %#v err=%v continued=%v checks=%d", got, err, continued.Load(), checks.Load())
	}
	// The result returned must be the one history recorded, not a rewrite.
	var recorded *core.Message
	for _, m := range history.Messages() {
		if m.Kind == core.MessageToolResult && m.ToolCallID == "stales-late" {
			m := m
			recorded = &m
		}
	}
	if recorded == nil || recorded.ToolStatus != got.Results[0].Status || recorded.Content != string(got.Results[0].Result) {
		t.Fatalf("returned result %#v disagrees with history %#v", got.Results[0], recorded)
	}
}

func TestToolLoopEmptyContinuationCompletes(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage("{\"ok\":true}")}, nil
	}), ToolDefinition{Name: "x"})
	got, err := loop.Run(context.Background(), []ToolCall{testCall("final-text", "x", "{}")}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		return nil, false, nil // provider answered with text, no further calls
	})
	if err != nil || !got.Done {
		t.Fatalf("empty continuation = %#v err=%v, want done without error", got, err)
	}
}

func TestToolLoopWithCurrentFenceComposesPredicates(t *testing.T) {
	for _, tc := range []struct {
		name              string
		configured, added bool
		wantExecute       bool
	}{
		{name: "configured false rejects even if added true", configured: false, added: true},
		{name: "added false rejects even if configured true", configured: true, added: false},
		{name: "both true execute", configured: true, added: true, wantExecute: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var executions atomic.Int32
			cfg := testLoopConfig()
			cfg.IsCurrent = func(ToolCall) bool { return tc.configured }
			history := core.NewConversationContext(core.ContextConfig{MaxMessages: 16, MaxTokens: 1000})
			base, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
				executions.Add(1)
				return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"ok":true}`)}, nil
			}), history, cfg)
			if err != nil {
				t.Fatal(err)
			}
			derived := base.WithCurrentFence(func(ToolCall) bool { return tc.added })
			got, runErr := derived.Run(context.Background(), []ToolCall{testCall("compose-"+tc.name, "x", "{}")}, nil)
			if tc.wantExecute {
				if runErr != nil || got.Done != true || executions.Load() != 1 {
					t.Fatalf("result=%#v err=%v executions=%d", got, runErr, executions.Load())
				}
			} else if !errors.Is(runErr, ErrStaleToolCall) || got.Done || executions.Load() != 0 {
				t.Fatalf("fenced result=%#v err=%v executions=%d", got, runErr, executions.Load())
			}
			if _, err := base.Run(context.Background(), []ToolCall{testCall("base-"+tc.name, "x", "{}")}, nil); tc.configured {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrStaleToolCall) {
				t.Fatalf("base predicate changed: %v", err)
			}
		})
	}
}

func TestToolLoopWithCurrentFenceDerivedLoopsAreConcurrentAndIndependent(t *testing.T) {
	cfg := testLoopConfig()
	cfg.IsCurrent = func(ToolCall) bool { return true }
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 32, MaxTokens: 2000})
	var executions atomic.Int32
	base, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		executions.Add(1)
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	allowA := base.WithCurrentFence(func(c ToolCall) bool { return c.ID == "derived-a" })
	allowB := base.WithCurrentFence(func(c ToolCall) bool { return c.ID == "derived-b" })
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, item := range []struct {
		loop *ToolLoop
		id   string
	}{{allowA, "derived-a"}, {allowB, "derived-b"}} {
		wg.Add(1)
		go func(loop *ToolLoop, id string) {
			defer wg.Done()
			_, runErr := loop.Run(context.Background(), []ToolCall{testCall(id, "x", "{}")}, nil)
			errs <- runErr
		}(item.loop, item.id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if executions.Load() != 2 {
		t.Fatalf("executions=%d", executions.Load())
	}
	if _, err := base.Run(context.Background(), []ToolCall{testCall("base-unfenced", "x", "{}")}, nil); err != nil {
		t.Fatalf("base loop inherited derived fence: %v", err)
	}
}

func TestToolLoopWithCurrentFenceRechecksGenerationAfterExecutor(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	cfg := testLoopConfig()
	basePredicateCalls := atomic.Int32{}
	cfg.IsCurrent = func(ToolCall) bool { basePredicateCalls.Add(1); return true }
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 16, MaxTokens: 1000})
	base, err := NewToolLoop([]ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		generation.Store(2)
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"old":true}`)}, nil
	}), history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	derived := base.WithCurrentFence(func(c ToolCall) bool { return uint64(c.Generation) == generation.Load() })
	call := testCall("flips-during-execute", "x", "{}")
	got, err := derived.Run(context.Background(), []ToolCall{call}, func(context.Context, []ToolResult) ([]ToolCall, bool, error) {
		t.Fatal("stale generation continued")
		return nil, true, nil
	})
	if !errors.Is(err, ErrStaleToolCall) || got.Done || len(got.Results) != 1 || got.Results[0].ErrorCategory != "stale_generation" {
		t.Fatalf("generation flip result=%#v err=%v", got, err)
	}
	for _, message := range history.Messages() {
		if message.Kind == core.MessageToolResult && message.Content == `{"old":true}` {
			t.Fatal("stale executor payload committed")
		}
	}
	if basePredicateCalls.Load() < 2 {
		t.Fatalf("configured predicate was not rechecked: %d", basePredicateCalls.Load())
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
