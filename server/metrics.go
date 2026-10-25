package server

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/server/retention"
	"govoice/smartturn"
)

// Metrics holds the full suite of V1 product pipeline telemetry matching spec §35.
// Labels avoid high-cardinality session_id values.
type Metrics struct {
	StartTime time.Time

	// OTelStats reports (dropped, failed) exported spans; nil when otel is off.
	OTelStats func() (dropped, failed uint64)

	// SmartTurnWorkers reports (up, total) smart-turn worker processes; nil when not process-isolated.
	SmartTurnWorkers func() (up, total int)

	// Runtime Metrics
	ActiveSessions        atomic.Int64
	Draining              atomic.Int64 // 1 once SIGTERM drain began
	DrainRemaining        atomic.Int64 // sessions still live during drain
	SessionsStartedTotal  atomic.Uint64
	SessionsStoppedTotal  atomic.Uint64
	SessionsFailedTotal   atomic.Uint64
	ContextEvictionsTotal atomic.Uint64
	MaxDurationTermTotal  atomic.Uint64

	ManagementAuthFailuresTotal, ManagementAuthThrottledTotal atomic.Uint64
	LeakedWorkTotal                                           atomic.Uint64
	LeakedSessionsTotal                                       atomic.Uint64
	AdmissionRejectsLimit                                     atomic.Uint64
	AdmissionRejectsPrincipal                                 atomic.Uint64
	AdmissionRejectsVAD                                       atomic.Uint64
	AdmissionRejectsAgent                                     atomic.Uint64
	AdmissionRejectsRate                                      atomic.Uint64

	// VAD Metrics
	VADStaleChunksTotal    atomic.Uint64
	VADDroppedChunksTotal  atomic.Uint64
	VADActiveStreams       atomic.Int64
	VADWorkerRestartsTotal atomic.Uint64

	// STT Metrics
	STTErrorsTotal           atomic.Uint64
	STTRotationsTotal        atomic.Uint64
	STTRotationFailuresTotal atomic.Uint64
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
	WSPingFailuresTotal    atomic.Uint64
	ProtocolErrorsTotal    atomic.Uint64

	TurnsTotal atomic.Uint64 // recorded call turns

	// circuits holds breakers that exist per endpoint (nvidia: one per NIM base URL / Riva endpoint);
	// providerErrors counts stage errors per provider (see Server.recordProcessorMetrics).
	circuits       dynamicCircuits
	providerErrors providerErrors

	latency      latencyHistograms
	llmFailovers failoverCounts
	stt          sttMetrics
	ttsFailovers failoverCounts
	// TTSFallbacksSkippedTotal counts configured TTS fallbacks left out of a call.
	TTSFallbacksSkippedTotal atomic.Uint64
	// LLMFallbacksSkippedTotal counts configured LLM fallbacks left out of a call
	// (unavailable provider, unresolvable credential or a factory error).
	LLMFallbacksSkippedTotal atomic.Uint64
	// UserMuteDroppedTotal counts user VAD/transcript frames dropped by a user mute strategy.
	UserMuteDroppedTotal atomic.Uint64
	// MinWordsNoInterimsTotal counts calls with turn.min_words whose STT sent a final with no interim before it.
	MinWordsNoInterimsTotal atomic.Uint64
	// Idle controller (per call, added when the call ends): prompts sent, calls
	// hung up for user idle, stuck-bot cancellations, calls hung up for repeated ones.
	UserIdlePromptsTotal, UserIdleHangupsTotal, BotIdleStuckTotal, BotIdleHangupsTotal atomic.Uint64
	// GreetingFailedTotal counts greetings that could not be queued (the session was closing).
	GreetingFailedTotal atomic.Uint64
	// AsyncToolTurnsTotal / AsyncToolResultsDroppedTotal mirror core.AsyncTrigger.
	AsyncToolTurnsTotal          atomic.Uint64
	AsyncToolResultsDroppedTotal atomic.Uint64

	// MCPSyncOKTotal / MCPSyncFailedTotal count background MCP server syncs.
	MCPSyncOKTotal, MCPSyncFailedTotal atomic.Uint64

	// Retention job: whether it runs, runs by result, what it removed, and when it last completed.
	RetentionEnabled                                    atomic.Bool
	RetentionIntervalSeconds                            atomic.Int64
	RetentionRunsOK, RetentionRunsFailed                atomic.Uint64
	RetentionTurnsPurgedTotal, RetentionSessionsDeleted atomic.Uint64
	// Calls erased on request: one at a time, or by caller.
	RetentionErasedCalls, RetentionErasedByCaller atomic.Uint64
	retentionLastOK                               atomic.Pointer[time.Time] // monotonic reading kept: ages are durations

	// Eval worker: jobs by outcome (evalJobResults order), eval calls in flight, runs it finished.
	evalJobs         [4]atomic.Uint64
	evalCallsActive  atomic.Int64
	evalRunsFinished atomic.Uint64

	filters filterMetrics
	summary summaryMetrics
	handoff handoffMetrics
	// completion verdicts: complete, incomplete_short, incomplete_long, no_marker.
	completion [4]atomic.Uint64
}

