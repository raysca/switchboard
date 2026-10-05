package google

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

func TestTTS_RejectsWAVContainer(t *testing.T) {
	// Pinned requirement: "TTS bytes begin with PCM samples, not a WAV container"
	// Create mock that returns WAV bytes beginning with "RIFF"
	wavBytes := []byte("RIFF\x24\x08\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00")
	mockClient := &mockTTSClient{
		synthesizeFn: func(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
			return wavBytes, nil
		},
	}

	cfg := DefaultTTSConfig()
	cfg.Client = mockClient

	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}

	pipe, _ := core.NewPipeline(proc)
	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Attach(session, 0)

	var emittedFrames []core.Frame
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		emittedFrames = append(emittedFrames, f)
	}

	_ = session.Start()
	defer session.Close()

	// Push valid SynthesisUnitFrame
	su := &core.SynthesisUnitFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  1,
		Text:    "Hello world.",
		IsFinal: true,
	}
	_ = session.Post(su)

	// Wait for processing
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify that NO OutputAudioFrame was emitted, and an ErrorFrame was emitted instead
	hasAudio := false
	hasError := false
	for _, f := range emittedFrames {
		if f.Type() == core.FrameOutputAudio {
			hasAudio = true
		}
		if f.Type() == core.FrameError {
			hasError = true
		}
	}

	if hasAudio {
		t.Fatal("audio frames were emitted even though TTS returned WAV container!")
	}
	if !hasError {
		t.Fatal("expected ErrorFrame when TTS returned WAV container, got none")
	}
}

func TestTTS_UnitSizeBounds(t *testing.T) {
	// Pinned requirement: max 250 characters
	cfg := DefaultTTSConfig()
	cfg.MaxSynthesisUnitChars = 250
	mockClient := &mockTTSClient{}
	cfg.Client = mockClient

	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}

	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()

	// 1. Text with 251 characters must be rejected immediately
	tooLongText := strings.Repeat("a", 251)
	err = session.Post(&core.SynthesisUnitFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  1,
		Text:    tooLongText,
		IsFinal: true,
	})
	// Session run loop fails with ErrUnitTooLong
	time.Sleep(50 * time.Millisecond)
	if session.Err() == nil || !errors.Is(session.Err(), ErrUnitTooLong) {
		t.Fatalf("expected ErrUnitTooLong, got %v", session.Err())
	}
}

