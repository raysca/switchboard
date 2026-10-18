package core_test

import (
	"govoice/core"
	"govoice/core/coretest"

	"context"
	"sync"
	"testing"
	"time"
)

// TestUrgentControl_BypassesCongestedMediaQueue verifies that when the ordinary data queue
// is filled with media frames, an urgent control frame (Cancel, Flush, Interruption, End)
// is admitted and delivered ahead of the congested media frames.
func TestUrgentControl_BypassesCongestedMediaQueue(t *testing.T) {
	var processed []core.Frame
	var mu sync.Mutex

	// Processor records processed frames
	pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		mu.Lock()
		processed = append(processed, f)
		mu.Unlock()
		return e.PushDirection(f, d)
	}})

	// Budget with large data queue but small enough to test queueing
	budget := core.DefaultResourceBudget()
	budget.MaxQueuedFrames = 100
	budget.MaxControlFrames = 50

	s, err := core.NewSession(200, pipe, budget)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Do NOT start session yet so we can pre-populate queues
	// Pre-fill data queue with 10 media frames
	for i := 0; i < 10; i++ {
		audioFrame := &core.InputAudioFrame{
			Samples:    []byte{byte(i)},
			Encoding:   core.EncodingPCM16LE,
			SampleRate: 16000,
			Channels:   1,
		}
		if err := s.Post(audioFrame); err != nil {
			t.Fatalf("Post audio %d: %v", i, err)
		}
	}

	// Two distinguishable control frames, in this order: the mute (seen by the
	// processor as FrameInputMutedState), then a flush (FrameFlush).
	if err := s.Post(&core.InputMuteFrame{}); err != nil {
		t.Fatalf("Post InputMuteFrame: %v", err)
	}
	if err := s.Post(&core.FlushFrame{Gen: 1}); err != nil {
		t.Fatalf("Post FlushFrame: %v", err)
	}

	// Start the session to let run loop process envelopes
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Wait briefly for run loop to process initial items
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(processed) < 2 {
		t.Fatalf("expected at least 2 processed frames, got %d", len(processed))
	}

	// Control frames run before the congested data queue, in posting order.
	if processed[0].Type() != core.FrameInputMutedState || processed[1].Type() != core.FrameFlush {
		t.Fatalf("expected [FrameInputMutedState FrameFlush] first, got %v then %v", processed[0].Type(), processed[1].Type())
	}
}

// TestUrgentControl_PreservesControlOrder verifies that multiple urgent frames
// posted consecutively maintain strict FIFO order among themselves.
func TestUrgentControl_PreservesControlOrder(t *testing.T) {
	var processedTypes []core.FrameType
	var mu sync.Mutex

	pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		mu.Lock()
		processedTypes = append(processedTypes, f.Type())
		mu.Unlock()
		return e.PushDirection(f, d)
	}})

	s, _ := core.NewSession(201, pipe, core.DefaultResourceBudget())

	// Post control frames in specific order: Flush -> mute state
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Post(&core.InputMutedStateFrame{})

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(processedTypes) < 2 {
		t.Fatalf("expected 2 processed frames, got %d: %v", len(processedTypes), processedTypes)
	}

	if processedTypes[0] != core.FrameFlush {
		t.Fatalf("expected first frame FrameFlush, got %v", processedTypes[0])
	}
	if processedTypes[1] != core.FrameInputMutedState {
		t.Fatalf("expected second frame FrameInputMutedState, got %v", processedTypes[1])
	}
}

// TestUrgentControl_StarvationPrevention ensures that heavy urgent/control traffic
// does not completely starve the data queue indefinitely (fair scheduling between queues).
func TestUrgentControl_StarvationPrevention(t *testing.T) {
	var dataCount int
	var controlCount int
	var mu sync.Mutex

	pipe, _ := core.NewPipeline(&testFnProcessor{fn: func(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
		mu.Lock()
		if f.Type() == core.FrameInputAudio {
			dataCount++
		} else {
			controlCount++
		}
		mu.Unlock()
		return e.PushDirection(f, d)
	}})

	s, _ := core.NewSession(202, pipe, core.DefaultResourceBudget())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Interleave data frames and control frames
	var wg sync.WaitGroup
	wg.Add(2)

	// Stream media data
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = s.Post(&core.InputAudioFrame{Samples: []byte{byte(i)}, Encoding: core.EncodingPCM16LE})
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Stream controls
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = s.Post(&core.InputMutedStateFrame{})
			time.Sleep(2 * time.Millisecond)
		}
	}()

	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if dataCount == 0 {
		t.Fatal("data queue was completely starved by control lane")
	}
	if controlCount == 0 {
		t.Fatal("control queue frames were not processed")
	}
}

// TestUrgentControl_ShutdownGrace verifies that an urgent EndFrame immediately
// begins orderly teardown even when media frames are queued.
func TestUrgentControl_ShutdownGrace(t *testing.T) {
	pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
	s, _ := core.NewSession(203, pipe, core.DefaultResourceBudget())

	// Enqueue media frames
	for i := 0; i < 5; i++ {
		_ = s.Post(&core.InputAudioFrame{Samples: []byte{1, 2}, Encoding: core.EncodingPCM16LE})
	}

	// Post EndFrame
	if err := s.Post(&core.EndFrame{}); err != nil {
		t.Fatalf("Post EndFrame: %v", err)
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-s.Done():
		// Session successfully tore down without stalling
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for session teardown on urgent EndFrame")
	}
}
