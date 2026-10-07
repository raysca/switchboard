//go:build xaispeech

package xai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// TTSConfig holds configuration for the xAI Text-to-Speech processor.
type TTSConfig struct {
	Endpoint       string        // Default: "https://api.x.ai/v1/tts"
	Voice          string        // Default: "eve" (e.g. "ara", "eve", "leo", "rex", "sal")
	Language       string        // Default: "en"
	SampleRate     int           // Default: 24000
	APIKey         string        // Bearer token
	Timeout        time.Duration // Default: 10s
	CircuitBreaker *resilience.Breaker
	RetryPolicy    resilience.Policy
	Client         TTSClient
}

// DefaultTTSConfig returns recommended default settings for xAI TTS.
func DefaultTTSConfig() TTSConfig {
	return TTSConfig{
		Endpoint:    "https://api.x.ai/v1/tts",
		Voice:       "eve",
		Language:    "en",
		SampleRate:  24000,
		Timeout:     10 * time.Second,
		RetryPolicy: resilience.DefaultPolicy(),
	}
}

// TTSClient abstracts HTTP synthesis execution for xAI.
type TTSClient interface {
	Synthesize(ctx context.Context, text string, cfg TTSConfig) ([]byte, error)
}

// RealXAITTSClient implements TTSClient against https://api.x.ai/v1/tts.
type RealXAITTSClient struct {
	httpClient *http.Client
}

// NewRealXAITTSClient creates an HTTP client for xAI TTS.
func NewRealXAITTSClient() *RealXAITTSClient {
	return &RealXAITTSClient{
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

type ttsOutputFormat struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
}

type ttsRequestBody struct {
	Text         string          `json:"text"`
	VoiceID      string          `json:"voice_id"`
	Language     string          `json:"language"`
	OutputFormat ttsOutputFormat `json:"output_format"`
}

func (c *RealXAITTSClient) Synthesize(ctx context.Context, text string, cfg TTSConfig) ([]byte, error) {
	sr := cfg.SampleRate
	if sr <= 0 {
		sr = 24000
	}
	voice := cfg.Voice
	if voice == "" {
		voice = "eve"
	}
	lang := cfg.Language
	if lang == "" {
		lang = "en"
	}

	reqPayload := ttsRequestBody{
		Text:     text,
		VoiceID:  voice,
		Language: lang,
		OutputFormat: ttsOutputFormat{
			Codec:      "pcm",
			SampleRate: sr,
		},
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal tts request: %w", err)
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://api.x.ai/v1/tts"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create tts request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, MapError("tts", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, MapHTTPError("tts", resp, string(respBody), nil)
	}

	pcmBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, MapError("tts", err)
	}
	return pcmBytes, nil
}

type ttsCancel struct {
	seq    uint64
	cancel context.CancelFunc
}

// TTSProcessor converts SynthesisUnitFrame into raw OutputAudioFrame chunks using xAI.
type TTSProcessor struct {
	cfg     TTSConfig
	client  TTSClient
	session core.SessionController
	index   int

	mu          sync.Mutex
	cond        *sync.Cond
	cancels     []ttsCancel
	nextSeq     uint64
	finishedSeq uint64
	activeGen   uint64
	closed      bool

	// Metrics
	RequestsTotal atomic.Uint64
	RetriesTotal  atomic.Uint64
	AudioFrames   atomic.Uint64
	AudioBytes    atomic.Uint64
	ErrorsTotal   atomic.Uint64
	StaleTotal    atomic.Uint64
}

// NewTTSProcessor creates a new xAI TTSProcessor.
func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.x.ai/v1/tts"
	}
	if cfg.Voice == "" {
		cfg.Voice = "eve"
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = resilience.DefaultPolicy().MaxAttempts
	}
	cfg.RetryPolicy.AttemptTimeout = cfg.Timeout // Timeout is the one knob: it bounds each synthesis request
	client := cfg.Client
	if client == nil {
		client = NewRealXAITTSClient()
	}
	p := &TTSProcessor{
		cfg:    cfg,
		client: client,
	}
	p.cond = sync.NewCond(&p.mu)
	return p, nil
}

