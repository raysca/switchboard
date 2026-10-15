package server

import (
	"testing"
	"time"
)

func TestProviderAvailabilityAnswersTheFlowPolicyFromTheConfig(t *testing.T) {
	c := DefaultConfig(ProfileProduction)
	c.Audio.Backgrounds = map[string]CueConfig{"office": {File: "x.wav"}, "cafe": {File: "y.wav"}}
	if p := policyFrom(c); p.AllowFlowBackground() || len(p.Backgrounds()) != 2 || p.Backgrounds()[0] != "cafe" || p.MinUserTimeout() != 0 {
		t.Fatalf("production defaults: allow=%v names=%v min=%v", p.AllowFlowBackground(), p.Backgrounds(), p.MinUserTimeout())
	}
	c.Audio.AllowFlowBackground, c.DTMF.Aggregate = true, true
	if p := policyFrom(c); !p.AllowFlowBackground() || p.MinUserTimeout() != 2*time.Second {
		t.Fatalf("allow=%v min=%v", p.AllowFlowBackground(), p.MinUserTimeout())
	}
	c.DTMF.AggregateTimeout = 5 * time.Second
	if p := policyFrom(c); p.MinUserTimeout() != 5*time.Second {
		t.Fatalf("min=%v", p.MinUserTimeout())
	}
}
