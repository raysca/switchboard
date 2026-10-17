# Switchboard — Observability, Health & Telemetry

This document details the configuration profiles, health and readiness endpoints, Prometheus metrics, per-session diagnostic timelines, and incident troubleshooting runbooks for Switchboard.

---

## 1. Configuration Profiles

Switchboard uses validated YAML configuration files loaded via `server/config.go`. Unknown fields are rejected at startup (`KnownFields(true)`).

### Profile Comparison

| Setting | Development (`config/development.yaml`) | Production (`config/production.yaml`) |
|---|---|---|
| **Environment** | `development` | `production` |
| **VAD Executor** | `sharded` (in-process) | `process` (isolated worker children) |
| **Origin Verification** | Permissive (wildcard `*` or missing) | Strict domain allowlist |
| **Token Secret** | Permissive (dev default permitted) | Strict: minimum 32 chars, file permissions `0600` |
| **Admission Limits** | Relaxed for local tests | Bounded: 250 calls, 64 pending handshakes |
| **Shutdown Drain** | 5 seconds | 30 seconds drain, 15 seconds force |

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
  4. Flow store: unready if flow storage is not enabled, or if a ping of the store fails (the verdict is cached for 1 s). Calls run stored flows only.
- **Provider circuits** (`google_stt`, `google_llm`, `google_tts`, `xai_llm`, `xai_stt`, `xai_tts` when built in, plus `nvidia_<stage>:<endpoint>` once an Nvidia endpoint has been used) are reported as `closed`, `open` or `half_open` for diagnosis but do not make the server unready: an open circuit only affects flows that use that provider. Per-flow call readiness is reported by `GET /api/v1/flows/{id}` (`readiness`). `/ready` no longer reports `usable_pipelines`.
- **Response**:
  - Ready: `HTTP 200 OK` with `{"status":"ready", "circuits":{...}}`
  - Unready: `HTTP 503 Service Unavailable` with `{"status":"unready", "reason":"..."}`

---

## 3. Prometheus Telemetry (Spec §35)

The server exposes Prometheus metrics at `GET /metrics` on the probe and admin listeners. Alert rules: `deploy/alerts/govoice.rules.yml`. A test (`server/docs_contract_test.go`) fails if this document, the runbook or the rules name a metric the server does not emit.

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
| `voice_admission_rejects_total` | Counter | Calls refused by admission control (`reason`: `session_limit`, `principal_limit`, `vad_unavailable`, `flow_limit`, `start_rate_limit`) |
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
| `voice_stt_circuit_state` / `voice_llm_circuit_state` / `voice_tts_circuit_state` | Gauge | Circuit breaker per `provider` (0=closed, 1=open, 2=half_open); Nvidia adds an `endpoint` label |
| `voice_latency_seconds` | Histogram | Per-turn latency by `stage` (`stt_final`, `llm_ttft`, `tts_ttfb`, `voice_to_voice`, `startup`) |
| `voice_transport_connected_clients` | Gauge | Connected WebSocket clients |
| `voice_transport_auth_failures_total` | Counter | Token authentication failures |
| `voice_transport_disconnects_total` | Counter | Disconnects by `reason` |
| `voice_transport_slow_client_terminations_total` | Counter | Slow clients cut off |
| `voice_ws_ping_failures_total` | Counter | Pings that got no pong in time |
| `voice_user_idle_hangups_total` | Counter | Calls ended after the user stayed silent |
| `voice_otel_spans_dropped_total` / `voice_otel_spans_failed_total` | Counter | OTel export losses (only with `otel.enabled`) |

The series for smart-turn, audio filters, summaries, failover and warm-up are described in the runbook sections for those features.


---

## 4. Per-Session Diagnostic Timeline

Switchboard provides an in-memory, circular ring buffer for deep incident diagnostics at `GET /debug/timeline?session=<ID>` on the admin listener:
- **Buffer Size**: Bounded to `observability.timeline_max_events` events per session (1000 in production.yaml).
- **TTL**: Idle rings are evicted after `observability.timeline_ttl` (15m in production.yaml).
- **Zero Content Leakage**: Transcripts and audio samples are **omitted by default**; only event names, state transitions, generation IDs, and latencies are recorded.
- **Non-Blocking**: The timeline logger never blocks the session run loop. If the ring buffer fills, older events are overwritten and an `evicted_count` counter is incremented.

### Routes between steps

