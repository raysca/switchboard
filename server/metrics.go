package server

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics holds the full suite of V1 product pipeline telemetry matching spec §35.
// Labels avoid high-cardinality session_id values.
type Metrics struct {
	StartTime time.Time

	// Runtime Metrics
	ActiveSessions            atomic.Int64
	SessionsStartedTotal      atomic.Uint64
	SessionsStoppedTotal      atomic.Uint64
	SessionsFailedTotal       atomic.Uint64
	ContextEvictionsTotal     atomic.Uint64
	MaxDurationTermTotal      atomic.Uint64
	LeakedWorkTotal           atomic.Uint64
	LeakedSessionsTotal       atomic.Uint64
	AdmissionRejectsLimit     atomic.Uint64
	AdmissionRejectsPrincipal atomic.Uint64
	AdmissionRejectsVAD       atomic.Uint64

	// VAD Metrics
	VADStaleChunksTotal    atomic.Uint64
	VADDroppedChunksTotal  atomic.Uint64
	VADActiveStreams       atomic.Int64
	VADWorkerRestartsTotal atomic.Uint64

	// STT Metrics
	STTErrorsTotal           atomic.Uint64
	STTRotationsTotal        atomic.Uint64
	STTRotationFailuresTotal atomic.Uint64
	STTDeduplicatedTotal     atomic.Uint64
	STTCircuitStateGoogle    atomic.Int64
	STTCircuitStateXAI       atomic.Int64

	// LLM Metrics
	LLMRequestsTotal        atomic.Uint64
	LLMErrorsTotal          atomic.Uint64
	LLMRetriesTotal         atomic.Uint64
	LLMStaleDeltasDiscarded atomic.Uint64
	LLMCircuitStateGoogle   atomic.Int64 // 0=Closed, 1=Open, 2=HalfOpen
	LLMCircuitStateXAI      atomic.Int64

	// TTS Metrics
	TTSRequestsTotal       atomic.Uint64
	TTSErrorsTotal         atomic.Uint64
	TTSRetriesTotal        atomic.Uint64
	TTSStaleAudioDiscarded atomic.Uint64
	TTSCircuitStateGoogle  atomic.Int64
	TTSCircuitStateXAI     atomic.Int64

	// Transport Metrics
	ConnectedClients       atomic.Int64
	AuthFailuresTotal      atomic.Uint64
	ReplayRejectionsTotal  atomic.Uint64
	OriginRejectionsTotal  atomic.Uint64
	DisconnectsNormal      atomic.Uint64
	DisconnectsClientError atomic.Uint64
	DisconnectsServerError atomic.Uint64
	DisconnectsTimeout     atomic.Uint64
	FramesInTotal          atomic.Uint64
	FramesOutTotal         atomic.Uint64
	BytesInTotal           atomic.Uint64
	BytesOutTotal          atomic.Uint64
	FlushAcksTotal         atomic.Uint64
	FlushTimeoutsTotal     atomic.Uint64
	SlowClientTermTotal    atomic.Uint64
	ProtocolErrorsTotal    atomic.Uint64

	// Operational and Latency Tracking
	TurnsTotal     atomic.Uint64
	latencyMu      sync.Mutex
	latencySamples []float64
	latencyIdx     int
}

// RecordTurnLatency records turn processing latency in milliseconds and advances turn count.
func (m *Metrics) RecordTurnLatency(latencyMs float64) {
	if latencyMs < 0 || math.IsNaN(latencyMs) || math.IsInf(latencyMs, 0) {
		return
	}
	m.TurnsTotal.Add(1)
	m.latencyMu.Lock()
	const maxSamples = 1024
	if len(m.latencySamples) < maxSamples {
		m.latencySamples = append(m.latencySamples, latencyMs)
	} else {
		m.latencySamples[m.latencyIdx] = latencyMs
		m.latencyIdx = (m.latencyIdx + 1) % maxSamples
	}
	m.latencyMu.Unlock()
}

// LatencyStats returns average and 95th percentile latency in milliseconds across the bounded sample window.
func (m *Metrics) LatencyStats() (avg float64, p95 float64) {
	m.latencyMu.Lock()
	n := len(m.latencySamples)
	if n == 0 {
		m.latencyMu.Unlock()
		return 0, 0
	}
	copied := make([]float64, n)
	copy(copied, m.latencySamples)
	m.latencyMu.Unlock()

	var sum float64
	for _, v := range copied {
		sum += v
	}
	avg = sum / float64(n)
	sort.Float64s(copied)
	p95Idx := int(float64(n-1) * 0.95)
	p95 = copied[p95Idx]
	return avg, p95
}

