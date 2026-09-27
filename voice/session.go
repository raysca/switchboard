package voice

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrQueueFull     = errors.New("session queue full")
	ErrRunaway       = errors.New("runaway frame generation")
	ErrTooManyTasks  = errors.New("async task budget exceeded")
	ErrWatchdog      = errors.New("processor exceeded MaxFrameProcessing")
	ErrAudioShedding = errors.New("sustained audio shedding (session not consuming)")
	ErrClosed        = errors.New("session closed")
)

type Budget struct {
	MaxQueuedFrames    int           // data inbox capacity
	MaxControlFrames   int           // control inbox capacity (overflow fails session)
	MaxAsyncTasks      int           // concurrent managed goroutines
	MaxPendingVAD      int           // outstanding VAD inferences per session
	MaxBufferedAudio   time.Duration // audio buffered in VAD processor while inference outstanding
	MaxEmitsPerFrame   int           // runaway emit guard, per inbox envelope
	MaxFrameProcessing time.Duration // watchdog on a single inbox envelope
	MaxDroppedAudio    int           // inbound audio drops tolerated per watchdog window
	TeardownTimeout    time.Duration
}

func DefaultBudget() Budget {
	return Budget{
		MaxQueuedFrames:    64,
		MaxControlFrames:   32,
		MaxAsyncTasks:      16,
		MaxPendingVAD:      1,
		MaxBufferedAudio:   256 * time.Millisecond,
		MaxEmitsPerFrame:   64,
		MaxFrameProcessing: 2 * time.Second,
		MaxDroppedAudio:    10, // per second; inbound audio arrives at ~31 frames/s, so this must stay well below it
		TeardownTimeout:    2 * time.Second,
	}
}

type State int32

const (
	StateStarting State = iota
	StateRunning
	StateStopping
	StateStopped // clean teardown, everything reclaimed
	StateFailed  // failed, everything reclaimed
	StateLeaked  // teardown timed out: some managed goroutine never returned
)

func (s State) String() string {
	return [...]string{"starting", "running", "stopping", "stopped", "failed", "leaked"}[s]
}

type envelope struct {
	f   Frame
	dir Direction
	to  int // processor index to deliver to; len(procs) = output sink, -1 = upstream sink
}

// Session is one call: its own pipeline, cancellation domain, supervisor and budget.
type Session struct {
	ID     uint64
	Budget Budget
	m      *Manager

	ctx    context.Context
	cancel context.CancelFunc

	procs    []Processor
	emitters []emitter
	control  chan envelope
	data     chan envelope

	state   atomic.Int32
	errOnce sync.Once
	err     atomic.Pointer[error]

	tasks     sync.WaitGroup
	taskCount atomic.Int32
	loopDone  chan struct{}
	ready     chan struct{}
	stopOnce  sync.Once
	stopped   chan struct{} // closed when teardown finished

	emits     int          // run-loop only
	procStart atomic.Int64 // unix nanos when current envelope began, 0 = idle

	// OnOutput receives frames leaving the end of the pipeline (run-loop goroutine).
	OnOutput func(Frame)
	// OnUpstream receives upstream frames leaving the head of the pipeline.
	OnUpstream func(Frame)

	// Metrics (per-session).
	VADLatency   Hist // chunk due -> VAD result applied
	FramesIn     atomic.Uint64
	AudioDropped atomic.Uint64 // inbound audio shed at inbox
	VADDropped   atomic.Uint64 // stale/rejected VAD chunks
	dropWindow   atomic.Int64
	Created      time.Time
	Labels       map[string]string
}

type emitter struct {
	s *Session
	i int
}

func (e *emitter) Push(f Frame) error { return e.PushDirection(f, Downstream) }

func (e *emitter) PushDirection(f Frame, dir Direction) error {
	s := e.s
	s.emits++
	if s.emits > s.Budget.MaxEmitsPerFrame {
		return ErrRunaway
	}
	if dir == Downstream {
		return s.deliver(f, dir, e.i+1)
	}
	return s.deliver(f, dir, e.i-1)
}

func (s *Session) deliver(f Frame, dir Direction, to int) error {
	switch {
	case to >= len(s.procs):
		if s.OnOutput != nil {
			s.OnOutput(f)
		}
		return nil
	case to < 0:
		if s.OnUpstream != nil {
			s.OnUpstream(f)
		}
		return nil
	}
	return s.procs[to].ProcessFrame(s.ctx, f, dir, &s.emitters[to])
}

