# Switchboard — Observability, Health & Telemetry

This document details the configuration profiles, health and readiness endpoints, Prometheus metrics, per-session diagnostic timelines and OpenTelemetry export for Switchboard.

---

## 1. Configuration Profiles

Switchboard uses validated YAML configuration files loaded via `server/config.go`. Unknown fields are rejected at startup (`KnownFields(true)`). The shipped profiles (`config/development.yaml`, `config/production.yaml`, `config/cloudrun.yaml`) and what each sets are listed in [deployment.md](deployment.md#4-configuration-files), together with the checks the production profile enforces at startup.

---

## 2. Health and Readiness Semantics

`/health`, `/ready` and `/metrics` are served on the probe listener (`probe_listen_addr`, `0.0.0.0:9092` in `config/production.yaml`, unauthenticated, no management API) and on the admin listener (`admin_listen_addr`, default loopback `:9090`, which also serves the management API and `/debug/timeline`). Keep the admin listener on loopback; never expose the probe port publicly.

### Liveness Probe: `GET /health`
- **Purpose**: Local process liveness for orchestrators (Kubernetes / Docker Compose / Systemd).
- **Semantics**: Ultra-cheap local check. It executes **zero external network calls**, zero provider queries, and zero VAD inferences.
- **Response**: `HTTP 200 OK` with `{"status":"live"}`.

### Compound Readiness Probe: `GET /ready`
- **Purpose**: Verifies the server can safely admit new customer calls.
- **Inspection Logic** (first failure wins):
  1. Startup/shutdown: unready until initialization finishes and while draining or shutting down.
  2. Admission: unready if admission has been globally disabled.
  3. VAD capacity: unready if no native VAD worker is online.
  4. Agent store: unready if agent storage is not enabled, or if a ping of the store fails (the verdict is cached for 1 s). Calls run stored agents only.
- **Provider circuits** (`google_stt`, `google_llm`, `google_tts`, `xai_llm`, `xai_stt`, `xai_tts` when built in, plus `nvidia_<stage>:<endpoint>` once an Nvidia endpoint has been used) are reported as `closed`, `open` or `half_open` for diagnosis but do not make the server unready: an open circuit only affects agents that use that provider. Per-agent call readiness is reported by `GET /api/v1/agents/{id}` (`readiness`). `/ready` no longer reports `usable_pipelines`.
- **Response**:
  - Ready: `HTTP 200 OK` with `{"status":"ready", "circuits":{...}}`
  - Unready: `HTTP 503 Service Unavailable` with `{"status":"unready", "reason":"..."}`

---

## 3. Prometheus Telemetry (Spec §35)

The server exposes Prometheus metrics at `GET /metrics` on the probe and admin listeners. Alert rules: `deploy/alerts/govoice.rules.yml`. A test (`server/docs_contract_test.go`) fails if this document, the operations guide, the deployment guide or the rules name a metric the server does not emit.

### Cardinality Discipline
To prevent memory exhaustion in Prometheus and TSDB instances:
- **STRICT PROHIBITION**: `session_id`, `user_id`, and `turn_id` are **NEVER** used as Prometheus metric labels.
- Only bounded labels (`provider`, `status`, `direction`, `code`) are permitted.

### Core Metrics Summary

| Metric Name | Type | Description |
|---|---|---|
| `voice_build_info` | Gauge | Always 1; labels `version`, `commit` |
| `voice_active_sessions` | Gauge | Currently active concurrent voice sessions |
| `voice_sessions_started_total` | Counter | Voice sessions started |
| `voice_sessions_stopped_total` | Counter | Voice sessions stopped |
| `voice_sessions_failed_total` | Counter | Sessions that failed (setup or run) |
| `voice_admission_rejects_total` | Counter | Calls refused by admission control (`reason`: `session_limit`, `principal_limit`, `vad_unavailable`, `agent_limit`, `start_rate_limit`) |
| `voice_draining` | Gauge | 1 while draining for shutdown |
| `voice_drain_sessions_remaining` | Gauge | Sessions still live while draining |
| `voice_max_duration_terminations_total` | Counter | Calls hung up by the max call duration guard |
| `voice_leaked_work_total` | Counter | Tasks that ignored cancellation at teardown |
| `voice_leaked_sessions_total` | Counter | Sessions that leaked at least one task |
| `voice_process_goroutines` / `voice_process_heap_bytes` | Gauge | Process runtime |
| `voice_vad_active_streams` | Gauge | Active VAD streams |
| `voice_vad_stale_chunks_total` / `voice_vad_dropped_chunks_total` | Counter | VAD chunks discarded as stale / dropped on overload |
| `voice_vad_worker_restarts_total` | Counter | VAD child workers respawned |
| `voice_stt_errors_total` / `voice_llm_errors_total` / `voice_tts_errors_total` | Counter | Provider errors per stage (all providers) |
| `voice_provider_errors_total` | Counter | Stage errors by `provider` and `stage` (`provider="failover"` when the stage had fallbacks) |
| `voice_provider_requests_total` | Counter | Requests sent to a provider (`provider`, `stage`), retries included |
| `voice_smartturn_fallbacks_total` | Counter | Turns that fell back to the speech timeout instead of the smart-turn model, by `reason` (`timeout`, `error`, `overload`, `unavailable`, `no_audio`) |
| `voice_smartturn_worker_crashes_total` | Counter | Smart-turn worker processes lost (exit, hang, bad output) |
| `voice_stt_circuit_state` / `voice_llm_circuit_state` / `voice_tts_circuit_state` | Gauge | Circuit breaker per `provider` (0=closed, 1=open, 2=half_open); Nvidia adds an `endpoint` label |
| `voice_latency_seconds` | Histogram | Per-turn latency by `stage` (`stt_final`, `llm_ttft`, `tts_ttfb`, `voice_to_voice`, `startup`) |
| `voice_transport_connected_clients` | Gauge | Connected WebSocket clients |
| `voice_transport_auth_failures_total` | Counter | Token authentication failures |
| `voice_transport_disconnects_total` | Counter | Disconnects by `reason` |
| `voice_transport_slow_client_terminations_total` | Counter | Slow clients cut off |
| `voice_ws_ping_failures_total` | Counter | Pings that got no pong in time |
| `voice_user_idle_hangups_total` | Counter | Calls ended after the user stayed silent |
| `voice_otel_spans_dropped_total` / `voice_otel_spans_failed_total` | Counter | OTel export losses (only with `otel.enabled`) |
| `voice_retention_enabled` | Gauge | 1 when the automatic retention job runs |
| `voice_retention_runs_total{result}` | Counter | Retention runs, `ok` or `failed` |
| `voice_retention_turns_purged_total` / `voice_retention_sessions_deleted_total` | Counter | What the job blanked and deleted |
| `voice_retention_interval_seconds` | Gauge | How often the job runs (`retention.interval`) |
| `voice_retention_last_success_age_seconds` | Gauge | Seconds since the last completed run (the uptime before the first); `GovoiceRetentionStalled` fires above three intervals |

Every closed connection counts exactly one disconnect by cause (normal, client, server, timeout), so the causes sum to the connections closed; a frame the server rejects (1007/1008/1009), an admission or setup refusal (1013/1011), a drain and any call the server ends itself (end of call, fatal error, idle hang-up, slow client, ready timeout) are `server`; the client's `disconnect-bot` is `normal`; a client that stops answering pings is `client`. `timeout` is a read timeout. Messages larger than 21 KB (the largest valid frame is a 16 KB RTVI message) are refused with 1009.

The wire message `bot-interrupted` (sent when the assistant's interruption barrier runs) has no metric of its own; it is visible in the client's RTVI log. It does not clear audio the client already buffered (`docs/compatibility/playback-clear-decision.json`).

The series for smart-turn, audio filters, summaries, failover and warm-up are described in the feature sections of [operations.md](operations.md). The response to each alert (VAD worker restarts, leaked work, open circuits) is in its incident section there.


---

## 4. Per-Session Diagnostic Timeline

Switchboard provides an in-memory, circular ring buffer for deep incident diagnostics at `GET /debug/timeline?session=<ID>` on the admin listener:
- **Buffer Size**: Bounded to `observability.timeline_max_events` events per session (1000 in production.yaml).
- **TTL**: Idle rings are evicted after `observability.timeline_ttl` (15m in production.yaml).
- **Zero Content Leakage**: Transcripts and audio samples are **omitted by default**; only event names, state transitions, generation IDs, and latencies are recorded.
- **Non-Blocking**: The timeline logger never blocks the session run loop. If the ring buffer fills, older events are overwritten and an `evicted_count` counter is incremented.

### Handoffs between steps

An agent with more than one step moves the call between steps. Every timeline event carries a `step` field (the step that was answering). Handoffs add these event types: `step_preparing` (target being built), `handoff` (`from->to`, `duration_ms` from the transfer to the new step being active), `step_active`, `step_closed` (the step the call left was closed) and `handoff_failed` (the caller stays on the old step; `error` is redacted). When storage is enabled the same lifecycle is persisted as trace spans named `handoff.preparing`, `handoff.active`, `handoff.closed`, `handoff.failed` (attributes `from_step`, `to_step`) on the stored session, so the designer can replay the path.

Metrics (unlabelled: step ids are tenant data): `voice_handoffs_total`, `voice_handoff_failures_total`, and the `voice_handoff_seconds` histogram.

### Provider retries and circuits

`provider_retry` is recorded each time an LLM or TTS request of the call is retried (`provider`, `details` `<stage> retry <n>`; `n` = 1 for the second attempt). `circuit_state` is recorded when a provider circuit the call uses changes state (`provider`, `details` `<stage> <closed|open|half_open>`); circuits are shared by every call, so each live call that uses the provider for that stage in its start step gets the event, whichever call's requests moved the circuit. Only the circuits exported as `voice_{stt,llm,tts}_circuit_state` report (google, xai, echo, offline, nvidia): a call on an OpenAI-compatible, Deepgram, Cartesia or ElevenLabs stage gets `provider_retry` events but no `circuit_state` (those breakers have no observer). A call that starts while a circuit is already open only learns of the next state change. STT stream dials do not record retries. Breaker reports from concurrent requests can arrive out of order, so a timeline can end on `open` while the circuit has closed again (the gauge has the same race).

---

## Latency observers

`core/observe` consumes the session's DebugEvents (`session.started`, `user.stopped`, `stt.final`, `llm.request`, `llm.first_token`, `tts.request`, `bot.first_audio`, emitted once per turn by `core/debug_frames.go`) and computes per-turn `stt_final`, `llm_ttft`, `tts_ttfb`, `voice_to_voice` plus session `startup`. The server exposes them as `voice_latency_seconds{stage}` histograms (stage is the only label) and as `latency` timeline events (`details` = stage, `duration_ms`). `voice_to_voice` is user-stop to the first non-empty bot audio frame the TTS pushes into the pipeline (not transport playout); turns without a preceding user-stop (greetings) report no `stt_final` or `voice_to_voice`. Observers are bounded and drop-counted, so a slow one never delays the pipeline.

## OpenTelemetry export (optional)

Off by default: with `otel.enabled: false` (or the section absent) no exporter is built and sessions run with no OTel code path. When enabled, each call exports one trace: `govoice.session` -> `govoice.turn` -> `govoice.llm|tts|tool`. An STT span (`govoice.stt`) covers one provider stream. Streams open on the first audio, before a turn is bound, so they normally hang off the session span (no `govoice.turn.id`); a stream opened inside a turn is the exception and hangs off that turn. Google streams rotate every 240 s, so one utterance may have several. A stream that ends because the call closed or the stream was cancelled or rotated ends OK; only a provider failure sets error status. Per-frame and per-processor spans are not exported.

```yaml
otel:
  enabled: true
  endpoint: https://collector.example.com:4318/v1/traces   # OTLP/HTTP protobuf
  protocol: http/protobuf          # only supported value
  headers_env:                     # header name -> env var holding the value (never logged)
    Authorization: OTEL_AUTH_HEADER
  sampling_ratio: 0.1              # default 1; 0 exports nothing
  service_name: govoice
  resource_attributes: {deployment.environment: prod}
  capture_content: false           # default; true admits transcript/prompt text into spans
  queue_size: 2048                 # finished spans buffered; overflow is dropped, never blocks a call
  shutdown_timeout: 5s             # bounded final flush during Shutdown
```

Attributes (prefix `govoice.`): `agent.id`, `agent.version`, `session.id`, `turn.id`, `provider`, `model`, usage (`quantity`/`unit`), and `latency.{stt_final,llm_ttft,tts_ttfb,voice_to_voice,startup}_ms` on the turn (startup on the session span). Failed spans carry `error.class` (Go type only; the message is never exported) and provider failure/failover events are attached as span events. Transcript, prompt, tool arguments/results and audio are not attributes unless `capture_content: true`.

Metrics: `voice_otel_spans_dropped_total` (queue full) and `voice_otel_spans_failed_total` (OTLP export errors). Export is best effort and cannot block or fail a call.
