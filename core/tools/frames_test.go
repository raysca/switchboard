package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "govoice/core"
)

// frameLog records what a loop reports: "call:<id>" and "result:<id>:<status>:<category>".
type frameLog struct {
	mu     sync.Mutex
	got    []string
	frames []core.Frame
}

func (f *frameLog) emit(fr core.Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, fr)
	switch v := fr.(type) {
	case *core.ToolCallFrame:
		f.got = append(f.got, "call:"+v.Call.ID)
	case *core.ToolResultFrame:
		f.got = append(f.got, "result:"+v.Result.CallID+":"+string(v.Result.Status)+":"+v.Result.ErrorCategory)
	}
}

func (f *frameLog) seq() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.got, " ")
}

func TestToolLoopReportsEachCallThenItsResult(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		if c.ID == "b" {
			return ToolResult{}, errors.New("backend down")
		}
		return ToolResult{Result: json.RawMessage(`{"ok":true}`)}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}"), testCall("b", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a result:a:succeeded: call:b result:b:failed:executor_error"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	call, res := log.frames[0].(*core.ToolCallFrame), log.frames[1].(*core.ToolResultFrame)
	if call.Call.Generation != 1 || call.Call.TurnID != "t" || call.Timestamp.IsZero() {
		t.Fatalf("call frame %+v", call)
	}
	if res.Turn != "t" || res.Gen != 1 || string(res.Result.Result) != `{"ok":true}` || res.Result.EndedAt.Before(res.Result.StartedAt) {
		t.Fatalf("result frame %+v", res)
	}
}

func TestParallelRoundReportsCallsInClaimOrderAndResultsInCallOrder(t *testing.T) {
	bDone := make(chan struct{})
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		if c.ID == "a" {
			<-bDone // a finishes after b; its result is still settled first
		} else {
			defer close(bDone)
		}
		return okResult(c.ID), nil
	}), nil, ToolDefinition{Name: "x"})
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}"), testCall("b", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a call:b result:a:succeeded: result:b:succeeded:"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestToolLoopReportsNothingForAStaleGeneration(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Error("stale call executed")
		return ToolResult{}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	_, err := loop.WithCurrentFence(func(ToolCall) bool { return false }).WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}")}, nil)
	if !errors.Is(err, ErrStaleToolCall) {
		t.Fatalf("err = %v", err)
	}
	if got := log.seq(); got != "" {
		t.Fatalf("a stale call was reported: %q", got)
	}
}

func TestToolLoopSettlesAnAnnouncedCallCancelledOnBargeIn(t *testing.T) {
	var current atomic.Bool
	current.Store(true)
	started := make(chan struct{})
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		close(started)
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	}), func(c *ToolLoopConfig) {
		c.IsCurrent = func(ToolCall) bool { return current.Load() }
		c.DefaultCallTimeout, c.RoundTimeout = 5*time.Second, 5*time.Second
	}, ToolDefinition{Name: "slow"})
	go func() { <-started; current.Store(false) }() // barge-in while the call runs
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "slow", "{}")}, nil); !errors.Is(err, ErrStaleToolCall) {
		t.Fatalf("err = %v", err)
	}
	if got, want := log.seq(), "call:a result:a:cancelled:stale_generation"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestToolLoopSettlesAnUnknownToolAsFailed(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "nope", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a result:a:failed:unknown_tool"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestToolLoopReplayReportsNothing(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Result: json.RawMessage(`{}`)}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	call := testCall("a", "x", "{}")
	if _, err := loop.Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.seq(); got != "" {
		t.Fatalf("a replay was reported: %q", got)
	}
}

