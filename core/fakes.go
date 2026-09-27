package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrFakeProcessor = errors.New("fake: configured processor error")
)

// PassthroughProcessor records frames received and forwards them.
type PassthroughProcessor struct {
	mu     sync.Mutex
	Frames []Frame
}

func (p *PassthroughProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	p.mu.Lock()
	p.Frames = append(p.Frames, f)
	p.mu.Unlock()
	return e.PushDirection(f, dir)
}

func (p *PassthroughProcessor) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Frames)
}

// LatencyProcessor sleeps for a configured duration before forwarding the frame.
type LatencyProcessor struct {
	Delay time.Duration
}

func (p *LatencyProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	select {
	case <-time.After(p.Delay):
		return e.PushDirection(f, dir)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrorProcessor returns an error when it encounters the configured frame type.
type ErrorProcessor struct {
	FailOn FrameType
}

func (p *ErrorProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	if f.Type() == p.FailOn {
		return ErrFakeProcessor
	}
	return e.PushDirection(f, dir)
}

// PanicProcessor panics when it encounters the configured frame type.
type PanicProcessor struct {
	PanicOn FrameType
}

func (p *PanicProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	if f.Type() == p.PanicOn {
		panic("deliberate inline processor panic")
	}
	return e.PushDirection(f, dir)
}

// ChildPanicProcessor initiates an async task via session.Go that panics.
type ChildPanicProcessor struct {
	session SessionController
}

func (p *ChildPanicProcessor) Attach(s SessionController, index int) error {
	p.session = s
	return nil
}

func (p *ChildPanicProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return p.session.Go(func(ctx context.Context) {
		time.Sleep(10 * time.Millisecond)
		panic("deliberate child task panic in session.Go")
	})
}

// FloodProcessor attempts to emit count frames in response to a single frame.
type FloodProcessor struct {
	EmitCount int
}

func (p *FloodProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	for i := 0; i < p.EmitCount; i++ {
		if err := e.Push(&UserStartedSpeakingFrame{Seq: uint64(i)}); err != nil {
			return err
		}
	}
	return nil
}

// StallProcessor sleeps longer than the watchdog timeout to trigger watchdog failure.
type StallProcessor struct {
	Duration time.Duration
}

func (p *StallProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	time.Sleep(p.Duration)
	return e.Push(f)
}

// LateCompletionProcessor starts an async task that finishes with an old generation.
type LateCompletionProcessor struct {
	session SessionController
	index   int
	Delay   time.Duration
}

func (p *LateCompletionProcessor) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *LateCompletionProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	gen := p.session.CurrentGeneration()
	return p.session.Go(func(ctx context.Context) {
		time.Sleep(p.Delay)
		// Reenter with the captured old generation
		_ = p.session.Reenter(&FinalTranscriptionFrame{
			Text: "late transcript",
			Gen:  gen,
		}, Downstream, p.index+1, gen)
	})
}

// IgnoredCancellationProcessor ignores context cancellation in session.Go to test leaked task detection.
type IgnoredCancellationProcessor struct {
	session SessionController
}

func (p *IgnoredCancellationProcessor) Attach(s SessionController, index int) error {
	p.session = s
	return nil
}

func (p *IgnoredCancellationProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return p.session.Go(func(ctx context.Context) {
		// Deliberately ignore ctx.Done() and sleep through teardown
		time.Sleep(10 * time.Second)
	})
}
