package google

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"govoice/core/netpool"
	"govoice/core/ttsunit"
)

// StreamingTTSProcessor speaks Chirp 3 HD voices over StreamingSynthesize: audio is emitted as it
// arrives instead of after the whole sentence is synthesized. Units play in order on the shared
// ttsunit.Runner (barge-in cancel, retries before audio, tracing, usage). One stream per unit,
// because a Google stream has no per-utterance boundary. A retry after a failed stream uses the
// unary request for that unit, so a stream problem never silences the call.
type StreamingTTSProcessor struct {
	*ttsunit.Runner
	cfg    TTSConfig
	client StreamingTTSClient
	idle   time.Duration // longest a stream (or a fallback request) may go without progress
}

// streamAttemptCap bounds a whole streaming attempt. The producer is paced to playback speed, so a
// long unit outlives any request deadline; the idle limit is what detects a stalled stream.
const streamAttemptCap = 5 * time.Minute

// NewStreamingTTSProcessor needs cfg.Client to implement StreamingTTSClient.
func NewStreamingTTSProcessor(cfg TTSConfig) (*StreamingTTSProcessor, error) {
	cfg = normalizeTTSConfig(cfg)
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = ttsRetryPolicy()
	}
	sc, ok := cfg.Client.(StreamingTTSClient)
	if !ok {
		return nil, errors.New("google tts streaming: the client does not implement StreamingTTSClient")
	}
	p := &StreamingTTSProcessor{cfg: cfg, client: sc, idle: cfg.RetryPolicy.AttemptTimeout}
	if p.idle <= 0 {
		p.idle = ttsRetryPolicy().AttemptTimeout
	}
	policy := cfg.RetryPolicy
	policy.AttemptTimeout = streamAttemptCap
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "google", Model: cfg.Voice, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: policy, Classify: classify,
		Attempt: p.attempt,
	})
	return p, nil
}

// attempt synthesizes one unit. Attempt 0 streams; a retry (the Runner only retries before the
// first audio frame) synthesizes the pieces with the unary request and plays them as one unit.
func (p *StreamingTTSProcessor) attempt(ctx context.Context, u *ttsunit.Unit, attempt int) error {
	pieces := splitUnit(u.Frame.Text, p.cfg.MaxSynthesisUnitChars)
	if len(pieces) == 0 {
		return u.Finish()
	}
	if attempt == 0 {
		if err := p.stream(ctx, u, pieces); err != nil {
			return err
		}
		return u.Finish()
	}
	ctx, cancel := context.WithTimeout(ctx, p.idle) // the fallback keeps the request's own deadline
	defer cancel()
	var audio []byte // nothing is written until every piece is synthesized, so this attempt may retry too
	for _, piece := range pieces {
		pcm, err := p.client.SynthesizeSpeech(ctx, piece, p.cfg.Voice, p.cfg.SampleRate)
		if err != nil {
			return err
		}
		audio = append(audio, pcm...)
	}
	if err := u.Write(audio); err != nil {
		return err
	}
	return u.Finish()
}

// stream runs the streaming request. It gives up when no audio arrives for p.idle (time spent in
// u.Write, which waits for playback, does not count), and a failure that is not a client error
// (an unmapped gRPC code, an idle stall) is made retryable so the unary request serves the unit.
func (p *StreamingTTSProcessor) stream(ctx context.Context, u *ttsunit.Unit, pieces []string) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	timer := time.AfterFunc(p.idle, func() { stalled.Store(true); cancel() })
	defer timer.Stop()
	err := p.client.StreamSynthesize(sctx, pieces, p.cfg.Voice, p.cfg.SampleRate, func(b []byte) error {
		timer.Stop()
		err := u.Write(b)
		timer.Reset(p.idle)
		return err
	})
	if stalled.Load() && ctx.Err() == nil {
		return &ProviderError{Capability: "tts", BaseErr: ErrProviderTimeout, SafeMessage: "stream stalled", Retryable: true}
	}
	var pe *ProviderError
	if errors.As(err, &pe) && !pe.Retryable && errors.Is(pe.BaseErr, ErrProviderInternal) {
		c := *pe
		c.Retryable = true
		return &c
	}
	return err
}

// Warm delegates to the synthesis client when it can warm itself.
func (p *StreamingTTSProcessor) Warm(ctx context.Context) error {
	w, ok := p.cfg.Client.(netpool.Warmer)
	if !ok || breakerOpen(p.cfg.CircuitBreaker, "tts") {
		return nil
	}
	return w.Warm(ctx)
}

// Close cancels in-flight synthesis and closes the client.
func (p *StreamingTTSProcessor) Close() error {
	err := p.Runner.Close()
	if c, ok := p.cfg.Client.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return err
}
