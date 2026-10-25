package server

import (
	"errors"

	"govoice/core"
)

// offlineBearer is the constant token the xai processors send to the local stack, which ignores it.
// The offline provider reads no credential, so no key is stored for it.
const offlineBearer = "offline"

// offlineEndpoint refuses an empty endpoint: the xai processors would fall back to api.x.ai and send
// the call's prompt, transcripts and text to a cloud provider, the opposite of this provider.
func offlineEndpoint(key, value string) error {
	if value == "" {
		return errors.New("offline: providers.offline." + key + " is not set")
	}
	return nil
}

// PruneOffline drops every offline stage whose endpoint is empty, so it is reported unavailable.
// The production profile ships no offline endpoints: the provider is off until an operator sets
// providers.offline.*, because it talks plaintext to whatever listens there.
func (r *Registry) PruneOffline(cfg *Config) {
	for stage, endpoint := range map[string]string{
		"llm": cfg.Providers.Offline.LLMEndpoint, "stt": cfg.Providers.Offline.STTEndpoint, "tts": cfg.Providers.Offline.TTSEndpoint,
	} {
		if endpoint == "" {
			delete(r.factories, factoryKey("offline", stage))
		}
	}
}

// registerOffline registers the local stack (offline/) on every build: offline.llm needs no build
// tag; offline.stt and offline.tts exist only where the xAI speech adapters do.
func registerOffline(r *Registry) {
	r.Register("offline.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if err := offlineEndpoint("llm_endpoint", cfg.Providers.Offline.LLMEndpoint); err != nil {
			return nil, err
		}
		return xaiLLMProcessor(spec, cfg.Providers.Offline.LLMEndpoint, offlineBearer, r.breaker("llm", "offline"), "offline.llm")
	})
	registerOfflineSpeech(r)
}
