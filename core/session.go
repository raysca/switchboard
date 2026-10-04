package core

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
	ErrQueueFull         = errors.New("core: data queue full")
	ErrControlQueueFull  = errors.New("core: control queue full")
	ErrRunawayEmits      = errors.New("core: runaway frame emits exceeded budget")
	ErrTooManyTasks      = errors.New("core: async task budget exceeded")
	ErrProcessorWatchdog = errors.New("core: processor exceeded MaxFrameProcessing watchdog")
	ErrSessionClosed     = errors.New("core: session closed")
	ErrStaleGeneration   = errors.New("core: frame discarded due to stale generation")
	ErrTeardownTimeout   = errors.New("core: teardown timeout exceeded, leaked tasks present")
	ErrSlowOutput        = errors.New("core: current-generation output audio queue is full; session terminated")
)

// SessionState tracks the lifecycle phase of a voice session.
type SessionState int32

const (
	StateStarting SessionState = iota
	StateRunning
	StateStopping
	StateStopped // Orderly teardown complete, all tasks and resources reclaimed
	StateFailed  // Teardown due to error or panic, all tasks reclaimed
	StateLeaked  // Teardown timed out: one or more managed goroutines did not exit
)

func (s SessionState) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	case StateLeaked:
		return "leaked"
	default:
		return fmt.Sprintf("state(%d)", s)
	}
}

// Envelope wraps a frame travelling through the session inbox or async re-entry.
type Envelope struct {
	Frame       Frame
	Dir         Direction
	ResumeIndex int    // Processor index to resume delivery at
	Gen         uint64 // Generation when envelope was generated
}

// Session represents a single active voice session with its own pipeline,
// run loop, cancellation domain, supervisor, and resource budget.
type Session struct {
	id     uint64
	budget ResourceBudget
	pipe   *Pipeline

	ctx    context.Context
	cancel context.CancelFunc

	control chan Envelope
	data    chan Envelope

	state atomic.Int32
	gen   atomic.Uint64

	// Concurrency & Supervisor
	taskWg      sync.WaitGroup
	activeTasks atomic.Int32
	leakedTasks atomic.Int32

	errOnce sync.Once
	err     atomic.Pointer[error]

	runLoopDone chan struct{}
	ready       chan struct{}
	stopped     chan struct{}
	stopOnce    sync.Once

	// Run-loop private execution state
	emitsCount int
	procStart  atomic.Int64 // Unix nanos when current envelope began processing

	// Edge callbacks
	OnOutput   func(Frame)
	OnUpstream func(Frame)
	OnFrame    func(Frame, Direction)

	// Metrics
	FramesIn       atomic.Uint64
	FramesOut      atomic.Uint64
	StaleDiscarded atomic.Uint64
	AudioDropped   atomic.Uint64
	Created        time.Time
}

