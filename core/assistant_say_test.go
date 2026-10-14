package core_test

import (
	"strings"
	"testing"
	"time"

	"govoice/core"
)

// sayRig is an assistant context processor alone in a session; its output edge is `out`.
func sayRig(t *testing.T, id uint64) (*core.ConversationContext, *core.AssistantContextProcessor, *core.Session, chan core.Frame) {
	t.Helper()
	ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4000})
	a := core.NewAssistantContextProcessor(ctx, core.AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
	pipe, err := core.NewPipeline(a)
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(id, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan core.Frame, 64)
	s.OnOutput = func(f core.Frame) { out <- f }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return ctx, a, s, out
}

// sayUnits collects the synthesis units that reach the edge until the final one.
func sayUnits(t *testing.T, out <-chan core.Frame) []*core.SynthesisUnitFrame {
	t.Helper()
	var got []*core.SynthesisUnitFrame
	for {
		select {
		case f := <-out:
			if u, ok := f.(*core.SynthesisUnitFrame); ok {
				got = append(got, u)
				if u.IsFinal {
					return got
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no final unit; got %d units", len(got))
		}
	}
}

func assistantTexts(ctx *core.ConversationContext) (out []core.Message) {
	for _, m := range ctx.Messages() {
		if m.Role == core.RoleAssistant {
			out = append(out, m)
		}
	}
	return
}

func eventuallyMessages(t *testing.T, ctx *core.ConversationContext, n int) []core.Message {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if m := assistantTexts(ctx); len(m) >= n {
			return m
		}
	}
	t.Fatalf("history has %d assistant messages, want %d", len(assistantTexts(ctx)), n)
	return nil
}

func TestAssistantSaySpeaksAndRecordsTheLine(t *testing.T) {
	ctx, a, s, out := sayRig(t, 9101)
	if err := a.Say("Hello, thanks for calling."); err != nil {
		t.Fatal(err)
	}
	us := sayUnits(t, out)
	var spoken strings.Builder
	for _, u := range us {
		spoken.WriteString(u.Text)
		_ = s.Post(&core.WriteCompletionFrame{Turn: u.Turn, Gen: u.Gen, UnitID: u.UnitID, IsFinal: u.IsFinal, Media: 500 * time.Millisecond})
	}
	if strings.TrimSpace(spoken.String()) != "Hello, thanks for calling." {
		t.Fatalf("spoken %q", spoken.String())
	}
	msgs := eventuallyMessages(t, ctx, 1)
	if len(msgs) != 1 || msgs[0].Content != "Hello, thanks for calling." || msgs[0].Partial {
		t.Fatalf("history %+v", msgs)
	}
	// An AssistantMessageFrame (the turn recorder's trigger) is emitted for the finished turn.
	for {
		select {
		case f := <-out:
			if am, ok := f.(*core.AssistantMessageFrame); ok {
				if am.Content != "Hello, thanks for calling." || am.Partial {
					t.Fatalf("assistant message %+v", am)
				}
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no AssistantMessageFrame")
		}
	}
}

func TestAssistantSayKeepsOnlyTheHeardPartWhenInterrupted(t *testing.T) {
	ctx, a, s, out := sayRig(t, 9102)
	if err := a.Say("First sentence. Second part is long and wordy."); err != nil {
		t.Fatal(err)
	}
	us := sayUnits(t, out)
	if len(us) < 2 {
		t.Fatalf("want two units, got %d", len(us))
	}
	first := us[0]
	_ = s.Post(&core.WriteCompletionFrame{Turn: first.Turn, Gen: first.Gen, UnitID: first.UnitID, Media: time.Second})
	_ = s.Post(&core.InterruptionFrame{Turn: first.Turn, Gen: first.Gen})
	msgs := eventuallyMessages(t, ctx, 1)
	if !msgs[0].Partial || strings.Contains(msgs[0].Content, "wordy") || !strings.Contains(msgs[0].Content, "First sentence.") {
		t.Fatalf("history %+v", msgs)
	}
}

func TestAssistantSayTurnsAreDistinctAndNeverReachTheModelStage(t *testing.T) {
	_, a, _, out := sayRig(t, 9103)
	if err := a.Say("One."); err != nil {
		t.Fatal(err)
	}
	if err := a.Say("Two."); err != nil {
		t.Fatal(err)
	}
	turns := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(turns) < 2 {
		select {
		case f := <-out:
			if u, ok := f.(*core.SynthesisUnitFrame); ok {
				turns[u.Turn] = true
			}
		case <-deadline:
			t.Fatalf("turns seen %v", turns)
		}
	}
}

func TestAssistantSayRefusesEmptyTextAndAClosedSession(t *testing.T) {
	_, a, s, _ := sayRig(t, 9104)
	for _, text := range []string{"", "   ", "\n\t"} {
		if err := a.Say(text); err == nil {
			t.Fatalf("Say(%q) was accepted", text)
		}
	}
	_ = s.Close()
	if err := a.Say("late"); err == nil {
		t.Fatal("Say on a closed session was accepted")
	}
}
