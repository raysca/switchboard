package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestObserver_DeliveryOrderingAndCorrelation(t *testing.T) {
	var observed []ObserverEvent
	var mu sync.Mutex

	obs := ObserverFunc(func(e ObserverEvent) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, e)
	})

	proc1 := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		return e.PushDirection(f, d)
	}}
	proc2 := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		return e.PushDirection(f, d)
	}}

	pipe, _ := NewPipeline(proc1, proc2)
	s, _ := NewSession(500, pipe, DefaultResourceBudget())
	if err := s.AddObserver(obs); err != nil {
		t.Fatalf("AddObserver: %v", err)
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	input := &InputAudioFrame{Samples: []byte{1, 2, 3}, Seq: 1}
	_ = s.Post(input)

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	events := make([]ObserverEvent, len(observed))
	copy(events, observed)
	mu.Unlock()

	if len(events) < 2 {
		t.Fatalf("expected at least 2 observer events (one per processor in pipeline), got %d", len(events))
	}

	// Verify ordering: proc1 (index 0) delivered first, then proc2 (index 1)
	if events[0].ProcessorIndex != 0 || events[1].ProcessorIndex != 1 {
		t.Fatalf("processor index order mismatch: %d -> %d", events[0].ProcessorIndex, events[1].ProcessorIndex)
	}

	// Verify correlation and frame kind
	for i, ev := range events[:2] {
		if ev.SessionID != SessionID("500") {
			t.Fatalf("event %d: expected SessionID 500, got %s", i, ev.SessionID)
		}
		if ev.FrameKind != FrameInputAudio {
			t.Fatalf("event %d: expected FrameInputAudio, got %v", i, ev.FrameKind)
		}
		if ev.Direction != Downstream {
			t.Fatalf("event %d: expected Downstream, got %v", i, ev.Direction)
		}
	}
}

func TestObserver_PanicIsolationDoesNotFailSession(t *testing.T) {
	// A misbehaving observer panicking must never disrupt the session run-loop or pipeline
	panickingObs := ObserverFunc(func(e ObserverEvent) {
		panic("observer crashed unexpectedly!")
	})

	deliveredCount := 0
	var mu sync.Mutex
	proc := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		mu.Lock()
		deliveredCount++
		mu.Unlock()
		return e.PushDirection(f, d)
	}}

	pipe, _ := NewPipeline(proc)
	s, _ := NewSession(501, pipe, DefaultResourceBudget())
	_ = s.AddObserver(panickingObs)

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	_ = s.Post(&InputAudioFrame{Samples: []byte{4, 5}})
	time.Sleep(100 * time.Millisecond)

	// Session must remain running despite observer panic
	if s.State() > StateRunning {
		t.Fatalf("session should remain running despite observer panic, got state=%v err=%v", s.State(), s.Err())
	}

	mu.Lock()
	count := deliveredCount
	mu.Unlock()
	if count == 0 {
		t.Fatal("frame was not processed by pipeline processor due to observer panic")
	}
}

func TestObserver_MutationImpossibility(t *testing.T) {
	// Observer receives value copy of ObserverEvent and deep copy of attributes
	var capturedEv ObserverEvent
	var mu sync.Mutex
	obs := ObserverFunc(func(e ObserverEvent) {
		mu.Lock()
		defer mu.Unlock()
		capturedEv = e
		// Mutating returned attributes does not affect other callers
		attrs := e.Attributes()
		attrs["mutated"] = true
	})

	proc := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		return e.PushDirection(f, d)
	}}

	pipe, _ := NewPipeline(proc)
	s, _ := NewSession(502, pipe, DefaultResourceBudget())
	_ = s.AddObserver(obs)
	_ = s.Start()
	defer s.Close()

	_ = s.Post(&InputAudioFrame{Samples: []byte{6, 7}})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	attrs := capturedEv.Attributes()
	mu.Unlock()

	if _, ok := attrs["mutated"]; ok {
		t.Fatal("subsequent Attributes() call should return pristine cloned map without mutations")
	}
}