var completionVerdicts = [4]string{core.VerdictComplete, core.VerdictIncompleteShort, core.VerdictIncompleteLong, core.VerdictNoMarker}

func (m *Metrics) completionVerdict(v string) {
	if i := slices.Index(completionVerdicts[:], v); i >= 0 {
		m.completion[i].Add(1)
	}
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

// scalar is one unlabelled sample: its HELP and TYPE lines and its value.
type scalar struct {
	name, help, kind string
	v                int64
}

func writeScalars(w io.Writer, ss ...scalar) {
	for _, s := range ss {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", s.name, s.help, s.name, s.kind, s.name, s.v)
	}
}

func counter(name, help string, v uint64) scalar { return scalar{name, help, "counter", int64(v)} }
func gauge(name, help string, v int64) scalar    { return scalar{name, help, "gauge", v} }

// WritePrometheus formats and writes all metrics in OpenMetrics/Prometheus exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	fmt.Fprintf(w, "# HELP voice_build_info Build version and commit (value is always 1).\n# TYPE voice_build_info gauge\nvoice_build_info{version=%q,commit=%q} 1\n", Version, Commit)

	// Runtime
	writeScalars(w,
		gauge("voice_active_sessions", "Number of currently active voice sessions.", int64(m.ActiveSessions.Load())),
		gauge("voice_draining", "1 while the server is draining for shutdown.", int64(m.Draining.Load())),
		gauge("voice_drain_sessions_remaining", "Sessions still live while draining.", int64(m.DrainRemaining.Load())),
		counter("voice_sessions_started_total", "Total number of voice sessions started.", uint64(m.SessionsStartedTotal.Load())),
		counter("voice_sessions_stopped_total", "Total number of voice sessions stopped cleanly.", uint64(m.SessionsStoppedTotal.Load())),
		counter("voice_sessions_failed_total", "Total number of voice sessions that failed.", uint64(m.SessionsFailedTotal.Load())),
	)

	fmt.Fprintf(w, "# HELP voice_admission_rejects_total Admission rejections partitioned by reason.\n")
	fmt.Fprintf(w, "# TYPE voice_admission_rejects_total counter\n")
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"session_limit\"} %d\n", m.AdmissionRejectsLimit.Load())
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"principal_limit\"} %d\n", m.AdmissionRejectsPrincipal.Load())
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"vad_unavailable\"} %d\n", m.AdmissionRejectsVAD.Load())
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"agent_limit\"} %d\n", m.AdmissionRejectsAgent.Load())
	fmt.Fprintf(w, "voice_admission_rejects_total{reason=\"start_rate_limit\"} %d\n", m.AdmissionRejectsRate.Load())

	writeScalars(w,
		counter("voice_context_evictions_total", "Total number of oldest turn pairs evicted.", uint64(m.ContextEvictionsTotal.Load())),
		counter("voice_max_duration_terminations_total", "Total calls terminated due to max duration guard.", uint64(m.MaxDurationTermTotal.Load())),
		counter("voice_management_auth_failures_total", "Failed management API authentications.", uint64(m.ManagementAuthFailuresTotal.Load())),
		counter("voice_management_auth_throttled_total", "Management API requests refused for too many failed authentications.", uint64(m.ManagementAuthThrottledTotal.Load())),
		counter("voice_leaked_work_total", "Total number of tasks that ignored cancellation.", uint64(m.LeakedWorkTotal.Load())),
	)
	if m.OTelStats != nil {
		d, f := m.OTelStats()
		writeScalars(w,
			counter("voice_otel_spans_dropped_total", "Spans dropped because the export queue was full.", d),
			counter("voice_otel_spans_failed_total", "Spans lost to OTLP export errors.", f),
		)
	}
	writeScalars(w,
		counter("voice_leaked_sessions_total", "Total number of sessions marked as having leaked tasks.", uint64(m.LeakedSessionsTotal.Load())),
		gauge("voice_process_goroutines", "Number of running goroutines.", int64(runtime.NumGoroutine())),
		gauge("voice_process_heap_bytes", "Number of heap bytes allocated.", int64(mem.HeapAlloc)),
		// VAD
		gauge("voice_vad_active_streams", "Current active VAD streams.", int64(m.VADActiveStreams.Load())),
		counter("voice_vad_stale_chunks_total", "Stale VAD chunks discarded.", uint64(m.VADStaleChunksTotal.Load())),
		counter("voice_vad_dropped_chunks_total", "VAD chunks dropped due to worker overload.", uint64(m.VADDroppedChunksTotal.Load())),
		counter("voice_vad_worker_restarts_total", "Worker processes restarted.", uint64(m.VADWorkerRestartsTotal.Load())),
	)

	// Smart-turn model
	smartturn.WriteMetrics(w)
	netpool.WriteMetrics(w)
	if m.SmartTurnWorkers != nil {
		up, total := m.SmartTurnWorkers()
		fmt.Fprintf(w, "# HELP voice_smartturn_workers Smart-turn worker processes.\n# TYPE voice_smartturn_workers gauge\nvoice_smartturn_workers{state=\"up\"} %d\nvoice_smartturn_workers{state=\"total\"} %d\n", up, total)
	}

	// STT
	writeScalars(w,
		counter("voice_stt_errors_total", "Total STT provider errors.", uint64(m.STTErrorsTotal.Load())),
		counter("voice_stt_rotations_total", "Total pre-limit stream rotations completed.", uint64(m.STTRotationsTotal.Load())),
		counter("voice_stt_rotation_failures_total", "Total pre-limit stream rotations that failed.", uint64(m.STTRotationFailuresTotal.Load())),
	)
	fmt.Fprintf(w, "# HELP voice_stt_circuit_state STT circuit state (0=closed, 1=open, 2=half_open).\n")
	fmt.Fprintf(w, "# TYPE voice_stt_circuit_state gauge\n")
	fmt.Fprintf(w, "voice_stt_circuit_state{provider=\"google\"} %d\n", m.STTCircuitStateGoogle.Load())
	fmt.Fprintf(w, "voice_stt_circuit_state{provider=\"xai\"} %d\n", m.STTCircuitStateXAI.Load())
	m.circuits.write(w, "stt")

	// LLM
	writeScalars(w,
		counter("voice_llm_requests_total", "Total LLM turn requests.", uint64(m.LLMRequestsTotal.Load())),
		counter("voice_llm_errors_total", "Total LLM provider errors.", uint64(m.LLMErrorsTotal.Load())),
		counter("voice_llm_retries_total", "Total LLM retry attempts.", uint64(m.LLMRetriesTotal.Load())),
		counter("voice_llm_stale_deltas_discarded_total", "Late LLM deltas discarded.", uint64(m.LLMStaleDeltasDiscarded.Load())),
	)
	fmt.Fprintf(w, "# HELP voice_llm_circuit_state Circuit state (0=closed, 1=open, 2=half_open).\n")
	fmt.Fprintf(w, "# TYPE voice_llm_circuit_state gauge\n")
	fmt.Fprintf(w, "voice_llm_circuit_state{provider=\"google\"} %d\n", m.LLMCircuitStateGoogle.Load())
	fmt.Fprintf(w, "voice_llm_circuit_state{provider=\"xai\"} %d\n", m.LLMCircuitStateXAI.Load())
	m.circuits.write(w, "llm")

	// TTS
	writeScalars(w,
		counter("voice_tts_requests_total", "Total TTS synthesis requests.", uint64(m.TTSRequestsTotal.Load())),
		counter("voice_tts_errors_total", "Total TTS synthesis errors.", uint64(m.TTSErrorsTotal.Load())),
		counter("voice_tts_retries_total", "Total TTS retry attempts.", uint64(m.TTSRetriesTotal.Load())),
		counter("voice_tts_stale_audio_discarded_total", "Stale TTS audio frames discarded.", uint64(m.TTSStaleAudioDiscarded.Load())),
	)
	fmt.Fprintf(w, "# HELP voice_tts_circuit_state TTS circuit state (0=closed, 1=open, 2=half_open).\n")
	fmt.Fprintf(w, "# TYPE voice_tts_circuit_state gauge\n")
	fmt.Fprintf(w, "voice_tts_circuit_state{provider=\"google\"} %d\n", m.TTSCircuitStateGoogle.Load())
	fmt.Fprintf(w, "voice_tts_circuit_state{provider=\"xai\"} %d\n", m.TTSCircuitStateXAI.Load())
	m.circuits.write(w, "tts")

	m.providerErrors.write(w)
	m.latency.write(w)
	m.llmFailovers.write(w)
	m.stt.write(w)
	m.ttsFailovers.writeAs(w, "voice_tts_failover_total", "TTS synthesis moved to a fallback provider (replayed before audio, or next unit after a mid-unit failure).")
	writeScalars(w,
		counter("voice_tts_fallbacks_skipped_total", "Configured TTS fallbacks left out of a call (unresolvable).", uint64(m.TTSFallbacksSkippedTotal.Load())),
		counter("voice_llm_fallbacks_skipped_total", "Configured LLM fallbacks left out of a call (unresolvable).", uint64(m.LLMFallbacksSkippedTotal.Load())),
		counter("voice_user_mute_dropped_total", "User input frames (VAD start/stop, transcripts) dropped while muted.", uint64(m.UserMuteDroppedTotal.Load())),
		counter("voice_turn_min_words_no_interims_total", "Calls with turn.min_words whose STT sent a final transcript before any interim (min_words cannot start turns early).", uint64(m.MinWordsNoInterimsTotal.Load())),
		counter("voice_async_tool_turns_total", "Model turns started to deliver finished async tool results or operator context notes.", uint64(m.AsyncToolTurnsTotal.Load())),
	)
	fmt.Fprintf(w, "# HELP voice_turn_completion_total LLM turn-completion verdicts (turn.stop llm-completion).\n# TYPE voice_turn_completion_total counter\n")
	for i, v := range completionVerdicts {
		fmt.Fprintf(w, "voice_turn_completion_total{verdict=%q} %d\n", v, m.completion[i].Load())
	}
	writeScalars(w, counter("voice_async_tool_results_dropped_total", "Finished async tool results or operator context notes that could not be injected (session closing, context budget).", uint64(m.AsyncToolResultsDroppedTotal.Load())))
	fmt.Fprintf(w, "# HELP voice_mcp_sync_total Background MCP server syncs by result.\n# TYPE voice_mcp_sync_total counter\n")
	fmt.Fprintf(w, "voice_mcp_sync_total{result=\"ok\"} %d\n", m.MCPSyncOKTotal.Load())
	fmt.Fprintf(w, "voice_mcp_sync_total{result=\"failed\"} %d\n", m.MCPSyncFailedTotal.Load())

	fmt.Fprintf(w, "# HELP voice_eval_jobs_total Eval jobs the worker ended, by result (done, retry, dead, lease_lost).\n# TYPE voice_eval_jobs_total counter\n")
	for i, r := range evalJobResults {
		fmt.Fprintf(w, "voice_eval_jobs_total{result=%q} %d\n", r, m.evalJobs[i].Load())
	}
	enabled := int64(0)
	if m.RetentionEnabled.Load() {
		enabled = 1
	}
	writeScalars(w,
		gauge("voice_eval_calls_active", "Eval calls in flight on this instance.", int64(m.evalCallsActive.Load())),
		counter("voice_eval_runs_finished_total", "Eval runs this instance finished (its job was the run's last).", uint64(m.evalRunsFinished.Load())),
		gauge("voice_retention_enabled", "1 when the automatic retention job runs.", enabled),
		gauge("voice_retention_interval_seconds", "How often the retention job runs.", int64(m.RetentionIntervalSeconds.Load())),
	)
	// Without a completed run the age is the uptime: a job that never completes ages like one that stopped.
	age := time.Since(m.StartTime)
	if last := m.retentionLastOK.Load(); last != nil {
		age = time.Since(*last)
	}
	fmt.Fprintf(w, "# HELP voice_retention_runs_total Retention job runs by result.\n# TYPE voice_retention_runs_total counter\n")
	fmt.Fprintf(w, "voice_retention_runs_total{result=\"ok\"} %d\n", m.RetentionRunsOK.Load())
	fmt.Fprintf(w, "voice_retention_runs_total{result=\"failed\"} %d\n", m.RetentionRunsFailed.Load())
	writeScalars(w,
		counter("voice_retention_turns_purged_total", "Turns whose transcript and assistant text the retention job blanked.", uint64(m.RetentionTurnsPurgedTotal.Load())),
		counter("voice_retention_sessions_deleted_total", "Sessions (with their turns, spans and tool calls) the retention job deleted.", uint64(m.RetentionSessionsDeleted.Load())),
	)
	fmt.Fprintf(w, "# HELP voice_retention_erasures_total Calls erased on request, by how they were asked for.\n# TYPE voice_retention_erasures_total counter\n")
	fmt.Fprintf(w, "voice_retention_erasures_total{kind=\"call\"} %d\n", m.RetentionErasedCalls.Load())
	fmt.Fprintf(w, "voice_retention_erasures_total{kind=\"caller\"} %d\n", m.RetentionErasedByCaller.Load())
	fmt.Fprintf(w, "# HELP voice_retention_last_success_age_seconds Seconds since the retention job last completed a run (the uptime before the first one).\n# TYPE voice_retention_last_success_age_seconds gauge\nvoice_retention_last_success_age_seconds %.0f\n", age.Seconds())

	writeScalars(w,
		counter("voice_user_idle_prompts_total", "Prompts sent to a silent user.", m.UserIdlePromptsTotal.Load()),
		counter("voice_user_idle_hangups_total", "Calls ended after the user stayed silent through every prompt.", m.UserIdleHangupsTotal.Load()),
		counter("voice_bot_idle_stuck_total", "User turns the bot never answered (generation cancelled).", m.BotIdleStuckTotal.Load()),
		counter("voice_bot_idle_hangups_total", "Calls ended after repeated unanswered turns.", m.BotIdleHangupsTotal.Load()),
		counter("voice_greeting_failed_total", "Greetings that could not be queued (the session was closing).", m.GreetingFailedTotal.Load()),
	)

	m.filters.write(w)
	m.summary.write(w)
	m.handoff.write(w)

	// Transport
	writeScalars(w,
		gauge("voice_transport_connected_clients", "Number of connected clients.", int64(m.ConnectedClients.Load())),
		counter("voice_transport_auth_failures_total", "Token authentication failures.", uint64(m.AuthFailuresTotal.Load())),
		counter("voice_transport_replay_rejections_total", "Token replay attempts rejected.", uint64(m.ReplayRejectionsTotal.Load())),
		counter("voice_transport_origin_rejections_total", "Origin check failures.", uint64(m.OriginRejectionsTotal.Load())),
	)
	fmt.Fprintf(w, "# HELP voice_transport_disconnects_total Disconnects by reason.\n")
	fmt.Fprintf(w, "# TYPE voice_transport_disconnects_total counter\n")
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"normal\"} %d\n", m.DisconnectsNormal.Load())
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"client_error\"} %d\n", m.DisconnectsClientError.Load())
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"server_error\"} %d\n", m.DisconnectsServerError.Load())
	fmt.Fprintf(w, "voice_transport_disconnects_total{reason=\"timeout\"} %d\n", m.DisconnectsTimeout.Load())
	writeScalars(w,
		counter("voice_transport_frames_in_total", "Audio and control frames received.", uint64(m.FramesInTotal.Load())),
		counter("voice_transport_frames_out_total", "Audio and control frames sent.", uint64(m.FramesOutTotal.Load())),
		counter("voice_transport_bytes_in_total", "Inbound network bytes.", uint64(m.BytesInTotal.Load())),
		counter("voice_transport_bytes_out_total", "Outbound network bytes.", uint64(m.BytesOutTotal.Load())),
		counter("voice_transport_flush_acks_total", "Transport flush acknowledgements.", uint64(m.FlushAcksTotal.Load())),
		counter("voice_transport_flush_timeouts_total", "Transport flush barrier timeouts.", uint64(m.FlushTimeoutsTotal.Load())),
		counter("voice_transport_slow_client_terminations_total", "Slow client terminations.", uint64(m.SlowClientTermTotal.Load())),
		counter("voice_ws_ping_failures_total", "Server WebSocket pings that got no pong in time.", uint64(m.WSPingFailuresTotal.Load())),
		counter("voice_transport_protocol_errors_total", "Wire protocol errors.", uint64(m.ProtocolErrorsTotal.Load())),
	)
}

