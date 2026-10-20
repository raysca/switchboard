package server

import (
	"maps"
	"slices"
	"time"

	"govoice/server/catalog"
	"govoice/server/flow"
)

// providerAvailability answers "can this build and environment run provider/service".
type providerAvailability struct {
	cat      *catalog.Catalog
	res      *catalog.Resolver
	supports func(provider, service string) bool
	policy   serverPolicy
}

// serverPolicy is flow.Policy answered from the configuration.
type serverPolicy struct {
	sounds   []string
	cues     []string
	allow    bool
	minTimer time.Duration
	manager  bool
	maxMute  time.Duration
	smart    bool
}

func (p serverPolicy) Backgrounds() []string         { return p.sounds }
func (p serverPolicy) Cues() []string                { return p.cues }
func (p serverPolicy) AllowFlowBackground() bool     { return p.allow }
func (p serverPolicy) MinUserTimeout() time.Duration { return p.minTimer }
func (p serverPolicy) TurnManager() bool             { return p.manager }
func (p serverPolicy) MaxMute() time.Duration        { return p.maxMute }
func (p serverPolicy) SmartTurn() bool               { return p.smart }

// policyFrom reads the rules a flow's call settings must respect: the sound catalogue, the operator's
// ceiling on flow backgrounds, and the DTMF window a quiet-caller wait has to exceed (idle.validate's rule).
func policyFrom(cfg *Config) serverPolicy {
	p := serverPolicy{sounds: slices.Sorted(maps.Keys(cfg.Audio.Backgrounds)), cues: slices.Sorted(maps.Keys(cfg.Audio.Cues)), allow: cfg.Audio.AllowFlowBackground,
		manager: cfg.Turn.Manager, maxMute: cfg.Turn.effectiveMaxMute(), smart: slices.Contains(cfg.Turn.Stop, "turn-analyzer")}
	if cfg.DTMF.Aggregate {
		p.minTimer = max(cfg.DTMF.AggregateTimeout, 2*time.Second)
	}
	return p
}

func (a providerAvailability) Backgrounds() []string         { return a.policy.Backgrounds() }
func (a providerAvailability) Cues() []string                { return a.policy.Cues() }
func (a providerAvailability) AllowFlowBackground() bool     { return a.policy.AllowFlowBackground() }
func (a providerAvailability) MinUserTimeout() time.Duration { return a.policy.MinUserTimeout() }
func (a providerAvailability) TurnManager() bool             { return a.policy.TurnManager() }
func (a providerAvailability) MaxMute() time.Duration        { return a.policy.MaxMute() }
func (a providerAvailability) SmartTurn() bool               { return a.policy.SmartTurn() }

func (a providerAvailability) Check(provider, service string) flow.Unavailable {
	p, ok := a.cat.Provider(provider)
	if !ok {
		return flow.Unavailable{Reason: "unknown_provider"}
	}
	if _, ok := p.Services[service]; !ok || !a.supports(provider, service) {
		return flow.Unavailable{Reason: "unsupported_provider"}
	}
	if st := a.res.Status(provider, service); !st.Detected {
		return flow.Unavailable{Reason: "missing_credential", MissingEnv: st.MissingEnv}
	}
	return flow.Unavailable{}
}
