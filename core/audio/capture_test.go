package audio

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type memoryAudioSink struct {
	mu     sync.Mutex
	chunks []CapturedChunk
	closed bool
}

func (s *memoryAudioSink) WriteChunk(ctx context.Context, chunk CapturedChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sink closed")
	}
	s.chunks = append(s.chunks, chunk)
	return nil
}

func (s *memoryAudioSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *memoryAudioSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chunks)
}

func TestCaptureConsent_ConsentOffDiscardsAllAudio(t *testing.T) {
	sink := &memoryAudioSink{}
	cfg := CaptureConfig{
		Mode:       CaptureModeMixed,
		Consent:    false, // Consent NOT granted
		Sink:       sink,
		BufferSize: 16,
	}
	proc, err := NewCaptureProcessor(cfg)
	if err != nil {
		t.Fatalf("NewCaptureProcessor: %v", err)
	}

	ctx := context.Background()
	inAudio := &core.InputAudioFrame{
		Samples:    []byte{1, 2, 3, 4},
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
	}

	outAudio := &core.OutputAudioFrame{
		Samples:    []byte{5, 6, 7, 8},
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
	}

	// Deliver through processor
	if err := proc.ProcessFrame(ctx, inAudio, core.Downstream, &noopEmitter{}); err != nil {
		t.Fatalf("ProcessFrame: %v", err)
	}
	if err := proc.ProcessFrame(ctx, outAudio, core.Downstream, &noopEmitter{}); err != nil {
		t.Fatalf("ProcessFrame: %v", err)
	}

	_ = proc.Close()

	if sink.count() != 0 {
		t.Fatalf("expected 0 chunks captured when Consent is false, got %d", sink.count())
	}
	if proc.DroppedSamples() == 0 {
		t.Fatal("expected dropped samples count to be non-zero when consent is denied")
	}
}

func TestAudioCapture_ModesAndMetadata(t *testing.T) {
	tests := []struct {
		name        string
		mode        CaptureMode
		feedIn      bool
		feedOut     bool
		wantInCount int
		wantOutCnt  int
	}{
		{
			name:        "user only captures input, ignores output",
			mode:        CaptureModeUserOnly,
			feedIn:      true,
			feedOut:     true,
			wantInCount: 1,
			wantOutCnt:  0,
		},
		{
			name:        "assistant only captures output, ignores input",
			mode:        CaptureModeAssistantOnly,
			feedIn:      true,
			feedOut:     true,
			wantInCount: 0,
			wantOutCnt:  1,
		},
		{
			name:        "separate tracks captures both with source tags",
			mode:        CaptureModeSeparateTracks,
			feedIn:      true,
			feedOut:     true,
			wantInCount: 1,
			wantOutCnt:  1,
		},
		{
			name:        "mixed captures both",
			mode:        CaptureModeMixed,
			feedIn:      true,
			feedOut:     true,
			wantInCount: 1,
			wantOutCnt:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &memoryAudioSink{}
			proc, err := NewCaptureProcessor(CaptureConfig{
				Mode:       tc.mode,
				Consent:    true,
				Sink:       sink,
				BufferSize: 16,
			})
			if err != nil {
				t.Fatalf("NewCaptureProcessor: %v", err)
			}
			ctx := context.Background()

			now := time.Now()
			if tc.feedIn {
				inFrame := &core.InputAudioFrame{
					Samples:    []byte{10, 20, 30, 40},
					Encoding:   core.EncodingPCM16LE,
					SampleRate: 16000,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  now,
				}
				if err := proc.ProcessFrame(ctx, inFrame, core.Downstream, &noopEmitter{}); err != nil {
					t.Fatalf("feed in: %v", err)
				}
			}

			if tc.feedOut {
				outFrame := &core.OutputAudioFrame{
					Samples:    []byte{50, 60, 70, 80},
					Encoding:   core.EncodingPCM16LE,
					SampleRate: 16000,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  now.Add(20 * time.Millisecond),
					Turn:       "turn-1",
				}
				if err := proc.ProcessFrame(ctx, outFrame, core.Downstream, &noopEmitter{}); err != nil {
					t.Fatalf("feed out: %v", err)
				}
			}

			_ = proc.Close()

			inGot := 0
			outGot := 0
			for _, chunk := range sink.chunks {
				if chunk.Source == AudioSourceUser {
					inGot++
					if chunk.SampleRate != 16000 || chunk.Channels != 1 || chunk.Encoding != core.EncodingPCM16LE {
						t.Fatalf("user metadata mismatch: %+v", chunk)
					}
				} else if chunk.Source == AudioSourceAssistant {
					outGot++
					if chunk.SampleRate != 16000 || chunk.Channels != 1 || chunk.Turn != "turn-1" {
						t.Fatalf("assistant metadata mismatch: %+v", chunk)
					}
				}
			}

			if inGot != tc.wantInCount || outGot != tc.wantOutCnt {
				t.Fatalf("got in=%d out=%d, want in=%d out=%d", inGot, outGot, tc.wantInCount, tc.wantOutCnt)
			}
		})
	}
}

func TestAudioCapture_BufferOverflowBehavior(t *testing.T) {
	// Blocking or slow sink
	blockCh := make(chan struct{})
	slowSink := &blockingSink{blockCh: blockCh}

	// Tiny buffer of 2 chunks
	proc, err := NewCaptureProcessor(CaptureConfig{
		Mode:       CaptureModeUserOnly,
		Consent:    true,
		Sink:       slowSink,
		BufferSize: 2,
	})
	if err != nil {
		t.Fatalf("NewCaptureProcessor: %v", err)
	}

	ctx := context.Background()

	// Push 10 frames rapidly without blocking the caller
	for i := 0; i < 10; i++ {
		frame := &core.InputAudioFrame{
			Samples:    []byte{1, 2, 3, 4},
			Encoding:   core.EncodingPCM16LE,
			SampleRate: 16000,
			Channels:   1,
		}
		if err := proc.ProcessFrame(ctx, frame, core.Downstream, &noopEmitter{}); err != nil {
			t.Fatalf("ProcessFrame failed on overflow: %v", err)
		}
	}

	// Pipeline processor MUST NOT block on full sink buffer
	// Unblock sink
	close(blockCh)
	_ = proc.Close()

	if proc.DroppedSamples() == 0 {
		t.Fatal("expected dropped samples on buffer overflow")
	}
}

func TestAudioCapture_CancellationTeardown(t *testing.T) {
	sink := &memoryAudioSink{}
	proc, err := NewCaptureProcessor(CaptureConfig{
		Mode:       CaptureModeMixed,
		Consent:    true,
		Sink:       sink,
		BufferSize: 16,
	})
	if err != nil {
		t.Fatalf("NewCaptureProcessor: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	frame := &core.InputAudioFrame{
		Samples:    []byte{1, 2, 3, 4},
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
	}

	// Processing with cancelled context returns cleanly without deadlock or hanging
	err = proc.ProcessFrame(cancelledCtx, frame, core.Downstream, &noopEmitter{})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", err)
	}

	// Orderly close
	if err := proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type noopEmitter struct{}

func (n *noopEmitter) Push(f core.Frame) error                       { return nil }
func (n *noopEmitter) PushDirection(f core.Frame, d core.Direction) error { return nil }

type blockingSink struct {
	blockCh chan struct{}
	mu      sync.Mutex
	count   int
}

func (b *blockingSink) WriteChunk(ctx context.Context, chunk CapturedChunk) error {
	select {
	case <-b.blockCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	b.count++
	b.mu.Unlock()
	return nil
}

func (b *blockingSink) Close() error {
	return nil
}
