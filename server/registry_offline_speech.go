//go:build xaispeech

package server

import (
	"time"

	"govoice/core"
	"govoice/providers/xai"
)

// offlineSTTDrainTimeout is how long a stream waits for its final after audio.done: batch Whisper on
// a CPU needs 1 to 2.5 s for a 6 s segment and more under load, and a final that misses the wait
// drops that segment from the turn.
const offlineSTTDrainTimeout = 6 * time.Second

// registerOfflineSpeech wires offline.stt and offline.tts to the adapter in offline/.
func registerOfflineSpeech(r *Registry) {
	r.Register("offline.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if err := offlineEndpoint("stt_endpoint", cfg.Providers.Offline.STTEndpoint); err != nil {
			return nil, err
		}
		sttCfg := xai.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.DrainTimeout = offlineSTTDrainTimeout
		sttCfg.Endpoint, sttCfg.APIKey, sttCfg.CircuitBreaker = cfg.Providers.Offline.STTEndpoint, offlineBearer, r.breaker("stt", "offline")
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return xai.NewSTTProcessor(sttCfg)
	})
	r.Register("offline.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if err := offlineEndpoint("tts_endpoint", cfg.Providers.Offline.TTSEndpoint); err != nil {
			return nil, err
		}
		ttsCfg := xai.DefaultTTSConfig()
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if cfg.Providers.GoogleTTS.SampleRate > 0 { // the pipeline's output rate is configured once, under google_tts
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.Endpoint, ttsCfg.APIKey, ttsCfg.CircuitBreaker = cfg.Providers.Offline.TTSEndpoint, offlineBearer, r.breaker("tts", "offline")
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		ttsCfg.RetryPolicy.OnRetry = spec.OnRetry
		return xai.NewStreamingTTSProcessor(ttsCfg)
	})
}
