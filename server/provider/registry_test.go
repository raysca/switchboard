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
