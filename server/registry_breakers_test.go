package server

import (
	"bytes"
	"strings"
	"testing"

	"govoice/core/resilience"
)

func openBreaker(b *resilience.Breaker) {
	for range resilience.DefaultConfig().FailureThreshold {
		b.RecordFailure()
	}
}

// Every registered provider stage has its own breaker, so failover can check the one
// the processor records on; openai has one per base URL instead.
func TestEveryProviderStageHasItsOwnBreaker(t *testing.T) {
	r := NewRegistry()
	seen := map[*resilience.Breaker]string{}
	for name := range r.factories {
		provider, stage, _ := strings.Cut(name, ".")
		if provider == "openai" || stage == "turn" {
			continue
		}
		b := r.breaker(stage, provider)
		if b == nil {
			t.Errorf("%s has no breaker", name)
			continue
		}
		if other, dup := seen[b]; dup {
			t.Errorf("%s shares a breaker with %s", name, other)
		}
		seen[b] = name
		if r.breaker(stage, provider) != b {
			t.Errorf("%s: second lookup returned another breaker", name)
		}
	}
	if r.breaker("tts", "deepgram") != nil || r.breaker("llm", silenceProvider) != nil || r.breaker("stt", "nope") != nil {
		t.Fatal("a provider stage without a breaker got one")
	}
}

func TestObservedBreakersExportAndReport(t *testing.T) {
	r := NewRegistry()
	nv := r.breaker("llm", "nvidia") // created before metrics are bound
	m := NewMetrics()
	r.BindMetrics(m)
	var reports []string
	report := func(stage, provider string, st resilience.State) {
		reports = append(reports, provider+"_"+stage+"="+string(st))
	}
	r.onCircuit.Store(&report)

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	for _, want := range []string{
		`voice_llm_circuit_state{provider="google"} 0`,
		`voice_stt_circuit_state{provider="xai"} 0`,
		`voice_tts_circuit_state{provider="echo",endpoint="simulated"} 0`,
		`voice_stt_circuit_state{provider="silence",endpoint="text"} 0`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %s", want)
		}
	}

	openBreaker(r.breaker("llm", "google"))
	openBreaker(nv)
	snap := m.circuits.snapshot()
	if snap[circuitKey{"llm", "google", ""}] != 1 {
		t.Errorf("google gauge: %v", snap)
	}
	if snap[circuitKey{"llm", "nvidia", "integrate.api.nvidia.com"}] != 1 {
		t.Errorf("a breaker created before BindMetrics does not export: %v", snap)
	}
	if !strings.Contains(strings.Join(reports, " "), "google_llm=OPEN") {
		t.Errorf("no timeline report: %v", reports)
	}
}

// deepgram, cartesia, elevenlabs and openai report nothing: no gauge, no /ready key, no timeline event.
func TestUnobservedBreakersReportNothing(t *testing.T) {
	r := NewRegistry()
	r.BindMetrics(NewMetrics())
	reported := false
	report := func(string, string, resilience.State) { reported = true }
	r.onCircuit.Store(&report)
	for _, b := range []*resilience.Breaker{
		r.breaker("stt", "deepgram"), r.breaker("stt", "cartesia"), r.breaker("tts", "elevenlabs"),
		r.openaiBreaker("https://tenant.example/v1"),
	} {
		openBreaker(b)
		if !b.WouldRefuse() {
			t.Fatal("breaker did not open")
		}
	}
	for k := range r.metrics.Load().circuits.snapshot() {
		switch k.provider {
		case "deepgram", "cartesia", "elevenlabs", "openai":
			t.Errorf("unobserved breaker exported %v", k)
		}
	}
	if reported {
		t.Error("unobserved breaker reported to call timelines")
	}
	if r.breaker("stt", "cartesia") == r.breaker("tts", "cartesia") {
		t.Error("cartesia stt and tts share a breaker")
	}
}
