package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// hungProcessor blocks ignoring ctx, like a wedged provider or native call.
type hungProcessor struct{ release chan struct{} }

func (p hungProcessor) ProcessFrame(_ context.Context, _ Frame, _ Direction, _ Emitter) error {
	<-p.release
	return nil
}

type failingAttach struct{ closed chan struct{} }

func (failingAttach) ProcessFrame(_ context.Context, f Frame, d Direction, e Emitter) error {
	return e.PushDirection(f, d)
}
func (failingAttach) Attach(SessionController, int) error { return errors.New("attach failed") }
func (p failingAttach) Close() error                      { close(p.closed); return nil }

func teardownBudget() ResourceBudget {
	b := DefaultResourceBudget()
	b.MaxFrameProcessing = 200 * time.Millisecond
	b.TeardownTimeout = 300 * time.Millisecond
	return b
}

func TestWatchdogFailsHungProcessor(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	pipe, _ := NewPipeline(hungProcessor{release})
	s, _ := NewSession(1, pipe, teardownBudget())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	_ = s.Post(&SystemMessageFrame{})
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatalf("hung processor not contained: state=%v err=%v", s.State(), s.Err())
	}
	if !errors.Is(s.Err(), ErrProcessorWatchdog) {
		t.Fatalf("err = %v, want ErrProcessorWatchdog", s.Err())
	}
	if s.State() != StateLeaked || s.LeakedTasks() < 1 {
		t.Fatalf("stuck run loop must be reported as leaked: state=%v leaked=%d", s.State(), s.LeakedTasks())
	}
}

func TestCloseAfterFailedStartReturns(t *testing.T) {
	proc := failingAttach{closed: make(chan struct{})}
	pipe, _ := NewPipeline(proc)
	s, _ := NewSession(2, pipe, teardownBudget())
	if err := s.Start(); err == nil {
		t.Fatal("expected attach failure")
	}
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Close hung after failed Start (state=%v)", s.State())
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed")
	}
	select {
	case <-proc.closed:
	default:
		t.Fatal("processors attached before the failure were not closed")
	}
}

func TestGoRacingCloseNeverEscapesTeardown(t *testing.T) {
	for i := 0; i < 200; i++ {
		pipe, _ := NewPipeline(passthroughForTeardown{})
		s, _ := NewSession(uint64(i), pipe, teardownBudget())
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var ran, afterStop int32
		var mu sync.Mutex
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Go(func(context.Context) {
					mu.Lock()
					ran++
					select {
					case <-s.Done():
						afterStop++
					default:
					}
					mu.Unlock()
				})
			}
		}()
		_ = s.Close()
		wg.Wait()
		mu.Lock()
		if afterStop != 0 {
			t.Fatalf("iteration %d: %d tasks ran after teardown completed", i, afterStop)
		}
		mu.Unlock()
	}
}

type passthroughForTeardown struct{}

func (passthroughForTeardown) ProcessFrame(_ context.Context, f Frame, d Direction, e Emitter) error {
	return e.PushDirection(f, d)
}

type hangingCloser struct{ release chan struct{} }

func (hangingCloser) ProcessFrame(_ context.Context, f Frame, d Direction, e Emitter) error {
	return e.PushDirection(f, d)
}
func (p hangingCloser) Close() error { <-p.release; return nil }

func TestHungCloserCountedAsLeaked(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	pipe, _ := NewPipeline(hangingCloser{release})
	s, _ := NewSession(3, pipe, teardownBudget())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s.State() != StateLeaked || s.LeakedTasks() < 1 {
		t.Fatalf("hung Close() not accounted: state=%v leaked=%d", s.State(), s.LeakedTasks())
	}
}

// Close must not read run-loop-owned trace state while the loop is wedged;
// the race detector catches it when the processor later returns.
func TestWedgedLoopTeardownWithTracingIsRaceFree(t *testing.T) {
	release := make(chan struct{})
	pipe, _ := NewPipeline(hungProcessor{release})
	s, _ := NewSession(4, pipe, teardownBudget())
	if err := s.SetTracer(newRecordingTracerForTeardown(), TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	_ = s.Post(&SystemMessageFrame{})
	<-s.Done()
	close(release) // the wedged processor returns and the loop unwinds its trace state
	time.Sleep(50 * time.Millisecond)
	if s.State() != StateLeaked {
		t.Fatalf("state=%v", s.State())
	}
}

type teardownSpan struct{}

func (teardownSpan) SetAttribute(string, any) {}
func (teardownSpan) AddEvent(TraceEvent)      {}
func (teardownSpan) End(error)                {}

type teardownTracer struct{}

func (teardownTracer) StartSpan(context.Context, TraceEvent) Span { return teardownSpan{} }

func newRecordingTracerForTeardown() Tracer { return teardownTracer{} }
