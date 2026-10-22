package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type probeStage struct {
	frames, closed, attached atomic.Int64
	block                    chan struct{}
}

func (p *probeStage) ProcessFrame(ctx context.Context, f Frame, d Direction, e Emitter) error {
	p.frames.Add(1)
	if p.block != nil {
		<-p.block
	}
	return nil
}
func (p *probeStage) Attach(SessionController, int) error { p.attached.Add(1); return nil }
func (p *probeStage) Close() error                        { p.closed.Add(1); return nil }

type fakeSession struct{ SessionController }

func TestStageSwapHandoffsToCurrent(t *testing.T) {
	a, b := &probeStage{}, &probeStage{}
	s := NewStageSwap(a)
	if err := s.Attach(fakeSession{}, 3); err != nil {
		t.Fatal(err)
	}
	_ = s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	retire, err := s.Replace(b)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	if a.frames.Load() != 1 || b.frames.Load() != 1 || b.attached.Load() != 1 {
		t.Fatalf("a=%d b=%d attached=%d", a.frames.Load(), b.frames.Load(), b.attached.Load())
	}
	if err := retire(time.Second); err != nil || a.closed.Load() != 1 || b.closed.Load() != 0 {
		t.Fatalf("retire err=%v a.closed=%d b.closed=%d", err, a.closed.Load(), b.closed.Load())
	}
}

func TestStageSwapReplaceBeforeAttach(t *testing.T) {
	if _, err := NewStageSwap(&probeStage{}).Replace(&probeStage{}); !errors.Is(err, ErrSwapNotAttached) {
		t.Fatalf("want ErrSwapNotAttached, got %v", err)
	}
}

func TestStageSwapRetireWaitsForInflight(t *testing.T) {
	a := &probeStage{block: make(chan struct{})}
	s := NewStageSwap(a)
	_ = s.Attach(fakeSession{}, 0)
	go s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	for a.frames.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	retire, _ := s.Replace(&probeStage{})
	done := make(chan error, 1)
	go func() { done <- retire(2 * time.Second) }()
	time.Sleep(30 * time.Millisecond)
	if a.closed.Load() != 0 {
		t.Fatal("closed while a frame was still in flight")
	}
	close(a.block)
	if err := <-done; err != nil || a.closed.Load() != 1 {
		t.Fatalf("err=%v closed=%d", err, a.closed.Load())
	}
}

func TestStageSwapRetireTimeoutStillCloses(t *testing.T) {
	a := &probeStage{block: make(chan struct{})}
	s := NewStageSwap(a)
	_ = s.Attach(fakeSession{}, 0)
	go s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	for a.frames.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	retire, _ := s.Replace(&probeStage{})
	if err := retire(20 * time.Millisecond); !errors.Is(err, ErrRetireTimeout) || a.closed.Load() != 1 {
		t.Fatalf("a hung stage must be closed after the timeout: err=%v closed=%d", err, a.closed.Load())
	}
	close(a.block)
}

type ctxProbe struct {
	probeStage
	got atomic.Int64
}

func (c *ctxProbe) SetContextProvider(func() []Message) { c.got.Add(1) }

func TestStageSwapForwardsContextProviderToLaterStages(t *testing.T) {
	a, b := &ctxProbe{}, &ctxProbe{}
	s := NewStageSwap(a)
	_ = s.Attach(fakeSession{}, 0)
	s.SetContextProvider(func() []Message { return nil })
	if _, err := s.Replace(b); err != nil {
		t.Fatal(err)
	}
	if a.got.Load() != 1 || b.got.Load() != 1 {
		t.Fatalf("provider must reach every inner stage: a=%d b=%d", a.got.Load(), b.got.Load())
	}
}

func TestStageSwapBeforeHookRunsAheadOfFrame(t *testing.T) {
	a := &probeStage{}
	s := NewStageSwap(a)
	_ = s.Attach(fakeSession{}, 0)
	var seen atomic.Int64
	s.SetBefore(func(Frame, Direction) { seen.Store(a.frames.Load() + 10) })
	_ = s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	if seen.Load() != 10 {
		t.Fatalf("hook must run before the inner stage sees the frame, saw frames=%d", seen.Load()-10)
	}
}

func TestStageSwapSummarizeWithoutSupport(t *testing.T) {
	s := NewStageSwap(&probeStage{})
	if _, err := s.SummarizeText(context.Background(), "s", "u"); !errors.Is(err, ErrNoSummarizer) {
		t.Fatalf("want ErrNoSummarizer, got %v", err)
	}
}
