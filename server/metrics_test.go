package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/observe"
	"govoice/providers/google"
	"govoice/providers/openai"
	"govoice/providers/xai"
)

func TestMetrics_ExpositionFormatAndBoundedLabels(t *testing.T) {
	m := NewMetrics()

	// Simulate increments
	m.ActiveSessions.Store(5)
	m.SessionsStartedTotal.Add(10)
	m.SessionsStoppedTotal.Add(5)
	m.AdmissionRejectsLimit.Add(2)
	m.LeakedWorkTotal.Add(1)
	m.LeakedSessionsTotal.Add(1)
	m.LLMCircuitStateGoogle.Store(0)
	m.LLMCircuitStateXAI.Store(1)

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	output := buf.String()

	requiredMetrics := []string{
		"voice_active_sessions 5",
		"voice_sessions_started_total 10",
		"voice_sessions_stopped_total 5",
		"voice_admission_rejects_total{reason=\"session_limit\"} 2",
		"voice_leaked_work_total 1",
		"voice_leaked_sessions_total 1",
		"voice_llm_circuit_state{provider=\"google\"} 0",
		"voice_llm_circuit_state{provider=\"xai\"} 1",
		"voice_process_goroutines",
		"voice_process_heap_bytes",
	}

	for _, req := range requiredMetrics {
		if !strings.Contains(output, req) {
			t.Fatalf("missing expected metric line %q in output:\n%s", req, output)
		}
	}

	// Verify absence of session_id labels
	if strings.Contains(output, "session_id") {
		t.Fatal("metrics output must not contain unbounded session_id labels")
	}
}

func TestLatencyHistogramBoundedLabels(t *testing.T) {
	m := NewMetrics()
	m.ObserveLatency(observe.StageVoiceToVoice, 250*time.Millisecond)
	m.ObserveLatency("bogus-session-123", time.Second) // unknown stage ignored: no label growth
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`voice_latency_seconds_bucket{stage="voice_to_voice",le="0.2"} 0`,
		`voice_latency_seconds_bucket{stage="voice_to_voice",le="0.3"} 1`,
		`voice_latency_seconds_count{stage="voice_to_voice"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "bogus") {
		t.Error("unbounded label leaked")
	}
}

type ttsCountsStub struct{ req, errs, retries, stale uint64 }

func (ttsCountsStub) ProcessFrame(context.Context, core.Frame, core.Direction, core.Emitter) error {
	return nil
}
func (s ttsCountsStub) TTSCounts() (uint64, uint64, uint64, uint64) {
	return s.req, s.errs, s.retries, s.stale
}

// Any TTS processor reporting counters (xai, ttsunit-based cartesia/elevenlabs,
// google) is exported, also when wrapped by the failover.
func TestRecordProcessorMetrics_TTSCountersIncludingFailoverCandidates(t *testing.T) {
	s := &Server{metrics: NewMetrics()}
	fo, err := core.NewTTSFailover(core.TTSFailoverConfig{Candidates: []core.TTSCandidate{
		{Provider: "a", Proc: ttsCountsStub{1, 2, 3, 4}},
		{Provider: "b", Proc: ttsCountsStub{10, 20, 30, 40}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.recordProcessorMetrics(fo)
	s.recordProcessorMetrics(ttsCountsStub{100, 200, 300, 400})
	m := s.metrics
	if m.TTSRequestsTotal.Load() != 111 || m.TTSErrorsTotal.Load() != 222 || m.TTSRetriesTotal.Load() != 333 || m.TTSStaleAudioDiscarded.Load() != 444 {
		t.Fatalf("req=%d err=%d retries=%d stale=%d", m.TTSRequestsTotal.Load(), m.TTSErrorsTotal.Load(), m.TTSRetriesTotal.Load(), m.TTSStaleAudioDiscarded.Load())
	}
}

var (
	_ core.LLMCounters = (*google.LLMProcessor)(nil)
	_ core.LLMCounters = (*xai.Processor)(nil)
	_ core.LLMCounters = (*openai.Processor)(nil)
	_ core.STTCounters = (*google.STTProcessor)(nil)
	_ core.TTSCounters = (*google.TTSProcessor)(nil)
)

// Provider LLM/STT counters are exported plain and wrapped by the failover.
func TestRecordProcessorMetrics_LLMAndSTTCountersIncludingFailoverCandidates(t *testing.T) {
	s := &Server{metrics: NewMetrics()}
	g, x, o := &google.LLMProcessor{}, &xai.Processor{}, &openai.Processor{}
	g.Requests.Store(1)
	g.Errors.Store(2)
	g.Retries.Store(3)
	g.Stale.Store(4)
	x.Requests.Store(10)
	x.Stale.Store(40)
	o.Requests.Store(100)
	o.Errors.Store(200)
	llm, err := core.NewLLMFailover(core.LLMFailoverConfig{Candidates: []core.LLMCandidate{{Provider: "xai", Proc: x}, {Provider: "openai", Proc: o}}})
	if err != nil {
		t.Fatal(err)
	}
	gs1, gs2 := &google.STTProcessor{}, &google.STTProcessor{}
	gs1.RotationsCount.Store(1)
	gs1.RotationFailures.Store(2)
	gs1.Errors.Store(4)
	gs2.RotationsCount.Store(10)
	gs2.Errors.Store(40)
	stt, err := core.NewSTTFailover(core.STTFailoverConfig{Candidates: []core.STTCandidate{{Provider: "google", Proc: gs2}, {Provider: "other", Proc: &coretest.PassthroughProcessor{}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []core.Processor{g, llm, gs1, stt} {
		s.recordProcessorMetrics(p)
	}
	m := s.metrics
	if m.LLMRequestsTotal.Load() != 111 || m.LLMErrorsTotal.Load() != 202 || m.LLMRetriesTotal.Load() != 3 || m.LLMStaleDeltasDiscarded.Load() != 44 {
		t.Fatalf("llm req=%d err=%d retries=%d stale=%d", m.LLMRequestsTotal.Load(), m.LLMErrorsTotal.Load(), m.LLMRetriesTotal.Load(), m.LLMStaleDeltasDiscarded.Load())
	}
	if m.STTRotationsTotal.Load() != 11 || m.STTRotationFailuresTotal.Load() != 2 || m.STTErrorsTotal.Load() != 44 {
		t.Fatalf("stt rot=%d fail=%d err=%d", m.STTRotationsTotal.Load(), m.STTRotationFailuresTotal.Load(), m.STTErrorsTotal.Load())
	}
}
