package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type captureEmitter struct{ frames []Frame }

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

func TestContextToolOrderingCorrelationAndReplay(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	if err := ctx.AddMessage(Message{Role: RoleUser, Content: "weather", TurnID: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := ctx.AddToolCall("t", 1, "call-1", "weather", []byte(`{"city":"LON"}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ctx.AddToolResult("t", 1, "call-1", []byte(`{"temp":12}`), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ctx.AddToolResult("t", 1, "call-1", []byte(`{"temp":12}`), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	msgs := ctx.Messages()
	if len(msgs) != 3 || msgs[1].Role != RoleAssistant || msgs[1].ToolCallID != "call-1" || msgs[2].Role != RoleTool || msgs[2].ToolCallID != "call-1" {
		t.Fatalf("bad tool exchange: %#v", msgs)
	}
	if err := ctx.AddToolResult("t", 1, "missing", nil, "bad", time.Now()); !errors.Is(err, ErrUnmatchedToolResult) {
		t.Fatalf("unmatched result error = %v", err)
	}
}

func TestContextAggregatorConsumesPublicToolFramesInOrder(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	p := &ContextAggregator{Context: store}
	e := &captureEmitter{}
	frames := []Frame{
		&SystemMessageFrame{Content: "be concise"},
		&ToolCallFrame{Call: ToolCall{ID: "c", Name: "lookup", Arguments: json.RawMessage(`{"q":1}`), TurnID: "t", Generation: 3}},
		&ToolResultFrame{Turn: "t", Gen: 3, Result: ToolResult{CallID: "c", Status: ToolSucceeded, Result: json.RawMessage(`{"ok":true}`)}},
	}
	for _, f := range frames {
		if err := p.ProcessFrame(context.Background(), f, Downstream, e); err != nil {
			t.Fatal(err)
		}
	}
	msgs := store.Messages()
	if len(msgs) != 3 || msgs[0].Role != RoleSystem || msgs[1].Kind != MessageToolCall || msgs[2].Kind != MessageToolResult {
		t.Fatalf("ordered context = %#v", msgs)
	}
	bad := &ToolResultFrame{Turn: "t", Gen: 3, Result: ToolResult{CallID: "absent", Status: ToolSucceeded, Result: json.RawMessage(`{}`)}}
	if err := p.ProcessFrame(context.Background(), bad, Downstream, e); !errors.Is(err, ErrUnmatchedToolResult) {
		t.Fatalf("malformed correlation = %v", err)
	}
}

func TestContextConcurrentToolReplayCommitsExactlyOnce(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 20, MaxTokens: 1000})
	var done = make(chan error, 16)
	for i := 0; i < cap(done); i++ {
		go func() { done <- store.AddToolCall("t", 1, "c", "n", []byte(`{}`), time.Now()) }()
	}
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.Messages()); got != 1 {
		t.Fatalf("replayed calls committed %d messages", got)
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

func TestAssistantContextCancelDropsUndeliveredAndLateFinality(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	p := NewAssistantContextProcessor(store, DefaultAssistantContextConfig())
	e := &captureEmitter{}
	_ = p.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t", Gen: 2}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &LLMTextDeltaFrame{Turn: "t", Gen: 2, Delta: "not delivered"}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &CancelFrame{Gen: 2}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &LLMTextFrame{Turn: "t", Gen: 2, Text: "not delivered"}, Downstream, e)
	_ = p.ProcessFrame(context.Background(), &WriteCompletionFrame{Turn: "t", Gen: 2, UnitID: 1, IsFinal: true}, Downstream, e)
	if len(store.Messages()) != 0 {
		t.Fatalf("cancelled output entered context: %#v", store.Messages())
	}
}

func TestContextToolResultFailsClosedAtBudget(t *testing.T) {
	ctx := NewConversationContext(ContextConfig{MaxMessages: 0, MaxTokens: 20})
	_ = ctx.AddMessage(Message{Role: RoleUser, Content: "x", TurnID: "t"})
	_ = ctx.AddToolCall("t", 1, "c", "lookup", []byte(`{}`), time.Now())
	before := ctx.Messages()
	if err := ctx.AddToolResult("t", 1, "c", []byte(`{"very_large_result":"this cannot fit"}`), "", time.Now()); !errors.Is(err, ErrContextLimitExceeded) {
		t.Fatalf("budget error = %v", err)
	}
	if len(ctx.Messages()) != len(before) {
		t.Fatal("failed result changed context")
	}
}
