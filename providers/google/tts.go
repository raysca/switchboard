package google

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

// TTSConfig configures Google Cloud Text-to-Speech V1.
type TTSConfig struct {
	Voice                 string        // Pinned: "en-US-Journey-F"
	LanguageCode          string        // Used when the voice name has no xx-YY prefix (default "en-US")
	SampleRate            int           // Pinned: 24000 Hz
	MaxSynthesisUnitChars int           // Pinned: 250
	ChunkDuration         time.Duration // Default: 20ms
	CredentialsFile       string
	CredentialsJSON       []byte // In-memory service account JSON; preferred over CredentialsFile/APIKey
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
	client       *texttospeech.Client
	languageCode string
}

func NewRealGoogleTTSClient(ctx context.Context, cfg TTSConfig) (*RealGoogleTTSClient, error) {
	c, err := texttospeech.NewClient(ctx, clientAuthOptions(cfg.CredentialsJSON, cfg.CredentialsFile, cfg.APIKey)...)
	if err != nil {
		return nil, MapError("tts", err)
	}
	return &RealGoogleTTSClient{client: c, languageCode: cfg.LanguageCode}, nil
}

// voiceLanguage returns the language a Google voice name starts with
// ("en-GB-Neural2-A" -> "en-GB"), else fallback, else "en-US".
func voiceLanguage(voice, fallback string) string {
	if parts := strings.SplitN(voice, "-", 3); len(parts) == 3 {
		return parts[0] + "-" + parts[1]
	}
	if fallback != "" {
		return fallback
	}
	return "en-US"
}

func (c *RealGoogleTTSClient) SynthesizeSpeech(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
	req := &texttospeechpb.SynthesizeSpeechRequest{
		Input: &texttospeechpb.SynthesisInput{
			InputSource: &texttospeechpb.SynthesisInput_Text{Text: text},
		},
		Voice: &texttospeechpb.VoiceSelectionParams{
			LanguageCode: voiceLanguage(voice, c.languageCode),
			Name:         voice,
		},
		AudioConfig: &texttospeechpb.AudioConfig{
			// Journey voices reject AudioEncoding_PCM. LINEAR16 is headered WAV;
			// strip the container so the pipeline still sees raw PCM16LE.
			AudioEncoding:   texttospeechpb.AudioEncoding_LINEAR16,
			SampleRateHertz: int32(sampleRate),
		},
	}

	resp, err := c.client.SynthesizeSpeech(ctx, req)
	if err != nil {
		return nil, MapError("tts", err)
	}
	pcm, err := stripWAV(resp.AudioContent)
	if err != nil {
		return nil, err
	}
	return pcm, nil
}

func stripWAV(b []byte) ([]byte, error) {
	if len(b) < 12 || !bytes.Equal(b[:4], []byte("RIFF")) {
		return b, nil
	}
	idx := bytes.Index(b, []byte("data"))
	if idx < 0 || idx+8 > len(b) {
		return nil, fmt.Errorf("google tts LINEAR16 response is missing a WAV data chunk")
	}
	n := binary.LittleEndian.Uint32(b[idx+4 : idx+8])
	start := idx + 8
	if start+int(n) > len(b) {
		return nil, fmt.Errorf("google tts LINEAR16 WAV data is truncated")
	}
	return append([]byte(nil), b[start:start+int(n)]...), nil
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

	mu          sync.Mutex
	cond        *sync.Cond
	cancels     []ttsCancel
	nextSeq     uint64
	finishedSeq uint64
	activeGen   uint64
	closed      bool

	Requests atomic.Uint64
	Errors   atomic.Uint64
	Retries  atomic.Uint64
	Stale    atomic.Uint64
}

type ttsCancel struct {
	seq    uint64
	cancel context.CancelFunc
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
	p := &TTSProcessor{cfg: cfg}
	p.cond = sync.NewCond(&p.mu)
	return p, nil
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
	p.cancelAllLocked()
	p.finishedSeq = p.nextSeq
	p.cond.Broadcast()
	if closer, ok := p.cfg.Client.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	return nil
}

func (p *TTSProcessor) cancelAllLocked() {
	for _, c := range p.cancels {
		c.cancel()
	}
	p.cancels = nil
}

