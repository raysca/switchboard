package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type summaryRig struct {
	c    *ConversationContext
	s    *ContextSummarizer
	done chan struct{}
	evs  chan string
}

// newSummaryRig runs spawned tasks in a goroutine; done receives one signal per task.
func newSummaryRig(t *testing.T, cfg ContextConfig, sc SummaryConfig) *summaryRig {
	r := &summaryRig{c: NewConversationContext(cfg), done: make(chan struct{}, 8), evs: make(chan string, 1024)}
	spawn := func(fn func(context.Context)) error {
		go func() { fn(context.Background()); r.done <- struct{}{} }()
		return nil
	}
	r.s = NewContextSummarizer(r.c, sc, spawn, func(e DebugEvent) { r.evs <- e.Name })
	clk := time.Unix(1000, 0)
	r.s.now = func() time.Time { return clk }
	_ = r.c.SetSystemInstructionChecked("sys", time.Time{})
	return r
}

func (r *summaryRig) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("summarizer task did not finish")
	}
}

func (r *summaryRig) turn(t *testing.T, i int, tool bool) {
	t.Helper()
	id := fmt.Sprintf("t%d", i)
	at := time.Unix(int64(i), 0)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.c.AddMessage(Message{Role: RoleUser, Content: "question " + id + pad, TurnID: id, CreatedAt: at}))
	if tool {
		must(seedToolCall(r.c, id, 1, "c"+id, "lookup", []byte(`{}`), at))
		must(seedToolResult(r.c, id, 1, "c"+id, []byte(`"ok"`), "", at))
	}
	must(r.c.AddMessage(Message{Role: RoleAssistant, Content: "answer " + id + pad, TurnID: id, CreatedAt: at}))
}

const pad = " ................................................................................"

func fakeSummarizer(msgs []Message) (string, error) {
	return fmt.Sprintf("summary of %d messages", len(msgs)), nil
}

func assertNoOrphans(t *testing.T, msgs []Message) {
	t.Helper()
	calls := map[string]bool{}
	for _, m := range msgs {
		switch m.Kind {
		case MessageToolCall:
			calls[m.ToolCallID] = true
		case MessageToolResult:
			if !calls[m.ToolCallID] {
				t.Fatalf("orphan tool result %q", m.ToolCallID)
			}
		}
	}
}

func TestSummary_ReplacesOlderSpan_KeepsSystemAndRecentTurns(t *testing.T) {
	r := newSummaryRig(t, ContextConfig{}, SummaryConfig{TriggerMessages: 6, KeepTurns: 2,
		Summarizer: func(_ context.Context, m []Message) (string, error) { return fakeSummarizer(m) }})
	for i := 1; i <= 5; i++ {
		r.turn(t, i, i == 2)
	}
	if !r.s.Maybe() {
		t.Fatal("expected summarization to start")
	}
	if r.s.Maybe() {
		t.Fatal("second task must not start while one is in flight")
	}
	r.wait(t)
	msgs := r.c.Messages()
	if msgs[0].Content != "sys" || !strings.HasPrefix(msgs[1].Content, SummaryPrefix+"summary of 8") || msgs[1].Role != RoleUser {
		t.Fatalf("bad head: %+v", msgs[:2])
	}
	if len(msgs) != 1+1+4 || msgs[2].TurnID != "t4" {
		t.Fatalf("recent turns not kept verbatim: %+v", msgs)
	}
	assertNoOrphans(t, msgs)
	if a, f, d := r.s.Counts(); a != 1 || f != 0 || d != 0 || <-r.evs != DebugContextSummarized {
		t.Fatal("counts/event wrong")
	}
	// Rolling: below trigger now, so nothing starts.
	if r.s.Maybe() {
		t.Fatal("below trigger should not start")
	}
}

func TestSummary_NeverSplitsInFlightToolPair(t *testing.T) {
	r := newSummaryRig(t, ContextConfig{}, SummaryConfig{TriggerMessages: 3, KeepTurns: 1,
		Summarizer: func(_ context.Context, m []Message) (string, error) { return fakeSummarizer(m) }})
	r.turn(t, 1, false)
	r.turn(t, 2, false)
	// Turn 3: call in flight (no result yet), then turn 4 is newest.
	_ = r.c.AddMessage(Message{Role: RoleUser, Content: "q3" + pad, TurnID: "t3", CreatedAt: time.Unix(3, 0)})
	_ = seedToolCall(r.c, "t3", 1, "ct3", "slow", []byte(`{}`), time.Unix(3, 0))
	r.turn(t, 4, false)
	if !r.s.Maybe() {
		t.Fatal("expected start")
	}
	r.wait(t)
	msgs := r.c.Messages()
	if !strings.Contains(msgs[1].Content, "summary of 4") { // t1+t2 only; turn 3 stays whole
		t.Fatalf("span should stop before the in-flight call: %q", msgs[1].Content)
	}
	if err := seedToolResult(r.c, "t3", 1, "ct3", []byte(`"late"`), "", time.Unix(5, 0)); err != nil {
		t.Fatalf("late result must still match its call: %v", err)
	}
	assertNoOrphans(t, r.c.Messages())
}

