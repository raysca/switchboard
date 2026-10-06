package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

func TestDiscoveryIsDeterministicBoundedAndSanitized(t *testing.T) {
	adapter := DiscoveryFunc(func(_ context.Context, _ Capability) (DiscoveryResult, error) {
		return DiscoveryResult{Models: []Option{{ID: "z-model", DisplayName: "Z"}, {ID: "a-model", DisplayName: "A"}}, Voices: []Option{{ID: "voice-2", DisplayName: "Second"}, {ID: "voice-1", DisplayName: "First"}}}, nil
	})
	r := NewRegistry(Definitions())
	first, err := r.Discover(context.Background(), "google", CapabilityTTS, adapter)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Discover(context.Background(), "google", CapabilityTTS, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.Voices[0].ID != "voice-1" || first.Models[0].ID != "a-model" {
		t.Fatalf("discovery not deterministic: %#v", first)
	}
	if _, err := r.Discover(context.Background(), "google", CapabilityMCP, adapter); err != ErrUnsupportedCapability {
		t.Fatalf("unsupported discovery err=%v", err)
	}
	large := DiscoveryFunc(func(context.Context, Capability) (DiscoveryResult, error) {
		return DiscoveryResult{Models: make([]Option, MaxDiscoveryResults+1)}, nil
	})
	if _, err := r.Discover(context.Background(), "google", CapabilityLLM, large); err != ErrDiscoveryLimit {
		t.Fatalf("unbounded discovery err=%v", err)
	}
	const sentinel = "provider-error-secret-sentinel"
	failed := DiscoveryFunc(func(context.Context, Capability) (DiscoveryResult, error) {
		return DiscoveryResult{}, errors.New(sentinel)
	})
	_, err = r.Discover(context.Background(), "google", CapabilityLLM, failed)
	if err != ErrDiscoveryFailed || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("provider error leaked: %v", err)
	}
	panicked := DiscoveryFunc(func(context.Context, Capability) (DiscoveryResult, error) { panic(sentinel) })
	if _, err := r.Discover(context.Background(), "google", CapabilityLLM, panicked); err != ErrDiscoveryFailed {
		t.Fatalf("discovery panic leaked: %v", err)
	}
	malformed := DiscoveryFunc(func(context.Context, Capability) (DiscoveryResult, error) {
		return DiscoveryResult{Models: []Option{{ID: "bad\nmodel", DisplayName: "Bad"}}}, nil
	})
	if _, err := r.Discover(context.Background(), "google", CapabilityLLM, malformed); err != ErrDiscoveryFailed {
		t.Fatalf("control character accepted: %v", err)
	}
}