// Attach registers the processor with the session pipeline and supervisor.
func (p *TTSProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = s
	p.index = index
	return nil
}

// Close gracefully closes the processor and cancels active synthesis operations.
func (p *TTSProcessor) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cancelAllLocked()
	p.finishedSeq = p.nextSeq
	p.cond.Broadcast()
	return nil
}

func (p *TTSProcessor) cancelAllLocked() {
	for _, c := range p.cancels {
		c.cancel()
	}
	p.cancels = nil
}

// ProcessFrame processes frames traversing the pipeline.
func (p *TTSProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.SynthesisUnitFrame:
		return p.synthesizeUnit(ctx, frame, e)

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
		return e.PushDirection(f, dir)
	}
}

func (p *TTSProcessor) synthesizeUnit(ctx context.Context, unit *core.SynthesisUnitFrame, e core.Emitter) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return core.ErrSessionClosed
	}

	s := p.session
	if s == nil {
		p.mu.Unlock()
		return errors.New("tts processor not attached to session")
	}

	// Stale generation check
	if unit.Gen != 0 && unit.Gen < s.CurrentGeneration() {
		p.StaleTotal.Add(1)
		p.mu.Unlock()
		return nil
	}

	parent := s.Context()
	synthCtx, cancel := context.WithCancel(parent) // barge-in; the request deadline is per attempt
	seq := p.nextSeq + 1
	p.nextSeq = seq
	p.cancels = append(p.cancels, ttsCancel{seq: seq, cancel: cancel})
	p.activeGen = unit.Gen
	p.mu.Unlock()

	// Push unit frame downstream for assistant context tracking
	if err := e.Push(unit); err != nil {
		cancel()
		return err
	}

	p.RequestsTotal.Add(1)
	return s.Go(func(sessionCtx context.Context) {
		traceSession := core.SessionID(strconv.FormatUint(s.ID(), 10))
		traceTurn, traceGen := core.TurnID(unit.Turn), core.Generation(unit.Gen)
		span := core.StartContextSpan(ctx, "tts.request", traceSession, traceTurn, traceGen, map[string]any{"provider": "xai"})
		policy := core.TraceContextFromContext(ctx).Policy
		traceFailure := error(errors.New("canceled"))
		defer func() {
			if v := recover(); v != nil {
				traceFailure = errors.New("provider_failure")
				span.AddEvent(core.NewTraceEvent("tts.failure", time.Now(), traceSession, traceTurn, traceGen, policy, nil))
				span.End(traceFailure)
				panic(v)
			}
			name := "tts.complete"
			if traceFailure != nil {
				name = "tts.failure"
			}
			span.AddEvent(core.NewTraceEvent(name, time.Now(), traceSession, traceTurn, traceGen, policy, nil))
			span.End(traceFailure)
		}()
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

		var pcmData []byte
		var committed bool

		opErr := resilience.Execute(synthCtx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, classify, func() bool { return committed }, func(attemptCtx context.Context, attempt int) error {
			if attempt > 0 {
				p.RetriesTotal.Add(1)
			}
			data, err := p.client.Synthesize(attemptCtx, unit.Text, p.cfg)
			if err != nil {
				return err
			}
			pcmData = data
			return nil
		})

		if opErr != nil {
			if errors.Is(opErr, context.Canceled) || synthCtx.Err() != nil {
				return
			}
			traceFailure = errors.New("provider_failure")
			p.ErrorsTotal.Add(1)
			_ = s.Reenter(&core.ErrorFrame{
				Err:   opErr,
				Fatal: false,
			}, core.Downstream, p.index+1, unit.Gen)
			return
		}

		// Wait for turn in emission sequence: units must be emitted strictly in arrival order
		p.mu.Lock()
		for p.finishedSeq+1 != seq && synthCtx.Err() == nil && sessionCtx.Err() == nil && !p.closed {
			p.cond.Wait()
		}
		stale := p.closed || synthCtx.Err() != nil || sessionCtx.Err() != nil || (unit.Gen != 0 && unit.Gen < s.CurrentGeneration())
		p.mu.Unlock()

		if stale {
			p.StaleTotal.Add(1)
			return
		}

		p.mu.Lock()
		currentGen := s.CurrentGeneration()
		if unit.Gen != 0 && unit.Gen < currentGen {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		// Chunk raw PCM into 20ms frames:
		// SampleRate (24000) * 2 bytes/sample * 0.02s = 960 bytes per chunk
		sampleRate := p.cfg.SampleRate
		if sampleRate <= 0 {
			sampleRate = 24000
		}
		chunkSize := int(float64(sampleRate*2) * 0.02)
		if chunkSize <= 0 {
			chunkSize = 960
		}

		// Emit TTSStartedFrame
		if err := s.Reenter(&core.TTSStartedFrame{
			Turn:   unit.Turn,
			Gen:    unit.Gen,
			UnitID: unit.UnitID,
		}, core.Downstream, p.index+1, unit.Gen); err != nil {
			traceFailure = errors.New("transport_failure")
			return
		}

		totalBytes := len(pcmData)
		firstAudio := true
		if totalBytes > 0 {
			var seq uint64
			for offset := 0; offset < totalBytes; offset += chunkSize {
				end := offset + chunkSize
				isFinal := false
				if end >= totalBytes {
					end = totalBytes
					isFinal = true
				}

				chunk := make([]byte, end-offset)
				copy(chunk, pcmData[offset:end])
				seq++

				p.mu.Lock()
				if unit.Gen != 0 && unit.Gen < s.CurrentGeneration() {
					p.mu.Unlock()
					return
				}
				p.mu.Unlock()

				audioFrame := &core.OutputAudioFrame{
					Samples:    chunk,
					Encoding:   core.EncodingPCM16LE,
					SampleRate: sampleRate,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  time.Now(),
					Turn:       unit.Turn,
					Gen:        unit.Gen,
					UnitID:     unit.UnitID,
					Seq:        seq,
					IsFinal:    isFinal,
					TurnFinal:  unit.IsFinal && isFinal,
				}

				p.AudioFrames.Add(1)
				p.AudioBytes.Add(uint64(len(chunk)))

				if err := s.Reenter(audioFrame, core.Downstream, p.index+1, unit.Gen); err != nil {
					traceFailure = errors.New("transport_failure")
					return
				}
				if firstAudio {
					firstAudio = false
					span.AddEvent(core.NewTraceEvent("tts.first_audio", time.Now(), traceSession, traceTurn, traceGen, policy, nil))
				}
			}
		} else {
			// Preserve the whole-turn drain boundary even when the provider
			// returns no samples for the final synthesis unit.
			empty := &core.OutputAudioFrame{
				Encoding: core.EncodingPCM16LE, SampleRate: sampleRate, Channels: 1,
				Turn: unit.Turn, Gen: unit.Gen, UnitID: unit.UnitID,
				IsFinal: true, TurnFinal: unit.IsFinal,
			}
			if err := s.Reenter(empty, core.Downstream, p.index+1, unit.Gen); err != nil {
				traceFailure = errors.New("transport_failure")
				return
			}
		}

		// Emit TTSStoppedFrame
		if err := s.Reenter(&core.TTSStoppedFrame{
			Turn:   unit.Turn,
			Gen:    unit.Gen,
			UnitID: unit.UnitID,
		}, core.Downstream, p.index+1, unit.Gen); err != nil {
			traceFailure = errors.New("transport_failure")
			return
		}
		traceFailure = nil
	})
}