func TestSummary_FailureAndTimeout_DegradeToTruncation(t *testing.T) {
	for name, fn := range map[string]Summarizer{
		"error":   func(context.Context, []Message) (string, error) { return "", errors.New("boom") },
		"timeout": func(ctx context.Context, _ []Message) (string, error) { <-ctx.Done(); return "", ctx.Err() },
		"empty":   func(context.Context, []Message) (string, error) { return "  ", nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ContextConfig{MaxTokens: 150}
			r := newSummaryRig(t, cfg, SummaryConfig{TriggerMessages: 4, KeepTurns: 1, Summarizer: fn, Timeout: 10 * time.Millisecond})
			for i := 1; i <= 3; i++ {
				r.turn(t, i, false)
			}
			before := r.c.Messages()
			if !r.s.Maybe() {
				t.Fatal("expected start")
			}
			r.wait(t)
			if got := r.c.Messages(); len(got) != len(before) {
				t.Fatal("failed summary must not change context")
			}
			if _, f, _ := r.s.Counts(); f != 1 || <-r.evs != DebugContextSummaryFailed {
				t.Fatal("failure not counted/emitted")
			}
			if r.s.Maybe() {
				t.Fatal("retry must wait for RetryAfter")
			}
			// Budget truncation still bounds the context.
			for i := 4; i <= 30; i++ {
				r.turn(t, i, false)
				if r.c.EstimatedTokens() > cfg.MaxTokens {
					t.Fatal("budget exceeded")
				}
			}
		})
	}
}

func TestSummary_MessageAppendedDuringSummarization_IsKept(t *testing.T) {
	release := make(chan struct{})
	r := newSummaryRig(t, ContextConfig{}, SummaryConfig{TriggerMessages: 6, KeepTurns: 1,
		Summarizer: func(_ context.Context, m []Message) (string, error) { <-release; return fakeSummarizer(m) }})
	for i := 1; i <= 4; i++ {
		r.turn(t, i, false)
	}
	r.s.Maybe()
	r.turn(t, 5, false) // appended while summarizing
	close(release)
	r.wait(t)
	msgs := r.c.Messages()
	if a, _, _ := r.s.Counts(); a != 1 || msgs[len(msgs)-1].Content != "answer t5"+pad {
		t.Fatalf("append lost or summary not applied: %+v", msgs)
	}
}

func TestSummary_SpanMutatedMeanwhile_Discarded(t *testing.T) {
	release := make(chan struct{})
	// MaxMessages makes the budget evict the oldest turn (inside the span) mid-flight.
	r := newSummaryRig(t, ContextConfig{MaxMessages: 9}, SummaryConfig{TriggerMessages: 6, KeepTurns: 1,
		Summarizer: func(_ context.Context, m []Message) (string, error) { <-release; return fakeSummarizer(m) }})
	for i := 1; i <= 4; i++ {
		r.turn(t, i, false)
	}
	r.s.Maybe()
	r.turn(t, 5, false) // 11 messages > 9: evicts t1 from the span
	close(release)
	r.wait(t)
	if a, _, d := r.s.Counts(); a != 0 || d != 1 {
		t.Fatalf("stale summary must be discarded, got applied=%d discarded=%d", a, d)
	}
	for _, m := range r.c.Messages() {
		if m.TurnID == summaryTurnID {
			t.Fatal("summary must not be applied")
		}
	}
}

