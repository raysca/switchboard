package server

import "testing"

func TestOfflineRegisteredAndBuildsWithoutAKey(t *testing.T) {
	reg := NewRegistry()
	cfg := DefaultConfig(ProfileDevelopment)
	stages := map[string]StageSpec{"llm": {Model: "govoice-test"}}
	if xaiSpeechEnabled {
		stages["stt"], stages["tts"] = StageSpec{Model: "s"}, StageSpec{Voice: "af_heart"}
	}
	for stage, spec := range stages {
		if !reg.Supports("offline", stage) {
			t.Errorf("offline.%s is not registered", stage)
			continue
		}
		if _, err := reg.factories["offline."+stage](cfg, spec); err != nil { // no Secret: offline reads none
			t.Errorf("offline.%s with no secret: %v", stage, err)
		}
	}
}

func TestOfflineSpeechAbsentWithoutXAISpeechTag(t *testing.T) {
	if xaiSpeechEnabled {
		t.Skip("built with xaispeech")
	}
	reg := NewRegistry()
	if reg.Supports("offline", "stt") || reg.Supports("offline", "tts") {
		t.Fatal("offline STT/TTS registered in a build without xaispeech")
	}
	if !reg.Supports("offline", "llm") {
		t.Fatal("offline.llm must exist on every build")
	}
}

func TestOfflineBreakersAreSeparate(t *testing.T) {
	reg := NewRegistry()
	if reg.offlineLLMBreaker == reg.xaiLLMBreaker || reg.offlineSTTBreaker == reg.xaiSTTBreaker || reg.offlineTTSBreaker == reg.xaiTTSBreaker {
		t.Fatal("offline shares a breaker with xai: a local outage would open the real provider's circuit")
	}
}

func TestOfflineDefaultEndpoints(t *testing.T) {
	c := DefaultConfig(ProfileDevelopment).Providers.Offline
	if c.LLMEndpoint != "http://127.0.0.1:11434/v1/responses" || c.STTEndpoint != "ws://127.0.0.1:8099/v1/stt" || c.TTSEndpoint != "http://127.0.0.1:8099/v1/tts" {
		t.Fatalf("defaults: %+v", c)
	}
}
