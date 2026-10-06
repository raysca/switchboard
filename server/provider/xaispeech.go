//go:build xaispeech

package provider

func init() { xaiCapabilities = []Capability{CapabilityLLM, CapabilitySTT, CapabilityTTS} }
