package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrSwapNotAttached = errors.New("core: stage swap used before Attach")
	ErrNoSummarizer    = errors.New("core: active stage cannot summarize")
	ErrRetireTimeout   = errors.New("core: retired stage did not drain before the timeout")
)

// StageSwap is one pipeline stage whose implementation can be replaced while
// the call runs (a handoff swaps the LLM and the TTS). Frames go to the
// current inner stage; a replaced stage is retired once its in-flight frames
// finish, or after a timeout.
type StageSwap struct {
	cur atomic.Pointer[swapSlot]

	mu      sync.Mutex // guards the fields below; never held across inner calls
	session SessionController
	index   int
	before  func(Frame, Direction)
	ctxProv func() []Message
}

type swapSlot struct {
	p        Processor
	inflight atomic.Int64
}

func NewStageSwap(initial Processor) *StageSwap {
	s := &StageSwap{}
	s.cur.Store(&swapSlot{p: initial})
	return s
}

func (s *StageSwap) Attach(sc SessionController, index int) error {
	s.mu.Lock()
	s.session, s.index = sc, index
	s.mu.Unlock()
	return s.prepare(s.cur.Load().p, sc, index)
}

// prepare attaches an inner stage and hands it the context provider.
func (s *StageSwap) prepare(p Processor, sc SessionController, index int) error {
	if a, ok := p.(Attacher); ok {
		if err := a.Attach(sc, index); err != nil {
			return err
		}
	}
	s.mu.Lock()
	fn := s.ctxProv
	s.mu.Unlock()
	if fn != nil {
		if c, ok := p.(interface{ SetContextProvider(func() []Message) }); ok {
			c.SetContextProvider(fn)
		}
	}
	return nil
}

// Session is the controller given to Attach (nil before it).
func (s *StageSwap) Session() SessionController {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// SetBefore installs a hook that runs ahead of every frame; it may call Replace.
func (s *StageSwap) SetBefore(fn func(Frame, Direction)) {
	s.mu.Lock()
	s.before = fn
	s.mu.Unlock()
}

func (s *StageSwap) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	s.mu.Lock()
	before := s.before
	s.mu.Unlock()
	if before != nil {
		before(f, dir) // may Replace: load the slot afterwards
	}
	slot := s.cur.Load()
	slot.inflight.Add(1)
	defer slot.inflight.Add(-1)
	return slot.p.ProcessFrame(ctx, f, dir, e)
}

// Replace attaches next at this stage's position and makes it current. The
// returned retire func closes the replaced stage once its in-flight frames
// finish (or after timeout, returning ErrRetireTimeout); run it off the audio path.
func (s *StageSwap) Replace(next Processor) (func(time.Duration) error, error) {
	s.mu.Lock()
	sc, index := s.session, s.index
	s.mu.Unlock()
	if sc == nil {
		return nil, ErrSwapNotAttached
	}
	if err := s.prepare(next, sc, index); err != nil {
		return nil, err
	}
	old := s.cur.Swap(&swapSlot{p: next})
	return func(timeout time.Duration) error {
		deadline := time.Now().Add(timeout)
		var werr error
		for old.inflight.Load() > 0 {
			if time.Until(deadline) <= 0 {
				werr = ErrRetireTimeout
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if c, ok := old.p.(Closer); ok {
			if err := c.Close(); err != nil && werr == nil {
				werr = err
			}
		}
		return werr
	}, nil
}

func (s *StageSwap) Close() error {
	if c, ok := s.cur.Load().p.(Closer); ok {
		return c.Close()
	}
	return nil
}

func (s *StageSwap) SetContextProvider(fn func() []Message) {
	s.mu.Lock()
	s.ctxProv = fn
	s.mu.Unlock()
	if c, ok := s.cur.Load().p.(interface{ SetContextProvider(func() []Message) }); ok {
		c.SetContextProvider(fn)
	}
}

func (s *StageSwap) SummarizeText(ctx context.Context, system, user string) (string, error) {
	if c, ok := s.cur.Load().p.(interface {
		SummarizeText(context.Context, string, string) (string, error)
	}); ok {
		return c.SummarizeText(ctx, system, user)
	}
	return "", ErrNoSummarizer
}
