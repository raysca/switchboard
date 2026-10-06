package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type policyTestProcessor struct {
	failCount    int
	maxFails     int
	errToReturn  error
	received     []Frame
	emittedCount int
	mu           sync.Mutex
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
		t.Fatal("timed out waiting for session failure ")
	}
}

func TestProcessorFailure_SessionIsolation(t *testing.T) {
	// If session 1 crashes a processor with an error,
	// session 2 using an independent session and pipeline remains completely healthy.
	failErr := errors.New("catastrophic failure")
	pipe1, _ := NewPipeline(&policyTestProcessor{maxFails: 1, errToReturn: failErr})
	pipe2, _ := NewPipeline(&policyTestProcessor{maxFails: 0})

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

func TestProcessorFailure_ContextCancelledDuringExecution(t *testing.T) {
	// When context is cancelled during processing, it should terminate cleanly
	blockCh := make(chan struct{})
	proc := &testFnProcessor{
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
