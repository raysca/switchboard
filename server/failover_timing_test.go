package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
	"govoice/server/flow"
)

func TestFailoverTimingConfig(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	if b, f := llmFailoverTiming(cfg); b != 60*time.Second || f != 8*time.Second {
		t.Fatalf("defaults %v %v", b, f)
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// zero (unset) and nil fall back to the defaults
	cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken = 0, 0
	if b, f := llmFailoverTiming(cfg); b != defaultLLMFailoverBudget || f != defaultLLMFirstToken {
		t.Fatalf("zero %v %v", b, f)
	}
	if b, f := llmFailoverTiming(nil); b != defaultLLMFailoverBudget || f != defaultLLMFirstToken {
		t.Fatalf("nil %v %v", b, f)
	}
	for name, set := range map[string]func(){
		"first token >= budget":         func() { cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken = 5*time.Second, 5*time.Second },
		"first token >= default budget": func() { cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken = 0, 90*time.Second },
		"negative":                      func() { cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken = -time.Second, 0 },
	} {
		set()
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "failover.llm_") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestFirstTokenTimeoutClassIsTimeout(t *testing.T) {
	if got := llmClassify(errors.Join(errors.New("x"), core.ErrFirstTokenTimeout)); got != flow.ClassTimeout {
		t.Fatalf("class %q", got)
	}
	if !(flow.FailoverPolicy{}).Allows(flow.ClassTimeout) {
		t.Fatal("timeout must fail over by default")
	}
}

func TestFailoverPolicyOnlyTightens(t *testing.T) {
	// providers' worst cases: cartesia/elevenlabs 3 x 15 s, google tts 3 x 10 s, deepgram 3 x 5 s
	cases := []struct {
		stage         string
		base          resilience.Policy
		attempts      int
		timeout, wost time.Duration
	}{
		// tts keeps its timeout: it bounds the whole unit, a long sentence must not be cut
		{"tts", resilience.Policy{MaxAttempts: 3, AttemptTimeout: 15 * time.Second}, 2, 15 * time.Second, 30 * time.Second},
		{"tts", resilience.Policy{MaxAttempts: 3, AttemptTimeout: 10 * time.Second}, 2, 10 * time.Second, 20 * time.Second},
		{"stt", resilience.Policy{MaxAttempts: 3, AttemptTimeout: 5 * time.Second}, 2, 5 * time.Second, 10 * time.Second},
		{"stt", resilience.Policy{MaxAttempts: 1, AttemptTimeout: 2 * time.Second}, 1, 2 * time.Second, 2 * time.Second}, // never loosened
		{"tts", resilience.Policy{}, 2, 0, 0}, // zero timeout: the provider's default applies
	}
	for _, c := range cases {
		got := failoverPolicy(c.stage, c.base)
		if got.MaxAttempts != c.attempts || got.AttemptTimeout != c.timeout || time.Duration(got.MaxAttempts)*got.AttemptTimeout != c.wost {
			t.Errorf("%s %+v -> %+v", c.stage, c.base, got)
		}
	}
}

func TestBuildMarksOnlyNonLastCandidatesForFailover(t *testing.T) {
	reg := NewRegistry()
	var seen []bool
	build := func(_ string, sp StageSpec) (core.Processor, error) {
		seen = append(seen, sp.Failover)
		return &core.PassthroughProcessor{}, nil
	}
	if _, err := reg.buildTTS(nil, CallSpec{TTS: StageSpec{Provider: "google"}, TTSFallbacks: []StageSpec{{Provider: "xai"}}}, build); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.buildSTT(nil, CallSpec{STT: StageSpec{Provider: "google"}, STTFallbacks: []StageSpec{{Provider: "deepgram"}}}, build); err != nil {
		t.Fatal(err)
	}
	if want := []bool{true, false, true, false}; len(seen) != 4 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] || seen[3] != want[3] {
		t.Fatalf("Failover flags %v", seen)
	}
	seen = nil
	if _, err := reg.buildTTS(nil, CallSpec{TTS: StageSpec{Provider: "google"}}, build); err != nil || seen[0] {
		t.Fatalf("a primary without fallbacks must keep its full policy: %v %v", seen, err)
	}
}