// NewSession creates and initializes a new Session with the specified pipeline and budget.
func NewSession(id uint64, pipeline *Pipeline, budget ResourceBudget) (*Session, error) {
	if pipeline == nil || pipeline.Len() == 0 {
		return nil, ErrPipelineEmpty
	}
	if err := budget.Validate(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Session{
		id:          id,
		budget:      budget,
		pipe:        pipeline,
		ctx:         ctx,
		cancel:      cancel,
		control:     make(chan Envelope, budget.MaxControlFrames),
		data:        make(chan Envelope, budget.MaxQueuedFrames),
		runLoopDone: make(chan struct{}),
		ready:       make(chan struct{}),
		stopped:     make(chan struct{}),
		Created:     time.Now(),
	}

	s.state.Store(int32(StateStarting))
	s.gen.Store(1) // Generation begins at 1

	return s, nil
}

// ID returns the unique session identifier.
func (s *Session) ID() uint64 {
	return s.id
}

// Context is the session lifetime context. Provider I/O must derive from it so
// Close cancels in-flight calls before waiting for supervised tasks.
func (s *Session) Context() context.Context {
	return s.ctx
}

// State returns the current lifecycle state of the session.
func (s *Session) State() SessionState {
	return SessionState(s.state.Load())
}

// CurrentGeneration returns the active turn generation of the session.
func (s *Session) CurrentGeneration() uint64 {
	return s.gen.Load()
}

// IncrementGeneration atomically advances the turn generation.
func (s *Session) IncrementGeneration() uint64 {
	return s.gen.Add(1)
}

// Err returns the terminal error of the session, if any.
func (s *Session) Err() error {
	p := s.err.Load()
	if p == nil {
		return nil
	}
	return *p
}

// Ready returns a channel that is closed when the session has initialized and started running.
func (s *Session) Ready() <-chan struct{} {
	return s.ready
}

// Done returns a channel that is closed when the session teardown has concluded.
func (s *Session) Done() <-chan struct{} {
	return s.stopped
}

// LeakedTasks returns the count of background tasks that failed to exit within teardown timeout.
func (s *Session) LeakedTasks() int {
	return int(s.leakedTasks.Load())
}

// Processors returns the session pipeline processors.
func (s *Session) Processors() []Processor {
	if s.pipe == nil {
		return nil
	}
	return s.pipe.Processors()
}

// Start attaches all pipeline processors and begins the session run loop.
func (s *Session) Start() error {
	// Attach pipeline processors
	for i, proc := range s.pipe.Processors() {
		if attacher, ok := proc.(Attacher); ok {
			if err := attacher.Attach(s, i); err != nil {
				s.cancel()
				return fmt.Errorf("attaching processor at index %d: %w", i, err)
			}
		}
	}

	s.state.Store(int32(StateRunning))
	close(s.ready)

	go s.runLoop()
	return nil
}

// Post queues an incoming frame to the session run loop from outside the pipeline.
func (s *Session) Post(f Frame) error {
	if s.State() > StateRunning {
		return ErrSessionClosed
	}

	s.FramesIn.Add(1)
	gen := s.CurrentGeneration()
	if gf, ok := f.(GenerationalFrame); ok && gf.Generation() != 0 {
		gen = gf.Generation()
	}

	env := Envelope{
		Frame:       f,
		Dir:         Downstream,
		ResumeIndex: 0,
		Gen:         gen,
	}

	// Classify queue policy
	if isControlFrame(f) {
		select {
		case s.control <- env:
			return nil
		default:
			s.fail(ErrControlQueueFull)
			return ErrControlQueueFull
		}
	}

	select {
	case s.data <- env:
		return nil
	default:
		return s.enqueueData(env)
	}
}

// TailIndex is the last pipeline processor. Upstream re-entry starts there
// so a transport acknowledgement walks back to the assistant.
func (s *Session) TailIndex() int {
	n := s.pipe.Len()
	if n == 0 {
		return 0
	}
	return n - 1
}

// Reenter queues an asynchronous operation result back into the pipeline at resumeIndex.
func (s *Session) Reenter(f Frame, dir Direction, resumeIndex int, gen uint64) error {
	if s.State() > StateRunning {
		return ErrSessionClosed
	}

	env := Envelope{
		Frame:       f,
		Dir:         dir,
		ResumeIndex: resumeIndex,
		Gen:         gen,
	}

	if isControlFrame(f) {
		select {
		case s.control <- env:
			return nil
		default:
			s.fail(ErrControlQueueFull)
			return ErrControlQueueFull
		}
	}

	select {
	case s.data <- env:
		return nil
	default:
		return s.enqueueData(env)
	}
}

// enqueueData sheds input audio when the inbox is full. Current-generation output
// waits for the run loop to drain, then terminates the session if it does not.
func (s *Session) enqueueData(env Envelope) error {
	select {
	case s.data <- env:
		return nil
	default:
	}
	if env.Frame.Type() == FrameInputAudio {
		s.AudioDropped.Add(1)
		return nil
	}
	if env.Frame.Type() != FrameOutputAudio {
		return ErrQueueFull
	}
	timer := time.NewTimer(s.budget.MaxFrameProcessing)
	defer timer.Stop()
	select {
	case s.data <- env:
		return nil
	case <-timer.C:
		s.fail(ErrSlowOutput)
		return ErrSlowOutput
	case <-s.ctx.Done():
		return ErrSessionClosed
	}
}

// Go executes fn as a supervised, session-owned asynchronous task.
// Recovers panics, enforces task budget, and tracks lifecycle during teardown.
func (s *Session) Go(fn func(ctx context.Context)) error {
	if s.State() > StateRunning {
		return ErrSessionClosed
	}

	current := s.activeTasks.Add(1)
	if int(current) > s.budget.MaxAsyncTasks {
		s.activeTasks.Add(-1)
		return ErrTooManyTasks
	}

	s.taskWg.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				err := fmt.Errorf("session.Go panic: %v\nstack:\n%s", r, stack)
				s.fail(err)
			}
			s.activeTasks.Add(-1)
			s.taskWg.Done()
		}()

		fn(s.ctx)
	}()

	return nil
}

// Close gracefully terminates the session, draining active envelopes and running teardown.
func (s *Session) Close() error {
	s.stopOnce.Do(func() {
		s.state.Store(int32(StateStopping))
		s.cancel()

		// Cancel provider I/O before waiting for supervised tasks. Processors
		// parent their calls on s.ctx; Close also drops any extra handles.
		for _, proc := range s.pipe.Processors() {
			if closer, ok := proc.(Closer); ok {
				_ = closer.Close()
			}
		}

		// Wait for run loop exit
		<-s.runLoopDone

		// Teardown wait with deadline
		teardownTimer := time.NewTimer(s.budget.TeardownTimeout)
		defer teardownTimer.Stop()

		tasksDone := make(chan struct{})
		go func() {
			s.taskWg.Wait()
			close(tasksDone)
		}()

		select {
		case <-tasksDone:
			// All tasks cleanly exited
			if s.Err() != nil {
				s.state.Store(int32(StateFailed))
			} else {
				s.state.Store(int32(StateStopped))
			}
		case <-teardownTimer.C:
			// Leaked task detection
			rem := s.activeTasks.Load()
			s.leakedTasks.Store(rem)
			s.state.Store(int32(StateLeaked))
			s.recordErr(fmt.Errorf("%w: %d background tasks failed to exit", ErrTeardownTimeout, rem))
		}

		close(s.stopped)
	})
	return s.Err()
}

