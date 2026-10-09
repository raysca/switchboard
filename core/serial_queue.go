package core

import (
	"context"
	"sync"
)

// SerialQueue runs jobs one at a time, in order, on a single supervised task.
// Queued jobs hold no task slot, so a long backlog (one TTS unit per sentence)
// cannot exhaust the session's MaxAsyncTasks. The zero value is ready to use.
type SerialQueue struct {
	mu      sync.Mutex
	jobs    []func(ctx context.Context)
	running bool
}

// Go queues fn behind earlier jobs. It starts the worker task when idle, so it
// fails like s.Go (ErrTooManyTasks, ErrSessionClosed) only then; on failure fn
// is not queued. Callers must serialize Go calls (processors do: one run loop).
// Jobs still run after the session ctx ends (they must check
// it and return), so their cleanup always happens.
func (q *SerialQueue) Go(s interface{ Go(func(context.Context)) error }, fn func(ctx context.Context)) error {
	q.mu.Lock()
	if q.running {
		q.jobs = append(q.jobs, fn)
		q.mu.Unlock()
		return nil
	}
	q.running = true
	q.jobs = append(q.jobs, fn)
	q.mu.Unlock()
	if err := s.Go(q.drain); err != nil {
		q.mu.Lock()
		q.running, q.jobs = false, nil
		q.mu.Unlock()
		return err
	}
	return nil
}

func (q *SerialQueue) drain(ctx context.Context) {
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		fn := q.jobs[0]
		q.jobs = q.jobs[1:]
		q.mu.Unlock()
		fn(ctx)
	}
}
