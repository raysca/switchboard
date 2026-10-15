//go:build xaispeech

package server

import (
	"govoice/core"
	"govoice/providers/xai"
)

// registerOfflineSpeech wires offline.stt and offline.tts to the adapter in offline/.
func registerOfflineSpeech(r *Registry) {
	r.Register("offline.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		sttCfg := xai.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.Endpoint, sttCfg.APIKey, sttCfg.CircuitBreaker = cfg.Providers.Offline.STTEndpoint, offlineBearer, r.offlineSTTBreaker
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return xai.NewSTTProcessor(sttCfg)
	})
	r.Register("offline.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		ttsCfg := xai.DefaultTTSConfig()
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if cfg.Providers.GoogleTTS.SampleRate > 0 { // the pipeline's output rate is configured once, under google_tts
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.Endpoint, ttsCfg.APIKey, ttsCfg.CircuitBreaker = cfg.Providers.Offline.TTSEndpoint, offlineBearer, r.offlineTTSBreaker
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		return xai.NewStreamingTTSProcessor(ttsCfg)
	})
}