// runLoop executes the sequential processing of envelopes on a single goroutine.
func (s *Session) runLoop() {
	defer close(s.runLoopDone)

	// Watchdog ticker
	watchdogTicker := time.NewTicker(200 * time.Millisecond)
	defer watchdogTicker.Stop()

	// Maximum session duration timer
	var maxDurationCh <-chan time.Time
	if s.budget.MaxSessionDuration > 0 {
		maxTimer := time.NewTimer(s.budget.MaxSessionDuration)
		defer maxTimer.Stop()
		maxDurationCh = maxTimer.C
	}

	for {
		select {
		case <-s.ctx.Done():
			return

		case <-maxDurationCh:
			s.fail(fmt.Errorf("session reached maximum duration (%v)", s.budget.MaxSessionDuration))
			return

		case <-watchdogTicker.C:
			start := s.procStart.Load()
			if start > 0 && time.Since(time.Unix(0, start)) > s.budget.MaxFrameProcessing {
				s.fail(ErrProcessorWatchdog)
				return
			}

		case env := <-s.control:
			if !s.processEnvelope(env) {
				return
			}

		default:
			// Process data if control queue has no pending items
			select {
			case env := <-s.control:
				if !s.processEnvelope(env) {
					return
				}
			case env := <-s.data:
				if !s.processEnvelope(env) {
					return
				}
			case <-s.ctx.Done():
				return
			case <-maxDurationCh:
				s.fail(fmt.Errorf("session reached maximum duration (%v)", s.budget.MaxSessionDuration))
				return
			case <-watchdogTicker.C:
				start := s.procStart.Load()
				if start > 0 && time.Since(time.Unix(0, start)) > s.budget.MaxFrameProcessing {
					s.fail(ErrProcessorWatchdog)
					return
				}
			}
		}
	}
}

// processEnvelope executes the inline delivery of a single envelope.
func (s *Session) processEnvelope(env Envelope) bool {
	// Generation fencing: discard old-generation async re-entry before mutating processor or context
	if env.Gen != 0 && env.Gen < s.CurrentGeneration() {
		s.StaleDiscarded.Add(1)
		return true // Discard and proceed
	}

	// Reset per-envelope emit counter and set watchdog start time
	s.emitsCount = 0
	s.procStart.Store(time.Now().UnixNano())
	defer s.procStart.Store(0)

	// Panic containment for inline processor execution
	var deliveryErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				deliveryErr = fmt.Errorf("processor panic on frame %v: %v\nstack:\n%s", env.Frame.Type(), r, stack)
			}
		}()
		deliveryErr = s.deliver(env.Frame, env.Dir, env.ResumeIndex)
	}()

	if deliveryErr != nil {
		s.fail(deliveryErr)
		return false
	}

	// If StartFrame or EndFrame, handle lifecycle
	if env.Frame.Type() == FrameEnd {
		go s.Close()
		return false
	}

	return true
}

// deliver dispatches a frame inline to the processor at index 'to'.
func (s *Session) deliver(f Frame, dir Direction, to int) error {
	if s.OnFrame != nil {
		s.OnFrame(f, dir)
	}

	switch {
	case to >= s.pipe.Len():
		// Reached downstream edge
		s.FramesOut.Add(1)
		if s.OnOutput != nil {
			s.OnOutput(f)
		}
		return nil

	case to < 0:
		// Reached upstream edge
		if s.OnUpstream != nil {
			s.OnUpstream(f)
		}
		return nil

	default:
		proc := s.pipe.At(to)
		e := &sessionEmitter{
			s:     s,
			index: to,
		}
		return proc.ProcessFrame(s.ctx, f, dir, e)
	}
}

type sessionEmitter struct {
	s     *Session
	index int
}

func (e *sessionEmitter) Push(f Frame) error {
	return e.PushDirection(f, Downstream)
}

func (e *sessionEmitter) PushDirection(f Frame, dir Direction) error {
	e.s.emitsCount++
	if e.s.emitsCount > e.s.budget.MaxEmitsPerFrame {
		return ErrRunawayEmits
	}

	if dir == Downstream {
		return e.s.deliver(f, dir, e.index+1)
	}
	return e.s.deliver(f, dir, e.index-1)
}

func (s *Session) fail(err error) {
	s.recordErr(err)
	s.cancel()
	// Release admission and provider work without waiting for the socket timeout.
	go s.Close()
}

func (s *Session) recordErr(err error) {
	s.errOnce.Do(func() {
		s.err.Store(&err)
	})
}

func isControlFrame(f Frame) bool {
	switch f.Type() {
	case FrameStart, FrameEnd, FrameCancel, FrameError, FrameInterruption, FrameFlush, FrameFlushProcessed, FrameWriteCompletion:
		return true
	default:
		return false
	}
}
