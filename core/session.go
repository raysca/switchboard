package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	ErrQueueFull                = errors.New("core: data queue full")
	ErrControlQueueFull         = errors.New("core: control queue full")
	ErrRunawayEmits             = errors.New("core: runaway frame emits exceeded budget")
	ErrTooManyTasks             = errors.New("core: async task budget exceeded")
	ErrProcessorWatchdog        = errors.New("core: processor exceeded MaxFrameProcessing watchdog")
	ErrSessionClosed            = errors.New("core: session closed")
	ErrStaleGeneration          = errors.New("core: frame discarded due to stale generation")
	ErrInvalidControlGeneration = errors.New("core: control frame generation conflicts with envelope")
	ErrTeardownTimeout          = errors.New("core: teardown timeout exceeded, leaked tasks present")
	ErrSlowOutput               = errors.New("core: current-generation output audio queue is full; session terminated")
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
	Frame          Frame
	Dir            Direction
	ResumeIndex    int    // Processor index to resume delivery at
	Gen            uint64 // Generation when envelope was generated
	DropInput      bool   // Input was admitted while muted
	CommittedFlush bool   // This admission advanced the generation fence
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
	// outSlots caps bot audio queued in data so caller audio never waits behind
	// more than maxQueuedOutputAudio paced frames (the transport blocks the run
	// loop per frame to pace output).
	outSlots chan struct{}
	// outputGate, if set, holds a bot-audio producer until playback has room,
	// so the transport's pacing backpressure stops landing on the run loop.
	outputGate atomic.Pointer[func(ctx context.Context, d time.Duration)]

	state          atomic.Int32
	gen            atomic.Uint64
	unitSeq        atomic.Uint64 // session-wide synthesis unit ids (assistant and idle speech)
	inputMuted     atomic.Bool
	admissionMu    sync.Mutex
	admissionMuted bool

	// Concurrency & Supervisor
	taskWg      sync.WaitGroup
	activeTasks atomic.Int32
	leakedTasks atomic.Int32

	errOnce sync.Once
	err     atomic.Pointer[error]

	runLoopDone chan struct{}
	loopStarted atomic.Bool
	ready       chan struct{}
	stopped     chan struct{}
	stopOnce    sync.Once

	// Run-loop private execution state
	emitsCount int
	procStart  atomic.Int64 // Monotonic nanos since Created when current envelope began processing (+1 offset so >0 indicates active processing)

	// Edge callbacks
	OnOutput   func(Frame)
	OnUpstream func(Frame)
	OnFrame    func(Frame, Direction)

	// Metrics
	StaleDiscarded atomic.Uint64
	AudioDropped   atomic.Uint64
	Created        time.Time

	observers      observerSet
	debugMu        sync.Mutex
	debugSeen      map[string]string // last turn emitted per latency DebugEvent name
	debugLastStop  *UserStoppedSpeakingFrame
	tracer         Tracer
	tracePolicy    TracePolicy
	traceCtx       context.Context // run-loop owned after Start
	traceRootCtx   context.Context // immutable session parent for external async boundaries
	sessionSpan    Span
	traceQueue     *AsyncTracer
	ownsTraceQueue bool
	tracing        bool
	turnSpans      map[TurnID]Span                 // run-loop owned; finalized after runLoopDone
	traceOrigins   map[traceFrameIdentity]struct{} // run-loop owned, reset for each envelope
}

// A Frame interface can contain a non-comparable concrete value. The two
// interface words give this envelope's boxed frame a comparable identity,
// without hashing or inspecting user-defined fields. The map lives only for
// one run-loop envelope and its nested inline deliveries.
type traceFrameIdentity struct{ typ, data unsafe.Pointer }

func traceIdentity(f Frame) traceFrameIdentity { return *(*traceFrameIdentity)(unsafe.Pointer(&f)) }

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
		outSlots:    make(chan struct{}, maxQueuedOutputAudio),
		runLoopDone: make(chan struct{}),
		ready:       make(chan struct{}),
		stopped:     make(chan struct{}),
		Created:     time.Now(),
	}

	s.state.Store(int32(StateStarting))
	s.gen.Store(1) // Generation begins at 1
	s.tracer = NoopTracer{}

	return s, nil
}

