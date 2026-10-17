package xai

import (
	"context"
	"errors"
	"testing"

	"govoice/core"
)

// spentSession is a session whose task budget is used up: every Go is refused.
type spentSession struct{ *mockSessionController }

func (spentSession) Go(func(context.Context)) error { return core.ErrTooManyTasks }

func TestRefusedTaskLosesTheTurnNotTheCall(t *testing.T) {
	p, err := NewProcessor(Config{Endpoint: "http://127.0.0.1:1/v1/responses", APIKey: "xai-test-key-1234567890"})
	if err != nil {
		t.Fatal(err)
	}
	s := spentSession{newMockSessionController(1, 1)}
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatalf("a spent task budget failed the call: %v", err)
	}
	s.reenteredMu.Lock()
	defer s.reenteredMu.Unlock()
	if len(s.reentered) != 1 {
		t.Fatalf("re-entered %d frames, want one error", len(s.reentered))
	}
	ef, ok := s.reentered[0].(*core.ErrorFrame)
	if !ok || ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || ef.Turn != "t1" || ef.Gen != 1 {
		t.Fatalf("frame %+v", s.reentered[0])
	}
}
