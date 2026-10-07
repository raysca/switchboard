package tools

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	core "govoice/core"
)

func addUser(t *testing.T, h *core.ConversationContext, turn, text string) {
	t.Helper()
	if err := h.AddMessage(core.Message{Role: core.RoleUser, Content: text, TurnID: turn}); err != nil {
		t.Fatal(err)
	}
}

func mustValid(t *testing.T, h *core.ConversationContext) []core.Message {
	t.Helper()
	msgs := h.Messages()
	if err := core.ValidateToolPairing(msgs); err != nil {
		t.Fatalf("invalid history: %v\n%+v", err, msgs)
	}
	return msgs
}

func indexOf(msgs []core.Message, pred func(core.Message) bool) int {
	for i, m := range msgs {
		if pred(m) {
			return i
		}
	}
	return -1
}

func userIdx(msgs []core.Message, turn string) int {
	return indexOf(msgs, func(m core.Message) bool { return m.Role == core.RoleUser && m.TurnID == turn })
}

// slowExec blocks until release (or ctx), reporting when it has started.
func slowExec(started chan<- string, release <-chan struct{}) ToolExecutor {
	return ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		started <- c.ID
		select {
		case <-release:
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		}
		return okResult(c.ID), nil
	})
}

func TestRoundUserMessageMidToolLandsAfterResults(t *testing.T) {
	started, release := make(chan string, 4), make(chan struct{})
	loop, h := newParallelLoop(t, slowExec(started, release), nil, ToolDefinition{Name: "lookup"})
	done := make(chan error, 1)
	go func() {
		_, err := loop.Run(context.Background(), []ToolCall{testCall("a", "lookup", "{}")}, nil)
		done <- err
	}()
	<-started
	addUser(t, h, "u2", "hello?") // committed while the call is open
	// the provider-facing view mid-round is already valid and carries the message
	if msgs := mustValid(t, h); userIdx(msgs, "u2") < 0 {
		t.Fatalf("mid-round view lost the user message: %+v", msgs)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	msgs := mustValid(t, h)
	call := indexOf(msgs, func(m core.Message) bool { return m.Kind == core.MessageToolCall })
	res := indexOf(msgs, func(m core.Message) bool { return m.Kind == core.MessageToolResult })
	if res != call+1 || userIdx(msgs, "u2") != res+1 {
		t.Fatalf("order call=%d result=%d user=%d", call, res, userIdx(msgs, "u2"))
	}
}

func TestRoundParallelToolsAndTwoRounds(t *testing.T) {
	var rounds atomic.Int32
	started, release := make(chan string, 8), make(chan struct{})
	loop, h := newParallelLoop(t, slowExec(started, release), nil, ToolDefinition{Name: "lookup"})
	done := make(chan error, 1)
	go func() {
		_, err := loop.Run(context.Background(), []ToolCall{testCall("a", "lookup", "{}"), testCall("b", "lookup", "{}")}, func(_ context.Context, _ []ToolResult) ([]ToolCall, bool, error) {
			if userIdx(h.Messages(), "u2") < 0 {
				t.Error("user message should have been flushed before continuation")
			}
			if rounds.Add(1) > 1 {
				return nil, true, nil
			}
			return []ToolCall{testCall("c", "lookup", "{}")}, false, nil
		})
		done <- err
	}()
	<-started
	<-started
	addUser(t, h, "u2", "mid-1")
	mustValid(t, h)
	// release both, then a second round starts: another message mid-round 2
	go func() {
		for {
			select {
			case release <- struct{}{}:
			case <-time.After(300 * time.Millisecond):
				return
			}
		}
	}()
	<-started // c
	addUser(t, h, "u3", "mid-2")
	mustValid(t, h)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	msgs := mustValid(t, h)
	if !(userIdx(msgs, "u2") < userIdx(msgs, "u3")) || userIdx(msgs, "u2") < 0 {
		t.Fatalf("user messages out of order: %+v", msgs)
	}
}

func TestRoundBargeInLeavesValidHistoryAndNextGenerationSeesUser(t *testing.T) {
	var gen atomic.Uint64
	gen.Store(1)
	started := make(chan string, 4)
	exec := ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		started <- c.ID
		<-ctx.Done() // cooperative: stops on cancellation
		return ToolResult{}, ctx.Err()
	})
	loop, h := newParallelLoop(t, exec, func(c *ToolLoopConfig) {
		c.IsCurrent = func(call ToolCall) bool { return uint64(call.Generation) == gen.Load() }
	}, ToolDefinition{Name: "lookup"})
	done := make(chan error, 1)
	go func() {
		_, err := loop.Run(context.Background(), []ToolCall{testCall("a", "lookup", "{}")}, nil)
		done <- err
	}()
	<-started
	addUser(t, h, "u2", "stop")
	gen.Store(2) // barge-in
	// the next generation snapshots history right now, possibly before the old round unwound
	if msgs := mustValid(t, h); userIdx(msgs, "u2") < 0 {
		t.Fatalf("next generation does not see the user message: %+v", msgs)
	}
	<-done
	msgs := mustValid(t, h)
	i := indexOf(msgs, func(m core.Message) bool { return m.Kind == core.MessageToolResult })
	if i < 0 || msgs[i].ToolStatus != core.ToolCancelled || userIdx(msgs, "u2") != i+1 {
		t.Fatalf("want cancelled result then user: %+v", msgs)
	}
}