// SetTracer configures provider-neutral runtime tracing before the session is
// started. A nil tracer selects the no-op implementation.
func (s *Session) SetTracer(tracer Tracer, policy TracePolicy) error {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.State() != StateStarting {
		return errors.New("core: tracer must be configured before session start")
	}
	if tracer == nil {
		tracer = NoopTracer{}
	}
	s.tracer = tracer
	s.tracePolicy = policy
	return nil
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

// NextUnitID returns the next synthesis unit id. Ids are unique and increasing
// across everything that speaks, which the transport pacer requires.
func (s *Session) NextUnitID() uint64 { return s.unitSeq.Add(1) }

// CurrentGeneration returns the active turn generation of the session.
func (s *Session) CurrentGeneration() uint64 {
	return s.gen.Load()
}

// IncrementGeneration atomically advances the turn generation.
func (s *Session) IncrementGeneration() uint64 {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	return s.gen.Add(1)
}

func (s *Session) InputMuted() bool { return s.inputMuted.Load() }

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

	s.admissionMu.Lock()
	s.state.Store(int32(StateRunning))
	tracer, policy := s.tracer, s.tracePolicy
	if _, noop := tracer.(NoopTracer); !noop {
		if async, alreadyAsync := tracer.(*AsyncTracer); alreadyAsync {
			s.traceQueue = async
		} else {
			s.traceQueue = NewAsyncTracer(tracer, defaultTraceQueueSize)
			s.tracer = s.traceQueue
			s.traceQueue = s.tracer.(*AsyncTracer)
			s.ownsTraceQueue = true
		}
		tracer = s.tracer
		s.tracing = true
	}
	s.admissionMu.Unlock()
	s.traceCtx = s.ctx
	s.sessionSpan = StartSpanSafe(s.ctx, tracer, NewTraceEvent("session.start", time.Now(), SessionID(strconv.FormatUint(s.id, 10)), "", Generation(s.CurrentGeneration()), policy, nil))
	s.traceCtx = ContextWithTraceContext(ContextWithSpan(s.ctx, s.sessionSpan), s.tracer, policy)
	s.traceRootCtx = s.traceCtx
	s.turnSpans = make(map[TurnID]Span)
	close(s.ready)
	s.EmitDebug(NewDebugEvent(DebugSessionStarted, time.Now(), SessionID(strconv.FormatUint(s.id, 10)), "", 0, map[string]any{"latency_ms": time.Since(s.Created).Milliseconds()}))

	s.loopStarted.Store(true)
	go s.runLoop()
	go s.watchdog()
	return nil
}

// watchdog fails the session when one envelope runs longer than
// MaxFrameProcessing. It must live outside the run loop: a stuck processor
// blocks the loop, so a check inside it could never fire.
func (s *Session) watchdog() {
	interval := s.budget.MaxFrameProcessing / 4
	if interval <= 0 || interval > 200*time.Millisecond {
		interval = 200 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.runLoopDone:
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			start := s.procStart.Load()
			if start > 0 {
				elapsed := time.Since(s.Created) - time.Duration(start-1)
				if elapsed > s.budget.MaxFrameProcessing {
					s.fail(ErrProcessorWatchdog)
					return
				}
			}
		}
	}
}

// RootTraceContext returns the immutable session parent context after Start.
// External async components can create session-correlated child spans without
// reading the run loop's transient processor context.
func (s *Session) RootTraceContext() context.Context {
	if s.traceRootCtx != nil {
		return s.traceRootCtx
	}
	return s.ctx
}

// Post queues an incoming frame to the session run loop from outside the pipeline.
func (s *Session) Post(f Frame) error {
	return s.admit(f, Downstream, 0, 0)
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
	return s.admit(f, dir, resumeIndex, gen)
}

// admit orders control commitment, input tagging, and inbox insertion. A
// blocking output enqueue runs after unlocking, carrying its admission tag.
func (s *Session) admit(f Frame, dir Direction, resumeIndex int, suppliedGen uint64) error {
	s.admissionMu.Lock()
	if s.State() > StateRunning {
		s.admissionMu.Unlock()
		return ErrSessionClosed
	}
	gen := s.CurrentGeneration()
	if suppliedGen != 0 {
		gen = suppliedGen
	} else if gf, ok := f.(GenerationalFrame); ok && gf.Generation() != 0 {
		gen = gf.Generation()
	}
	env := Envelope{Frame: f, Dir: dir, ResumeIndex: resumeIndex, Gen: gen, DropInput: f.Type() == FrameInputAudio && s.admissionMuted}
	switch f.Type() {
	case FrameInputMute:
		s.admissionMuted = true
	case FrameInputUnmute:
		s.admissionMuted = false
	case FrameFlush:
		ff := f.(*FlushFrame)
		if ff.Gen == 0 || (suppliedGen != 0 && suppliedGen != ff.Gen) {
			s.admissionMu.Unlock()
			return ErrInvalidControlGeneration
		}
		gen = ff.Gen
		if gen == 0 || gen != s.CurrentGeneration() {
			s.admissionMu.Unlock()
			return ErrStaleGeneration
		}
		s.gen.Add(1)
		env.Frame = &FlushFrame{Gen: gen, committed: true}
		env.Gen = gen
		env.CommittedFlush = true
	case FrameInterruption:
		if gen != 0 && gen < s.CurrentGeneration() {
			s.admissionMu.Unlock()
			return ErrStaleGeneration
		}
	}
	if isControlFrame(f) {
		select {
		case s.control <- env:
			s.admissionMu.Unlock()
			return nil
		default:
			s.admissionMu.Unlock()
			s.fail(ErrControlQueueFull)
			return ErrControlQueueFull
		}
	}
	if f.Type() == FrameOutputAudio {
		s.admissionMu.Unlock()
		return s.enqueueOutputAudio(env)
	}
	select {
	case s.data <- env:
		s.admissionMu.Unlock()
		return nil
	default:
		s.admissionMu.Unlock()
		return s.enqueueData(env)
	}
}