// ProcessFrame handles pipeline frames.
func (p *TTSProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.SynthesisUnitFrame:
		return p.handleSynthesisUnit(ctx, frame, e)

	case *core.InterruptionFrame:
		p.mu.Lock()
		p.cancelAllLocked()
		p.finishedSeq = p.nextSeq
		p.cond.Broadcast()
		p.mu.Unlock()
		return e.Push(f)

	case *core.CancelFrame:
		p.mu.Lock()
		p.cancelAllLocked()
		p.finishedSeq = p.nextSeq
		p.cond.Broadcast()
		p.mu.Unlock()
		return e.Push(f)

	default:
		// Preserve direction. A flush acknowledgement walks upstream from the
		// transport and must not be turned back downstream.
		return e.PushDirection(f, dir)
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
			p.Stale.Add(1)
			p.mu.Unlock()
			return nil // Drop stale unit
		}
	}

	parent := context.Background()
	if p.session != nil {
		parent = p.session.Context()
	}
	unitCtx, cancel := context.WithCancel(parent)
	seq := p.nextSeq + 1
	p.nextSeq = seq
	p.cancels = append(p.cancels, ttsCancel{seq: seq, cancel: cancel})
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

	// Synthesize speech asynchronously, in unit order. Cancellation covers every
	// in-flight request, not only the most recently started one.
	return p.session.Go(func(taskCtx context.Context) {
		defer cancel()
		defer func() {
			p.mu.Lock()
			kept := make([]ttsCancel, 0, len(p.cancels))
			for _, c := range p.cancels {
				if c.seq != seq {
					kept = append(kept, c)
				}
			}
			p.cancels = kept
			if p.finishedSeq+1 == seq {
				p.finishedSeq = seq
			}
			p.cond.Broadcast()
			p.mu.Unlock()
		}()

		p.mu.Lock()
		for p.finishedSeq+1 != seq && unitCtx.Err() == nil && taskCtx.Err() == nil && !p.closed {
			p.cond.Wait()
		}
		stale := p.closed || unitCtx.Err() != nil || taskCtx.Err() != nil
		if !stale && p.session != nil && gen != 0 && gen < p.session.CurrentGeneration() {
			stale = true
		}
		p.mu.Unlock()
		if stale {
			p.Stale.Add(1)
			return
		}
		p.Requests.Add(1)
		traceSession := core.SessionID(strconv.FormatUint(p.session.ID(), 10))
		traceTurn, traceGen := core.TurnID(su.Turn), core.Generation(su.Gen)
		span := core.StartContextSpan(ctx, "tts.request", traceSession, traceTurn, traceGen, map[string]any{"provider": "google"})
		tracePolicy := core.TraceContextFromContext(ctx).Policy
		traceFailure := error(errors.New("canceled"))
		defer func() {
			if recoverValue := recover(); recoverValue != nil {
				traceFailure = errors.New("provider_failure")
				span.AddEvent(core.NewTraceEvent("tts.failure", time.Now(), traceSession, traceTurn, traceGen, tracePolicy, nil))
				span.End(traceFailure)
				panic(recoverValue)
			}
			name := "tts.complete"
			if traceFailure != nil {
				name = "tts.failure"
			}
			span.AddEvent(core.NewTraceEvent(name, time.Now(), traceSession, traceTurn, traceGen, tracePolicy, nil))
			span.End(traceFailure)
		}()

		var rawAudio []byte
		var committed bool

		err := ExecuteWithRetry(unitCtx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, func() bool {
			return committed
		}, func(attemptCtx context.Context, attempt int) error {
			if attempt > 0 {
				p.Retries.Add(1)
			}
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
			traceFailure = errors.New("provider_failure")
			p.Errors.Add(1)
			provErr := MapError("tts", err)
			_ = p.session.Reenter(&core.ErrorFrame{Err: provErr, Fatal: false}, core.Downstream, p.index+1, gen)
			return
		}

		// Critical verification check: verify rawAudio begins with PCM samples, NOT a WAV/RIFF container
		if len(rawAudio) >= 4 && bytes.Equal(rawAudio[:4], []byte("RIFF")) {
			traceFailure = errors.New("provider_failure")
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
		if err := p.session.Reenter(&core.TTSStartedFrame{
			Turn:   su.Turn,
			Gen:    gen,
			UnitID: su.UnitID,
		}, core.Downstream, p.index+1, gen); err != nil {
			traceFailure = errors.New("transport_failure")
			return
		}

		// Calculate 20ms chunk size in bytes:
		// SampleRate (24000) * 2 bytes/sample (16-bit mono) * 0.02s = 960 bytes
		bytesPerSample := 2
		chunkBytes := int(float64(p.cfg.SampleRate*bytesPerSample) * p.cfg.ChunkDuration.Seconds())
		if chunkBytes%2 != 0 {
			chunkBytes++ // Ensure 2-byte sample alignment
		}

		numChunks := (len(rawAudio) + chunkBytes - 1) / chunkBytes
		firstAudio := true
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
				TurnFinal:  su.IsFinal && isFinalChunk,
			}

			if err := p.session.Reenter(audioFrame, core.Downstream, p.index+1, gen); err != nil {
				traceFailure = errors.New("transport_failure")
				return
			}
			if firstAudio {
				firstAudio = false
				span.AddEvent(core.NewTraceEvent("tts.first_audio", time.Now(), traceSession, traceTurn, traceGen, tracePolicy, nil))
			}
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
				TurnFinal:  su.IsFinal,
			}
			if err := p.session.Reenter(emptyFrame, core.Downstream, p.index+1, gen); err != nil {
				traceFailure = errors.New("transport_failure")
				return
			}
		}

		// Emit TTSStoppedFrame
		if err := p.session.Reenter(&core.TTSStoppedFrame{
			Turn:   su.Turn,
			Gen:    gen,
			UnitID: su.UnitID,
		}, core.Downstream, p.index+1, gen); err != nil {
			traceFailure = errors.New("transport_failure")
			return
		}
		traceFailure = nil

		// Emit optional TTS usage metrics (character count and audio duration)
		chars := int64(len([]rune(su.Text)))
		var durationMs int64
		if p.cfg.SampleRate > 0 && len(rawAudio) > 0 {
			// 16-bit mono = 2 bytes per sample
			samples := len(rawAudio) / 2
			durationMs = int64(samples * 1000 / p.cfg.SampleRate)
		}
		usageFrame := &core.UsageMetricsFrame{
			SessionID:       traceSession,
			Turn:            su.Turn,
			Gen:             gen,
			Provider:        "google",
			Model:           p.cfg.Voice,
			Service:         "tts",
			Timestamp:       time.Now(),
			Characters:      core.Int64Ptr(chars),
			AudioDurationMs: core.Int64Ptr(durationMs),
			Requests:        core.Int64Ptr(1),
		}
		_ = p.session.Reenter(usageFrame, core.Downstream, p.index+1, gen)
	})
}
