package server

import (
	"testing"
	"time"
)

func TestProviderAvailabilityAnswersTheAgentPolicyFromTheConfig(t *testing.T) {
	c := DefaultConfig(ProfileProduction)
	c.Audio.Backgrounds = map[string]CueConfig{"office": {File: "x.wav"}, "cafe": {File: "y.wav"}}
	if p := policyFrom(c); p.AllowAgentBackground() || len(p.Backgrounds()) != 2 || p.Backgrounds()[0] != "cafe" || p.MinUserTimeout() != 0 {
		t.Fatalf("production defaults: allow=%v names=%v min=%v", p.AllowAgentBackground(), p.Backgrounds(), p.MinUserTimeout())
	}
	c.Audio.AllowAgentBackground, c.DTMF.Aggregate = true, true
	if p := policyFrom(c); !p.AllowAgentBackground() || p.MinUserTimeout() != 2*time.Second {
		t.Fatalf("allow=%v min=%v", p.AllowAgentBackground(), p.MinUserTimeout())
	}
	c.DTMF.AggregateTimeout = 5 * time.Second
	if p := policyFrom(c); p.MinUserTimeout() != 5*time.Second {
		t.Fatalf("min=%v", p.MinUserTimeout())
	}
}
