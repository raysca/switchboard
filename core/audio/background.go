package audio

import (
	"context"
	"errors"
	"sync"
	"time"

	"govoice/core"
)

var (
	ErrInvalidAudioChunk = errors.New("audio: invalid audio chunk size or duration")
	ErrInvalidVolume     = errors.New("audio: volume must be between 0.0 and 1.0")
	ErrProcessorClosed   = errors.New("audio: processor is closed")
)

// BackgroundAction indicates the command for background audio playback.
type BackgroundAction string

const (
	BackgroundStart  BackgroundAction = "start"
	BackgroundStop   BackgroundAction = "stop"
	BackgroundPause  BackgroundAction = "pause"
	BackgroundResume BackgroundAction = "resume"
)

// BackgroundControlFrame commands background audio playback behavior.
type BackgroundControlFrame struct {
	Action  BackgroundAction `json:"action"`
	TrackID string           `json:"track_id"`
	Volume  float64          `json:"volume"`
	Loop    bool             `json:"loop"`
	PCM16LE []byte           `json:"pcm16le,omitempty"`
	Turn    string           `json:"turn,omitempty"`
	Gen     uint64           `json:"gen,omitempty"`
}

func (f *BackgroundControlFrame) Type() core.FrameType { return core.FrameBackgroundAudioControl }
func (f *BackgroundControlFrame) Generation() uint64   { return f.Gen }

// EarconControlFrame requests injection of an earcon cue.
type EarconControlFrame struct {
	EarconID    string                 `json:"earcon_id"`
	Volume      float64                `json:"volume"`
	PCM16LE     []byte                 `json:"pcm16le"`
	Disposition core.OutputDisposition `json:"disposition"` // OutputInterruptible or OutputUninterruptible
	Turn        string                 `json:"turn,omitempty"`
	Gen         uint64                 `json:"gen,omitempty"`
}

func (f *EarconControlFrame) Type() core.FrameType { return core.FrameEarconControl }
func (f *EarconControlFrame) Generation() uint64   { return f.Gen }

// BackgroundEarconConfig configures the BackgroundAndEarconProcessor.
type BackgroundEarconConfig struct {
	SampleRate   int           // Default: 24000
	Channels     int           // Default: 1
	ChunkDur     time.Duration // Paced chunk duration (e.g. 20ms)
	InitialVol   float64       // Default volume (0.0 - 1.0)
	DuckOnSpeech bool          // Automatically duck background audio when assistant speaks
	DuckedVol    float64       // Volume when ducked (default 0.2)
}

// BackgroundAndEarconProcessor coordinates background audio streaming and earcon audio playback.
type BackgroundAndEarconProcessor struct {
	cfg        BackgroundEarconConfig
	mu         sync.Mutex
	closed     bool
	controller core.SessionController
	index      int

	// Background track state
	bgTrackID string
	bgPlaying bool
	bgLoop    bool
	bgVolume  float64
	bgAudio   []byte
	bgOffset  int
	bgDucked  bool

	// Active earcon state
	earconAudio       []byte
	earconOffset      int
	earconDisposition core.OutputDisposition
	earconGen         uint64

	chunkBytes int
}

// NewBackgroundAndEarconProcessor constructs a new processor for background audio and earcons.
func NewBackgroundAndEarconProcessor(cfg BackgroundEarconConfig) (*BackgroundAndEarconProcessor, error) {
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.Channels <= 0 {
		cfg.Channels = 1
	}
	if cfg.ChunkDur <= 0 {
		cfg.ChunkDur = 20 * time.Millisecond
	}
	if cfg.InitialVol < 0.0 || cfg.InitialVol > 1.0 {
		cfg.InitialVol = 1.0
	}
	if cfg.DuckedVol < 0.0 || cfg.DuckedVol > 1.0 {
		cfg.DuckedVol = 0.2
	}

	// 2 bytes per sample (int16 PCM) * channels * (sampleRate * chunkDur)
	samplesPerChunk := int(float64(cfg.SampleRate) * cfg.ChunkDur.Seconds())
	chunkBytes := samplesPerChunk * cfg.Channels * 2

	return &BackgroundAndEarconProcessor{
		cfg:        cfg,
		bgVolume:   cfg.InitialVol,
		chunkBytes: chunkBytes,
	}, nil
}

// Attach registers the session controller and pipeline index.
func (p *BackgroundAndEarconProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.controller = s
	p.index = index
	return nil
}

// Close terminates processor resources and stops playback.
func (p *BackgroundAndEarconProcessor) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.bgPlaying = false
	p.bgAudio = nil
	p.earconAudio = nil
	return nil
}