// dynamicCircuits is the circuit gauge of breakers created per endpoint at run time.
type dynamicCircuits struct {
	mu sync.Mutex
	m  map[circuitKey]*atomic.Int64
}

type circuitKey struct{ stage, provider, endpoint string }

func (d *dynamicCircuits) set(stage, provider, endpoint string, state int64) {
	k := circuitKey{stage, provider, endpoint}
	d.mu.Lock()
	if d.m == nil {
		d.m = map[circuitKey]*atomic.Int64{}
	}
	g := d.m[k]
	if g == nil {
		g = new(atomic.Int64)
		d.m[k] = g
	}
	d.mu.Unlock()
	g.Store(state)
}

func (d *dynamicCircuits) snapshot() map[circuitKey]int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[circuitKey]int64, len(d.m))
	for k, g := range d.m {
		out[k] = g.Load()
	}
	return out
}

// write emits the stage's samples; they continue voice_<stage>_circuit_state's family.
func (d *dynamicCircuits) write(w io.Writer, stage string) {
	snap := d.snapshot()
	keys := make([]circuitKey, 0, len(snap))
	for k := range snap {
		if k.stage == stage {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].endpoint < keys[j].endpoint
	})
	for _, k := range keys {
		fmt.Fprintf(w, "voice_%s_circuit_state{provider=%q,endpoint=%q} %d\n", stage, k.provider, k.endpoint, snap[k])
	}
}

