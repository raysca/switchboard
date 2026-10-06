package core

import (
	"context"
	"testing"
)

type capableMockProcessor struct {
	caps ProcessorCapabilities
}

func (c *capableMockProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return e.Push(f)
}

func (c *capableMockProcessor) Capabilities() ProcessorCapabilities {
	return c.caps
}

func TestProcessorCapabilities_DiscoveryAndQuery(t *testing.T) {
	procSTT := &capableMockProcessor{
		caps: ProcessorCapabilities{
			Name:         "mock-stt",
			Version:      "v1.0",
			Capabilities: []ProcessorCapability{CapabilitySpeechToText, CapabilityAudioInput},
			SideEffects:  true,
		},
	}
	procVAD := &capableMockProcessor{
		caps: ProcessorCapabilities{
			Name:         "mock-vad",
			Capabilities: []ProcessorCapability{CapabilityVAD},
			SideEffects:  false,
		},
	}
	procPlain := &testFnProcessor{}

	pipe, err := NewPipeline(procSTT, procVAD, procPlain)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	if !HasCapability(procSTT, CapabilitySpeechToText) {
		t.Errorf("expected procSTT to have SpeechToText capability")
	}
	if HasCapability(procSTT, CapabilityVAD) {
		t.Errorf("did not expect procSTT to have VAD capability")
	}

	matchesSTT := pipe.FindProcessorsWithCapability(CapabilitySpeechToText)
	if len(matchesSTT) != 1 || matchesSTT[0] != procSTT {
		t.Errorf("unexpected STT matches: %v", matchesSTT)
	}

	// Plain processor has default empty capabilities
	plainCaps := GetProcessorCapabilities(procPlain)
	if plainCaps.Name != "unspecified" || len(plainCaps.Capabilities) != 0 {
		t.Errorf("unexpected plainCaps: %+v", plainCaps)
	}
}
