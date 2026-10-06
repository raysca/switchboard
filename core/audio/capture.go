package audio

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

// CaptureMode specifies which media stream(s) are captured.
type CaptureMode string

const (
	CaptureModeUserOnly       CaptureMode = "user_only"
	CaptureModeAssistantOnly  CaptureMode = "assistant_only"
	CaptureModeMixed          CaptureMode = "mixed"
	CaptureModeSeparateTracks CaptureMode = "separate_tracks"
)

// AudioSource identifies the origin of a captured chunk.
type AudioSource string

const (
	AudioSourceUser      AudioSource = "user"
	AudioSourceAssistant AudioSource = "assistant"
)

// CapturedChunk represents an immutable slice of recorded audio with format metadata.
type CapturedChunk struct {
	Source     AudioSource
	Samples    []byte
	Encoding   core.AudioEncoding
	SampleRate int
	Channels   int
	Duration   time.Duration
	Timestamp  time.Time
	Turn       string
	Seq        uint64
	Gen        uint64
}

// AudioSink is the application-provided receiver for captured audio frames.
type AudioSink interface {
	WriteChunk(ctx context.Context, chunk CapturedChunk) error
	Close() error
}

// CaptureConfig configures the opt-in audio capture processor.
type CaptureConfig struct {
	Mode       CaptureMode
	Consent    bool      // Must be explicitly true; if false, all audio is dropped.
	Sink       AudioSink // Application-provided storage/streaming sink.
	BufferSize int       // Bounded asynchronous buffer queue size.
}

func (c *CaptureConfig) validate() error {
	if c.Sink == nil {
		return errors.New("audio: capture sink is required")
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 64
	}
	switch c.Mode {
	case CaptureModeUserOnly, CaptureModeAssistantOnly, CaptureModeMixed, CaptureModeSeparateTracks:
		return nil
	default:
		return fmt.Errorf("audio: unsupported capture mode %q", c.Mode)
	}
}

// CaptureProcessor is a non-blocking, privacy-respecting audio capture pipeline processor.
type CaptureProcessor struct {
	cfg            CaptureConfig
	queue          chan CapturedChunk
	ctx            context.Context
	cancel         context.CancelFunc
	workerWg       sync.WaitGroup
	droppedSamples atomic.Uint64
	closed         atomic.Bool
}

// NewCaptureProcessor creates and initializes an audio capture processor.
func NewCaptureProcessor(cfg CaptureConfig) (*CaptureProcessor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := &CaptureProcessor{
		cfg:    cfg,
		queue:  make(chan CapturedChunk, cfg.BufferSize),
		ctx:    ctx,
		cancel: cancel,
	}

	p.workerWg.Add(1)
	go p.workerLoop()

	return p, nil
}

func (p *CaptureProcessor) workerLoop() {
	defer p.workerWg.Done()

	for {
		select {
		case <-p.ctx.Done():
			// Drain remaining buffered chunks with a short deadline on shutdown
			for {
				select {
				case chunk, ok := <-p.queue:
					if !ok {
						return
					}
					_ = p.cfg.Sink.WriteChunk(context.Background(), chunk)
				default:
					return
				}
			}
		case chunk, ok := <-p.queue:
			if !ok {
				return
			}
			_ = p.cfg.Sink.WriteChunk(p.ctx, chunk)
		}
	}
}

// ProcessFrame inspects incoming/outgoing audio frames and forwards them downstream.
func (p *CaptureProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Always forward frame downstream or upstream first so pipeline flow is not blocked
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}

	// If consent was not explicitly granted, drop all capture work immediately
	if !p.cfg.Consent {
		if in, ok := f.(*core.InputAudioFrame); ok {
			p.droppedSamples.Add(uint64(len(in.Samples) / 2))
		} else if out, ok := f.(*core.OutputAudioFrame); ok {
			p.droppedSamples.Add(uint64(len(out.Samples) / 2))
		}
		return nil
	}

	switch frame := f.(type) {
	case *core.InputAudioFrame:
		if p.cfg.Mode == CaptureModeUserOnly || p.cfg.Mode == CaptureModeMixed || p.cfg.Mode == CaptureModeSeparateTracks {
			chunk := CapturedChunk{
				Source:     AudioSourceUser,
				Samples:    append([]byte(nil), frame.Samples...),
				Encoding:   frame.Encoding,
				SampleRate: frame.SampleRate,
				Channels:   frame.Channels,
				Duration:   frame.Duration,
				Timestamp:  frame.Timestamp,
				Seq:        frame.Seq,
				Gen:        frame.Gen,
			}
			p.enqueue(chunk)
		}

	case *core.OutputAudioFrame:
		if p.cfg.Mode == CaptureModeAssistantOnly || p.cfg.Mode == CaptureModeMixed || p.cfg.Mode == CaptureModeSeparateTracks {
			chunk := CapturedChunk{
				Source:     AudioSourceAssistant,
				Samples:    append([]byte(nil), frame.Samples...),
				Encoding:   frame.Encoding,
				SampleRate: frame.SampleRate,
				Channels:   frame.Channels,
				Duration:   frame.Duration,
				Timestamp:  frame.Timestamp,
				Turn:       frame.Turn,
				Seq:        frame.Seq,
				Gen:        frame.Gen,
			}
			p.enqueue(chunk)
		}
	}

	return nil
}

func (p *CaptureProcessor) enqueue(chunk CapturedChunk) {
	if p.closed.Load() {
		p.droppedSamples.Add(uint64(len(chunk.Samples) / 2))
		return
	}

	select {
	case p.queue <- chunk:
	default:
		// Bounded buffer overflow: shed chunk without blocking pipeline
		p.droppedSamples.Add(uint64(len(chunk.Samples) / 2))
	}
}

// DroppedSamples returns the count of audio samples discarded due to missing consent or buffer overflow.
func (p *CaptureProcessor) DroppedSamples() uint64 {
	return p.droppedSamples.Load()
}

// Close gracefully closes the capture processor and releases resources.
func (p *CaptureProcessor) Close() error {
	if p.closed.Swap(true) {
		return nil
	}

	p.cancel()
	close(p.queue)
	p.workerWg.Wait()

	return p.cfg.Sink.Close()
}
