package core

import (
	"context"
	"sync"
	"sync/atomic"
)

const defaultObserverBuffer = 64

// DebugObserver receives DebugEvents on its own session-supervised goroutine,
// so it may be slow without affecting frame processing.
type DebugObserver func(DebugEvent)

// Subscription is one observer's bounded, drop-counted event queue.
type Subscription struct {
	ch      chan DebugEvent
	done    chan struct{}
	once    sync.Once
	dropped atomic.Uint64
}

// Dropped reports events discarded because this observer's buffer was full.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// observerSet is a per-session fan-out of DebugEvents. emit never blocks: a full
// buffer drops the event for that observer only and counts it.
type observerSet struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
	n    atomic.Int32 // len(subs); lock-free "anyone listening?" for hot paths
}

// emit offers e to every subscriber without blocking.
func (o *observerSet) emit(e DebugEvent) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for s := range o.subs {
		select {
		case s.ch <- e:
		default:
			s.dropped.Add(1)
		}
	}
}

// Subscribe registers fn on a supervised session task with a buffer of the
// given size (<=0 selects 64). The returned cancel is idempotent. The task
// exits on cancel or session teardown. A panic in fn is recovered and that event
// skipped; it never fails the session. fn that blocks forever cannot be killed,
// so Close honestly reports it as a leaked task; it never stalls the pipeline.
func (s *Session) Subscribe(buffer int, fn DebugObserver) (*Subscription, func(), error) {
	if buffer <= 0 {
		buffer = defaultObserverBuffer
	}
	sub := &Subscription{ch: make(chan DebugEvent, buffer), done: make(chan struct{})}
	err := s.Go(func(ctx context.Context) {
		for {
			select {
			case e := <-sub.ch:
				func() {
					defer func() { _ = recover() }() // ponytail: swallowed; add a counter if anyone needs it
					fn(e)
				}()
			case <-sub.done:
				return
			case <-ctx.Done():
				return
			}
		}
	})
	if err != nil {
		return nil, nil, err
	}
	o := &s.observers
	o.mu.Lock()
	if o.subs == nil {
		o.subs = make(map[*Subscription]struct{})
	}
	o.subs[sub] = struct{}{}
	o.n.Store(int32(len(o.subs)))
	o.mu.Unlock()
	cancel := func() {
		sub.once.Do(func() {
			o.mu.Lock()
			delete(o.subs, sub)
			o.n.Store(int32(len(o.subs)))
			o.mu.Unlock()
			close(sub.done)
		})
	}
	return sub, cancel, nil
}

// EmitDebug delivers e to this session's observers without blocking.
func (s *Session) EmitDebug(e DebugEvent) { s.observers.emit(e) }
