package server

import "govoice/core"

// offlineBearer is the constant token the xai processors send to the local stack, which ignores it.
// The offline provider reads no credential, so no key is stored for it.
const offlineBearer = "offline"

// registerOffline registers the local stack (offline/) on every build: offline.llm needs no build
// tag; offline.stt and offline.tts exist only where the xAI speech adapters do.
func registerOffline(r *Registry) {
	r.Register("offline.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		return xaiLLMProcessor(spec, cfg.Providers.Offline.LLMEndpoint, offlineBearer, r.offlineLLMBreaker, "offline.llm")
	})
	registerOfflineSpeech(r)
}
