package google

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"govoice/core"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
	"google.golang.org/api/option"
)

// TTSConfig configures Google Cloud Text-to-Speech V1.
type TTSConfig struct {
	Voice                 string        // Pinned: "en-US-Journey-F"
	LanguageCode          string        // Default: "en-US"
	SampleRate            int           // Pinned: 24000 Hz
	MaxSynthesisUnitChars int           // Pinned: 250
	ChunkDuration         time.Duration // Default: 20ms
	CredentialsFile       string
	APIKey                string
	Client                TTSClient
	RetryPolicy           RetryPolicy
	CircuitBreaker        *CircuitBreaker
}

// DefaultTTSConfig returns configuration matching v1-lock.json.
func DefaultTTSConfig() TTSConfig {
	return TTSConfig{
		Voice:                 "en-US-Journey-F",
		LanguageCode:          "en-US",
		SampleRate:            24000,
		MaxSynthesisUnitChars: 250,
		ChunkDuration:         20 * time.Millisecond,
		RetryPolicy:           DefaultRetryPolicy(),
		CircuitBreaker:        NewCircuitBreaker(DefaultCircuitConfig()),
	}
}

// TTSClient abstracts synchronous speech synthesis.
type TTSClient interface {
	SynthesizeSpeech(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error)
}

// RealGoogleTTSClient wraps cloud.google.com/go/texttospeech/apiv1.
type RealGoogleTTSClient struct {
	client *texttospeech.Client
}

func NewRealGoogleTTSClient(ctx context.Context, cfg TTSConfig) (*RealGoogleTTSClient, error) {
	var opts []option.ClientOption
	if cfg.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	} else if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}

	c, err := texttospeech.NewClient(ctx, opts...)
	if err != nil {
		return nil, MapError("tts", err)
	}
	return &RealGoogleTTSClient{client: c}, nil
}

func (c *RealGoogleTTSClient) SynthesizeSpeech(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
	req := &texttospeechpb.SynthesizeSpeechRequest{
		Input: &texttospeechpb.SynthesisInput{
			InputSource: &texttospeechpb.SynthesisInput_Text{Text: text},
		},
		Voice: &texttospeechpb.VoiceSelectionParams{
			LanguageCode: "en-US",
			Name:         voice,
		},
		AudioConfig: &texttospeechpb.AudioConfig{
			AudioEncoding:   texttospeechpb.AudioEncoding_PCM,
			SampleRateHertz: int32(sampleRate),
		},
	}

	resp, err := c.client.SynthesizeSpeech(ctx, req)
	if err != nil {
		return nil, MapError("tts", err)
	}
	return resp.AudioContent, nil
}

func (c *RealGoogleTTSClient) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// TTSProcessor implements core.Processor for Google Cloud Text-to-Speech.
type TTSProcessor struct {
	cfg     TTSConfig
	session core.SessionController
	index   int

	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeGen    uint64
	closed       bool
}

// NewTTSProcessor creates a new TTSProcessor.
func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	if cfg.Voice == "" {
		cfg.Voice = "en-US-Journey-F"
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.MaxSynthesisUnitChars <= 0 {
		cfg.MaxSynthesisUnitChars = 250
	}
	if cfg.ChunkDuration <= 0 {
		cfg.ChunkDuration = 20 * time.Millisecond
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = NewCircuitBreaker(DefaultCircuitConfig())
	}
	return &TTSProcessor{
		cfg: cfg,
	}, nil
}

// Attach registers session controller and index.
func (p *TTSProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = s
	p.index = index
	return nil
}

// Close gracefully terminates active operations.
func (p *TTSProcessor) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.activeCancel != nil {
		p.activeCancel()
		p.activeCancel = nil
	}
	return nil
}

// ProcessFrame handles pipeline frames.
func (p *TTSProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.SynthesisUnitFrame:
		return p.handleSynthesisUnit(ctx, frame, e)

	case *core.InterruptionFrame:
		p.mu.Lock()
		if p.activeCancel != nil {
			p.activeCancel()
			p.activeCancel = nil
		}
		p.mu.Unlock()
		return e.Push(f)

	case *core.CancelFrame:
		p.mu.Lock()
		if p.activeCancel != nil {
			p.activeCancel()
			p.activeCancel = nil
		}
		p.mu.Unlock()
		return e.Push(f)

	default:
		return e.Push(f)
	}
}