// providerErrors counts stage errors by provider. A stage with fallbacks reports provider
// "failover": the processor sums its whole chain, so the culprit is not known here.
type providerErrors struct {
	mu sync.Mutex
	m  map[[2]string]uint64
}

func (p *providerErrors) add(provider, stage string, n uint64) {
	if n == 0 {
		return
	}
	p.mu.Lock()
	if p.m == nil {
		p.m = map[[2]string]uint64{}
	}
	p.m[[2]string{provider, stage}] += n
	p.mu.Unlock()
}

func (p *providerErrors) write(w io.Writer) {
	p.mu.Lock()
	keys := make([][2]string, 0, len(p.m))
	for k := range p.m {
		keys = append(keys, k)
	}
	vals := make(map[[2]string]uint64, len(p.m))
	for k, v := range p.m {
		vals[k] = v
	}
	p.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	fmt.Fprint(w, "# HELP voice_provider_errors_total Stage errors by provider (provider=\"failover\" when the stage had fallbacks).\n# TYPE voice_provider_errors_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(w, "voice_provider_errors_total{provider=%q,stage=%q} %d\n", k[0], k[1], vals[k])
	}
}

// ObserveErasure counts calls erased on request; kind is "call" or "caller".
func (m *Metrics) ObserveErasure(kind string, n int) {
	if kind == "caller" {
		m.RetentionErasedByCaller.Add(uint64(n))
		return
	}
	m.RetentionErasedCalls.Add(uint64(n))
}

// ObserveRetention records a finished retention run.
func (m *Metrics) ObserveRetention(rec retention.RunRecord) {
	// Batches that committed before a failure removed rows: count them either way.
	m.RetentionTurnsPurgedTotal.Add(uint64(rec.Result.TurnsPurged))
	m.RetentionSessionsDeleted.Add(uint64(rec.Result.SessionsDeleted))
	if rec.Err != "" {
		m.RetentionRunsFailed.Add(1)
		return
	}
	now := time.Now()
	m.retentionLastOK.Store(&now)
	m.RetentionRunsOK.Add(1)
}