const maxQueuedOutputAudio = 2

// SetOutputGate installs a wait the producer of each output audio frame runs
// before queueing it (with the frame's duration). The transport uses it to
// apply playback backpressure to TTS instead of blocking the run loop, where
// it delays caller audio. The gate must return on ctx cancellation and should
// give up after a bounded time; the transport's own enqueue still enforces its
// budget. Safe to call at any time, including while audio is being posted
// (the transport installs it from its read loop); frames already past the gate
// are unaffected. A nil g removes the gate.
func (s *Session) SetOutputGate(g func(ctx context.Context, d time.Duration)) {
	if g == nil {
		s.outputGate.Store(nil)
		return
	}
	s.outputGate.Store(&g)
}

// enqueueOutputAudio takes an output slot (released when the run loop dequeues
// the frame) before queueing; a producer that cannot get one within the frame
// budget ends the session like any other output that does not fit.
func (s *Session) enqueueOutputAudio(env Envelope) error {
	if gate := s.outputGate.Load(); gate != nil {
		if f, ok := env.Frame.(*OutputAudioFrame); ok && f.Duration > 0 {
			(*gate)(s.ctx, f.Duration)
		}
	}
	select {
	case s.outSlots <- struct{}{}:
	default:
		timer := time.NewTimer(s.budget.MaxFrameProcessing)
		defer timer.Stop()
		select {
		case s.outSlots <- struct{}{}:
		case <-timer.C:
			s.fail(ErrSlowOutput)
			return ErrSlowOutput
		case <-s.ctx.Done():
			return ErrSessionClosed
		}
	}
	err := s.enqueueData(env)
	if err != nil {
		<-s.outSlots
	}
	return err
}

// enqueueData sheds input audio when the inbox is full. Other frames wait for the
// run loop to drain; output audio that does not fit terminates the session.
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
	terminalTranscript := env.Frame.Type() == FrameTranscriptInvalidation
	if errFrame, ok := env.Frame.(*ErrorFrame); ok && errors.Is(errFrame.Err, ErrTranscriptInvalidated) {
		terminalTranscript = true
	}
	if terminalTranscript {
		// Keep terminal invalidation and its observable error behind already
		// admitted candidates. If the queue cannot drain, close the session so
		// a partial candidate cannot later become a committed transcript.
		timer := time.NewTimer(s.budget.MaxFrameProcessing)
		defer timer.Stop()
		select {
		case s.data <- env:
			return nil
		case <-timer.C:
			s.fail(ErrQueueFull)
			return ErrQueueFull
		case <-s.ctx.Done():
			return ErrSessionClosed
		}
	}
	// Internal frames come from shared workers (VAD) serving every session; one
	// slow session must not stall them.
	if env.Frame.Type() == FrameInternal {
		return ErrQueueFull
	}
	// Producers (LLM streams, TTS) block here while the run loop drains at media
	// pace; only output audio that still cannot fit ends the session.
	timer := time.NewTimer(s.budget.MaxFrameProcessing)
	defer timer.Stop()
	select {
	case s.data <- env:
		return nil
	case <-timer.C:
		if env.Frame.Type() != FrameOutputAudio {
			return ErrQueueFull
		}
		s.fail(ErrSlowOutput)
		return ErrSlowOutput
	case <-s.ctx.Done():
		return ErrSessionClosed
	}
}

