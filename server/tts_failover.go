package server

import (
	"errors"
	"io"
	"time"

	"govoice/core"
	"govoice/core/resilience"
	"govoice/providers/cartesia"
	"govoice/providers/elevenlabs"
	"govoice/providers/nvidia"
	"govoice/server/flow"
)

// defaultTTSRate is the output rate every TTS provider defaults to; the
// pipeline rate is Providers.GoogleTTS.SampleRate when set.
const defaultTTSRate = 24000

// ttsClassify is the failover class of a failed TTS candidate ("" = none).
// google and xai expose ClientError() on their ProviderError; cartesia,
// elevenlabs and nvidia carry it as a field, which the method check would flatten into
// server_error.
func ttsClassify(err error) string {
	var ce interface{ ClientError() bool }
	var cp *cartesia.ProviderError
	var ep *elevenlabs.ProviderError
	var np *nvidia.ProviderError
	client := errors.As(err, &ce) && ce.ClientError() ||
		errors.As(err, &cp) && cp.ClientError ||
		errors.As(err, &ep) && ep.ClientError ||
		errors.As(err, &np) && np.ClientError
	return flow.ClassOf(err, resilience.Verdict{ClientError: client})
}

// ttsBreaker is the breaker a TTS stage of this provider uses (nil if unknown).
func (r *Registry) ttsBreaker(provider string) *resilience.Breaker {
	switch provider {
	case "google":
		return r.googleTTSBreaker
	case "echo":
		return r.echoTTSBreaker
	case "xai":
		return r.xaiTTSBreaker
	case "offline":
		return r.offlineTTSBreaker
	case "cartesia":
		return r.cartesiaBreaker
	case "elevenlabs":
		return r.elevenlabsBreaker
	case "nvidia":
		return r.nvidiaBreaker("tts|" + nvidia.DefaultEndpoint)
	}
	return nil
}

// failoverPolicy tightens a provider's retry policy for a stage candidate that
// has a fallback behind it: 2 tries instead of 3, and for stt opens at most
// sttFailoverAttempt each, so a hung STT dial reports (and the fallback
// starts) after about 3 s, before the turn processor drops the untranscribed
// turn (turn.DefaultNoTranscriptTimeout, 5 s after silence onset); for tts
// after 2 x the provider's request timeout instead of up to 45 s. It only ever shortens
// base (a zero base field counts as unset). The last candidate keeps its full
// policy. LLMs are instead bounded by the first-token deadline (core.LLMFailover).
//
// ponytail: tts keeps its attempt timeout because the unary tts providers apply it
// to the whole unit (dial through the last audio byte; the Google and xAI streaming paths use it
// as an idle limit instead), so shortening it would
// cut long sentences mid-word; a first-audio deadline in ttsunit is the upgrade
// if 2 x 15 s proves too slow.
func failoverPolicy(stage string, base resilience.Policy) resilience.Policy {
	if base.MaxAttempts <= 0 || base.MaxAttempts > 2 {
		base.MaxAttempts = 2
	}
	if stage == "stt" && (base.AttemptTimeout <= 0 || base.AttemptTimeout > sttFailoverAttempt) {
		base.AttemptTimeout = sttFailoverAttempt
	}
	return base
}

// sttFailoverAttempt bounds one STT open of a candidate with a fallback: a
// websocket/gRPC handshake on a healthy provider takes well under it.
// ponytail: a cold Google channel (TLS + token fetch) can need longer; Warm
// pre-dials, and a slow open only costs a needless failover, not the turn.
const sttFailoverAttempt = 1500 * time.Millisecond

// ttsRate is the one output rate the call's TTS candidates are built for. The
// transport builds its resampler from the first frame and cannot follow a
// rate change, so every candidate is configured to this rate (the factories
// read it from the same setting) and core.NewTTSFailover rejects a mismatch.
func ttsRate(cfg *Config) int {
	if cfg != nil && cfg.Providers.GoogleTTS.SampleRate > 0 {
		return cfg.Providers.GoogleTTS.SampleRate
	}
	return defaultTTSRate
}

// buildTTS builds the TTS stage: the primary alone (the pre-failover pipeline,
// unchanged) or, with fallbacks, a core.TTSFailover over the chain. Fallbacks
// that cannot be built are skipped (counted in voice_tts_fallbacks_skipped_total).
func (r *Registry) buildTTS(cfg *Config, call CallSpec, build func(string, StageSpec) (core.Processor, error)) (core.Processor, error) {
	if len(call.TTSFallbacks) == 0 {
		return build("tts", call.TTS)
	}
	specs := append([]StageSpec{call.TTS}, call.TTSFallbacks...)
	var cands []core.TTSCandidate
	for i, sp := range specs {
		sp.Failover = i < len(specs)-1
		p, err := build("tts", sp)
		if err != nil {
			if i == 0 {
				closeProcs(cands)
				return nil, err
			}
			if r.onTTSFallbackSkipped != nil {
				r.onTTSFallbackSkipped()
			}
			continue
		}
		c := core.TTSCandidate{Provider: sp.Provider, Proc: p, Rate: ttsRate(cfg)}
		br := r.ttsBreaker(sp.Provider)
		if br != nil {
			c.Open = br.WouldRefuse
		}
		cands = append(cands, c)
	}
	if len(cands) == 1 {
		return cands[0].Proc, nil
	}
	fo, err := core.NewTTSFailover(core.TTSFailoverConfig{
		Candidates: cands, Classify: ttsClassify, Allow: call.Failover.Allows, OnFailover: r.onTTSFailover,
	})
	if err != nil {
		closeProcs(cands)
	}
	return fo, err
}

func closeProcs(cands []core.TTSCandidate) {
	for _, c := range cands {
		if cl, ok := c.Proc.(io.Closer); ok {
			_ = cl.Close()
		}
	}
}
