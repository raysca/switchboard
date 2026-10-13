package core_test

import (
	"govoice/core"
	"govoice/core/coretest"

	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 1. Test processor ordering, consumption, and multi-frame emission
func TestCore_ProcessorOrderingAndEmission(t *testing.T) {
	var executionOrder []string
	var mu sync.Mutex

	record := func(name string) {
		mu.Lock()
		executionOrder = append(executionOrder, name)
		mu.Unlock()
	}

	p1 := &testFnProcessor{fn: func(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
		record("p1")
		// Emit two frames downstream
		_ = e.Push(&core.UserStartedSpeakingFrame{Seq: 1})
		_ = e.Push(&core.UserStartedSpeakingFrame{Seq: 2})
		return nil
	}}

	p2 := &testFnProcessor{fn: func(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
		record("p2")
		return e.Push(f)
	}}

	pipe, err := core.NewPipeline(p1, p2)
	if err != nil {
		t.Fatalf("NewPipeline error: %v", err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession error: %v", err)
	}

	var outputFrames []core.Frame
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		outputFrames = append(outputFrames, f)
		mu.Unlock()
	}

	if err := session.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer session.Close()

	if err := session.Post(&core.StartFrame{}); err != nil {
		t.Fatalf("Post error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// p1 processed StartFrame and emitted 2 frames to p2
	if len(executionOrder) != 3 || executionOrder[0] != "p1" || executionOrder[1] != "p2" || executionOrder[2] != "p2" {
		t.Fatalf("Unexpected execution order: %v", executionOrder)
	}

	if len(outputFrames) != 2 {
		t.Fatalf("Expected 2 output frames, got %d", len(outputFrames))
	}
}

// 2. Test downstream and upstream routing
func TestCore_BidirectionalRouting(t *testing.T) {
	var upstreamReceived core.Frame
	var downstreamReceived core.Frame
	var mu sync.Mutex

	pMiddle := &testFnProcessor{fn: func(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
		if f.Type() == core.FrameStart {
			// Emit one upstream, one downstream
			_ = e.PushDirection(&core.CancelFrame{Reason: "up"}, core.Upstream)
			_ = e.PushDirection(&core.UserStartedSpeakingFrame{Seq: 10}, core.Downstream)
		}
		return nil
	}}

	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{}, pMiddle, &coretest.PassthroughProcessor{})
	session, _ := core.NewSession(2, pipe, core.DefaultResourceBudget())

	session.OnUpstream = func(f core.Frame) {
		mu.Lock()
		upstreamReceived = f
		mu.Unlock()
	}

	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		downstreamReceived = f
		mu.Unlock()
	}

	_ = session.Start()
	defer session.Close()

	_ = session.Post(&core.StartFrame{})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if upstreamReceived == nil || upstreamReceived.Type() != core.FrameCancel {
		t.Fatalf("Expected upstream CancelFrame, got %v", upstreamReceived)
	}
	if downstreamReceived == nil || downstreamReceived.Type() != core.FrameUserStartedSpeaking {
		t.Fatalf("Expected downstream UserStartedSpeakingFrame, got %v", downstreamReceived)
	}
}

// 3. Test async re-entry resumes at the correct processor
func TestCore_AsyncReentryPosition(t *testing.T) {
	var visitedProcs []int
	var mu sync.Mutex

	makeProc := func(idx int) core.Processor {
		return &testFnProcessor{fn: func(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
			mu.Lock()
			visitedProcs = append(visitedProcs, idx)
			mu.Unlock()
			return e.Push(f)
		}}
	}

	pipe, _ := core.NewPipeline(makeProc(0), makeProc(1), makeProc(2))
	session, _ := core.NewSession(3, pipe, core.DefaultResourceBudget())

	_ = session.Start()
	defer session.Close()

	// Reenter at index 1: should skip index 0 and visit 1 then 2
	err := session.Reenter(&core.UserStartedSpeakingFrame{Seq: 99}, core.Downstream, 1, session.CurrentGeneration())
	if err != nil {
		t.Fatalf("Reenter error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(visitedProcs) != 2 || visitedProcs[0] != 1 || visitedProcs[1] != 2 {
		t.Fatalf("Reentry did not resume at index 1: visited %v", visitedProcs)
	}
}

// 4. Test old-generation re-entry is discarded before processor mutation
func TestCore_StaleGenerationDiscard(t *testing.T) {
	var visited bool
	var mu sync.Mutex

	proc := &testFnProcessor{fn: func(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
		mu.Lock()
		visited = true
		mu.Unlock()
		return e.Push(f)
	}}

	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(4, pipe, core.DefaultResourceBudget())

	_ = session.Start()
	defer session.Close()

	// Capture initial generation (gen = 1)
	oldGen := session.CurrentGeneration()

	// Advance session generation (turn change / barge-in)
	session.IncrementGeneration() // gen = 2

	// Reenter with stale generation 1
	_ = session.Reenter(&core.FinalTranscriptionFrame{
		Text: "stale transcript",
		Gen:  oldGen,
	}, core.Downstream, 0, oldGen)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if visited {
		t.Fatalf("Stale generation re-entry should have been discarded before processor delivery!")
	}
	if session.StaleDiscarded.Load() == 0 {
		t.Fatalf("Expected StaleDiscarded metric to increment, got 0")
	}
}

// 5. Test queue overflow policies and resource budgets
func TestCore_QueueOverflowPolicies(t *testing.T) {
	budget := core.DefaultResourceBudget()
	budget.MaxQueuedFrames = 2
	budget.MaxControlFrames = 2
	budget.MaxAsyncTasks = 2
	budget.MaxEmitsPerFrame = 3

	// 5a. Control queue full terminates session
	pipe1, _ := core.NewPipeline(&coretest.LatencyProcessor{Delay: 100 * time.Millisecond})
	s1, _ := core.NewSession(51, pipe1, budget)
	_ = s1.Start()
	defer s1.Close()

	// Fill control queue. The run loop may already hold the first frame, so
	// overflow comes on the 3rd or 4th post.
	var errCtrl error
	for i := 0; i < 4 && errCtrl == nil; i++ {
		errCtrl = s1.Post(&core.StartFrame{})
	}
	if errCtrl != core.ErrControlQueueFull {
		t.Fatalf("Expected ErrControlQueueFull, got: %v", errCtrl)
	}

	// 5b. Task budget exceeded
	pipe2, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s2, _ := core.NewSession(52, pipe2, budget)
	_ = s2.Start()
	defer s2.Close()

	_ = s2.Go(func(ctx context.Context) { time.Sleep(100 * time.Millisecond) })
	_ = s2.Go(func(ctx context.Context) { time.Sleep(100 * time.Millisecond) })
	errTask := s2.Go(func(ctx context.Context) { time.Sleep(100 * time.Millisecond) })
	if !errors.Is(errTask, core.ErrTooManyTasks) {
		t.Fatalf("Expected ErrTooManyTasks, got: %v", errTask)
	}

	// 5c. Runaway emit guard
	pipe3, _ := core.NewPipeline(&coretest.FloodProcessor{EmitCount: 10})
	s3, _ := core.NewSession(53, pipe3, budget)
	_ = s3.Start()
	defer s3.Close()

	_ = s3.Post(&core.StartFrame{})
	time.Sleep(50 * time.Millisecond)
	if !errors.Is(s3.Err(), core.ErrRunawayEmits) {
		t.Fatalf("Expected ErrRunawayEmits, got: %v", s3.Err())
	}
}

// LLM text produced faster than a slow run loop drains (TTS pacing at media
// time) must wait for room, not fail the turn with ErrQueueFull.
func TestCore_DataFramesWaitForSlowRunLoop(t *testing.T) {
	budget := core.DefaultResourceBudget()
	budget.MaxQueuedFrames = 2
	pipe, _ := core.NewPipeline(&coretest.LatencyProcessor{Delay: 30 * time.Millisecond})
	s, _ := core.NewSession(55, pipe, budget)
	_ = s.Start()
	defer s.Close()

	for i := 0; i < 6; i++ {
		if err := s.Post(&core.LLMTextDeltaFrame{Delta: "x", Turn: "t1"}); err != nil {
			t.Fatalf("delta %d: Post() = %v, want nil (backpressure)", i, err)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("session failed: %v", err)
	}
}

type internalTestFrame struct{}

func (*internalTestFrame) Type() core.FrameType { return core.FrameInternal }

// Internal frames come from shared workers (VAD); a full inbox fails them at
// once instead of stalling the worker for every other session.
func TestCore_InternalFramesFailFastWhenFull(t *testing.T) {
	budget := core.DefaultResourceBudget()
	budget.MaxQueuedFrames = 1
	pipe, _ := core.NewPipeline(&coretest.LatencyProcessor{Delay: time.Second})
	s, _ := core.NewSession(56, pipe, budget)
	_ = s.Start()
	defer s.Close()

	_ = s.Post(&core.LLMTextDeltaFrame{Delta: "busy"}) // run loop now busy for 1s
	_ = s.Post(&core.LLMTextDeltaFrame{Delta: "fills the inbox"})
	start := time.Now()
	if err := s.Post(&internalTestFrame{}); !errors.Is(err, core.ErrQueueFull) {
		t.Fatalf("Post(internal) = %v, want ErrQueueFull", err)
	}
	if waited := time.Since(start); waited > 200*time.Millisecond {
		t.Fatalf("internal frame waited %v for room", waited)
	}
}

// 6. Test processor and session.Go panic containment
func TestCore_PanicContainment(t *testing.T) {
	// 6a. Inline processor panic
	pipeInline, _ := core.NewPipeline(&coretest.PanicProcessor{PanicOn: core.FrameStart})
	sInline, _ := core.NewSession(61, pipeInline, core.DefaultResourceBudget())
	_ = sInline.Start()

	_ = sInline.Post(&core.StartFrame{})
	time.Sleep(50 * time.Millisecond)
	_ = sInline.Close()

	if sInline.State() != core.StateFailed {
		t.Fatalf("Inline panic should set session state to StateFailed, got %v", sInline.State())
	}
	if sInline.Err() == nil {
		t.Fatalf("Expected recorded panic error on session")
	}

	// 6b. Async session.Go child task panic
	pipeChild, _ := core.NewPipeline(&coretest.ChildPanicProcessor{})
	sChild, _ := core.NewSession(62, pipeChild, core.DefaultResourceBudget())
	_ = sChild.Start()

	_ = sChild.Post(&core.StartFrame{})
	time.Sleep(50 * time.Millisecond)
	_ = sChild.Close()

	if sChild.State() != core.StateFailed {
		t.Fatalf("Child task panic should set session state to StateFailed, got %v", sChild.State())
	}
}

// 7. Test cancellation and teardown timeout with leaked-task reporting
func TestCore_TeardownAndLeakedTaskDetection(t *testing.T) {
	budget := core.DefaultResourceBudget()
	budget.TeardownTimeout = 50 * time.Millisecond // Short teardown for test

	pipe, _ := core.NewPipeline(&coretest.IgnoredCancellationProcessor{})
	session, _ := core.NewSession(7, pipe, budget)
	_ = session.Start()

	_ = session.Post(&core.StartFrame{})
	time.Sleep(20 * time.Millisecond)

	// Close session: task ignores cancellation, so teardown will timeout
	_ = session.Close()

	if session.State() != core.StateLeaked {
		t.Fatalf("Expected StateLeaked for task ignoring cancellation, got: %v", session.State())
	}
	if session.LeakedTasks() != 1 {
		t.Fatalf("Expected 1 leaked task reported, got: %d", session.LeakedTasks())
	}
}

// 8. Test unrelated sessions progress while one stalls or floods
func TestCore_SessionIsolation(t *testing.T) {
	budget := core.DefaultResourceBudget()
	budget.MaxFrameProcessing = 100 * time.Millisecond

	// Bad session stalls
	pipeBad, _ := core.NewPipeline(&coretest.StallProcessor{Duration: 300 * time.Millisecond})
	sBad, _ := core.NewSession(81, pipeBad, budget)

	// Good session executes normally
	var goodCompleted bool
	var mu sync.Mutex
	pipeGood, _ := core.NewPipeline(&testFnProcessor{fn: func(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
		mu.Lock()
		goodCompleted = true
		mu.Unlock()
		return e.Push(f)
	}})
	sGood, _ := core.NewSession(82, pipeGood, budget)

	_ = sBad.Start()
	_ = sGood.Start()
	defer sBad.Close()
	defer sGood.Close()

	// Post to both
	_ = sBad.Post(&core.StartFrame{})
	_ = sGood.Post(&core.StartFrame{})

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	done := goodCompleted
	mu.Unlock()

	if !done {
		t.Fatalf("Good session was blocked by bad session stalling!")
	}
}

// Helper test processor
type testFnProcessor struct {
	fn func(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error
}

func (p *testFnProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	return p.fn(ctx, f, dir, e)
}
