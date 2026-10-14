package core

import (
	"context"
	"errors"
	"testing"
)

// failingSession accepts the first n-1 Reenter calls and refuses the nth.
type failingSession struct {
	failAt int
	calls  int
	frames []Frame
}

func (f *failingSession) ID() uint64                     { return 1 }
func (f *failingSession) Context() context.Context       { return context.Background() }
func (f *failingSession) CurrentGeneration() uint64      { return 1 }
func (f *failingSession) IncrementGeneration() uint64    { return 2 }
func (f *failingSession) Go(func(context.Context)) error { return nil }
func (f *failingSession) Reenter(fr Frame, _ Direction, _ int, _ uint64) error {
	f.calls++
	if f.calls == f.failAt {
		return errors.New("queue full")
	}
	f.frames = append(f.frames, fr)
	return nil
}

// A Say that fails halfway must not leave the assistant turn open: the bot would look busy until the next caller message.
func TestSayClosesItsTurnWhenAFrameCannotBeQueued(t *testing.T) {
	for _, failAt := range []int{2, 3} {
		s := &failingSession{failAt: failAt}
		p := NewAssistantContextProcessor(NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000}), AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
		if err := p.Attach(s, 0); err != nil {
			t.Fatal(err)
		}
		if err := p.Say("Hello there."); err == nil {
			t.Fatalf("failAt %d: Say reported success", failAt)
		}
		last, ok := s.frames[len(s.frames)-1].(*LLMTextFrame)
		if !ok || last.Text != "" {
			t.Fatalf("failAt %d: the turn was left open; last frame %#v", failAt, s.frames[len(s.frames)-1])
		}
	}
}
