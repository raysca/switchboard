//go:build xaispeech

package server

import (
	"fmt"

	"govoice/core"
	"govoice/providers/xai"
)

// xaiSpeechEnabled reports whether this build includes xAI STT/TTS.
const xaiSpeechEnabled = true

// registerXAISpeech wires the experimental xAI STT/TTS adapters.
func registerXAISpeech(r *Registry) {
	r.Register("xai.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("xai.stt: %w", err)
		}
		sttCfg := xai.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		if cfg.Providers.XAISTT.Endpoint != "" {
			sttCfg.Endpoint = cfg.Providers.XAISTT.Endpoint
		}
		sttCfg.APIKey, sttCfg.CircuitBreaker = key, r.xaiBreaker
		return xai.NewSTTProcessor(sttCfg)
	})

	r.Register("xai.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("xai.tts: %w", err)
		}
		ttsCfg := xai.DefaultTTSConfig()
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if cfg.Providers.XAITTS.Endpoint != "" {
			ttsCfg.Endpoint = cfg.Providers.XAITTS.Endpoint
		}
		// The pipeline's TTS output rate is configured once, under the google TTS settings.
		if cfg.Providers.GoogleTTS.SampleRate > 0 {
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.APIKey, ttsCfg.CircuitBreaker = key, r.xaiBreaker
		return xai.NewTTSProcessor(ttsCfg)
	})

}
