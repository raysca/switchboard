package core

import (
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

	p1 := &testFnProcessor{fn: func(ctx context.Context, f Frame, dir Direction, e Emitter) error {
		record("p1")
		// Emit two frames downstream
		_ = e.Push(&UserStartedSpeakingFrame{Seq: 1})
		_ = e.Push(&UserStartedSpeakingFrame{Seq: 2})
		return nil
	}}

	p2 := &testFnProcessor{fn: func(ctx context.Context, f Frame, dir Direction, e Emitter) error {
		record("p2")
		return e.Push(f)
	}}

	pipe, err := NewPipeline(p1, p2)
	if err != nil {
		t.Fatalf("NewPipeline error: %v", err)
	}

	session, err := NewSession(1, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession error: %v", err)
	}

	var outputFrames []Frame
	session.OnOutput = func(f Frame) {
		mu.Lock()
		outputFrames = append(outputFrames, f)
		mu.Unlock()
	}

	if err := session.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer session.Close()

	if err := session.Post(&StartFrame{}); err != nil {
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
	var upstreamReceived Frame
	var downstreamReceived Frame
	var mu sync.Mutex

	pMiddle := &testFnProcessor{fn: func(ctx context.Context, f Frame, dir Direction, e Emitter) error {
		if f.Type() == FrameStart {
			// Emit one upstream, one downstream
			_ = e.PushDirection(&CancelFrame{Reason: "up"}, Upstream)
			_ = e.PushDirection(&UserStartedSpeakingFrame{Seq: 10}, Downstream)
		}
		return nil
	}}

	pipe, _ := NewPipeline(&PassthroughProcessor{}, pMiddle, &PassthroughProcessor{})
	session, _ := NewSession(2, pipe, DefaultResourceBudget())

	session.OnUpstream = func(f Frame) {
		mu.Lock()
		upstreamReceived = f
		mu.Unlock()
	}

	session.OnOutput = func(f Frame) {
		mu.Lock()
		downstreamReceived = f
		mu.Unlock()
	}

	_ = session.Start()
	defer session.Close()

	_ = session.Post(&StartFrame{})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if upstreamReceived == nil || upstreamReceived.Type() != FrameCancel {
		t.Fatalf("Expected upstream CancelFrame, got %v", upstreamReceived)
	}
	if downstreamReceived == nil || downstreamReceived.Type() != FrameUserStartedSpeaking {
		t.Fatalf("Expected downstream UserStartedSpeakingFrame, got %v", downstreamReceived)
	}
}

// 3. Test async re-entry resumes at the correct processor
func TestCore_AsyncReentryPosition(t *testing.T) {
	var visitedProcs []int
	var mu sync.Mutex

	makeProc := func(idx int) Processor {
		return &testFnProcessor{fn: func(ctx context.Context, f Frame, dir Direction, e Emitter) error {
			mu.Lock()
			visitedProcs = append(visitedProcs, idx)
			mu.Unlock()
			return e.Push(f)
		}}
	}

	pipe, _ := NewPipeline(makeProc(0), makeProc(1), makeProc(2))
	session, _ := NewSession(3, pipe, DefaultResourceBudget())

	_ = session.Start()
	defer session.Close()

	// Reenter at index 1: should skip index 0 and visit 1 then 2
	err := session.Reenter(&UserStartedSpeakingFrame{Seq: 99}, Downstream, 1, session.CurrentGeneration())
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

	proc := &testFnProcessor{fn: func(ctx context.Context, f Frame, dir Direction, e Emitter) error {
		mu.Lock()
		visited = true
		mu.Unlock()
		return e.Push(f)
	}}

	pipe, _ := NewPipeline(proc)
	session, _ := NewSession(4, pipe, DefaultResourceBudget())

	_ = session.Start()
	defer session.Close()

	// Capture initial generation (gen = 1)
	oldGen := session.CurrentGeneration()

	// Advance session generation (turn change / barge-in)
	session.IncrementGeneration() // gen = 2

	// Reenter with stale generation 1
	_ = session.Reenter(&FinalTranscriptionFrame{
		Text: "stale transcript",
		Gen:  oldGen,
	}, Downstream, 0, oldGen)

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
	budget := DefaultResourceBudget()
	budget.MaxQueuedFrames = 2
	budget.MaxControlFrames = 2
	budget.MaxAsyncTasks = 2
	budget.MaxEmitsPerFrame = 3

	// 5a. Control queue full terminates session
	pipe1, _ := NewPipeline(&LatencyProcessor{Delay: 100 * time.Millisecond})
	s1, _ := NewSession(51, pipe1, budget)
	_ = s1.Start()
	defer s1.Close()

	// Fill control queue
	_ = s1.Post(&StartFrame{})
	_ = s1.Post(&StartFrame{})
	errCtrl := s1.Post(&StartFrame{})
	if errCtrl != ErrControlQueueFull {
		t.Fatalf("Expected ErrControlQueueFull, got: %v", errCtrl)
	}

	// 5b. Task budget exceeded
	pipe2, _ := NewPipeline(&PassthroughProcessor{})
	s2, _ := NewSession(52, pipe2, budget)
	_ = s2.Start()
	defer s2.Close()

	_ = s2.Go(func(ctx context.Context) { time.Sleep(100 * time.Millisecond) })
	_ = s2.Go(func(ctx context.Context) { time.Sleep(100 * time.Millisecond) })
	errTask := s2.Go(func(ctx context.Context) { time.Sleep(100 * time.Millisecond) })
	if !errors.Is(errTask, ErrTooManyTasks) {
		t.Fatalf("Expected ErrTooManyTasks, got: %v", errTask)
	}

	// 5c. Runaway emit guard
	pipe3, _ := NewPipeline(&FloodProcessor{EmitCount: 10})
	s3, _ := NewSession(53, pipe3, budget)
	_ = s3.Start()
	defer s3.Close()

	_ = s3.Post(&StartFrame{})
	time.Sleep(50 * time.Millisecond)
	if !errors.Is(s3.Err(), ErrRunawayEmits) {
		t.Fatalf("Expected ErrRunawayEmits, got: %v", s3.Err())
	}
}

// 6. Test processor and session.Go panic containment
func TestCore_PanicContainment(t *testing.T) {
	// 6a. Inline processor panic
	pipeInline, _ := NewPipeline(&PanicProcessor{PanicOn: FrameStart})
	sInline, _ := NewSession(61, pipeInline, DefaultResourceBudget())
	_ = sInline.Start()

	_ = sInline.Post(&StartFrame{})
	time.Sleep(50 * time.Millisecond)
	_ = sInline.Close()

	if sInline.State() != StateFailed {
		t.Fatalf("Inline panic should set session state to StateFailed, got %v", sInline.State())
	}
	if sInline.Err() == nil {
		t.Fatalf("Expected recorded panic error on session")
	}

	// 6b. Async session.Go child task panic
	pipeChild, _ := NewPipeline(&ChildPanicProcessor{})
	sChild, _ := NewSession(62, pipeChild, DefaultResourceBudget())
	_ = sChild.Start()

	_ = sChild.Post(&StartFrame{})
	time.Sleep(50 * time.Millisecond)
	_ = sChild.Close()

	if sChild.State() != StateFailed {
		t.Fatalf("Child task panic should set session state to StateFailed, got %v", sChild.State())
	}
}

// 7. Test cancellation and teardown timeout with leaked-task reporting
func TestCore_TeardownAndLeakedTaskDetection(t *testing.T) {
	budget := DefaultResourceBudget()
	budget.TeardownTimeout = 50 * time.Millisecond // Short teardown for test

	pipe, _ := NewPipeline(&IgnoredCancellationProcessor{})
	session, _ := NewSession(7, pipe, budget)
	_ = session.Start()

	_ = session.Post(&StartFrame{})
	time.Sleep(20 * time.Millisecond)

	// Close session: task ignores cancellation, so teardown will timeout
	_ = session.Close()

	if session.State() != StateLeaked {
		t.Fatalf("Expected StateLeaked for task ignoring cancellation, got: %v", session.State())
	}
	if session.LeakedTasks() != 1 {
		t.Fatalf("Expected 1 leaked task reported, got: %d", session.LeakedTasks())
	}
}

// 8. Test unrelated sessions progress while one stalls or floods
func TestCore_SessionIsolation(t *testing.T) {
	budget := DefaultResourceBudget()
	budget.MaxFrameProcessing = 100 * time.Millisecond

	// Bad session stalls
	pipeBad, _ := NewPipeline(&StallProcessor{Duration: 300 * time.Millisecond})
	sBad, _ := NewSession(81, pipeBad, budget)

	// Good session executes normally
	var goodCompleted bool
	var mu sync.Mutex
	pipeGood, _ := NewPipeline(&testFnProcessor{fn: func(ctx context.Context, f Frame, dir Direction, e Emitter) error {
		mu.Lock()
		goodCompleted = true
		mu.Unlock()
		return e.Push(f)
	}})
	sGood, _ := NewSession(82, pipeGood, budget)

	_ = sBad.Start()
	_ = sGood.Start()
	defer sBad.Close()
	defer sGood.Close()

	// Post to both
	_ = sBad.Post(&StartFrame{})
	_ = sGood.Post(&StartFrame{})

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
	fn func(ctx context.Context, f Frame, dir Direction, e Emitter) error
}

func (p *testFnProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return p.fn(ctx, f, dir, e)
}