func (s *Session) Context() context.Context { return s.ctx }
func (s *Session) Manager() *Manager        { return s.m }
func (s *Session) State() State             { return State(s.state.Load()) }
func (s *Session) Err() error {
	if p := s.err.Load(); p != nil {
		return *p
	}
	return nil
}
func (s *Session) Done() <-chan struct{}   { return s.stopped }
func (s *Session) Tasks() int              { return int(s.taskCount.Load()) }
func (s *Session) Processors() []Processor { return s.procs }

// Go runs session-owned async work under supervision: panics and errors fail
// the session, the task counts against MaxAsyncTasks, and teardown waits for it.
func (s *Session) Go(fn func(ctx context.Context) error) error {
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	if int(s.taskCount.Add(1)) > s.Budget.MaxAsyncTasks {
		s.taskCount.Add(-1)
		return ErrTooManyTasks
	}
	s.tasks.Add(1)
	s.m.managed.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Fail(fmt.Errorf("panic in managed task: %v\n%s", r, debug.Stack()))
			}
			s.taskCount.Add(-1)
			s.m.managed.Add(-1)
			s.tasks.Done()
		}()
		if err := fn(s.ctx); err != nil && s.ctx.Err() == nil {
			s.Fail(err)
		}
	}()
	return nil
}

// Post enqueues a frame for delivery to processor index `to` on the run loop.
// Non-blocking. Policy: inbound audio is shed (counted) when full; control/internal
// frames and any other data frame overflowing fail the session.
func (s *Session) Post(f Frame, dir Direction, to int) error {
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	env := envelope{f, dir, to}
	t := f.Type()
	ch := s.data
	if t == FrameControl || t == FrameInternal || t == FrameError {
		ch = s.control
	}
	select {
	case ch <- env:
		return nil
	default:
	}
	if a, ok := f.(*AudioFrame); ok && !a.Output && dir == Downstream {
		s.AudioDropped.Add(1)
		s.m.audioDropped.Add(1)
		s.dropWindow.Add(1)
		return ErrQueueFull
	}
	s.Fail(fmt.Errorf("%w: %T", ErrQueueFull, f))
	return ErrQueueFull
}

// PushAudio is the transport entry point (non-blocking).
func (s *Session) PushAudio(a *AudioFrame) error { return s.Post(a, Downstream, 0) }

// Fail marks the session failed and starts asynchronous teardown.
func (s *Session) Fail(err error) {
	s.errOnce.Do(func() {
		s.err.Store(&err)
		s.m.failures.Add(1)
	})
	go s.Stop()
}

func (s *Session) run() {
	s.m.loops.Add(1)
	defer s.m.loops.Add(-1)
	defer close(s.loopDone)
	close(s.ready)
	for s.ctx.Err() == nil {
		var env envelope
		select {
		case env = <-s.control:
		default:
			select {
			case <-s.ctx.Done():
				return
			case env = <-s.control:
			case env = <-s.data:
			}
		}
		s.FramesIn.Add(1)
		if err := s.handle(env); err != nil {
			s.Fail(err)
			return
		}
	}
}

func (s *Session) handle(env envelope) (err error) {
	s.emits = 0
	s.procStart.Store(time.Now().UnixNano())
	defer func() {
		s.procStart.Store(0)
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in processor: %v\n%s", r, debug.Stack())
		}
	}()
	return s.deliver(env.f, env.dir, env.to)
}

// Stop cancels the session and tears it down, waiting at most TeardownTimeout.
// Safe to call many times; returns the final state.
func (s *Session) Stop() State {
	s.stopOnce.Do(func() {
		s.state.Store(int32(StateStopping))
		s.cancel()
		deadline := time.NewTimer(s.Budget.TeardownTimeout)
		defer deadline.Stop()
		final := StateStopped
		if s.Err() != nil {
			final = StateFailed
		}
		select {
		case <-s.loopDone:
			closed := make(chan struct{})
			go func() { // ponytail: Close may hang; bounded by the same deadline
				for i := len(s.procs) - 1; i >= 0; i-- {
					if c, ok := s.procs[i].(Closer); ok {
						c.Close()
					}
				}
				close(closed)
			}()
			select {
			case <-closed:
			case <-deadline.C:
				final = StateLeaked
			}
		case <-deadline.C:
			final = StateLeaked
		}
		if final != StateLeaked {
			waited := make(chan struct{})
			go func() { s.tasks.Wait(); close(waited) }()
			select {
			case <-waited:
			case <-deadline.C:
				final = StateLeaked
			}
		}
		if final == StateLeaked {
			s.m.leakedSessions.Add(1)
		}
		s.state.Store(int32(final))
		s.m.remove(s)
		close(s.stopped)
	})
	<-s.stopped
	return s.State()
}