func TestAsyncToolIsSettledWhenItsBackgroundRunEnds(t *testing.T) {
	release, settled := make(chan struct{}), make(chan struct{})
	var log frameLog
	emit := func(f core.Frame) {
		log.emit(f)
		if _, ok := f.(*core.ToolResultFrame); ok {
			close(settled)
		}
	}
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		<-release
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"answer":42}`)}, nil
	}), func(c *ToolLoopConfig) { c.IsCurrent = func(ToolCall) bool { return true } }, ToolDefinition{Name: "report", Policy: ToolPolicy{Async: true}})
	if _, err := loop.WithFrames(emit).Run(context.Background(), []ToolCall{testCall("a", "report", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.seq(); got != "call:a" {
		t.Fatalf("before the background run ends: %q (the acknowledgement must not settle the call)", got)
	}
	close(release)
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("the async call was never settled")
	}
	if got, want := log.seq(), "call:a result:a:succeeded:"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if res := log.frames[1].(*core.ToolResultFrame); string(res.Result.Result) != `{"answer":42}` {
		t.Fatalf("settled with %s", res.Result.Result)
	}
}

func TestToolLoopDerivationsKeepTheFrameSink(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Result: json.RawMessage(`{}`)}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	derived := loop.WithFrames(log.emit).WithTraceContext(context.Background()).WithCurrentFence(func(ToolCall) bool { return true })
	if _, err := derived.Run(context.Background(), []ToolCall{testCall("a", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a result:a:succeeded:"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

// A model can put the caller's words into a tool name; a name the loop has no definition for is
// announced by its category, never as written.
func TestToolLoopAnnouncesAnUnknownToolByItsCategory(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "card_4111111111111111", "{}"), testCall("b", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	var names []string
	for _, f := range log.frames {
		if c, ok := f.(*core.ToolCallFrame); ok {
			names = append(names, c.Call.Name)
		}
	}
	if strings.Join(names, ",") != "unknown_tool,x" {
		t.Fatalf("announced names %q", names)
	}
}

// Closing the loop (route flip, stage swap, teardown) under a running async call settles it once, cancelled.
func TestAsyncToolIsSettledCancelledWhenTheLoopCloses(t *testing.T) {
	running := make(chan struct{})
	var log frameLog
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
		close(running)
		<-ctx.Done() // Close cancels the async context
		return ToolResult{}, ctx.Err()
	}), func(c *ToolLoopConfig) { c.IsCurrent = func(ToolCall) bool { return true } }, ToolDefinition{Name: "report", Policy: ToolPolicy{Async: true}})
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "report", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	<-running
	loop.Close() // waits for the background run
	if got, want := log.seq(), "call:a result:a:cancelled:cancelled"; got != want {
		t.Fatalf("frames %q, want %q (exactly one settlement, never succeeded)", got, want)
	}
}

// settledOnce asserts that the log holds exactly one result frame for each id.
func settledOnce(t *testing.T, log *frameLog, ids ...string) {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	n := map[string]int{}
	for _, f := range log.frames {
		if r, ok := f.(*core.ToolResultFrame); ok {
			n[r.Result.CallID]++
		}
	}
	for _, id := range ids {
		if n[id] != 1 {
			t.Fatalf("call %s settled %d times, want 1 (%v)", id, n[id], log.got)
		}
	}
}

func TestParallelBargeInSettlesEveryAnnouncedCallOnceInOrder(t *testing.T) {
	var current atomic.Bool
	current.Store(true)
	var running sync.WaitGroup
	running.Add(3)
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
		running.Done()
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	}), func(c *ToolLoopConfig) {
		c.IsCurrent = func(ToolCall) bool { return current.Load() }
		c.MaxCalls, c.DefaultCallTimeout, c.RoundTimeout = 8, 5*time.Second, 5*time.Second
	}, ToolDefinition{Name: "slow"})
	go func() { running.Wait(); current.Store(false) }()
	var log frameLog
	calls := []ToolCall{testCall("a", "slow", "{}"), testCall("b", "slow", "{}"), testCall("c", "slow", "{}")}
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), calls, nil); !errors.Is(err, ErrStaleToolCall) {
		t.Fatalf("err = %v", err)
	}
	want := "call:a call:b call:c result:a:cancelled:stale_generation result:b:cancelled:stale_generation result:c:cancelled:stale_generation"
	if got := log.seq(); got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestParallelClaimFailureSettlesTheEarlierAnnouncedCallOnce(t *testing.T) {
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Error("a call ran after the batch's claim failed")
		return ToolResult{}, nil
	}), nil, ToolDefinition{Name: "x"})
	var log frameLog
	bad := testCall("b", "x", "{}")
	bad.SessionID = ""
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}"), bad}, nil); !errors.Is(err, ErrMalformedToolCall) {
		t.Fatalf("err = %v", err)
	}
	if got, want := log.seq(), "call:a result:a:cancelled:cancelled"; got != want {
		t.Fatalf("frames %q, want %q (b was never claimed, so never announced)", got, want)
	}
}

func TestToolLoopSettlesAnExecutorFailureOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		exec ToolExecutor
		def  ToolDefinition
		want string
	}{
		"panic": {ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) { panic("boom") }),
			ToolDefinition{Name: "x"}, "call:a result:a:failed:executor_panic"},
		"timeout": {ToolExecutorFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		}),
			ToolDefinition{Name: "x", Policy: ToolPolicy{Timeout: 20 * time.Millisecond}}, "call:a result:a:cancelled:timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			loop, _ := newTestLoop(t, tc.exec, tc.def)
			defer loop.Close()
			var log frameLog
			if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}")}, nil); err != nil {
				t.Fatal(err)
			}
			if got := log.seq(); got != tc.want {
				t.Fatalf("frames %q, want %q", got, tc.want)
			}
			settledOnce(t, &log, "a")
		})
	}
}

// A history that refuses the result settles the call once as context_error, stamped with the time it was announced.
func TestToolLoopSettlesAHistoryRefusalOnceWithItsAnnouncedStart(t *testing.T) {
	var history *core.ConversationContext
	loop, h := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		// A conflicting result for the same call makes the loop's own write fail.
		if err := history.AddToolResultStatus("t", 1, c.ID, core.ToolSucceeded, []byte(`{"other":1}`), "", time.Time{}); err != nil {
			t.Error(err)
		}
		return okResult(c.ID), nil
	}), nil, ToolDefinition{Name: "x"})
	history = h
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}")}, nil); err == nil {
		t.Fatal("history refusal not returned")
	}
	if got, want := log.seq(), "call:a result:a:failed:context_error"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if res := log.frames[1].(*core.ToolResultFrame).Result; !res.StartedAt.Equal(fixedNow) {
		t.Fatalf("StartedAt = %v, want the announced time %v", res.StartedAt, fixedNow)
	}
}

// Closed before the background run could start: the ack stays in history, the call is settled cancelled once, the executor never runs.
func TestAsyncToolOnAClosedLoopIsSettledCancelledOnce(t *testing.T) {
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Error("executor ran on a closed loop")
		return ToolResult{}, nil
	}), func(c *ToolLoopConfig) { c.IsCurrent = func(ToolCall) bool { return true } }, ToolDefinition{Name: "report", Policy: ToolPolicy{Async: true}})
	loop.Close()
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "report", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	loop.Close() // waits for any background run: a late duplicate would be in the log by now
	if got, want := log.seq(), "call:a result:a:cancelled:cancelled"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}
