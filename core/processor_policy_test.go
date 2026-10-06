package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type policyTestProcessor struct {
	policy       FailurePolicy
	failCount    int
	maxFails     int
	errToReturn  error
	received     []Frame
	emittedCount int
	mu           sync.Mutex
}

func (p *policyTestProcessor) FailurePolicy() FailurePolicy {
	return p.policy
}

func (p *policyTestProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.received = append(p.received, f)

	if p.failCount < p.maxFails {
		p.failCount++
		return p.errToReturn
	}

	p.emittedCount++
	return e.PushDirection(f, dir)
}

func TestProcessorFailure_FailSessionPolicy(t *testing.T) {
	customErr := errors.New("unrecoverable model error")
	proc := &policyTestProcessor{
		policy:      PolicyFailSession,
		maxFails:    1,
		errToReturn: customErr,
	}

	pipe, _ := NewPipeline(proc)
	s, _ := NewSession(301, pipe, DefaultResourceBudget())

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_ = s.Post(&InputAudioFrame{Samples: []byte{1, 2}})

	select {
	case <-s.Done():
		if !errors.Is(s.Err(), customErr) {
			t.Fatalf("expected session to fail with %v, got %v", customErr, s.Err())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for session failure under PolicyFailSession")
	}
}

func TestProcessorFailure_DisablePolicy(t *testing.T) {
	disableErr := errors.New("optional filter crashed")
	proc := &policyTestProcessor{
		policy:      PolicyDisable,
		maxFails:    10, // will fail whenever called
		errToReturn: disableErr,
	}

	var passthroughFrames []Frame
	var mu sync.Mutex
	downstream := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		mu.Lock()
		passthroughFrames = append(passthroughFrames, f)
		mu.Unlock()
		return e.PushDirection(f, d)
	}}

	pipe, _ := NewPipeline(proc, downstream)
	s, _ := NewSession(302, pipe, DefaultResourceBudget())

	observedErrors := make(chan *ErrorFrame, 10)
	s.OnOutput = func(f Frame) {
		if ef, ok := f.(*ErrorFrame); ok {
			observedErrors <- ef
		}
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// First frame triggers failure -> processor disabled, error frame emitted non-fatal
	_ = s.Post(&InputAudioFrame{Samples: []byte{1, 2}})

	select {
	case ef := <-observedErrors:
		if !errors.Is(ef.Err, disableErr) {
			t.Fatalf("expected emitted error %v, got %v", disableErr, ef.Err)
		}
		if ef.Fatal {
			t.Fatal("expected non-fatal error frame for PolicyDisable")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for error frame")
	}

	// Session remains running!
	if s.State() > StateRunning {
		t.Fatalf("session should remain running, got %v", s.State())
	}

	// Subsequent frame bypasses disabled processor and reaches downstream!
	_ = s.Post(&InputAudioFrame{Samples: []byte{3, 4}})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := len(passthroughFrames)
	mu.Unlock()

	if count == 0 {
		t.Fatal("subsequent frame should reach downstream processor after first processor is disabled")
	}
}

func TestProcessorFailure_DegradePolicy(t *testing.T) {
	degradeErr := errors.New("enhancer unavailable")
	proc := &policyTestProcessor{
		policy:      PolicyDegrade,
		maxFails:    10,
		errToReturn: degradeErr,
	}

	var downstreamReceived []Frame
	var mu sync.Mutex
	downstream := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		mu.Lock()
		downstreamReceived = append(downstreamReceived, f)
		mu.Unlock()
		return e.PushDirection(f, d)
	}}

	pipe, _ := NewPipeline(proc, downstream)
	s, _ := NewSession(303, pipe, DefaultResourceBudget())

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Deliver audio frame
	_ = s.Post(&InputAudioFrame{Samples: []byte{7, 8}})
	time.Sleep(50 * time.Millisecond)

	// Session is running
	if s.State() > StateRunning {
		t.Fatalf("session failed under PolicyDegrade: %v", s.Err())
	}

	// Under degrade policy, frame is passed through downstream despite processor error
	mu.Lock()
	count := len(downstreamReceived)
	mu.Unlock()

	if count == 0 {
		t.Fatal("expected degraded frame to be passed downstream to next processor")
	}
}

