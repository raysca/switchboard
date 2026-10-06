package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestContextCompaction_HookDeclinedLeavesContextUsable(t *testing.T) {
	ctx := context.Background()
	cc := NewConversationContext(ContextConfig{MaxMessages: 20})
	cc.SetSystemInstruction("You are a helpful assistant.")

	// Add turn 1
	if err := cc.AddMessage(Message{Role: RoleUser, Content: "Hello", TurnID: "turn-1"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := cc.AddMessage(Message{Role: RoleAssistant, Content: "Hi there!", TurnID: "turn-1"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	// Register a hook that declines (returns false, nil)
	declinedCount := 0
	decliningHook := CompactionHookFunc(func(c context.Context, snapshot []Message) (*CompactionResult, bool, error) {
		declinedCount++
		return nil, false, nil // declined
	})

	cc.SetCompactionHook(decliningHook)

	// Trigger compaction
	applied, err := cc.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if applied {
		t.Fatal("expected compaction not applied when hook declines")
	}
	if declinedCount != 1 {
		t.Fatalf("expected hook called once, got %d", declinedCount)
	}

	// Verify context is still intact and completely usable
	msgs := cc.Messages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// Adding next turn works normally
	if err := cc.AddMessage(Message{Role: RoleUser, Content: "How are you?", TurnID: "turn-2"}); err != nil {
		t.Fatalf("AddMessage after declined compaction: %v", err)
	}
	if err := cc.AddMessage(Message{Role: RoleAssistant, Content: "Doing well!", TurnID: "turn-2"}); err != nil {
		t.Fatalf("AddMessage after declined compaction: %v", err)
	}
	if len(cc.Messages()) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(cc.Messages()))
	}
}

func TestContextCompaction_HookErrorLeavesContextUsable(t *testing.T) {
	ctx := context.Background()
	cc := NewConversationContext(ContextConfig{MaxMessages: 20})
	cc.SetSystemInstruction("System instruction.")

	if err := cc.AddMessage(Message{Role: RoleUser, Content: "User message 1", TurnID: "turn-1"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := cc.AddMessage(Message{Role: RoleAssistant, Content: "Assistant response 1", TurnID: "turn-1"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	// Register hook that errors out
	hookErr := errors.New("compactor service unavailable")
	erroringHook := CompactionHookFunc(func(c context.Context, snapshot []Message) (*CompactionResult, bool, error) {
		return nil, false, hookErr
	})

	cc.SetCompactionHook(erroringHook)

	applied, err := cc.Compact(ctx)
	if !errors.Is(err, hookErr) {
		t.Fatalf("expected hook error %v, got %v", hookErr, err)
	}
	if applied {
		t.Fatal("expected compaction not applied on hook error")
	}

	// Context remains usable and untouched
	msgs := cc.Messages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after hook failure, got %d", len(msgs))
	}
	if msgs[0].Role != RoleSystem || msgs[0].Content != "System instruction." {
		t.Fatal("system instruction altered or missing")
	}
}

func TestContextCompaction_SystemInstructionPreserved(t *testing.T) {
	ctx := context.Background()
	cc := NewConversationContext(ContextConfig{MaxMessages: 20})
	sysPrompt := "Strict system rules."
	cc.SetSystemInstruction(sysPrompt)

	for i := 1; i <= 3; i++ {
		turnID := "turn-" + string(rune('0'+i))
		if err := cc.AddMessage(Message{Role: RoleUser, Content: "Question", TurnID: turnID}); err != nil {
			t.Fatal(err)
		}
		if err := cc.AddMessage(Message{Role: RoleAssistant, Content: "Answer", TurnID: turnID}); err != nil {
			t.Fatal(err)
		}
	}

	// Hook that compacts old turns into a summary message
	summaryHook := CompactionHookFunc(func(c context.Context, snapshot []Message) (*CompactionResult, bool, error) {
		// Replace earlier turns with a single summary
		return &CompactionResult{
			Messages: []Message{
				{Role: RoleSystem, Content: sysPrompt},
				{Role: RoleAssistant, Content: "Summary of earlier conversation: user asked questions, assistant answered."},
				snapshot[len(snapshot)-2], // preserve latest user
				snapshot[len(snapshot)-1], // preserve latest assistant
			},
		}, true, nil
	})

	cc.SetCompactionHook(summaryHook)

	applied, err := cc.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !applied {
		t.Fatal("expected compaction applied")
	}

	msgs := cc.Messages()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages after compaction, got %d", len(msgs))
	}
	if msgs[0].Role != RoleSystem || msgs[0].Content != sysPrompt {
		t.Fatalf("system instruction violated: %+v", msgs[0])
	}
}

func TestContextCompaction_ToolStateRetained(t *testing.T) {
	ctx := context.Background()
	cc := NewConversationContext(ContextConfig{MaxMessages: 20})
	cc.SetSystemInstruction("System prompt.")

	now := time.Now()
	// Add tool call and tool result in turn-1
	if err := cc.AddMessage(Message{Role: RoleUser, Content: "Check weather", TurnID: "turn-1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := cc.AddToolCall("turn-1", 1, "call-1", "get_weather", []byte(`{"city":"SF"}`), now); err != nil {
		t.Fatal(err)
	}
	if err := cc.AddToolResult("turn-1", 1, "call-1", []byte(`{"temp":65}`), "", now); err != nil {
		t.Fatal(err)
	}
	if err := cc.AddMessage(Message{Role: RoleAssistant, Content: "It's 65 degrees in SF.", TurnID: "turn-1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// Compaction hook summarizes the text but retains tool call and result state
	toolRetainingHook := CompactionHookFunc(func(c context.Context, snapshot []Message) (*CompactionResult, bool, error) {
		// Filter out plain user question, keeping system + tool call + tool result + assistant summary
		res := make([]Message, 0)
		for _, m := range snapshot {
			if m.Role == RoleSystem || m.Kind == MessageToolCall || m.Kind == MessageToolResult {
				res = append(res, m)
			}
		}
		res = append(res, Message{Role: RoleAssistant, Content: "Weather summary: 65F in SF.", TurnID: "turn-1", CreatedAt: now})
		return &CompactionResult{Messages: res}, true, nil
	})

	cc.SetCompactionHook(toolRetainingHook)

	applied, err := cc.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !applied {
		t.Fatal("expected compaction applied")
	}

	// Verify tool replay ledger still recognizes the tool call identity
	if err := cc.AddToolCall("turn-1", 1, "call-1", "get_weather", []byte(`{"city":"SF"}`), now); err != nil {
		t.Fatalf("idempotent replay of call-1 failed after compaction: %v", err)
	}

	// Verify conflicting reuse is still rejected
	if err := cc.AddToolCall("turn-1", 1, "call-1", "different_tool", []byte(`{}`), now); err == nil {
		t.Fatal("conflicting tool reuse should have been rejected by ledger")
	}
}

func TestContextCompaction_RejectsInvalidResultWithoutSystemPrompt(t *testing.T) {
	ctx := context.Background()
	cc := NewConversationContext(ContextConfig{MaxMessages: 20})
	cc.SetSystemInstruction("Original system prompt.")

	if err := cc.AddMessage(Message{Role: RoleUser, Content: "Hi", TurnID: "turn-1"}); err != nil {
		t.Fatal(err)
	}
	if err := cc.AddMessage(Message{Role: RoleAssistant, Content: "Hello", TurnID: "turn-1"}); err != nil {
		t.Fatal(err)
	}

	// Malicious or buggy hook tries to strip system prompt completely
	buggyHook := CompactionHookFunc(func(c context.Context, snapshot []Message) (*CompactionResult, bool, error) {
		return &CompactionResult{
			Messages: []Message{
				{Role: RoleUser, Content: "Hi", TurnID: "turn-1"},
				{Role: RoleAssistant, Content: "Hello", TurnID: "turn-1"},
			},
		}, true, nil
	})

	cc.SetCompactionHook(buggyHook)

	applied, err := cc.Compact(ctx)
	if !errors.Is(err, ErrInvalidCompaction) {
		t.Fatalf("expected ErrInvalidCompaction, got %v", err)
	}
	if applied {
		t.Fatal("expected compaction rejected")
	}

	// System instruction is preserved
	msgs := cc.Messages()
	if msgs[0].Role != RoleSystem || msgs[0].Content != "Original system prompt." {
		t.Fatal("original system prompt altered despite invalid compaction result")
	}
}

func TestContextCompaction_BoundedBudgetEnforcement(t *testing.T) {
	ctx := context.Background()
	// Budget of max 4 messages
	cc := NewConversationContext(ContextConfig{MaxMessages: 4})
	cc.SetSystemInstruction("System.")

	// Compaction hook tries to return 6 messages, exceeding MaxMessages budget
	oversizedHook := CompactionHookFunc(func(c context.Context, snapshot []Message) (*CompactionResult, bool, error) {
		msgs := []Message{
			{Role: RoleSystem, Content: "System."},
			{Role: RoleUser, Content: "1", TurnID: "t1"},
			{Role: RoleAssistant, Content: "1", TurnID: "t1"},
			{Role: RoleUser, Content: "2", TurnID: "t2"},
			{Role: RoleAssistant, Content: "2", TurnID: "t2"},
			{Role: RoleUser, Content: "3", TurnID: "t3"},
		}
		return &CompactionResult{Messages: msgs}, true, nil
	})

	cc.SetCompactionHook(oversizedHook)

	applied, err := cc.Compact(ctx)
	if !errors.Is(err, ErrContextLimitExceeded) && !errors.Is(err, ErrInvalidCompaction) {
		t.Fatalf("expected budget error, got %v", err)
	}
	if applied {
		t.Fatal("expected oversized compaction rejected")
	}
}

func TestContextCompaction_ConcurrentSafety(t *testing.T) {
	ctx := context.Background()
	cc := NewConversationContext(ContextConfig{MaxMessages: 50})
	cc.SetSystemInstruction("System instructions.")

	hook := CompactionHookFunc(func(c context.Context, snapshot []Message) (*CompactionResult, bool, error) {
		time.Sleep(5 * time.Millisecond)
		return &CompactionResult{
			Messages: []Message{
				snapshot[0], // system
				{Role: RoleAssistant, Content: "Summarized"},
			},
		}, true, nil
	})
	cc.SetCompactionHook(hook)

	var wg sync.WaitGroup
	// Concurrently add messages and run compaction
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = cc.AddMessage(Message{Role: RoleUser, Content: "msg", TurnID: "turn"})
				time.Sleep(2 * time.Millisecond)
			}
		}(i)
	}

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cc.Compact(ctx)
		}()
	}

	wg.Wait()

	// Must be non-empty and have system message at index 0
	msgs := cc.Messages()
	if len(msgs) == 0 || msgs[0].Role != RoleSystem {
		t.Fatal("context corrupted under concurrency")
	}
}
