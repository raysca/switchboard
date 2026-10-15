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

// registerOffline registers the local stack (offline/) on every build: offline.llm needs no build
// tag; offline.stt and offline.tts exist only where the xAI speech adapters do.
func registerOffline(r *Registry) {
	r.Register("offline.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if err := offlineEndpoint("llm_endpoint", cfg.Providers.Offline.LLMEndpoint); err != nil {
			return nil, err
		}
		return xaiLLMProcessor(spec, cfg.Providers.Offline.LLMEndpoint, offlineBearer, r.offlineLLMBreaker, "offline.llm")
	})
	registerOfflineSpeech(r)
}
