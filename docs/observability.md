# Voice Framework V1 — Observability, Health & Telemetry

This document details the configuration profiles, health and readiness endpoints, Prometheus metrics, per-session diagnostic timelines, and incident troubleshooting runbooks for Voice Framework V1.

---

## 1. Configuration Profiles

Voice Framework V1 uses validated YAML configuration files loaded via `server/config.go`. Unknown fields are rejected at startup (`KnownFields(true)`).

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

All health endpoints are served exclusively on the protected admin listener (default `:9090`).

### Liveness Probe: `GET /health`
- **Purpose**: Local process liveness for orchestrators (Kubernetes / Docker Compose / Systemd).
- **Semantics**: Ultra-cheap local check. It executes **zero external network calls**, zero provider queries, and zero VAD inferences.
- **Response**: `HTTP 200 OK` with `{"status":"ok"}`.

### Compound Readiness Probe: `GET /ready`
- **Purpose**: Verifies the server can safely admit new customer calls.
- **Inspection Logic**:
  1. `startup`: Verifies the server has finished initialization.
  2. `shutdown`: Fails immediately if the server is in draining or shutdown state.
  3. `admission`: Fails if admission has been manually disabled or limits are saturated.
  4. `vad_capacity`: Verifies that at least one native VAD worker is online and reporting capacity.
  5. `circuits`: Verifies that critical provider circuit breakers (STT, LLM, TTS) are not Tripped/Open.
- **Response**:
  - Ready: `HTTP 200 OK` with `{"status":"ready", "uptime":"...", "checks":{...}}`
  - Unready: `HTTP 503 Service Unavailable` with `{"status":"unready", "checks":{...}}`

---

## 3. Prometheus Telemetry (Spec §35)

The server exposes Prometheus metrics at `GET /metrics` on the admin port.

### Cardinality Discipline
To prevent memory exhaustion in Prometheus and TSDB instances:
- **STRICT PROHIBITION**: `session_id`, `user_id`, and `turn_id` are **NEVER** used as Prometheus metric labels.
- Only bounded labels (`provider`, `status`, `direction`, `code`) are permitted.

### Core Metrics Summary

| Metric Name | Type | Description |
|---|---|---|
| `voice_sessions_active` | Gauge | Currently active concurrent voice sessions |
| `voice_sessions_total` | Counter | Total voice sessions initiated |
| `voice_sessions_rejected_total` | Counter | Sessions rejected by admission control (`reason`) |
| `voice_session_tasks_leaked_total` | Counter | Managed tasks that failed to exit within teardown timeout |
| `voice_frames_processed_total` | Counter | Frames processed by pipeline (`direction`, `type`) |
| `voice_audio_dropped_total` | Counter | Audio frames dropped due to queue congestion |
| `voice_interruption_total` | Counter | Interruption (barge-in) events triggered |
| `voice_interruption_latency_seconds` | Histogram | Latency from `UserStartedSpeaking` to transport flush |
| `vad_worker_capacity_total` | Gauge | Total VAD worker inference slots |
| `vad_worker_capacity_available` | Gauge | Available VAD worker inference slots |
| `vad_worker_errors_total` | Counter | Failures encountered by VAD child processes |
| `vad_worker_restarts_total` | Counter | Number of times VAD child workers respawned |
| `provider_request_duration_seconds` | Histogram | Request latency per provider (`provider`, `operation`) |
| `provider_circuit_state` | Gauge | Circuit breaker status (0=Closed, 1=Half-Open, 2=Open) |

---

## 4. Per-Session Diagnostic Timeline

Voice Framework V1 provides an in-memory, circular ring buffer for deep incident diagnostics at `GET /timeline?session_id=<ID>`:
- **Buffer Size**: Strictly bounded to 256 events per session.
- **TTL**: Evicted 5 minutes after session termination.
- **Zero Content Leakage**: Transcripts and audio samples are **omitted by default**; only event names, state transitions, generation IDs, and latencies are recorded.
- **Non-Blocking**: The timeline logger never blocks the session run loop. If the ring buffer fills, older events are overwritten and an `evicted_count` counter is incremented.

---

## 5. Incident Interpretation Runbook

### Alert: `voice_session_tasks_leaked_total > 0`
- **Meaning**: A session was closed, but one or more background goroutines failed to exit within `TeardownTimeout` (2 seconds).
- **Likely Causes**:
  - An external provider client ignored context cancellation.
  - A socket write blocked indefinitely without honoring write deadlines.
- **Diagnostic Action**: Inspect the session timeline (`/timeline?session_id=...`) to identify which provider call was in-flight at teardown.

### Alert: `vad_worker_restarts_total > 5` or `vad_worker_errors_total > 0`
- **Meaning**: A native Silero VAD worker process crashed (e.g. segmentation fault) or became unresponsive.
- **System Behavior**: The supervisor killed the faulty worker, reaped its PID, and respawned a clean worker. Other workers continued processing calls without disruption.
- **Action**: Check `stderr` logs for VAD child worker diagnostic dumps (bounded to 64KB).
