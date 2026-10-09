package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

// A tool result that nearly fills the context budget leaves no room for the
// spoken reply in the active turn. Recording it must not fail the session (M1-05).
func TestOverBudgetReplyDoesNotFailCommit(t *testing.T) {
	newTurn := func(t *testing.T) (*core.ConversationContext, *core.AssistantContextProcessor, *frameRec) {
		ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4000})
		if err := ctx.AddMessage(core.Message{Role: core.RoleUser, Content: "look it up", TurnID: "t1"}); err != nil {
			t.Fatal(err)
		}
		if err := ctx.AddToolCall("t1", 1, "c1", "search", []byte(`{}`), time.Time{}); err != nil {
			t.Fatal(err)
		}
		big := `"` + strings.Repeat("x", 15000) + `"`
		if err := ctx.AddToolResult("t1", 1, "c1", []byte(big), "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		p := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
		r := &frameRec{}
		bg := context.Background()
		_ = p.ProcessFrame(bg, &core.UserMessageFrame{Turn: "t1", Content: "look it up", Gen: 1}, core.Downstream, r)
		reply := strings.Repeat("Here is a fairly long spoken answer sentence. ", 30)
		_ = p.ProcessFrame(bg, &core.LLMTextFrame{Turn: "t1", Gen: 1, Text: reply}, core.Downstream, r)
		return ctx, p, r
	}

	{
		_, p, r := newTurn(t)
		for _, su := range r.units() {
			err := p.ProcessFrame(context.Background(), &core.WriteCompletionFrame{Turn: "t1", Gen: 1, UnitID: su.UnitID, IsFinal: true}, core.Upstream, r)
			if err != nil {
				t.Fatalf("commit returned %v: ProcessFrame errors fail the session", err)
			}
		}
		if p.Active() {
			t.Fatal("reply still active after the last unit played")
		}
	}
}