// Go executes fn as a supervised, session-owned asynchronous task.
// Recovers panics, enforces task budget, and tracks lifecycle during teardown.
func (s *Session) Go(fn func(ctx context.Context)) error {
	// Registration and Close's state transition share admissionMu, so a task is
	// either counted before Close waits or rejected; never added after Wait.
	s.admissionMu.Lock()
	if s.State() > StateRunning {
		s.admissionMu.Unlock()
		return ErrSessionClosed
	}
	current := s.activeTasks.Add(1)
	if int(current) > s.budget.MaxAsyncTasks {
		s.activeTasks.Add(-1)
		s.admissionMu.Unlock()
		return ErrTooManyTasks
	}
	s.taskWg.Add(1)
	s.admissionMu.Unlock()

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

// Close terminates the session. It cancels provider I/O, waits for the run
// loop, closes processors, then waits for supervised tasks, all under one
// TeardownTimeout deadline. Anything still running at the deadline marks the
// session StateLeaked; Close itself always returns and Done always closes.
func (s *Session) Close() error {
	s.stopOnce.Do(func() {
		s.admissionMu.Lock()
		s.state.Store(int32(StateStopping))
		s.admissionMu.Unlock()
		s.cancel()

		deadline := time.NewTimer(s.budget.TeardownTimeout)
		defer deadline.Stop()
		timedOut := false

		loopExited := true
		if s.loopStarted.Load() {
			select {
			case <-s.runLoopDone:
			case <-deadline.C:
				loopExited, timedOut = false, true
			}
		}

		// Processors are closed after the run loop exits so Close never races
		// ProcessFrame. A wedged loop still gets a best-effort Close to release
		// provider handles. Closers can hang, so they share the deadline.
		closersDone := make(chan struct{})
		go func() {
			defer close(closersDone)
			for _, proc := range s.pipe.Processors() {
				if closer, ok := proc.(Closer); ok {
					func() {
						defer func() { _ = recover() }()
						_ = closer.Close()
					}()
				}
			}
		}()
		closersFinished := false
		if !timedOut {
			select {
			case <-closersDone:
				closersFinished = true
			case <-deadline.C:
				timedOut = true
			}
		}

		tasksDone := make(chan struct{})
		go func() {
			s.taskWg.Wait()
			close(tasksDone)
		}()
		if !timedOut {
			select {
			case <-tasksDone:
			case <-deadline.C:
				timedOut = true
			}
		}

		if timedOut {
			rem := s.activeTasks.Load()
			if !loopExited {
				rem++ // the stuck run loop itself
			} else if !closersFinished {
				rem++ // a processor Close() that never returned
			}
			s.leakedTasks.Store(rem)
			s.state.Store(int32(StateLeaked))
			s.recordErr(fmt.Errorf("%w: %d background tasks failed to exit", ErrTeardownTimeout, rem))
		} else if s.Err() != nil {
			s.state.Store(int32(StateFailed))
		} else {
			s.state.Store(int32(StateStopped))
		}

		terminal := "session.end"
		if s.Err() != nil {
			terminal = "session.failure"
		}
		// turnSpans and traceCtx are run-loop owned; read them only after the
		// loop has exited. A wedged loop may still write them later.
		traceCtx := s.traceRootCtx
		if loopExited {
			traceCtx = s.traceCtx
			for _, span := range s.turnSpans {
				if span != nil {
					span.End(traceStatusError(s.Err(), "session_failure"))
				}
			}
		}
		if traceCtx == nil {
			traceCtx = s.ctx
		}
		terminalSpan := StartSpanSafe(traceCtx, s.tracer, NewTraceEvent(terminal, time.Now(), SessionID(strconv.FormatUint(s.id, 10)), "", Generation(s.CurrentGeneration()), s.tracePolicy, nil))
		terminalSpan.End(traceStatusError(s.Err(), "session_failure"))
		if s.sessionSpan != nil {
			s.sessionSpan.End(traceStatusError(s.Err(), "session_failure"))
		}
		if s.ownsTraceQueue && s.traceQueue != nil {
			s.traceQueue.Close()
		}

		close(s.stopped)
	})
	<-s.stopped
	return s.Err()
}

// runLoop executes the sequential processing of envelopes on a single goroutine.
func (s *Session) runLoop() {
	defer close(s.runLoopDone)

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
				if env.Frame.Type() == FrameOutputAudio {
					<-s.outSlots
				}
				if !s.processEnvelope(env) {
					return
				}
			case <-s.ctx.Done():
				return
			case <-maxDurationCh:
				s.fail(fmt.Errorf("session reached maximum duration (%v)", s.budget.MaxSessionDuration))
				return
			}
		}
	}
}