func TestSummary_SessionClosed_NotApplied(t *testing.T) {
	c := NewConversationContext(ContextConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := NewContextSummarizer(c, SummaryConfig{TriggerMessages: 2, KeepTurns: 1, Summarizer: func(_ context.Context, m []Message) (string, error) {
		cancel()
		return fakeSummarizer(m)
	}}, func(fn func(context.Context)) error { go func() { fn(ctx); close(done) }(); return nil }, nil)
	for i := 1; i <= 3; i++ {
		_ = c.AddMessage(Message{Role: RoleUser, Content: "q", TurnID: fmt.Sprint("t", i)})
	}
	s.Maybe()
	<-done
	if a, f, d := s.Counts(); a+f+d != 0 {
		t.Fatal("closed session must not apply or count")
	}
}

func TestSummary_LongConversation_BudgetNeverExceededNoOrphans(t *testing.T) {
	cfg := ContextConfig{MaxTokens: 400}
	r := newSummaryRig(t, cfg, SummaryConfig{TriggerTokens: 250, KeepTurns: 1,
		Summarizer: func(_ context.Context, m []Message) (string, error) { return fakeSummarizer(m) }})
	for i := 1; i <= 200; i++ {
		r.turn(t, i, i%3 == 0)
		if i%5 == 0 { // slow tool: call now, result two turns later, so it is in flight across a summary
			_ = seedToolCall(r.c, fmt.Sprint("t", i), 1, fmt.Sprint("slow", i), "slow", []byte(`{}`), time.Unix(int64(i), 0))
		}
		if i%5 == 2 && i > 5 {
			if err := seedToolResult(r.c, fmt.Sprint("t", i-2), 1, fmt.Sprint("slow", i-2), []byte(`"ok"`), "", time.Unix(int64(i), 0)); err != nil {
				t.Fatalf("turn %d: late result lost its call: %v", i, err)
			}
		}
		if r.c.EstimatedTokens() > cfg.MaxTokens {
			t.Fatalf("turn %d: budget exceeded: %d", i, r.c.EstimatedTokens())
		}
		assertNoOrphans(t, r.c.Messages())
		if r.s.Maybe() {
			r.wait(t)
		}
	}
	if a, _, _ := r.s.Counts(); a == 0 {
		t.Fatal("expected summaries to be applied")
	}
	if n := r.c.EvictionCount(); n != 0 { // summarizing, not truncation, kept it in budget
		t.Fatalf("expected no truncation evictions, got %d", n)
	}
}

func TestSummary_PullBackDoesNotOrphanEarlierLateResult(t *testing.T) {
	r := newSummaryRig(t, ContextConfig{}, SummaryConfig{TriggerMessages: 3, KeepTurns: 1,
		Summarizer: func(_ context.Context, m []Message) (string, error) { return fakeSummarizer(m) }})
	r.turn(t, 1, false)
	at := time.Unix(2, 0)
	_ = r.c.AddMessage(Message{Role: RoleUser, Content: "q2" + pad, TurnID: "t2", CreatedAt: at})
	_ = seedToolCall(r.c, "t2", 1, "c2", "slow", []byte(`{}`), at)
	_ = r.c.AddMessage(Message{Role: RoleUser, Content: "q3" + pad, TurnID: "t3", CreatedAt: at})
	_ = seedToolCall(r.c, "t3", 1, "c3", "slow", []byte(`{}`), at)   // still in flight
	_ = seedToolResult(r.c, "t2", 1, "c2", []byte(`"late"`), "", at) // lands after t3 started
	r.turn(t, 4, false)
	if r.s.Maybe() {
		r.wait(t)
	}
	assertNoOrphans(t, r.c.Messages())
}

// No summarization is started while a tool round is open (the round's queue
// flush and the span replacement must not interleave); it resumes after.
func TestSummarizerSkipsOpenToolRound(t *testing.T) {
	r := newSummaryRig(t, ContextConfig{MaxMessages: 100, MaxTokens: 100000}, SummaryConfig{
		Summarizer: func(_ context.Context, m []Message) (string, error) { return fakeSummarizer(m) }, TriggerTokens: 100, KeepTurns: 2})
	for i := 1; i <= 6; i++ {
		r.turn(t, i, false)
	}
	r.c.BeginToolRound()
	if r.s.Maybe() {
		t.Fatal("Maybe started a task mid tool round")
	}
	r.c.EndToolRound()
	if !r.s.Maybe() {
		t.Fatal("Maybe did not start after the round ended")
	}
	r.wait(t)
}

func TestSummarizerOnResult(t *testing.T) {
	type res struct {
		outcome string
		saved   int
	}
	got := make(chan res, 4)
	fail := false
	r := newSummaryRig(t, ContextConfig{MaxMessages: 100, MaxTokens: 100000}, SummaryConfig{
		Summarizer: func(_ context.Context, m []Message) (string, error) {
			if fail {
				return "", errors.New("down")
			}
			return fakeSummarizer(m)
		}, TriggerTokens: 100, KeepTurns: 2,
		OnResult: func(o string, _ time.Duration, saved int) { got <- res{o, saved} }})
	for i := 1; i <= 6; i++ {
		r.turn(t, i, false)
	}
	r.s.Maybe()
	r.wait(t)
	if a := <-got; a.outcome != "applied" || a.saved <= 0 {
		t.Fatalf("applied result = %+v", a)
	}
	fail = true
	for i := 7; i <= 12; i++ {
		r.turn(t, i, false)
	}
	r.s.Maybe()
	r.wait(t)
	if f := <-got; f.outcome != "failed" || f.saved != 0 {
		t.Fatalf("failed result = %+v", f)
	}
}
