package core_test

import (
	"errors"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

func TestUserContextV2AcceptsOnlyCommittedTranscript(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	user := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{
		TurnPrefix:                 "turn",
		RequireCommittedTranscript: true,
	})
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(user, sink)
	session, _ := core.NewSession(901, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	at := time.Unix(20, 0)
	_ = session.Post(&core.FinalTranscriptionFrame{Text: "raw", Turn: "A", Gen: 1, Timestamp: at})
	_ = session.Post(&core.FinalTranscriptionFrame{Text: "committed", Turn: "A", Gen: 1, Timestamp: at, Committed: true})
	time.Sleep(20 * time.Millisecond)
	msgs := ctxPool.Messages()
	if len(msgs) != 1 || msgs[0].Content != "committed" || msgs[0].CreatedAt != at {
		t.Fatalf("V2 context accepted raw or missed commit: %#v", msgs)
	}
}

func TestUserContextV2RejectsCommittedTranscriptWithoutPolicyTime(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	user := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{RequireCommittedTranscript: true})
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(user, sink)
	session, _ := core.NewSession(902, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.FinalTranscriptionFrame{Text: "invalid", Turn: "A", Gen: 1, Committed: true})
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("session did not reject malformed committed transcript")
	}
	if !errors.Is(session.Err(), core.ErrInvalidCommittedTranscript) {
		t.Fatalf("expected invalid committed transcript error, got %v", session.Err())
	}
	if len(ctxPool.Messages()) != 0 {
		t.Fatalf("malformed committed transcript entered context: %#v", ctxPool.Messages())
	}
}

func TestUserContextCommittedIdempotenceIsTurnGenerationScopedAndWindowed(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	user := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{RequireCommittedTranscript: true, MaxCommittedTurns: 2})
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(user, sink)
	session, _ := core.NewSession(903, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	at := time.Unix(30, 0)
	a := &core.FinalTranscriptionFrame{Text: "same", Turn: "A", Gen: 1, Timestamp: at, Committed: true}
	_ = session.Post(a)
	_ = session.Post(a)
	_ = session.Post(&core.FinalTranscriptionFrame{Text: "same", Turn: "B", Gen: 1, Timestamp: at, Committed: true})
	time.Sleep(20 * time.Millisecond)
	msgs := ctxPool.Messages()
	if len(msgs) != 2 || msgs[0].TurnID != "A" || msgs[1].TurnID != "B" {
		t.Fatalf("turn-scoped idempotence mismatch: %#v", msgs)
	}
	// The window holds two turns: C evicts A, the call goes on, and B (still in
	// the window) is still suppressed as a duplicate.
	_ = session.Post(&core.FinalTranscriptionFrame{Text: "third", Turn: "C", Gen: 1, Timestamp: at, Committed: true})
	_ = session.Post(&core.FinalTranscriptionFrame{Text: "same", Turn: "B", Gen: 1, Timestamp: at, Committed: true})
	time.Sleep(20 * time.Millisecond)
	if err := session.Err(); err != nil {
		t.Fatalf("session failed after the idempotence window filled: %v", err)
	}
	msgs = ctxPool.Messages()
	if len(msgs) != 3 || msgs[2].TurnID != "C" {
		t.Fatalf("window mismatch: %#v", msgs)
	}
}
