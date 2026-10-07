package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "govoice/core"
)

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newParallelLoop(t *testing.T, exec ToolExecutor, mod func(*ToolLoopConfig), defs ...ToolDefinition) (*ToolLoop, *core.ConversationContext) {
	t.Helper()
	for i := range defs {
		defs[i].InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	cfg := testLoopConfig()
	cfg.MaxParallel = 4
	cfg.StaleCheckInterval = time.Millisecond
	cfg.Now = func() time.Time { return fixedNow }
	if mod != nil {
		mod(&cfg)
	}
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 64, MaxTokens: 10000})
	loop, err := NewToolLoop(defs, exec, history, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loop.Close)
	return loop, history
}

func okResult(id string) ToolResult {
	return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"id":"` + id + `"}`)}
}

func TestParallelCallsRunConcurrentlyAndResultsStayOrdered(t *testing.T) {
	arrived := make(chan string, 2)
	release := make(chan struct{})
	loop, history := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		arrived <- c.ID
		select { // barrier: neither returns until both have started
		case <-release:
		case <-time.After(time.Second):
			return ToolResult{}, errors.New("no concurrency")
		}
		if c.ID == "a" { // a finishes last yet must still be reported/committed first
			time.Sleep(20 * time.Millisecond)
		}
		return okResult(c.ID), nil
	}), nil, ToolDefinition{Name: "lookup"})
	go func() { <-arrived; <-arrived; close(release) }()
	res, err := loop.Run(context.Background(), []ToolCall{testCall("a", "lookup", "{}"), testCall("b", "lookup", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 || res.Results[0].CallID != "a" || res.Results[1].CallID != "b" || res.Results[0].Status != ToolSucceeded || res.Results[1].Status != ToolSucceeded {
		t.Fatalf("results = %#v", res.Results)
	}
	var order []string
	for _, m := range history.Messages() {
		if m.Kind == core.MessageToolResult {
			order = append(order, m.ToolCallID)
		}
	}
	if strings.Join(order, ",") != "a,b" {
		t.Fatalf("history result order = %v", order)
	}
}

func TestParallelMaxParallelBoundsConcurrency(t *testing.T) {
	var cur, peak atomic.Int32
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		cur.Add(-1)
		return okResult(c.ID), nil
	}), func(c *ToolLoopConfig) { c.MaxParallel = 2 }, ToolDefinition{Name: "lookup"})
	calls := []ToolCall{testCall("a", "lookup", "{}"), testCall("b", "lookup", "{}"), testCall("c", "lookup", "{}"), testCall("d", "lookup", "{}")}
	if _, err := loop.Run(context.Background(), calls, nil); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak.Load())
	}
}

func TestParallelCancelsInFlightCallsOnStaleGeneration(t *testing.T) {
	var current atomic.Bool
	current.Store(true)
	started := make(chan struct{}, 2)
	var cancelled atomic.Int32
	loop, history := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled.Add(1)
		return ToolResult{}, ctx.Err()
	}), func(c *ToolLoopConfig) {
		c.IsCurrent = func(ToolCall) bool { return current.Load() }
		c.DefaultCallTimeout, c.RoundTimeout = 5*time.Second, 5*time.Second
	}, ToolDefinition{Name: "slow"})
	go func() { <-started; <-started; current.Store(false) }() // barge-in once both are in flight
	_, err := loop.Run(context.Background(), []ToolCall{testCall("a", "slow", "{}"), testCall("b", "slow", "{}")}, nil)
	if !errors.Is(err, ErrStaleToolCall) {
		t.Fatalf("err = %v", err)
	}
	if cancelled.Load() != 2 {
		t.Fatalf("cancelled executors = %d", cancelled.Load())
	}
	for _, m := range history.Messages() {
		if m.Kind == core.MessageToolResult && m.ToolErrorCategory != "stale_generation" {
			t.Fatalf("result %s category %q", m.ToolCallID, m.ToolErrorCategory)
		}
	}
}

func TestParallelEnforcesCallAndResultBounds(t *testing.T) {
	var ran atomic.Int32
	big := json.RawMessage(`"` + strings.Repeat("x", 2048) + `"`)
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		ran.Add(1)
		if c.ID == "b" {
			return ToolResult{Status: ToolSucceeded, Result: big}, nil
		}
		return okResult(c.ID), nil
	}), func(c *ToolLoopConfig) { c.MaxCalls = 3 }, ToolDefinition{Name: "lookup"})
	var calls []ToolCall
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		calls = append(calls, testCall(id, "lookup", "{}"))
	}
	res, err := loop.Run(context.Background(), calls, nil)
	if !errors.Is(err, ErrMaxToolCalls) {
		t.Fatalf("err = %v", err)
	}
	if ran.Load() != 3 || res.Calls != 3 || len(res.Results) != 3 {
		t.Fatalf("ran=%d calls=%d results=%d", ran.Load(), res.Calls, len(res.Results))
	}
	if res.Results[1].Status != ToolFailed || res.Results[1].ErrorCategory != "result_too_large" {
		t.Fatalf("oversized result = %#v", res.Results[1])
	}
}

func TestParallelPerCallTimeoutOnlyAffectsSlowCall(t *testing.T) {
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		if c.ID == "slow" {
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		}
		return okResult(c.ID), nil
	}), nil, ToolDefinition{Name: "slow", Policy: ToolPolicy{Timeout: 20 * time.Millisecond}}, ToolDefinition{Name: "fast"})
	res, err := loop.Run(context.Background(), []ToolCall{testCall("slow", "slow", "{}"), testCall("fast", "fast", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].ErrorCategory != "timeout" || res.Results[1].Status != ToolSucceeded {
		t.Fatalf("results = %#v", res.Results)
	}
}

func TestAsyncToolAcksImmediatelyAndInjectsResultLater(t *testing.T) {
	release := make(chan struct{})
	injected := make(chan ToolResult, 1)
	loop, history := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		<-release
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"answer":42}`)}, nil
	}), func(c *ToolLoopConfig) {
		c.OnAsyncResult = func(_ ToolCall, r ToolResult) { injected <- r }
		// An async tool outlives barge-in: the fence is stale but the call still lands.
		c.IsCurrent = func(ToolCall) bool { return true }
	}, ToolDefinition{Name: "report", Policy: ToolPolicy{Async: true}})
	res, err := loop.Run(context.Background(), []ToolCall{testCall("a", "report", "{}")}, nil)
	if err != nil || len(res.Results) != 1 || res.Results[0].Status != ToolSucceeded || !strings.Contains(string(res.Results[0].Result), `"running"`) {
		t.Fatalf("ack = %#v err=%v", res, err)
	}
	// The user keeps talking while the tool runs.
	if err := history.AddMessage(core.Message{Role: core.RoleUser, Content: "meanwhile, what time is it?"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case r := <-injected:
		if r.CallID != "a" || string(r.Result) != `{"answer":42}` {
			t.Fatalf("injected = %#v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("async result never injected")
	}
	var kinds []string
	for _, m := range history.Messages() {
		switch {
		case m.Kind == core.MessageToolCall:
			kinds = append(kinds, "call")
		case m.Kind == core.MessageToolResult:
			kinds = append(kinds, "ack")
		case strings.HasPrefix(m.Content, "meanwhile"):
			kinds = append(kinds, "user")
		case m.Metadata["async_tool_call_id"] == "a" && strings.Contains(m.Content, `{"answer":42}`):
			kinds = append(kinds, "async")
		}
	}
	if strings.Join(kinds, ",") != "call,ack,user,async" {
		t.Fatalf("history order = %v", kinds)
	}
}

func TestAsyncToolDroppedOnCloseAndBounded(t *testing.T) {
	block := make(chan struct{})
	var cancelled atomic.Bool
	loop, history := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		select {
		case <-ctx.Done():
			cancelled.Store(true)
		case <-block:
		}
		return okResult(c.ID), nil
	}), func(c *ToolLoopConfig) { c.MaxAsyncInFlight, c.MaxParallel = 1, 1 }, ToolDefinition{Name: "bg", Policy: ToolPolicy{Async: true}})
	res, err := loop.Run(context.Background(), []ToolCall{testCall("a", "bg", "{}"), testCall("b", "bg", "{}")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Status != ToolSucceeded || res.Results[1].ErrorCategory != "too_many_async" {
		t.Fatalf("results = %#v", res.Results)
	}
	n := len(history.Messages())
	loop.Close()
	if !cancelled.Load() {
		t.Fatal("close did not cancel async tool")
	}
	if len(history.Messages()) != n {
		t.Fatal("result injected after close")
	}
}

func historyShape(h *core.ConversationContext) string {
	var kinds []string
	for _, m := range h.Messages() {
		switch {
		case m.Kind == core.MessageToolCall:
			kinds = append(kinds, "call:"+m.ToolCallID)
		case m.Kind == core.MessageToolResult:
			kinds = append(kinds, "result:"+m.ToolCallID)
		case m.Metadata["async_tool_call_id"] != nil:
			kinds = append(kinds, "async:"+m.Metadata["async_tool_call_id"].(string))
		}
	}
	return strings.Join(kinds, ",")
}

func TestParallelPanickingExecutorIsContained(t *testing.T) {
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		if c.ID == "a" {
			panic("boom")
		}
		return okResult(c.ID), nil
	}), nil, ToolDefinition{Name: "lookup"})
	res, err := loop.Run(context.Background(), []ToolCall{testCall("a", "lookup", "{}"), testCall("b", "lookup", "{}")}, nil)
	if err != nil || res.Results[0].ErrorCategory != "executor_panic" || res.Results[1].Status != ToolSucceeded {
		t.Fatalf("results = %#v err=%v", res.Results, err)
	}
}

