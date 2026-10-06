package audio

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

type mockEmitter struct {
	frames []core.Frame
}

func (m *mockEmitter) Push(f core.Frame) error {
	m.frames = append(m.frames, f)
	return nil
}

func (m *mockEmitter) PushDirection(f core.Frame, dir core.Direction) error {
	m.frames = append(m.frames, f)
	return nil
}

func TestBackgroundAndEarconProcessor_Lifecycle(t *testing.T) {
	proc, err := NewBackgroundAndEarconProcessor(BackgroundEarconConfig{
		SampleRate:   24000,
		Channels:     1,
		ChunkDur:     20 * time.Millisecond,
		InitialVol:   1.0,
		DuckOnSpeech: true,
		DuckedVol:    0.2,
	})
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	emitter := &mockEmitter{}
	ctx := context.Background()

	// 1. Send EarconControlFrame
	// 480 samples = 960 bytes (20ms at 24kHz mono)
	sampleAudio := make([]byte, 960*2)
	for i := range sampleAudio {
		sampleAudio[i] = byte(i % 128)
	}

	earcon := &EarconControlFrame{
		EarconID:    "chime",
		Volume:      1.0,
		PCM16LE:     sampleAudio,
		Disposition: core.OutputUninterruptible,
		Turn:        "turn-1",
		Gen:         1,
	}

	if err := proc.ProcessFrame(ctx, earcon, core.Downstream, emitter); err != nil {
		t.Fatalf("failed to process earcon: %v", err)
	}

	// Should emit 2 chunks of earcon
	if len(emitter.frames) != 2 {
		t.Fatalf("expected 2 earcon frames, got %d", len(emitter.frames))
	}

	for _, f := range emitter.frames {
		out, ok := f.(*core.OutputAudioFrame)
		if !ok {
			t.Fatalf("expected OutputAudioFrame, got %T", f)
		}
		if out.Disposition != core.OutputUninterruptible {
			t.Errorf("expected uninterruptible disposition, got %v", out.Disposition)
		}
	}
}

func TestBackgroundAndEarconProcessor_PacedBackgroundAndDucking(t *testing.T) {
	proc, err := NewBackgroundAndEarconProcessor(BackgroundEarconConfig{
		SampleRate:   24000,
		Channels:     1,
		ChunkDur:     20 * time.Millisecond,
		InitialVol:   1.0,
		DuckOnSpeech: true,
		DuckedVol:    0.2,
	})
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	emitter := &mockEmitter{}
	ctx := context.Background()

	// Create 100ms of audio (5 chunks of 960 bytes)
	audio := make([]byte, 960*5)
	for i := 0; i+1 < len(audio); i += 2 {
		val := int16(1000)
		audio[i] = byte(val)
		audio[i+1] = byte(val >> 8)
	}

	startBg := &BackgroundControlFrame{
		Action:  BackgroundStart,
		TrackID: "ambient_music",
		Volume:  1.0,
		Loop:    false,
		PCM16LE: audio,
	}

	if err := proc.ProcessFrame(ctx, startBg, core.Downstream, emitter); err != nil {
		t.Fatalf("failed to start background: %v", err)
	}

	// Pull 1st chunk
	c1, ok := proc.NextBackgroundChunk()
	if !ok || c1 == nil {
		t.Fatalf("expected 1st background chunk")
	}

	s1 := int16(uint16(c1.Samples[0]) | uint16(c1.Samples[1])<<8)

	// Assistant starts speaking -> duck background
	if err := proc.ProcessFrame(ctx, &core.TTSStartedFrame{}, core.Downstream, emitter); err != nil {
		t.Fatalf("failed to process TTSStarted: %v", err)
	}

	c2, ok := proc.NextBackgroundChunk()
	if !ok || c2 == nil {
		t.Fatalf("expected 2nd background chunk")
	}

	s2 := int16(uint16(c2.Samples[0]) | uint16(c2.Samples[1])<<8)

	// Sample in c2 should be ducked (lower than c1)
	if s2 >= s1 || s2 <= 0 {
		t.Errorf("expected ducked sample to be attenuated: s1=%d, s2=%d", s1, s2)
	}

	// Assistant stops speaking -> unduck
	if err := proc.ProcessFrame(ctx, &core.TTSStoppedFrame{}, core.Downstream, emitter); err != nil {
		t.Fatalf("failed to process TTSStopped: %v", err)
	}

	c3, ok := proc.NextBackgroundChunk()
	if !ok || c3 == nil {
		t.Fatalf("expected 3rd background chunk")
	}

	s3 := int16(uint16(c3.Samples[0]) | uint16(c3.Samples[1])<<8)
	if s3 <= s2 {
		t.Errorf("expected unducked sample to be louder: s2=%d, s3=%d", s2, s3)
	}
}

func TestBackgroundAndEarconProcessor_Interruption(t *testing.T) {
	proc, err := NewBackgroundAndEarconProcessor(BackgroundEarconConfig{
		SampleRate: 24000,
		Channels:   1,
		ChunkDur:   20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	emitter := &mockEmitter{}
	ctx := context.Background()

	// Interruption frame pass-through
	intr := &core.InterruptionFrame{Gen: 2}
	if err := proc.ProcessFrame(ctx, intr, core.Downstream, emitter); err != nil {
		t.Fatalf("failed to process interruption: %v", err)
	}

	if len(emitter.frames) != 1 || emitter.frames[0] != intr {
		t.Fatalf("expected InterruptionFrame forwarded downstream")
	}
}
