package adapter

import (
	"context"
	"errors"
	"time"
)

// ErrBusy means no backend slot freed up within the queue deadline.
var ErrBusy = errors.New("adapter busy: no backend slot within the queue deadline")

// Limiter bounds simultaneous backend requests. Waiting is bounded by wait, so a burst cannot
// build an unbounded queue of goroutines holding audio.
type Limiter struct {
	slots chan struct{}
	wait  time.Duration
}

func NewLimiter(n int, wait time.Duration) *Limiter {
	return &Limiter{slots: make(chan struct{}, n), wait: wait}
}

func (l *Limiter) Acquire(ctx context.Context) (release func(), err error) {
	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, nil
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
