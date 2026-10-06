package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

var (
	ErrAudioSourceClosed = errors.New("audio: audio source is closed")
	ErrAudioSourceBusy   = errors.New("audio: audio source is already running")
	ErrInvalidChunkSize  = errors.New("audio: chunk size must be positive")
	ErrInvalidSampleRate = errors.New("audio: sample rate must be positive")
	ErrInvalidChannelNum = errors.New("audio: channel count must be positive")
)

// AudioSourceConfig configures a GenericAudioSource.
type AudioSourceConfig struct {
	SampleRate  int                    // e.g. 16000 or 24000
	Channels    int                    // e.g. 1
	ChunkDur    time.Duration          // e.g. 20ms
	Disposition core.OutputDisposition // Interruptible or Uninterruptible
	Direction   core.Direction         // Downstream (towards client) or Upstream (e.g. injected input)
}

// GenericAudioSource ingests audio from an arbitrary io.Reader and injects paced frames into the pipeline.
type GenericAudioSource struct {
	cfg        AudioSourceConfig
	mu         sync.Mutex
	controller core.SessionController
	index      int
	running    atomic.Bool
	closed     atomic.Bool
	cancel     context.CancelFunc
	chunkBytes int
}

// NewGenericAudioSource creates a new generic audio source processor.
func NewGenericAudioSource(cfg AudioSourceConfig) (*GenericAudioSource, error) {
	if cfg.SampleRate <= 0 {
		return nil, ErrInvalidSampleRate
	}
	if cfg.Channels <= 0 {
		return nil, ErrInvalidChannelNum
	}
	if cfg.ChunkDur <= 0 {
		cfg.ChunkDur = 20 * time.Millisecond
	}

	samplesPerChunk := int(float64(cfg.SampleRate) * cfg.ChunkDur.Seconds())
	if samplesPerChunk <= 0 {
		return nil, ErrInvalidChunkSize
	}
	chunkBytes := samplesPerChunk * cfg.Channels * 2 // 16-bit PCM

	return &GenericAudioSource{
		cfg:        cfg,
		chunkBytes: chunkBytes,
	}, nil
}

// Attach binds the processor to the pipeline and session supervisor.
func (s *GenericAudioSource) Attach(sc core.SessionController, index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controller = sc
	s.index = index
	return nil
}

// Close stops streaming and cleans up resources.
func (s *GenericAudioSource) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	return nil
}

// IsRunning indicates if the source is actively pumping audio.
func (s *GenericAudioSource) IsRunning() bool {
	return s.running.Load()
}

// ProcessFrame implements core.Processor; passes non-target frames through.
func (s *GenericAudioSource) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if s.closed.Load() {
		return ErrAudioSourceClosed
	}

	// Interruption cancels active interruptible playback if going downstream
	if _, ok := f.(*core.InterruptionFrame); ok && s.cfg.Disposition == core.OutputInterruptible {
		s.Stop()
	}

	return e.Push(f)
}

// Stop cancels any ongoing stream playback.
func (s *GenericAudioSource) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running.Store(false)
}

// PlayStream reads from an io.Reader, chunks it, and pumps it through the pipeline via the SessionController or Emitter.
func (s *GenericAudioSource) PlayStream(ctx context.Context, r io.Reader, e core.Emitter, turn string, gen uint64) error {
	if s.closed.Load() {
		return ErrAudioSourceClosed
	}

	if !s.running.CompareAndSwap(false, true) {
		return ErrAudioSourceBusy
	}
	defer s.running.Store(false)

	streamCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer cancel()

	buf := make([]byte, s.chunkBytes)
	ticker := time.NewTicker(s.cfg.ChunkDur)
	defer ticker.Stop()

	for {
		select {
		case <-streamCtx.Done():
			return streamCtx.Err()
		default:
		}

		n, err := io.ReadFull(r, buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			var frame core.Frame
			if s.cfg.Direction == core.Upstream {
				frame = &core.InputAudioFrame{
					Samples:    chunk,
					Encoding:   core.EncodingPCM16LE,
					SampleRate: s.cfg.SampleRate,
					Channels:   s.cfg.Channels,
					Duration:   s.cfg.ChunkDur,
					Timestamp:  time.Now(),
					Gen:        gen,
				}
			} else {
				frame = &core.OutputAudioFrame{
					Samples:     chunk,
					Encoding:    core.EncodingPCM16LE,
					SampleRate:  s.cfg.SampleRate,
					Channels:    s.cfg.Channels,
					Duration:    s.cfg.ChunkDur,
					Timestamp:   time.Now(),
					Turn:        turn,
					Gen:         gen,
					Disposition: s.cfg.Disposition,
				}
			}

			if emitErr := e.PushDirection(frame, s.cfg.Direction); emitErr != nil {
				return emitErr
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("audio: read error: %w", err)
		}

		select {
		case <-streamCtx.Done():
			return streamCtx.Err()
		case <-ticker.C:
		}
	}
}
