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

	// Post an urgent control frame: CancelFrame
	cancelFrame := &core.CancelFrame{}
	if err := s.Post(cancelFrame); err != nil {
		t.Fatalf("Post CancelFrame: %v", err)
	}

	// Also post an ordinary control frame: InputMuteFrame
	muteFrame := &core.InputMuteFrame{}
	if err := s.Post(muteFrame); err != nil {
		t.Fatalf("Post InputMuteFrame: %v", err)
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

	// The urgent frame (CancelFrame) must be processed BEFORE the ordinary data queue frames
	firstProcessed := processed[0]
	if firstProcessed.Type() != core.FrameCancel {
		t.Fatalf("expected first processed frame to be FrameCancel, got %v", firstProcessed.Type())
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

	// Post urgent frames in specific order: Flush -> Cancel
	_ = s.Post(&core.FlushFrame{Gen: 1})
	_ = s.Post(&core.CancelFrame{})

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
	if processedTypes[1] != core.FrameCancel {
		t.Fatalf("expected second frame FrameCancel, got %v", processedTypes[1])
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
			_ = s.Post(&core.CancelFrame{})
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
