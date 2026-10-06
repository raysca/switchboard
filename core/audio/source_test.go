package audio

import (
	"bytes"
	"context"
	"testing"
	"time"

	"govoice/core"
)

func TestGenericAudioSource_PacedStream(t *testing.T) {
	cfg := AudioSourceConfig{
		SampleRate:  16000,
		Channels:    1,
		ChunkDur:    10 * time.Millisecond,
		Disposition: core.OutputInterruptible,
		Direction:   core.Downstream,
	}
	src, err := NewGenericAudioSource(cfg)
	if err != nil {
		t.Fatalf("failed to create GenericAudioSource: %v", err)
	}

	emitter := &mockEmitter{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 10ms at 16kHz mono = 160 samples = 320 bytes per chunk
	// Create 3 chunks (960 bytes)
	audioData := make([]byte, 320*3)
	for i := range audioData {
		audioData[i] = byte(i % 256)
	}

	reader := bytes.NewReader(audioData)
	err = src.PlayStream(ctx, reader, emitter, "turn-1", 1)
	if err != nil {
		t.Fatalf("PlayStream failed: %v", err)
	}

	if len(emitter.frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(emitter.frames))
	}

	for _, f := range emitter.frames {
		out, ok := f.(*core.OutputAudioFrame)
		if !ok {
			t.Fatalf("expected OutputAudioFrame, got %T", f)
		}
		if len(out.Samples) != 320 {
			t.Errorf("expected 320 bytes sample chunk, got %d", len(out.Samples))
		}
	}
}

func TestGenericAudioSource_InterruptionStopsStream(t *testing.T) {
	cfg := AudioSourceConfig{
		SampleRate:  16000,
		Channels:    1,
		ChunkDur:    10 * time.Millisecond,
		Disposition: core.OutputInterruptible,
		Direction:   core.Downstream,
	}
	src, err := NewGenericAudioSource(cfg)
	if err != nil {
		t.Fatalf("failed to create GenericAudioSource: %v", err)
	}

	emitter := &mockEmitter{}
	ctx := context.Background()

	// 50 chunks of audio
	audioData := make([]byte, 320*50)
	reader := bytes.NewReader(audioData)

	done := make(chan error, 1)
	go func() {
		done <- src.PlayStream(ctx, reader, emitter, "turn-1", 1)
	}()

	// Wait until running
	time.Sleep(25 * time.Millisecond)
	if !src.IsRunning() {
		t.Fatalf("expected source to be running")
	}

	// Interruption frame received
	intr := &core.InterruptionFrame{Gen: 2}
	if err := src.ProcessFrame(ctx, intr, core.Downstream, emitter); err != nil {
		t.Fatalf("process frame failed: %v", err)
	}

	select {
	case err := <-done:
		// PlayStream should have exited due to stop context cancellation
		if err == nil {
			t.Fatalf("expected PlayStream to stop prematurely")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for stream to stop on interruption")
	}

	if src.IsRunning() {
		t.Errorf("expected source to no longer be running")
	}
}