func (p *TTSProcessor) handleSynthesisUnit(ctx context.Context, su *core.SynthesisUnitFrame, e core.Emitter) error {
	// Validate character length <= 250 chars
	if len([]rune(su.Text)) > p.cfg.MaxSynthesisUnitChars {
		return fmt.Errorf("%w: length %d exceeds limit %d", ErrUnitTooLong, len([]rune(su.Text)), p.cfg.MaxSynthesisUnitChars)
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}

	gen := su.Gen
	if p.session != nil {
		currentGen := p.session.CurrentGeneration()
		if gen != 0 && gen < currentGen {
			p.mu.Unlock()
			return nil // Drop stale unit
		}
	}

	unitCtx, cancel := context.WithCancel(context.Background())
	p.activeCancel = cancel
	p.activeGen = gen
	p.mu.Unlock()

	// Push SynthesisUnitFrame downstream for assistant context tracking
	if err := e.Push(su); err != nil {
		cancel()
		return err
	}

	if p.session == nil || p.cfg.Client == nil {
		cancel()
		return nil
	}

	// Synthesize speech asynchronously
	return p.session.Go(func(taskCtx context.Context) {
		defer cancel()

		var rawAudio []byte
		var committed bool

		err := ExecuteWithRetry(unitCtx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, func() bool {
			return committed
		}, func(attemptCtx context.Context, attempt int) error {
			audio, err := p.cfg.Client.SynthesizeSpeech(attemptCtx, su.Text, p.cfg.Voice, p.cfg.SampleRate)
			if err != nil {
				return err
			}
			rawAudio = audio
			return nil
		}, nil)

		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			provErr := MapError("tts", err)
			_ = p.session.Reenter(&core.ErrorFrame{Err: provErr, Fatal: false}, core.Downstream, p.index+1, gen)
			return
		}

		// Critical verification check: verify rawAudio begins with PCM samples, NOT a WAV/RIFF container
		if len(rawAudio) >= 4 && bytes.Equal(rawAudio[:4], []byte("RIFF")) {
			err := fmt.Errorf("google tts returned WAV container instead of headerless PCM")
			_ = p.session.Reenter(&core.ErrorFrame{Err: err, Fatal: true}, core.Downstream, p.index+1, gen)
			return
		}

		p.mu.Lock()
		currentGen := p.activeGen
		if p.session != nil {
			currentGen = p.session.CurrentGeneration()
		}
		if gen != currentGen {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		// Emit TTSStartedFrame
		_ = p.session.Reenter(&core.TTSStartedFrame{
			Turn:   su.Turn,
			Gen:    gen,
			UnitID: su.UnitID,
		}, core.Downstream, p.index+1, gen)

		// Calculate 20ms chunk size in bytes:
		// SampleRate (24000) * 2 bytes/sample (16-bit mono) * 0.02s = 960 bytes
		bytesPerSample := 2
		chunkBytes := int(float64(p.cfg.SampleRate*bytesPerSample) * p.cfg.ChunkDuration.Seconds())
		if chunkBytes%2 != 0 {
			chunkBytes++ // Ensure 2-byte sample alignment
		}

		numChunks := (len(rawAudio) + chunkBytes - 1) / chunkBytes
		for i := 0; i < len(rawAudio); i += chunkBytes {
			end := i + chunkBytes
			if end > len(rawAudio) {
				end = len(rawAudio)
			}
			chunkData := rawAudio[i:end]
			isFinalChunk := (i+chunkBytes >= len(rawAudio))

			p.mu.Lock()
			currentGen := p.activeGen
			if p.session != nil {
				currentGen = p.session.CurrentGeneration()
			}
			if gen != currentGen {
				p.mu.Unlock()
				return // Interrupted mid-synthesis: stop immediately
			}
			p.mu.Unlock()

			committed = true

			// Emit 24kHz PCM16LE OutputAudioFrame
			audioFrame := &core.OutputAudioFrame{
				Samples:    chunkData,
				Encoding:   core.EncodingPCM16LE,
				SampleRate: p.cfg.SampleRate,
				Channels:   1,
				Duration:   p.cfg.ChunkDuration,
				Timestamp:  time.Now(),
				Turn:       su.Turn,
				Gen:        gen,
				UnitID:     su.UnitID,
				IsFinal:    isFinalChunk,
			}

			_ = p.session.Reenter(audioFrame, core.Downstream, p.index+1, gen)
		}

		if numChunks == 0 {
			// Zero bytes synthesized, still mark final
			emptyFrame := &core.OutputAudioFrame{
				Samples:    nil,
				Encoding:   core.EncodingPCM16LE,
				SampleRate: p.cfg.SampleRate,
				Channels:   1,
				Duration:   0,
				Turn:       su.Turn,
				Gen:        gen,
				UnitID:     su.UnitID,
				IsFinal:    true,
			}
			_ = p.session.Reenter(emptyFrame, core.Downstream, p.index+1, gen)
		}

		// Emit TTSStoppedFrame
		_ = p.session.Reenter(&core.TTSStoppedFrame{
			Turn:   su.Turn,
			Gen:    gen,
			UnitID: su.UnitID,
		}, core.Downstream, p.index+1, gen)
	})
}
