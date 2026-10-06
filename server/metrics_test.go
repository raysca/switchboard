package server

import (
	"bytes"
	"strings"
	"testing"
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