// ErrorsTotal returns the total count of failures and provider/protocol errors.
func (m *Metrics) ErrorsTotal() uint64 {
	return m.SessionsFailedTotal.Load() +
		m.STTErrorsTotal.Load() +
		m.LLMErrorsTotal.Load() +
		m.TTSErrorsTotal.Load() +
		m.ProtocolErrorsTotal.Load() +
		m.AuthFailuresTotal.Load()
}

// NewMetrics initializes runtime metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		StartTime: time.Now(),
	}
}

// Handler returns an HTTP handler serving Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.WritePrometheus(w)
	})
}

// WritePrometheus formats and writes all metrics in OpenMetrics/Prometheus exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	numGoroutines := runtime.NumGoroutine()

	// Runtime
	fmt.Fprintf(w, "# HELP voice_active_sessions Number of currently active voice sessions.\n")
	fmt.Fprintf(w, "# TYPE voice_active_sessions gauge\n")
	fmt.Fprintf(w, "voice_active_sessions %d\n", m.ActiveSessions.Load())

	fmt.Fprintf(w, "# HELP voice_sessions_started_total Total number of voice sessions started.\n")
	fmt.Fprintf(w, "# TYPE voice_sessions_started_total counter\n")
	fmt.Fprintf(w, "voice_sessions_started_total %d\n", m.SessionsStartedTotal.Load())

	fmt.Fprintf(w, "# HELP voice_sessions_stopped_total Total number of voice sessions stopped cleanly.\n")
	fmt.Fprintf(w, "# TYPE voice_sessions_stopped_total counter\n")
	fmt.Fprintf(w, "voice_sessions_stopped_total %d\n", m.SessionsStoppedTotal.Load())

	fmt.Fprintf(w, "# HELP voice_sessions_failed_total Total number of voice sessions that failed.\n")
	fmt.Fprintf(w, "# TYPE voice_sessions_failed_total counter\n")
	fmt.Fprintf(w, "voice_sessions_failed_total %d\n", m.SessionsFailedTotal.Load())

	fmt.Fprintf(w, "# HELP voice_admission_rejects_total Admission rejections partitioned by reason.\n")
	fmt.Fprintf(w, "# TYPE voice_admission_rejects_total counter\n")
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"session_limit\"} %d\n", m.AdmissionRejectsLimit.Load())
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"principal_limit\"} %d\n", m.AdmissionRejectsPrincipal.Load())
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"vad_unavailable\"} %d\n", m.AdmissionRejectsVAD.Load())

	fmt.Fprintf(w, "# HELP voice_context_evictions_total Total number of oldest turn pairs evicted.\n")
	fmt.Fprintf(w, "# TYPE voice_context_evictions_total counter\n")
	fmt.Fprintf(w, "voice_context_evictions_total %d\n", m.ContextEvictionsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_max_duration_terminations_total Total calls terminated due to max duration guard.\n")
	fmt.Fprintf(w, "# TYPE voice_max_duration_terminations_total counter\n")
	fmt.Fprintf(w, "voice_max_duration_terminations_total %d\n", m.MaxDurationTermTotal.Load())

	fmt.Fprintf(w, "# HELP voice_leaked_work_total Total number of tasks that ignored cancellation.\n")
	fmt.Fprintf(w, "# TYPE voice_leaked_work_total counter\n")
	fmt.Fprintf(w, "voice_leaked_work_total %d\n", m.LeakedWorkTotal.Load())

	fmt.Fprintf(w, "# HELP voice_leaked_sessions_total Total number of sessions marked as having leaked tasks.\n")
	fmt.Fprintf(w, "# TYPE voice_leaked_sessions_total counter\n")
	fmt.Fprintf(w, "voice_leaked_sessions_total %d\n", m.LeakedSessionsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_process_goroutines Number of running goroutines.\n")
	fmt.Fprintf(w, "# TYPE voice_process_goroutines gauge\n")
	fmt.Fprintf(w, "voice_process_goroutines %d\n", numGoroutines)

	fmt.Fprintf(w, "# HELP voice_process_heap_bytes Number of heap bytes allocated.\n")
	fmt.Fprintf(w, "# TYPE voice_process_heap_bytes gauge\n")
	fmt.Fprintf(w, "voice_process_heap_bytes %d\n", mem.HeapAlloc)

	// VAD
	fmt.Fprintf(w, "# HELP voice_vad_active_streams Current active VAD streams.\n")
	fmt.Fprintf(w, "# TYPE voice_vad_active_streams gauge\n")
	fmt.Fprintf(w, "voice_vad_active_streams %d\n", m.VADActiveStreams.Load())

	fmt.Fprintf(w, "# HELP voice_vad_stale_chunks_total Stale VAD chunks discarded.\n")
	fmt.Fprintf(w, "# TYPE voice_vad_stale_chunks_total counter\n")
	fmt.Fprintf(w, "voice_vad_stale_chunks_total %d\n", m.VADStaleChunksTotal.Load())

	fmt.Fprintf(w, "# HELP voice_vad_dropped_chunks_total VAD chunks dropped due to worker overload.\n")
	fmt.Fprintf(w, "# TYPE voice_vad_dropped_chunks_total counter\n")
	fmt.Fprintf(w, "voice_vad_dropped_chunks_total %d\n", m.VADDroppedChunksTotal.Load())

	fmt.Fprintf(w, "# HELP voice_vad_worker_restarts_total Worker processes restarted.\n")
	fmt.Fprintf(w, "# TYPE voice_vad_worker_restarts_total counter\n")
	fmt.Fprintf(w, "voice_vad_worker_restarts_total %d\n", m.VADWorkerRestartsTotal.Load())

	// STT

	fmt.Fprintf(w, "# HELP voice_stt_errors_total Total STT provider errors.\n")
	fmt.Fprintf(w, "# TYPE voice_stt_errors_total counter\n")
	fmt.Fprintf(w, "voice_stt_errors_total %d\n", m.STTErrorsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_stt_rotations_total Total pre-limit stream rotations completed.\n")
	fmt.Fprintf(w, "# TYPE voice_stt_rotations_total counter\n")
	fmt.Fprintf(w, "voice_stt_rotations_total %d\n", m.STTRotationsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_stt_rotation_failures_total Total pre-limit stream rotations that failed.\n")
	fmt.Fprintf(w, "# TYPE voice_stt_rotation_failures_total counter\n")
	fmt.Fprintf(w, "voice_stt_rotation_failures_total %d\n", m.STTRotationFailuresTotal.Load())

	fmt.Fprintf(w, "# HELP voice_stt_deduplicated_results_total Deduplicated STT results ignored.\n")
	fmt.Fprintf(w, "# TYPE voice_stt_deduplicated_results_total counter\n")
	fmt.Fprintf(w, "voice_stt_deduplicated_results_total %d\n", m.STTDeduplicatedTotal.Load())

	fmt.Fprintf(w, "# HELP voice_stt_circuit_state STT circuit state (0=closed, 1=open, 2=half_open).\n")
	fmt.Fprintf(w, "# TYPE voice_stt_circuit_state gauge\n")
	fmt.Fprintf(w, "voice_stt_circuit_state{provider=\"google\"} %d\n", m.STTCircuitStateGoogle.Load())
	fmt.Fprintf(w, "voice_stt_circuit_state{provider=\"xai\"} %d\n", m.STTCircuitStateXAI.Load())

	// LLM
	fmt.Fprintf(w, "# HELP voice_llm_requests_total Total LLM turn requests.\n")
	fmt.Fprintf(w, "# TYPE voice_llm_requests_total counter\n")
	fmt.Fprintf(w, "voice_llm_requests_total %d\n", m.LLMRequestsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_llm_errors_total Total LLM provider errors.\n")
	fmt.Fprintf(w, "# TYPE voice_llm_errors_total counter\n")
	fmt.Fprintf(w, "voice_llm_errors_total %d\n", m.LLMErrorsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_llm_retries_total Total LLM retry attempts.\n")
	fmt.Fprintf(w, "# TYPE voice_llm_retries_total counter\n")
	fmt.Fprintf(w, "voice_llm_retries_total %d\n", m.LLMRetriesTotal.Load())

	fmt.Fprintf(w, "# HELP voice_llm_stale_deltas_discarded_total Late LLM deltas discarded.\n")
	fmt.Fprintf(w, "# TYPE voice_llm_stale_deltas_discarded_total counter\n")
	fmt.Fprintf(w, "voice_llm_stale_deltas_discarded_total %d\n", m.LLMStaleDeltasDiscarded.Load())

	fmt.Fprintf(w, "# HELP voice_llm_circuit_state Circuit state (0=closed, 1=open, 2=half_open).\n")
	fmt.Fprintf(w, "# TYPE voice_llm_circuit_state gauge\n")
	fmt.Fprintf(w, "voice_llm_circuit_state{provider=\"google\"} %d\n", m.LLMCircuitStateGoogle.Load())
	fmt.Fprintf(w, "voice_llm_circuit_state{provider=\"xai\"} %d\n", m.LLMCircuitStateXAI.Load())

	// TTS
	fmt.Fprintf(w, "# HELP voice_tts_requests_total Total TTS synthesis requests.\n")
	fmt.Fprintf(w, "# TYPE voice_tts_requests_total counter\n")
	fmt.Fprintf(w, "voice_tts_requests_total %d\n", m.TTSRequestsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_tts_errors_total Total TTS synthesis errors.\n")
	fmt.Fprintf(w, "# TYPE voice_tts_errors_total counter\n")
	fmt.Fprintf(w, "voice_tts_errors_total %d\n", m.TTSErrorsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_tts_retries_total Total TTS retry attempts.\n")
	fmt.Fprintf(w, "# TYPE voice_tts_retries_total counter\n")
	fmt.Fprintf(w, "voice_tts_retries_total %d\n", m.TTSRetriesTotal.Load())

	fmt.Fprintf(w, "# HELP voice_tts_stale_audio_discarded_total Stale TTS audio frames discarded.\n")
	fmt.Fprintf(w, "# TYPE voice_tts_stale_audio_discarded_total counter\n")
	fmt.Fprintf(w, "voice_tts_stale_audio_discarded_total %d\n", m.TTSStaleAudioDiscarded.Load())

	fmt.Fprintf(w, "# HELP voice_tts_circuit_state TTS circuit state (0=closed, 1=open, 2=half_open).\n")
	fmt.Fprintf(w, "# TYPE voice_tts_circuit_state gauge\n")
	fmt.Fprintf(w, "voice_tts_circuit_state{provider=\"google\"} %d\n", m.TTSCircuitStateGoogle.Load())
	fmt.Fprintf(w, "voice_tts_circuit_state{provider=\"xai\"} %d\n", m.TTSCircuitStateXAI.Load())

	// Transport

	fmt.Fprintf(w, "# HELP voice_transport_connected_clients Number of connected clients.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_connected_clients gauge\n")
	fmt.Fprintf(w, "voice_transport_connected_clients %d\n", m.ConnectedClients.Load())

	fmt.Fprintf(w, "# HELP voice_transport_auth_failures_total Token authentication failures.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_auth_failures_total counter\n")
	fmt.Fprintf(w, "voice_transport_auth_failures_total %d\n", m.AuthFailuresTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_replay_rejections_total Token replay attempts rejected.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_replay_rejections_total counter\n")
	fmt.Fprintf(w, "voice_transport_replay_rejections_total %d\n", m.ReplayRejectionsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_origin_rejections_total Origin check failures.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_origin_rejections_total counter\n")
	fmt.Fprintf(w, "voice_transport_origin_rejections_total %d\n", m.OriginRejectionsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_disconnects_total Disconnects by reason.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_disconnects_total counter\n")
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"normal\"} %d\n", m.DisconnectsNormal.Load())
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"client_error\"} %d\n", m.DisconnectsClientError.Load())
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"server_error\"} %d\n", m.DisconnectsServerError.Load())
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"timeout\"} %d\n", m.DisconnectsTimeout.Load())

	fmt.Fprintf(w, "# HELP voice_transport_frames_in_total Audio and control frames received.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_frames_in_total counter\n")
	fmt.Fprintf(w, "voice_transport_frames_in_total %d\n", m.FramesInTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_frames_out_total Audio and control frames sent.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_frames_out_total counter\n")
	fmt.Fprintf(w, "voice_transport_frames_out_total %d\n", m.FramesOutTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_bytes_in_total Inbound network bytes.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_bytes_in_total counter\n")
	fmt.Fprintf(w, "voice_transport_bytes_in_total %d\n", m.BytesInTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_bytes_out_total Outbound network bytes.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_bytes_out_total counter\n")
	fmt.Fprintf(w, "voice_transport_bytes_out_total %d\n", m.BytesOutTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_flush_acks_total Transport flush acknowledgements.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_flush_acks_total counter\n")
	fmt.Fprintf(w, "voice_transport_flush_acks_total %d\n", m.FlushAcksTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_flush_timeouts_total Transport flush barrier timeouts.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_flush_timeouts_total counter\n")
	fmt.Fprintf(w, "voice_transport_flush_timeouts_total %d\n", m.FlushTimeoutsTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_slow_client_terminations_total Slow client terminations.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_slow_client_terminations_total counter\n")
	fmt.Fprintf(w, "voice_transport_slow_client_terminations_total %d\n", m.SlowClientTermTotal.Load())

	fmt.Fprintf(w, "# HELP voice_transport_protocol_errors_total Wire protocol errors.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_protocol_errors_total counter\n")
	fmt.Fprintf(w, "voice_transport_protocol_errors_total %d\n", m.ProtocolErrorsTotal.Load())
}