A flow with more than one step moves the call between steps. Every timeline event carries a `step` field (the step that was answering). Routes add these event types: `step_preparing` (target being built), `route` (`from->to`, `duration_ms` from the transfer to the new step being active), `step_active`, `step_closed` (the step the call left was closed) and `route_failed` (the caller stays on the old step; `error` is redacted). When storage is enabled the same lifecycle is persisted as trace spans named `route.preparing`, `route.active`, `route.closed`, `route.failed` (attributes `from_step`, `to_step`) on the stored session, so the designer can replay the path.

Metrics (unlabelled: step ids are tenant data): `voice_routes_total`, `voice_route_failures_total`, and the `voice_route_seconds` histogram.

### Provider retries and circuits

`provider_retry` is recorded each time an LLM or TTS request of the call is retried (`provider`, `details` `<stage> retry <n>`; `n` = 1 for the second attempt). `circuit_state` is recorded when a provider circuit the call uses changes state (`provider`, `details` `<stage> <closed|open|half_open>`); circuits are shared by every call, so each live call that uses the provider for that stage in its start step gets the event, whichever call's requests moved the circuit. Only the circuits exported as `voice_{stt,llm,tts}_circuit_state` report (google, xai, echo, offline, nvidia): a call on an OpenAI-compatible, Deepgram, Cartesia or ElevenLabs stage gets `provider_retry` events but no `circuit_state` (those breakers have no observer). STT stream dials do not record retries. Breaker reports from concurrent requests can arrive out of order, so a timeline can end on `open` while the circuit has closed again (the gauge has the same race).

---

## 5. Incident Interpretation Runbook

### Alert: `GovoiceLeakedWork` (`increase(voice_leaked_work_total[15m]) > 0`)
- **Meaning**: A session was closed, but one or more background goroutines failed to exit within `TeardownTimeout` (2 seconds).
- **Likely Causes**:
  - An external provider client ignored context cancellation.
  - A socket write blocked indefinitely without honoring write deadlines.
- **Diagnostic Action**: Inspect the session timeline (`/debug/timeline?session=...`) to identify which provider call was in-flight at teardown.

### Alert: `GovoiceVADWorkerRestarts` (`increase(voice_vad_worker_restarts_total[10m]) > 5`)
- **Meaning**: A native Silero VAD worker process crashed (e.g. segmentation fault) or became unresponsive.
- **System Behavior**: The supervisor killed the faulty worker, reaped its PID, and respawned a clean worker. Other workers continued processing calls without disruption.
- **Action**: Check `stderr` logs for VAD child worker diagnostic dumps (bounded to 64KB).

## Latency observers

`core/observe` consumes the session's DebugEvents (`session.started`, `user.stopped`, `stt.final`, `llm.request`, `llm.first_token`, `tts.request`, `bot.first_audio`, emitted once per turn by `core/debug_frames.go`) and computes per-turn `stt_final`, `llm_ttft`, `tts_ttfb`, `voice_to_voice` plus session `startup`. The server exposes them as `voice_latency_seconds{stage}` histograms (stage is the only label) and as `latency` timeline events (`details` = stage, `duration_ms`). `voice_to_voice` is user-stop to the first non-empty bot audio frame the TTS pushes into the pipeline (not transport playout); turns without a preceding user-stop (greetings) report no `stt_final` or `voice_to_voice`. Observers are bounded and drop-counted, so a slow one never delays the pipeline.

## OpenTelemetry export (optional)

Off by default: with `otel.enabled: false` (or the section absent) no exporter is built and sessions run with no OTel code path. When enabled, each call exports one trace: `govoice.session` -> `govoice.turn` -> `govoice.stt|llm|tts|tool`. Per-frame and per-processor spans are not exported.

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

Attributes (prefix `govoice.`): `flow.id`, `flow.version`, `session.id`, `turn.id`, `provider`, `model`, usage (`quantity`/`unit`), and `latency.{stt_final,llm_ttft,tts_ttfb,voice_to_voice,startup}_ms` on the turn (startup on the session span). Failed spans carry `error.class` (Go type only; the message is never exported) and provider failure/failover events are attached as span events. Transcript, prompt, tool arguments/results and audio are not attributes unless `capture_content: true`.

Metrics: `voice_otel_spans_dropped_total` (queue full) and `voice_otel_spans_failed_total` (OTLP export errors). Export is best effort and cannot block or fail a call.
