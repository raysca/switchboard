package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/resilience"
	"govoice/providers/deepgram"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/server/agent"
	"govoice/server/runtime"
	"govoice/server/storage"
)

// maxSTTReplay caps the configurable replay buffer (60 s of 16 kHz PCM16 mono
// is ~1.9 MB per call with fallbacks).
const maxSTTReplay = 60 * time.Second

// sttClassify is the failover class of a failed STT candidate ("" = none). Each
// provider carries "the caller's fault" differently: google as a method, deepgram
// as a field, and xai marks every unclassified failure (a dropped stream included)
// non-retryable, so only a 4xx status counts as a client error there or an outage
// would be taken for a config bug. Their wrappers keep ErrCircuitOpen/ErrTimeout
// in the chain (checked by ClassOf); a provider-side deadline (a hung send) is a
// timeout, not the caller's own expiry.
func sttClassify(err error) string {
	var ge *google.ProviderError
	var xe *xai.ProviderError
	var de *deepgram.ProviderError
	isGoogle, isXAI, isDeepgram := errors.As(err, &ge), errors.As(err, &xe), errors.As(err, &de)
	client := isGoogle && ge.ClientError() ||
		isXAI && xe.StatusCode >= 400 && xe.StatusCode < 500 && xe.StatusCode != 429 ||
		isDeepgram && de.ClientError
	if (isGoogle || isXAI || isDeepgram) && errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return agent.ClassTimeout
	}
	return agent.ClassOf(err, resilience.Verdict{ClientError: client})
}

// sttBreaker is the breaker an STT stage of this provider uses (nil if unknown).
func (r *Registry) sttBreaker(provider string) *resilience.Breaker {
	switch provider {
	case "google":
		return r.googleSTTBreaker
	case "xai":
		return r.xaiBreaker
	case "deepgram":
		return r.deepgramBreaker
	}
	return nil
}

// buildSTT builds the STT stage: the primary alone (the pre-failover pipeline,
// unchanged: no wrapper, no audio copies) or, with fallbacks, a core.STTFailover
// over the chain. Fallbacks that cannot be built are skipped (counted in
// voice_stt_fallbacks_skipped_total).
func (r *Registry) buildSTT(cfg *Config, call CallSpec, build func(string, StageSpec) (core.Processor, error)) (core.Processor, error) {
	if len(call.STTFallbacks) == 0 {
		return build("stt", call.STT)
	}
	specs := append([]StageSpec{call.STT}, call.STTFallbacks...)
	var cands []core.STTCandidate
	for i, sp := range specs {
		sp.Failover = i < len(specs)-1
		p, err := build("stt", sp)
		if err != nil {
			if i == 0 {
				closeSTT(cands)
				return nil, err
			}
			if r.onSTTFallbackSkipped != nil {
				r.onSTTFallbackSkipped()
			}
			continue
		}
		c := core.STTCandidate{Provider: sp.Provider, Proc: p}
		if br := r.sttBreaker(sp.Provider); br != nil {
			c.Open = br.WouldRefuse
		}
		cands = append(cands, c)
	}
	if len(cands) == 1 {
		return cands[0].Proc, nil
	}
	buf := core.DefaultSTTReplayBytes
	if cfg != nil && cfg.STT.ReplayBuffer > 0 {
		buf = int(min(cfg.STT.ReplayBuffer, maxSTTReplay).Seconds() * 16000 * 2)
	}
	fo, err := core.NewSTTFailover(core.STTFailoverConfig{
		Candidates: cands, Classify: sttClassify, Allow: call.Failover.Allows, BufferBytes: buf,
		OnFailover: r.onSTTFailover, OnDropped: r.onSTTReplayDropped,
	})
	if err != nil {
		closeSTT(cands)
	}
	return fo, err
}

func closeSTT(cands []core.STTCandidate) {
	for _, c := range cands {
		if cl, ok := c.Proc.(io.Closer); ok {
			_ = cl.Close()
		}
	}
}

// sttFallbacks resolves the STT chain after the primary. A fallback that cannot
// be resolved now is skipped (logged and counted), not fatal. The fallback
// inherits the primary's language unless it sets its own; model is provider-specific.
func (s *Server) sttFallbacks(ctx context.Context, rs *runtime.Session, cfg agent.Config, creds []storage.Credential) []StageSpec {
	return s.resolveFallbacks(ctx, rs, creds, "stt", cfg, &s.metrics.stt.FallbacksSkippedTotal, func(c agent.Candidate, secret []byte) StageSpec {
		lang := c.Language
		if lang == "" {
			lang = cfg.STT.Language
		}
		return StageSpec{Provider: c.Provider, Model: c.Model, Language: lang, Secret: secret}
	})
}

// sttMetrics are the STT failover counters.
type sttMetrics struct {
	failovers failoverCounts
	// FallbacksSkippedTotal counts configured STT fallbacks left out of a call.
	FallbacksSkippedTotal atomic.Uint64
	// ReplayDroppedBytesTotal is open-utterance audio evicted from full replay buffers: an
	// utterance longer than the buffer cannot be replayed whole.
	ReplayDroppedBytesTotal atomic.Uint64
}

func (m *sttMetrics) write(w io.Writer) {
	m.failovers.writeAs(w, "voice_stt_failover_total", "STT streams moved to a fallback provider (utterance audio replayed), or traffic returned at an utterance start.")
	writeCounter(w, "voice_stt_fallbacks_skipped_total", "Configured STT fallbacks left out of a call (unresolvable).", m.FallbacksSkippedTotal.Load())
	writeCounter(w, "voice_stt_replay_dropped_bytes_total", "Utterance audio bytes evicted from STT replay buffers (utterances longer than stt.replay_buffer cannot be replayed whole).", m.ReplayDroppedBytesTotal.Load())
}

func writeCounter(w io.Writer, name, help string, v uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}