func TestTTS_HeaderlessPCM20msChunking(t *testing.T) {
	// 24000 Hz, 16-bit mono. 20ms chunk = 24000 * 2 * 0.02 = 960 bytes.
	// Provide 2400 bytes = 2 full chunks (960*2 = 1920) + 1 partial chunk (480 bytes).
	synthBytes := make([]byte, 2400)
	for i := range synthBytes {
		synthBytes[i] = byte(i % 128)
	}

	mockClient := &mockTTSClient{
		synthesizeFn: func(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
			return synthBytes, nil
		},
	}

	cfg := DefaultTTSConfig()
	cfg.SampleRate = 24000
	cfg.ChunkDuration = 20 * time.Millisecond
	cfg.Client = mockClient

	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var frames []core.Frame
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		frames = append(frames, f)
	}

	_ = session.Start()
	defer session.Close()

	su := &core.SynthesisUnitFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  10,
		Text:    "Valid synthesis unit.",
		IsFinal: true,
	}
	_ = session.Post(su)

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify order: SynthesisUnitFrame -> TTSStartedFrame -> OutputAudioFrames... -> TTSStoppedFrame
	var started *core.TTSStartedFrame
	var stopped *core.TTSStoppedFrame
	var audioFrames []*core.OutputAudioFrame

	for _, f := range frames {
		switch frame := f.(type) {
		case *core.TTSStartedFrame:
			started = frame
		case *core.TTSStoppedFrame:
			stopped = frame
		case *core.OutputAudioFrame:
			audioFrames = append(audioFrames, frame)
		}
	}

	if started == nil {
		t.Fatal("expected TTSStartedFrame, got none")
	}
	if started.UnitID != 10 {
		t.Fatalf("expected started UnitID 10, got %d", started.UnitID)
	}

	if len(audioFrames) != 3 {
		t.Fatalf("expected 3 audio chunks, got %d", len(audioFrames))
	}

	// Chunk 1: 960 bytes, not final
	if len(audioFrames[0].Samples) != 960 || audioFrames[0].IsFinal {
		t.Fatalf("chunk 0 invalid: len=%d isFinal=%v", len(audioFrames[0].Samples), audioFrames[0].IsFinal)
	}
	if audioFrames[0].SampleRate != 24000 {
		t.Fatalf("expected 24000 sample rate, got %d", audioFrames[0].SampleRate)
	}

	// Chunk 2: 960 bytes, not final
	if len(audioFrames[1].Samples) != 960 || audioFrames[1].IsFinal {
		t.Fatalf("chunk 1 invalid: len=%d isFinal=%v", len(audioFrames[1].Samples), audioFrames[1].IsFinal)
	}

	// Chunk 3: 480 bytes, final
	if len(audioFrames[2].Samples) != 480 || !audioFrames[2].IsFinal {
		t.Fatalf("chunk 2 invalid: len=%d isFinal=%v", len(audioFrames[2].Samples), audioFrames[2].IsFinal)
	}
	if audioFrames[0].TurnFinal || audioFrames[1].TurnFinal || !audioFrames[2].TurnFinal {
		t.Fatalf("whole-turn final marker was not limited to the last chunk: %v %v %v", audioFrames[0].TurnFinal, audioFrames[1].TurnFinal, audioFrames[2].TurnFinal)
	}

	// Reassembled samples must match exactly
	var reassembled []byte
	for _, af := range audioFrames {
		reassembled = append(reassembled, af.Samples...)
	}
	if !bytes.Equal(reassembled, synthBytes) {
		t.Fatal("reassembled audio does not match synthesized bytes!")
	}

	if stopped == nil {
		t.Fatal("expected TTSStoppedFrame, got none")
	}
	if stopped.UnitID != 10 {
		t.Fatalf("expected stopped UnitID 10, got %d", stopped.UnitID)
	}
}

func TestTTS_CancellationAndLateOutputFencing(t *testing.T) {
	// Synthesizes slowly, so an interruption cancels mid-flight
	synthStarted := make(chan struct{})
	mockClient := &mockTTSClient{
		synthesizeFn: func(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
			close(synthStarted)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
				return make([]byte, 4800), nil
			}
		},
	}

	cfg := DefaultTTSConfig()
	cfg.Client = mockClient
	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var audioCount int
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		if f.Type() == core.FrameOutputAudio {
			audioCount++
		}
	}

	_ = session.Start()
	defer session.Close()

	// Push synthesis unit for Gen 1
	_ = session.Post(&core.SynthesisUnitFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  1,
		Text:    "Interrupted text.",
		IsFinal: true,
	})

	// Wait for synthesis to start
	<-synthStarted

	// User barges in: push InterruptionFrame and advance generation
	_ = session.Post(&core.InterruptionFrame{Turn: "turn-1", Gen: 1})
	session.IncrementGeneration() // Gen = 2

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify no audio frames from old generation were delivered
	if audioCount != 0 {
		t.Fatalf("expected 0 audio frames delivered after interruption, got %d", audioCount)
	}
}

type directionEmitter struct {
	frame core.Frame
	dir   core.Direction
}

func (d *directionEmitter) Push(f core.Frame) error {
	return d.PushDirection(f, core.Downstream)
}

func (d *directionEmitter) PushDirection(f core.Frame, dir core.Direction) error {
	d.frame = f
	d.dir = dir
	return nil
}

func TestTTSForwardsUpstreamFlushAck(t *testing.T) {
	proc, err := NewTTSProcessor(DefaultTTSConfig())
	if err != nil {
		t.Fatal(err)
	}
	em := &directionEmitter{}
	frame := &core.FlushProcessedFrame{Gen: 1}
	if err := proc.ProcessFrame(context.Background(), frame, core.Upstream, em); err != nil {
		t.Fatal(err)
	}
	if em.dir != core.Upstream {
		t.Fatalf("flush ack direction = %v, want upstream", em.dir)
	}
	if em.frame != frame {
		t.Fatal("flush ack frame was replaced")
	}
}
