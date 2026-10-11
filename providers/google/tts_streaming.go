package google

import (
	"context"
	"errors"

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
}

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
	p := &StreamingTTSProcessor{cfg: cfg, client: sc}
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "google", Model: cfg.Voice, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify,
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
		if err := p.client.StreamSynthesize(ctx, pieces, p.cfg.Voice, p.cfg.SampleRate, u.Write); err != nil {
			return err
		}
		return u.Finish()
	}
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
