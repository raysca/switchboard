package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

// pacedSink blocks 20ms per output frame (the transport pacing the run loop)
// and timestamps caller audio when it arrives.
type pacedSink struct {
	mu    sync.Mutex
	inAt  []time.Time
	delay time.Duration
}

func (p *pacedSink) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch f.Type() {
	case FrameOutputAudio:
		time.Sleep(p.delay)
	case FrameInputAudio:
		p.mu.Lock()
		p.inAt = append(p.inAt, time.Now())
		p.mu.Unlock()
	}
	return e.PushDirection(f, dir)
}

// A burst of bot audio must not queue ahead of caller audio for longer than a
// couple of paced frames (VAD staleness), and must not shed caller audio.
func TestSession_OutputBacklogDoesNotDelayInputAudio(t *testing.T) {
	sink := &pacedSink{delay: 20 * time.Millisecond}
	pipe, _ := NewPipeline(sink)
	s, _ := NewSession(77, pipe, DefaultResourceBudget())
	_ = s.Start()
	defer s.Close()

	go func() { // TTS running far faster than real time
		for i := 0; i < 40; i++ {
			_ = s.Post(&OutputAudioFrame{Samples: make([]byte, 640), Encoding: EncodingPCM16LE, SampleRate: 16000, Channels: 1, Duration: 20 * time.Millisecond})
		}
	}()
	time.Sleep(100 * time.Millisecond) // backlog has built up behind the pacer

	start := time.Now()
	for i := 0; i < 10; i++ {
		_ = s.Post(&InputAudioFrame{Samples: make([]byte, 640), Encoding: EncodingPCM16LE, SampleRate: 16000, Channels: 1, Duration: 20 * time.Millisecond})
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		sink.mu.Lock()
		n := len(sink.inAt)
		var last time.Time
		if n > 0 {
			last = sink.inAt[n-1]
		}
		sink.mu.Unlock()
		if n == 10 {
			// Pre-fix the 10th frame waited behind ~38 queued frames (~760ms).
			if d := last.Sub(start); d > 250*time.Millisecond {
				t.Fatalf("caller audio delayed %v behind bot audio backlog", d)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/10 input frames delivered", n)
		}
		time.Sleep(time.Millisecond)
	}
	if d := s.AudioDropped.Load(); d != 0 {
		t.Fatalf("dropped %d input frames", d)
	}
}

// Interruptions that fence queued bot audio (dropped unprocessed) must still
// return every output slot, or TTS wedges after a few barge-ins.
func TestSession_OutputSlotsSurviveInterruptions(t *testing.T) {
	sink := &pacedSink{delay: 2 * time.Millisecond}
	pipe, _ := NewPipeline(sink)
	s, _ := NewSession(78, pipe, DefaultResourceBudget())
	_ = s.Start()
	defer s.Close()

	var mu sync.Mutex
	var out int
	s.OnFrame = func(f Frame, _ Direction) {
		if f.Type() == FrameOutputAudio {
			mu.Lock()
			out++
			mu.Unlock()
		}
	}
	tts := func(ctx context.Context, n int) {
		for i := 0; i < n && ctx.Err() == nil; i++ {
			gen := s.CurrentGeneration()
			if err := s.Reenter(&OutputAudioFrame{Samples: make([]byte, 640), Encoding: EncodingPCM16LE, SampleRate: 16000, Channels: 1, Duration: 20 * time.Millisecond, Gen: gen}, Downstream, 0, gen); err != nil {
				t.Errorf("reenter: %v", err)
				return
			}
		}
	}
	for p := 0; p < 3; p++ {
		if err := s.Go(func(ctx context.Context) { tts(ctx, 400) }); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		_ = s.Post(&FlushFrame{Gen: s.CurrentGeneration()})
		time.Sleep(time.Millisecond)
	}
	waitFor := func(cond func() bool, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor(func() bool { return s.activeTasks.Load() == 0 }, "producers finished")
	waitFor(func() bool { return len(s.outSlots) == 0 && len(s.data) == 0 }, "all output slots returned")
	if err := s.Err(); err != nil {
		t.Fatalf("session failed: %v", err)
	}
	if s.StaleDiscarded.Load() == 0 {
		t.Fatal("no queued bot audio was fenced; the test did not exercise the drop path")
	}

	mu.Lock()
	before := out
	mu.Unlock()
	if err := s.Go(func(ctx context.Context) { tts(ctx, 20) }); err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return out-before >= 20 }, "TTS flows after interruptions")
	waitFor(func() bool { return s.activeTasks.Load() == 0 && len(s.outSlots) == 0 }, "slots idle")
	_ = s.Close()
	if n := s.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
}

// The output gate runs on the producer, before the frame takes a slot, with
// the frame's duration; frames without a duration (closing markers) skip it.
func TestSession_OutputGateHoldsProducer(t *testing.T) {
	pipe, _ := NewPipeline(&pacedSink{})
	s, _ := NewSession(79, pipe, DefaultResourceBudget())
	release := make(chan struct{})
	var mu sync.Mutex
	var got []time.Duration
	s.SetOutputGate(func(ctx context.Context, d time.Duration) {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	_ = s.Start()
	defer s.Close()

	posted := make(chan error, 1)
	go func() {
		posted <- s.Post(&OutputAudioFrame{Samples: make([]byte, 640), Encoding: EncodingPCM16LE, SampleRate: 16000, Channels: 1, Duration: 20 * time.Millisecond})
	}()
	select {
	case <-posted:
		t.Fatal("producer was not held by the gate")
	case <-time.After(30 * time.Millisecond):
	}
	if len(s.outSlots) != 0 {
		t.Fatal("slot taken before the gate opened")
	}
	close(release)
	if err := <-posted; err != nil {
		t.Fatal(err)
	}
	if err := s.Post(&OutputAudioFrame{Encoding: EncodingPCM16LE, SampleRate: 16000, Channels: 1, IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != 20*time.Millisecond {
		t.Fatalf("gate calls = %v, want one of 20ms", got)
	}
}