func TestProcessorFailure_SafeRetryPolicy(t *testing.T) {
	retryErr := errors.New("transient network hiccup")
	// Fail once, then succeed on retry
	proc := &policyTestProcessor{
		policy:      PolicySafeRetry,
		maxFails:    1,
		errToReturn: retryErr,
	}

	var downstreamReceived []Frame
	var mu sync.Mutex
	downstream := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		mu.Lock()
		downstreamReceived = append(downstreamReceived, f)
		mu.Unlock()
		return e.PushDirection(f, d)
	}}

	pipe, _ := NewPipeline(proc, downstream)
	s, _ := NewSession(304, pipe, DefaultResourceBudget())

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	_ = s.Post(&InputAudioFrame{Samples: []byte{9, 10}})
	time.Sleep(50 * time.Millisecond)

	// Verify retry succeeded
	proc.mu.Lock()
	attempts := len(proc.received)
	proc.mu.Unlock()

	if attempts < 2 {
		t.Fatalf("expected at least 2 attempts (initial + retry), got %d", attempts)
	}

	mu.Lock()
	count := len(downstreamReceived)
	mu.Unlock()

	if count == 0 {
		t.Fatal("expected retried frame to reach downstream")
	}
	if s.State() > StateRunning {
		t.Fatalf("session should remain running after successful retry: %v", s.Err())
	}
}

func TestProcessorFailure_SessionIsolation(t *testing.T) {
	// If session 1 crashes a processor with PolicyFailSession,
	// session 2 using an independent session and pipeline remains completely healthy.
	failErr := errors.New("catastrophic failure")
	pipe1, _ := NewPipeline(&policyTestProcessor{policy: PolicyFailSession, maxFails: 1, errToReturn: failErr})
	pipe2, _ := NewPipeline(&policyTestProcessor{policy: PolicyFailSession, maxFails: 0})

	s1, _ := NewSession(305, pipe1, DefaultResourceBudget())
	s2, _ := NewSession(306, pipe2, DefaultResourceBudget())

	_ = s1.Start()
	_ = s2.Start()
	defer s2.Close()

	_ = s1.Post(&InputAudioFrame{Samples: []byte{1}})
	_ = s2.Post(&InputAudioFrame{Samples: []byte{2}})

	// s1 should fail
	select {
	case <-s1.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("s1 timed out")
	}

	// s2 must remain running and healthy!
	time.Sleep(50 * time.Millisecond)
	if s2.State() > StateRunning {
		t.Fatalf("s2 should remain running, got state=%v, err=%v", s2.State(), s2.Err())
	}
}

func TestProcessorFailure_RetryAbortedAfterOutput(t *testing.T) {
	// If a processor emits output and then fails, retrying could duplicate side effects.
	// PolicySafeRetry must abort retry and fail the session if outputs were already emitted.
	proc := &testFnProcessor{
		policy: PolicySafeRetry,
		fn: func(ctx context.Context, f Frame, d Direction, e Emitter) error {
			_ = e.Push(&OutputAudioFrame{Samples: []byte{1, 2, 3}})
			return errors.New("mid-stream failure after output emitted")
		},
	}

	pipe, _ := NewPipeline(proc)
	s, _ := NewSession(307, pipe, DefaultResourceBudget())

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_ = s.Post(&InputAudioFrame{Samples: []byte{1}})

	select {
	case <-s.Done():
		if !errors.Is(s.Err(), ErrRetryUnsafe) {
			t.Fatalf("expected ErrRetryUnsafe, got %v", s.Err())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for session failure on unsafe retry")
	}
}

func TestProcessorFailure_ContextCancelledDuringExecution(t *testing.T) {
	// When context is cancelled during processing, it should terminate cleanly
	blockCh := make(chan struct{})
	proc := &testFnProcessor{
		policy: PolicyFailSession,
		fn: func(ctx context.Context, f Frame, d Direction, e Emitter) error {
			close(blockCh)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	pipe, _ := NewPipeline(proc)
	s, _ := NewSession(308, pipe, DefaultResourceBudget())

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_ = s.Post(&InputAudioFrame{Samples: []byte{1}})
	<-blockCh
	_ = s.Close()

	select {
	case <-s.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for session close after context cancellation")
	}
}
