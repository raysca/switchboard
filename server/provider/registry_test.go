package provider

import (
	"reflect"
	"testing"
)

func TestRegistryHasStableExplicitCapabilities(t *testing.T) {
	r := NewRegistry(Definitions())
	got, ok := r.Provider("xai")
	if !ok {
		t.Fatal("xai provider missing")
	}
	// xAI speech is experimental and only compiled with the xaispeech tag.
	if !reflect.DeepEqual(got.Capabilities, xaiCapabilities) || got.Capabilities[0] != CapabilityLLM {
		t.Fatalf("capabilities=%v", got.Capabilities)
	}
	if r.Supports("xai", CapabilityMCP) {
		t.Fatal("xAI must not be treated as an MCP provider")
	}
	if _, ok := r.Provider("unknown"); ok {
		t.Fatal("unknown provider was inferred")
	}
}

func TestDeepgramIsSTTOnly(t *testing.T) {
	r := NewRegistry(Definitions())
	if !r.Supports("deepgram", CapabilitySTT) || r.Supports("deepgram", CapabilityLLM) || r.Supports("deepgram", CapabilityTTS) {
		t.Fatal("deepgram must offer STT only")
	}
}

func TestCartesiaIsTTSOnly(t *testing.T) {
	r := NewRegistry(Definitions())
	if !r.Supports("cartesia", CapabilityTTS) || r.Supports("cartesia", CapabilityLLM) || r.Supports("cartesia", CapabilitySTT) {
		t.Fatal("cartesia must offer TTS only")
	}
}

func TestElevenLabsIsTTSOnly(t *testing.T) {
	r := NewRegistry(Definitions())
	if !r.Supports("elevenlabs", CapabilityTTS) || r.Supports("elevenlabs", CapabilityLLM) || r.Supports("elevenlabs", CapabilitySTT) {
		t.Fatal("elevenlabs must offer TTS only")
	}
}

func TestOpenAIIsLLMOnly(t *testing.T) {
	r := NewRegistry(Definitions())
	if !r.Supports("openai", CapabilityLLM) || r.Supports("openai", CapabilitySTT) || r.Supports("openai", CapabilityTTS) {
		t.Fatal("openai must offer LLM only")
	}
}