// processEnvelope executes the inline delivery of a single envelope.
func (s *Session) processEnvelope(env Envelope) bool {
	// Generation fencing: discard old-generation async re-entry before mutating processor or context
	if env.DropInput {
		s.AudioDropped.Add(1)
		return true
	}
	if env.Gen != 0 && env.Gen < s.CurrentGeneration() && !env.CommittedFlush && !isFenceSurvivingControl(env.Frame) && !isUninterruptibleOutput(env.Frame) && !isPartialAssistantCommit(env.Frame) {
		s.StaleDiscarded.Add(1)
		return true // Discard and proceed
	}
	if env.Frame.Type() == FrameInputMute {
		s.inputMuted.Store(true)
		env.Frame = &InputMutedStateFrame{Muted: true}
	}
	if env.Frame.Type() == FrameInputUnmute {
		s.inputMuted.Store(false)
		env.Frame = &InputMutedStateFrame{Muted: false}
	}
	if env.CommittedFlush {
		env.Frame = &FlushFrame{Gen: env.Gen, committed: true}
	}
	if su, ok := env.Frame.(*SynthesisUnitFrame); ok && su.UnitID == 0 {
		// Out-of-band speech (idle) takes its id here, on the run loop, so it is
		// ordered with the assistant's units as the transport pacer requires.
		su.UnitID = s.NextUnitID()
	}

	// Reset per-envelope emit counter and set watchdog start time
	s.emitsCount = 0
	if s.tracing {
		s.traceOrigins = map[traceFrameIdentity]struct{}{traceIdentity(env.Frame): {}}
	}
	s.procStart.Store(int64(time.Since(s.Created)) + 1)
	defer s.procStart.Store(0)

	// Panic containment for inline processor execution.
	var deliveryErr error
	if s.tracing {
		func() {
			parent := s.traceCtx
			if parent == nil {
				parent = s.ctx
			}
			turn := traceTurnID(env.Frame)
			if turn != "" {
				if span, ok := s.turnSpans[turn]; ok && span != nil {
					parent = ContextWithSpan(s.traceCtx, span)
				} else if !ok {
					span := StartSpanSafe(parent, s.tracer, NewTraceEvent("turn.start", time.Now(), SessionID(strconv.FormatUint(s.id, 10)), turn, traceGeneration(env.Frame), s.tracePolicy, nil))
					s.turnSpans[turn] = span
					parent = ContextWithSpan(s.traceCtx, span)
				}
			}
			frameSpan := StartSpanSafe(parent, s.tracer, s.traceEvent(env.Frame))
			previous := s.traceCtx
			s.traceCtx = ContextWithSpan(parent, frameSpan)
			defer func() {
				s.traceCtx = previous
				frameSpan.End(traceStatusError(deliveryErr, "processor_error"))
				if turn != "" && traceTurnTerminal(env.Frame) {
					if span := s.turnSpans[turn]; span != nil {
						span.End(traceStatusError(deliveryErr, "processor_error"))
						s.turnSpans[turn] = nil
					}
				}
			}()
			defer func() {
				if r := recover(); r != nil {
					stack := string(debug.Stack())
					deliveryErr = fmt.Errorf("processor panic on frame %v: %v\nstack:\n%s", env.Frame.Type(), r, stack)
				}
			}()
			deliveryErr = s.deliver(env.Frame, env.Dir, env.ResumeIndex)
		}()
	} else {
		func() {
			defer func() {
				if r := recover(); r != nil {
					stack := string(debug.Stack())
					deliveryErr = fmt.Errorf("processor panic on frame %v: %v\nstack:\n%s", env.Frame.Type(), r, stack)
				}
			}()
			deliveryErr = s.deliver(env.Frame, env.Dir, env.ResumeIndex)
		}()
	}

	if deliveryErr != nil {
		// ErrSessionClosed only comes from Go/Reenter/Post once teardown has begun:
		// a processor that lost the race with Close is not a failure. A real cause,
		// if any, was already recorded by fail().
		if !errors.Is(deliveryErr, ErrSessionClosed) || s.State() <= StateRunning {
			s.fail(deliveryErr)
		}
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
func (s *Session) deliver(f Frame, dir Direction, to int) (resultErr error) {
	if intr, ok := f.(*InterruptionFrame); ok && intr.Gen != 0 && intr.Gen < s.CurrentGeneration() && (!intr.committed || intr.Gen+1 != s.CurrentGeneration()) {
		s.StaleDiscarded.Add(1)
		return nil
	}
	// Processors may emit controls inline. Commit before publishing a state or
	// flush event so their observable result cannot lie about session state.
	switch f.Type() {
	case FrameInputMute, FrameInputUnmute:
		return s.admit(f, dir, to, 0)
	case FrameFlush:
		ff := f.(*FlushFrame)
		s.admissionMu.Lock()
		if !ff.committed && ff.Gen == s.CurrentGeneration() {
			s.gen.Add(1)
			ff = &FlushFrame{Gen: ff.Gen, committed: true}
			f = ff
		} else if !ff.committed {
			s.admissionMu.Unlock()
			s.StaleDiscarded.Add(1)
			return nil
		}
		s.admissionMu.Unlock()
	}
	if gf, ok := f.(GenerationalFrame); ok && gf.Generation() != 0 && gf.Generation() < s.CurrentGeneration() && isInterruptibleOutput(f) {
		s.StaleDiscarded.Add(1)
		return nil
	}
	if s.OnFrame != nil {
		s.OnFrame(f, dir)
	}
	s.emitFrameDebug(f)

	switch {
	case to >= s.pipe.Len():
		// Reached downstream edge
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

		invokeProc := func() error {
			if !s.tracing {
				return proc.ProcessFrame(s.ctx, f, dir, &sessionEmitter{s: s, index: to})
			}
			parent := s.traceCtx
			if parent == nil {
				parent = s.ctx
			}
			component := reflect.TypeOf(proc).String()
			span := StartSpanSafe(parent, s.tracer, NewTraceEvent("processor.frame", time.Now(), SessionID(strconv.FormatUint(s.id, 10)), traceTurnID(f), traceGeneration(f), s.tracePolicy, map[string]any{"component": component, "frame": frameTraceLabel(f.Type()), "direction": dir.String()}))
			e := &sessionEmitter{
				s:     s,
				index: to,
			}
			previous := s.traceCtx
			s.traceCtx = ContextWithSpan(parent, span)
			var pErr error
			defer func() {
				s.traceCtx = previous
				if r := recover(); r != nil {
					span.End(errors.New("processor_error"))
					panic(r)
				}
				span.End(traceStatusError(pErr, "processor_error"))
			}()
			pErr = proc.ProcessFrame(s.traceCtx, f, dir, e)
			return pErr
		}

		return invokeProc()
	}
}

func (s *Session) traceEvent(f Frame) TraceEvent {
	name, attributes := frameTrace(f)
	event := NewTraceEvent(name, time.Now(), SessionID(strconv.FormatUint(s.id, 10)), traceTurnID(f), traceGeneration(f), s.tracePolicy, attributes)
	if tool, ok := f.(*ToolCallFrame); ok {
		event.ToolCallID = tool.Call.ID
	}
	if tool, ok := f.(*ToolResultFrame); ok {
		event.ToolCallID = tool.Result.CallID
	}
	return event
}

func frameTrace(f Frame) (string, map[string]any) {
	attrs := make(map[string]any)
	switch v := f.(type) {
	case *InputAudioFrame:
		attrs["count"], attrs["sample_rate"], attrs["channels"], attrs["format"], attrs["duration_ms"] = len(v.Samples), v.SampleRate, v.Channels, string(v.Encoding), float64(v.Duration.Microseconds())/1000
		return "frame.input_audio", attrs
	case *OutputAudioFrame:
		attrs["count"], attrs["sample_rate"], attrs["channels"], attrs["format"], attrs["duration_ms"] = len(v.Samples), v.SampleRate, v.Channels, string(v.Encoding), float64(v.Duration.Microseconds())/1000
		return "transport.output_audio", attrs
	case *UserStartedSpeakingFrame:
		return "speech.start", attrs
	case *UserStoppedSpeakingFrame:
		return "speech.stop", attrs
	case *InterimTranscriptionFrame:
		attrs["transcript"] = v.Text
		return "stt.partial", attrs
	case *FinalTranscriptionFrame:
		attrs["transcript"] = v.Text
		return "stt.final", attrs
	case *TranscriptCandidateFrame:
		attrs["transcript"], attrs["provider"] = v.Text, v.Provider
		if v.HasStability {
			attrs["custom.stt.stability"] = v.Stability
		}
		if v.Final {
			return "stt.final", attrs
		}
		return "stt.partial", attrs
	case *TranscriptAdvanceFrame:
		return "transcript.finalization", attrs
	case *UserMessageFrame:
		attrs["text"] = v.Content
		return "context.user_commit", attrs
	case *AssistantMessageFrame:
		attrs["text"] = v.Content
		attrs["status"] = "complete"
		if v.Partial {
			attrs["status"] = "partial"
		}
		return "context.assistant_commit", attrs
	case *LLMTextDeltaFrame:
		attrs["text"] = v.Delta
		return "llm.delta", attrs
	case *LLMTextFrame:
		attrs["text"] = v.Text
		return "llm.text", attrs
	case *ToolCallFrame:
		attrs["component"] = v.Call.Name
		return "tool.requested", attrs
	case *ToolResultFrame:
		attrs["status"] = string(v.Result.Status)
		if v.Result.Status == ToolSucceeded {
			return "tool.complete", attrs
		}
		return "tool.failed", attrs
	case *TTSStartedFrame:
		return "tts.started", attrs
	case *TTSAudioFrame:
		attrs["count"], attrs["sample_rate"], attrs["channels"], attrs["duration_ms"] = len(v.Audio), v.SampleRate, v.Channels, float64(v.Duration.Microseconds())/1000
		return "tts.audio_chunk", attrs
	case *TTSStoppedFrame:
		return "tts.stopped", attrs
	case *TurnEventFrame:
		switch v.Kind {
		case TurnEventStarted:
			return "turn.event", attrs
		case TurnEventStopped:
			return "speech.stop", attrs
		case TurnEventCompleted, TurnEventNoSpeechTimeout:
			return "turn.complete", attrs
		case TurnEventInterrupted:
			return "turn.interrupted", attrs
		}
	case *InterruptionFrame:
		return "turn.interrupted", attrs
	case *FlushFrame:
		return "output.flush", attrs
	case *ErrorFrame:
		attrs["error.category"] = "runtime"
		return "session.failure", attrs
	case *UsageMetricsFrame:
		attrs["provider"] = v.Provider
		attrs["service"] = v.Service
		if v.Model != "" {
			attrs["model"] = v.Model
		}
		if v.PromptTokens != nil {
			attrs["usage.prompt_tokens"] = *v.PromptTokens
		}
		if v.CompletionTokens != nil {
			attrs["usage.completion_tokens"] = *v.CompletionTokens
		}
		if v.TotalTokens != nil {
			attrs["usage.total_tokens"] = *v.TotalTokens
		}
		if v.Characters != nil {
			attrs["usage.characters"] = *v.Characters
		}
		if v.AudioDurationMs != nil {
			attrs["usage.audio_duration_ms"] = *v.AudioDurationMs
		}
		if v.Requests != nil {
			attrs["usage.requests"] = *v.Requests
		}
		return "usage.metrics", attrs
	}
	return "frame." + frameTraceLabel(f.Type()), attrs
}

func traceStatusError(err error, category string) error {
	if err == nil {
		return nil
	}
	return errors.New(category)
}

func traceTurnTerminal(frame Frame) bool {
	switch value := frame.(type) {
	case *TurnEventFrame:
		return value.Kind == TurnEventCompleted || value.Kind == TurnEventInterrupted || value.Kind == TurnEventNoSpeechTimeout
	case *InterruptionFrame:
		return true
	}
	return false
}

func traceTurnID(f Frame) TurnID {
	switch v := f.(type) {
	case *OutputAudioFrame:
		return TurnID(v.Turn)
	case *LLMTextDeltaFrame:
		return TurnID(v.Turn)
	case *LLMTextFrame:
		return TurnID(v.Turn)
	case *SynthesisUnitFrame:
		return TurnID(v.Turn)
	case *TTSStartedFrame:
		return TurnID(v.Turn)
	case *TTSAudioFrame:
		return TurnID(v.Turn)
	case *TTSStoppedFrame:
		return TurnID(v.Turn)
	case *UserMessageFrame:
		return TurnID(v.Turn)
	case *AssistantMessageFrame:
		return TurnID(v.Turn)
	case *TranscriptCandidateFrame:
		return v.Turn
	case *TranscriptInvalidationFrame:
		return v.Turn
	case *FinalTranscriptionFrame:
		return TurnID(v.Turn)
	case *InterimTranscriptionFrame:
		return TurnID(v.Turn)
	case *TurnEventFrame:
		return v.Turn
	case *InterruptionFrame:
		return TurnID(v.Turn)
	case *SpeakingStateFrame:
		return TurnID(v.Turn)
	case *WriteCompletionFrame:
		return TurnID(v.Turn)
	case *ToolCallFrame:
		return v.Call.TurnID
	case *ToolResultFrame:
		return v.Turn
	case *UsageMetricsFrame:
		return TurnID(v.Turn)
	}
	return ""
}

func traceGeneration(f Frame) Generation {
	if gf, ok := f.(GenerationalFrame); ok {
		return Generation(gf.Generation())
	}
	return 0
}

func frameTraceLabel(frame FrameType) string {
	switch frame {
	case FrameInputAudio:
		return "input_audio"
	case FrameOutputAudio:
		return "output_audio"
	case FrameTurnEvent:
		return "turn_event"
	case FrameTranscriptCandidate:
		return "transcript_candidate"
	case FrameTranscriptAdvance:
		return "transcript_advance"
	case FrameTranscriptInvalidation:
		return "transcript_invalidation"
	case FrameUserStartedSpeaking:
		return "speech_start"
	case FrameUserStoppedSpeaking:
		return "speech_stop"
	case FrameInterruption:
		return "interruption"
	case FrameFlush:
		return "flush"
	case FrameToolCall:
		return "tool_call"
	case FrameToolResult:
		return "tool_result"
	case FrameUsageMetrics:
		return "usage_metrics"
	case FramePushToTalk:
		return "push_to_talk"
	default:
		return strconv.Itoa(int(frame))
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

	if !e.s.tracing {
		if dir == Downstream {
			return e.s.deliver(f, dir, e.index+1)
		}
		return e.s.deliver(f, dir, e.index-1)
	}
	// A frame forwarded by a processor is the same object as its origin and
	// must not produce a second semantic event. Fresh inline frames are traced
	// at their emitting boundary exactly once.
	identity := traceIdentity(f)
	_, forwarded := e.s.traceOrigins[identity]
	if !forwarded {
		e.s.traceOrigins[identity] = struct{}{}
	}
	parent := e.s.traceCtx
	if parent == nil {
		parent = e.s.ctx
	}
	turn := traceTurnID(f)
	if !forwarded && turn != "" {
		if existing := e.s.turnSpans[turn]; existing != nil {
			parent = ContextWithSpan(e.s.traceCtx, existing)
		} else {
			turnSpan := StartSpanSafe(parent, e.s.tracer, NewTraceEvent("turn.start", time.Now(), SessionID(strconv.FormatUint(e.s.id, 10)), turn, traceGeneration(f), e.s.tracePolicy, nil))
			e.s.turnSpans[turn] = turnSpan
			parent = ContextWithSpan(e.s.traceCtx, turnSpan)
		}
	}
	var span Span
	if !forwarded {
		span = StartSpanSafe(parent, e.s.tracer, e.s.traceEvent(f))
	}
	var err error
	previous := e.s.traceCtx
	if span != nil {
		e.s.traceCtx = ContextWithSpan(parent, span)
	} else {
		e.s.traceCtx = parent
	}
	defer func() {
		e.s.traceCtx = previous
		if r := recover(); r != nil {
			if span != nil {
				span.End(errors.New("processor_error"))
			}
			if !forwarded && turn != "" && traceTurnTerminal(f) {
				if turnSpan := e.s.turnSpans[turn]; turnSpan != nil {
					turnSpan.End(errors.New("processor_error"))
					e.s.turnSpans[turn] = nil
				}
			}
			panic(r)
		}
		if span != nil {
			span.End(traceStatusError(err, "processor_error"))
		}
		if !forwarded && turn != "" && traceTurnTerminal(f) {
			if turnSpan := e.s.turnSpans[turn]; turnSpan != nil {
				turnSpan.End(traceStatusError(err, "processor_error"))
				e.s.turnSpans[turn] = nil
			}
		}
	}()
	if dir == Downstream {
		err = e.s.deliver(f, dir, e.index+1)
		return err
	}
	err = e.s.deliver(f, dir, e.index-1)
	return err
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
	if errFrame, ok := f.(*ErrorFrame); ok && errors.Is(errFrame.Err, ErrTranscriptInvalidated) {
		// Transcript invalidation must preserve candidate/invalidation order.
		// It remains observable downstream, but does not jump the data queue.
		return false
	}
	switch f.Type() {
	case FrameStart, FrameEnd, FrameCancel, FrameError, FrameInterruption, FrameFlush, FrameFlushProcessed, FrameWriteCompletion, FrameSpeakingState, FrameInputMute, FrameInputUnmute, FrameInputMutedState:
		return true
	default:
		return false
	}
}

// FenceSurvivor is implemented by frames that must be delivered even when the
// generation advanced after admission (a shared worker's result that clears
// the processor's in-flight state).
type FenceSurvivor interface{ SurvivesGenerationFence() }

func isFenceSurvivingControl(f Frame) bool {
	if _, ok := f.(FenceSurvivor); ok {
		return true
	}
	switch f.Type() {
	case FrameStart, FrameEnd, FrameCancel, FrameError, FrameFlushProcessed, FrameSpeakingState, FrameInputMute, FrameInputUnmute, FrameInputMutedState:
		return true
	}
	return false
}

func isUninterruptibleOutput(f Frame) bool {
	switch v := f.(type) {
	case *OutputAudioFrame:
		return v.Disposition == OutputUninterruptible
	case *TTSAudioFrame:
		return v.Disposition == OutputUninterruptible
	}
	return false
}

func isPartialAssistantCommit(f Frame) bool {
	m, ok := f.(*AssistantMessageFrame)
	return ok && m.Partial
}

func isInterruptibleOutput(f Frame) bool {
	if isPartialAssistantCommit(f) {
		return false
	}
	switch f.Type() {
	case FrameLLMTextDelta, FrameLLMText, FrameSynthesisUnit, FrameOutputTurnFinal, FrameTTSStarted, FrameTTSAudio, FrameTTSStopped, FrameOutputAudio, FrameAssistantMessage:
		return !isUninterruptibleOutput(f)
	}
	return false
}