func TestAsyncPanickingExecutorIsContained(t *testing.T) {
	injected := make(chan ToolResult, 1)
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) { panic("boom") }),
		func(c *ToolLoopConfig) { c.OnAsyncResult = func(_ ToolCall, r ToolResult) { injected <- r } },
		ToolDefinition{Name: "bg", Policy: ToolPolicy{Async: true}})
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("a", "bg", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if r := <-injected; r.ErrorCategory != "executor_panic" {
		t.Fatalf("injected = %#v", r)
	}
}

// With fewer slots than calls, every call is still claimed before any result is
// written, so providers see one call batch followed by its results.
func TestParallelHistoryKeepsCallBatchContiguous(t *testing.T) {
	loop, history := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		return okResult(c.ID), nil
	}), func(c *ToolLoopConfig) { c.MaxParallel = 2 }, ToolDefinition{Name: "lookup"})
	calls := []ToolCall{testCall("a", "lookup", "{}"), testCall("b", "nope", "{}"), testCall("c", "lookup", "{}")}
	if _, err := loop.Run(context.Background(), calls, nil); err != nil {
		t.Fatal(err)
	}
	if got := historyShape(history); got != "call:a,call:b,call:c,result:a,result:b,result:c" {
		t.Fatalf("history = %s", got)
	}
}

