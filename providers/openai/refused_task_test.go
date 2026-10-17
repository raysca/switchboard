package openai

import (
	"context"
	"errors"
	"testing"

	"govoice/core"
)

// spentSession is a session whose task budget is used up: every Go is refused.
type spentSession struct{ *fakeSession }

func (spentSession) Go(func(context.Context)) error { return core.ErrTooManyTasks }

// A refused generation task loses that turn (one non-fatal error, re-entered so a failover stage sees it),
// not the call.
func TestRefusedTaskLosesTheTurnNotTheCall(t *testing.T) {
	p, err := NewProcessor(Config{BaseURL: "http://127.0.0.1:1", AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	s := spentSession{newSession()}
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"}, core.Downstream, sink{}); err != nil {
		t.Fatalf("a spent task budget failed the call: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 1 {
		t.Fatalf("re-entered %d frames, want one error", len(s.got))
	}
	ef, ok := s.got[0].(*core.ErrorFrame)
	if !ok || ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || ef.Turn != "t1" || ef.Gen != 1 {
		t.Fatalf("frame %+v", s.got[0])
	}
}