// ProcessFrame inspects and processes frames flowing through the pipeline.
func (p *BackgroundAndEarconProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrProcessorClosed
	}

	switch frame := f.(type) {
	case *BackgroundControlFrame:
		return p.handleBackgroundControl(frame, e)

	case *EarconControlFrame:
		return p.handleEarconControl(frame, e)

	case *core.InterruptionFrame:
		// Interruption: cancel active interruptible earcons and ducked state
		if p.earconDisposition == core.OutputInterruptible {
			p.earconAudio = nil
			p.earconOffset = 0
		}
		p.bgDucked = false
		return e.Push(f)

	case *core.TTSStartedFrame:
		if p.cfg.DuckOnSpeech {
			p.bgDucked = true
		}
		return e.Push(f)

	case *core.TTSStoppedFrame:
		p.bgDucked = false
		return e.Push(f)

	default:
		return e.Push(f)
	}
}

func (p *BackgroundAndEarconProcessor) handleBackgroundControl(frame *BackgroundControlFrame, e core.Emitter) error {
	switch frame.Action {
	case BackgroundStart:
		if frame.Volume < 0.0 || frame.Volume > 1.0 {
			frame.Volume = p.cfg.InitialVol
		}
		p.bgTrackID = frame.TrackID
		p.bgVolume = frame.Volume
		p.bgLoop = frame.Loop
		p.bgAudio = frame.PCM16LE
		p.bgOffset = 0
		p.bgPlaying = true

	case BackgroundStop:
		p.bgPlaying = false
		p.bgAudio = nil
		p.bgOffset = 0

	case BackgroundPause:
		p.bgPlaying = false

	case BackgroundResume:
		if len(p.bgAudio) > 0 {
			p.bgPlaying = true
		}
	}
	return nil
}

func (p *BackgroundAndEarconProcessor) handleEarconControl(frame *EarconControlFrame, e core.Emitter) error {
	p.earconAudio = frame.PCM16LE
	p.earconOffset = 0
	p.earconDisposition = frame.Disposition
	p.earconGen = frame.Gen

	// Emit initial earcon chunks or entire earcon if small
	for p.earconOffset < len(p.earconAudio) {
		end := p.earconOffset + p.chunkBytes
		if end > len(p.earconAudio) {
			end = len(p.earconAudio)
		}
		chunk := p.earconAudio[p.earconOffset:end]
		p.earconOffset = end

		// Clone chunk to prevent mutating original data
		chunkCopy := make([]byte, len(chunk))
		copy(chunkCopy, chunk)

		out := &core.OutputAudioFrame{
			Samples:     chunkCopy,
			Encoding:    core.EncodingPCM16LE,
			SampleRate:  p.cfg.SampleRate,
			Channels:    p.cfg.Channels,
			Duration:    p.cfg.ChunkDur,
			Timestamp:   time.Now(),
			Gen:         frame.Gen,
			Turn:        frame.Turn,
			Disposition: frame.Disposition,
		}
		if err := e.Push(out); err != nil {
			return err
		}
	}

	p.earconAudio = nil
	return nil
}

// NextBackgroundChunk extracts the next paced PCM16 background chunk scaled according to active volume and ducking.
func (p *BackgroundAndEarconProcessor) NextBackgroundChunk() (*core.OutputAudioFrame, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.bgPlaying || len(p.bgAudio) == 0 || p.closed {
		return nil, false
	}

	end := p.bgOffset + p.chunkBytes
	if end > len(p.bgAudio) {
		if !p.bgLoop {
			// Finished track
			chunk := p.bgAudio[p.bgOffset:]
			p.bgPlaying = false
			p.bgAudio = nil
			p.bgOffset = 0
			if len(chunk) == 0 {
				return nil, false
			}
			return p.buildBackgroundFrame(chunk), true
		}
		// Looping: wrap around
		chunk := make([]byte, p.chunkBytes)
		firstPart := p.bgAudio[p.bgOffset:]
		copy(chunk, firstPart)
		rem := p.chunkBytes - len(firstPart)
		p.bgOffset = 0
		if rem > len(p.bgAudio) {
			rem = len(p.bgAudio)
		}
		copy(chunk[len(firstPart):], p.bgAudio[:rem])
		p.bgOffset = rem
		return p.buildBackgroundFrame(chunk), true
	}

	chunk := p.bgAudio[p.bgOffset:end]
	p.bgOffset = end
	return p.buildBackgroundFrame(chunk), true
}

func (p *BackgroundAndEarconProcessor) buildBackgroundFrame(chunk []byte) *core.OutputAudioFrame {
	vol := p.bgVolume
	if p.bgDucked {
		vol = p.cfg.DuckedVol
	}

	scaled := make([]byte, len(chunk))
	// Apply volume scaling to 16-bit PCM samples
	for i := 0; i+1 < len(chunk); i += 2 {
		val := int16(uint16(chunk[i]) | uint16(chunk[i+1])<<8)
		scaledVal := int16(float64(val) * vol)
		scaled[i] = byte(scaledVal)
		scaled[i+1] = byte(scaledVal >> 8)
	}

	return &core.OutputAudioFrame{
		Samples:     scaled,
		Encoding:    core.EncodingPCM16LE,
		SampleRate:  p.cfg.SampleRate,
		Channels:    p.cfg.Channels,
		Duration:    p.cfg.ChunkDur,
		Timestamp:   time.Now(),
		Disposition: core.OutputInterruptible,
	}
}