// An async result that arrives while its round is still running waits for the
// round's results, and MaxAsyncInFlight rejects in call order.
func TestAsyncResultNeverSplitsARound(t *testing.T) {
	injected := make(chan struct{})
	release := make(chan struct{})
	loop, history := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		if c.ID == "slow" {
			select {
			case <-injected:
				t.Error("async result injected mid-round")
			case <-release:
			}
		}
		return okResult(c.ID), nil
	}), func(c *ToolLoopConfig) {
		c.MaxAsyncInFlight = 1
		c.OnAsyncResult = func(ToolCall, ToolResult) { close(injected) }
	}, ToolDefinition{Name: "bg", Policy: ToolPolicy{Async: true}}, ToolDefinition{Name: "slow"})
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	res, err := loop.Run(context.Background(), []ToolCall{testCall("a", "bg", "{}"), testCall("b", "bg", "{}"), testCall("slow", "slow", "{}")}, nil)
	if err != nil || res.Results[1].ErrorCategory != "too_many_async" {
		t.Fatalf("results = %#v err=%v", res.Results, err)
	}
	<-injected
	if got := historyShape(history); got != "call:a,call:b,call:slow,result:a,result:b,result:slow,async:a" {
		t.Fatalf("history = %s", got)
	}
}

// AsyncSink defers injection to the owner, OnAsyncStart fires before the ack
// is in history, and a loop shared across rounds ("generations") keeps the call
// alive after the round that started it ended.
func TestAsyncSinkDefersInjectionAndStartPrecedesAck(t *testing.T) {
	release := make(chan struct{})
	sink := make(chan core.AsyncToolResult, 1)
	var started atomic.Int32
	var historyAtStart atomic.Int32
	var history *core.ConversationContext
	loop, history := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		<-release
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"ok":true}`)}, nil
	}), func(c *ToolLoopConfig) {
		c.AsyncSink = func(r core.AsyncToolResult) { sink <- r }
		c.OnAsyncStart = func(ToolCall) { started.Add(1); historyAtStart.Store(int32(len(history.Messages()))) }
	}, ToolDefinition{Name: "bg", Policy: ToolPolicy{Async: true}})
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("a", "bg", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 1 || historyAtStart.Load() != 0 { // claim only: the ack was not written, and a call without a result is hidden mid-round
		t.Fatalf("started=%d history-at-start=%d", started.Load(), historyAtStart.Load())
	}
	close(release)
	var r core.AsyncToolResult
	select {
	case r = <-sink:
	case <-time.After(time.Second):
		t.Fatal("no delivery")
	}
	for _, m := range history.Messages() {
		if m.Metadata["async_tool_call_id"] != nil {
			t.Fatal("envelope injected before the sink asked for it")
		}
	}
	if err := r.Inject(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range history.Messages() {
		if m.Metadata["async_tool_call_id"] == "a" {
			n++
		}
	}
	if n != 1 || r.Message.Role != core.RoleUser {
		t.Fatalf("envelope count = %d", n)
	}
	loop.Close()
	if err := r.Inject(); err == nil {
		t.Fatal("inject after close succeeded")
	}
}

func TestAsyncTimeoutCapsPolicyTimeout(t *testing.T) {
	sink := make(chan core.AsyncToolResult, 1)
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	}), func(c *ToolLoopConfig) {
		c.AsyncSink = func(r core.AsyncToolResult) { sink <- r }
		c.AsyncTimeout = 20 * time.Millisecond
	}, ToolDefinition{Name: "bg", Policy: ToolPolicy{Async: true, Timeout: time.Hour}})
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("a", "bg", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-sink:
		if r.Result.ErrorCategory != "timeout" {
			t.Fatalf("result = %#v", r.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("async call did not time out")
	}
}
