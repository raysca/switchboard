package google

import (
	"context"
	"errors"
	"sync"
	"testing"

	"govoice/core"
)

// spentSession is a session whose task budget is used up: every Go is refused.
type spentSession struct {
	mu  sync.Mutex
	got []core.Frame
}

func (*spentSession) ID() uint64                     { return 1 }
func (*spentSession) Context() context.Context       { return context.Background() }
func (*spentSession) CurrentGeneration() uint64      { return 1 }
func (*spentSession) IncrementGeneration() uint64    { return 2 }
func (*spentSession) Go(func(context.Context)) error { return core.ErrTooManyTasks }
func (s *spentSession) Reenter(f core.Frame, _ core.Direction, _ int, _ uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, f)
	return nil
}

func TestRefusedTaskLosesTheTurnNotTheCall(t *testing.T) {
	cfg := DefaultLLMConfig()
	cfg.Client = &mockLLMClient{}
	p, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &spentSession{}
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"}, core.Downstream, probeEmitter{}); err != nil {
		t.Fatalf("a spent task budget failed the call: %v", err)
	}
	if _, errs, _, _ := p.LLMCounts(); errs != 0 {
		t.Fatalf("a refused local task counted as %d provider errors", errs)
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