func TestRoundAsyncAckThenUserMessage(t *testing.T) {
	started, release := make(chan string, 4), make(chan struct{})
	loop, h := newParallelLoop(t, slowExec(started, release), nil, ToolDefinition{Name: "bg", Policy: ToolPolicy{Async: true}})
	if _, err := loop.Run(context.Background(), []ToolCall{testCall("a", "bg", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	<-started
	addUser(t, h, "u2", "while async runs")
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for len(h.Messages()) < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	msgs := mustValid(t, h)
	if len(msgs) != 4 || msgs[0].Kind != core.MessageToolCall || msgs[1].Kind != core.MessageToolResult {
		t.Fatalf("history = %+v", msgs)
	}
}

func TestRoundQueueIsBounded(t *testing.T) {
	h := core.NewConversationContext(core.ContextConfig{})
	h.BeginToolRound()
	var err error
	for i := 0; i < 1000 && err == nil; i++ {
		err = h.AddMessage(core.Message{Role: core.RoleUser, Content: "x"})
	}
	if err == nil {
		t.Fatal("queue is unbounded")
	}
	h.EndToolRound()
	if len(h.Messages()) == 0 {
		t.Fatal("queued messages were lost")
	}
}

func TestRoundNoOpenRoundAddsImmediately(t *testing.T) {
	h := core.NewConversationContext(core.ContextConfig{})
	addUser(t, h, "u1", "hi")
	if len(h.Messages()) != 1 {
		t.Fatal("message not immediately visible")
	}
	_ = json.RawMessage(nil)
}

// A failover candidate replaying a completed call must not add history.
func TestRoundReplayAfterUserMessageStaysValid(t *testing.T) {
	loop, h := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) { return okResult(c.ID), nil }), nil, ToolDefinition{Name: "lookup"})
	call := testCall("a", "lookup", "{}")
	if _, err := loop.Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	addUser(t, h, "u2", "again")
	n := len(h.Messages())
	if _, err := loop.Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustValid(t, h); len(got) != n {
		t.Fatalf("replay changed history: %d -> %d", n, len(got))
	}
}

// A tool that ignores ctx must not hold the round open: the call times out,
// the queued user message is flushed, and later messages land immediately.
func TestRoundExecutorIgnoringCtxDoesNotWedgeHistory(t *testing.T) {
	started, stuck := make(chan string, 1), make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	exec := ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		started <- c.ID
		<-stuck // ignores ctx entirely
		return okResult(c.ID), nil
	})
	loop, h := newParallelLoop(t, exec, func(c *ToolLoopConfig) {
		c.MaxParallel = 1
		c.RoundTimeout, c.DefaultCallTimeout = 50*time.Millisecond, 50*time.Millisecond
	}, ToolDefinition{Name: "lookup"})
	done := make(chan error, 1)
	go func() {
		_, err := loop.Run(context.Background(), []ToolCall{testCall("a", "lookup", "{}")}, nil)
		done <- err
	}()
	<-started
	addUser(t, h, "u2", "hello?")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run wedged on an executor ignoring ctx")
	}
	addUser(t, h, "u3", "still there?")
	msgs := mustValid(t, h)
	i := indexOf(msgs, func(m core.Message) bool { return m.Kind == core.MessageToolResult })
	if i < 0 || msgs[i].ToolErrorCategory != "timeout" || userIdx(msgs, "u2") != i+1 || userIdx(msgs, "u3") != i+2 {
		t.Fatalf("history = %+v", msgs)
	}
}

// A resolved call whose replay identity the ledger has forgotten must stay
// visible mid-round, or its result would be shown without its call.
func TestRoundForgottenIdentityStaysPaired(t *testing.T) {
	h := core.NewConversationContext(core.ContextConfig{MaxMessages: 64, MaxTokens: 10000, MaxToolIdentities: 1})
	for _, id := range []string{"a", "b"} {
		if err := h.AddToolCall("t", 1, id, "lookup", []byte("{}"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := h.AddToolResult("t", 1, id, []byte("{}"), "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	h.BeginToolRound()
	defer h.EndToolRound()
	if msgs := mustValid(t, h); len(msgs) != 4 {
		t.Fatalf("history = %+v", msgs)
	}
}
