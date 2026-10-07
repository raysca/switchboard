# Voice Framework V1 — Production Deployment & Operations Runbook

This runbook provides step-by-step instructions for deploying, operating, monitoring, and troubleshooting Voice Framework V1 in production environments.

---

## 1. Supported Production Environments

The container image is the V1 distribution. The production image is Debian bookworm. A host install on Ubuntu, Debian, or RHEL is not part of this release. Host resource numbers below are planning guidance, not a measured capacity result.

The following table is planning guidance for a host that runs the image:

| Distribution | Minimum Version | Architecture | C Runtime Requirement |
|---|---|---|---|
| **Ubuntu Linux** | 22.04 LTS (Jammy) | `amd64`, `arm64` | glibc >= 2.34 |
| **Debian Linux** | 12 (Bookworm) | `amd64`, `arm64` | glibc >= 2.34 |
| **Red Hat Enterprise Linux** | 9.x | `amd64`, `arm64` | glibc >= 2.34 |

### Host Resource Recommendations (Per 250 Concurrent Calls)
- **CPU**: 8 dedicated vCPUs (or modern bare-metal cores)
- **Memory**: 8 GB RAM
- **File Descriptors**: `LimitNOFILE=65536`
- **Processes / Threads**: `LimitNPROC=4096`
- **Network**: 1 Gbps NIC with low jitter connectivity to Google Cloud (`us-central1` recommended)

### Sizing VAD workers and CPU headroom

Measured with `tools/loadtest` (see `docs/benchmarks/capacity.md`; Apple M4 Pro, fake providers, so these are framework-only figures):

- **`vad.workers`**: Silero needs about 76 µs of one core per 32 ms window per call when unbatched, and about 52 µs at batch 8. That is about 2.4 ms of CPU per call-second in the worst case, so one worker (single-threaded) saturates near 400 calls. Size for 50% worker utilisation: `vad.workers = max(2, ceil(peak_concurrent_calls / 200))`. The default of 4 covers about 800 calls. More workers do not help latency below that. Re-measure on slower x86 cores, which can be 2 to 3 times slower per window.
- **CPU headroom matters more than worker count.** A VAD worker that has not answered a batch in about 150 ms is killed and respawned. When the host or container is CPU-starved (oversubscribed nodes, or a CPU limit that throttles in 100 ms periods), healthy workers can be killed. You then see `voice_vad_worker_restarts_total` increase, `vad_unavailable` admission rejects while every worker is down, and growth in `voice_vad_stale_chunks_total`, all with normal latency. Keep the steady state below about 70% of the CPU allotment. Prefer CPU requests without a tight CPU limit. Do not co-locate a load generator with the server when measuring capacity.
- **Admission limits**: set `limits.max_sessions` from a load test on the target hardware with real providers, at most about 70% of the session count where the p95 voice-to-voice budget or any of the failure counters above first break.

---

## 2. Deployment Path A: Container (Docker / Podman)

### Step 1: Build the Hardened Image
```bash
docker build -t govoice:v1 -f deploy/Dockerfile .
```
The Dockerfile builds a multi-stage image running as unprivileged user `govoice` (`uid=10001`), utilizing `tini` as PID 1 to ensure zombie child VAD worker processes are reaped correctly.

### Step 2: Configure Environment & Secrets
Store secrets in `/etc/govoice/secrets/`:
```bash
sudo mkdir -p /etc/govoice/secrets
sudo chmod 700 /etc/govoice/secrets

# Generate a strong 32+ character admission secret
openssl rand -hex 32 | sudo tee /etc/govoice/secrets/auth_secret.key
sudo chmod 600 /etc/govoice/secrets/auth_secret.key
```

The server also requires the agent store and its secrets. It refuses to start without them:

| Variable | Value |
|---|---|
| `TOKEN_SECRET` | Voice token signing secret (32+ characters). |
| `GOVOICE_MANAGEMENT_API_KEY` | Management API bearer key, at least 32 bytes, different from `TOKEN_SECRET`. |
| `GOVOICE_DATABASE_DSN` | Agent store DSN: a `postgres://` URL (`config/production.yaml` uses `storage.dialect: postgres`) or a SQLite file path. |
| `GOVOICE_CREDENTIAL_KEYS` | `id:base64(32-byte key)[,id:...]`; the first entry encrypts new provider secrets. Generate a key with `openssl rand -base64 32`. Back it up: stored credentials cannot be decrypted without it. To rotate, put the new key first, keep the old one, then call `POST /api/v1/providers/credentials/{id}/rotate`. |

Provider credentials (Google API key or service-account JSON, xAI API key) are not environment variables or mounted files. Add them after start-up with `POST /api/v1/providers/credentials`, then create agents with `POST /api/v1/agents` (see `docs/quickstart.md`). `GOOGLE_API_KEY`, `GOOGLE_APPLICATION_CREDENTIALS` and `XAI_API_KEY` are ignored.

### Step 3: Run with Docker Compose
Use the reference compose configuration in `deploy/docker-compose.yaml`:
```bash
docker compose -f deploy/docker-compose.yaml up -d
```
The container runs with `read_only: true`, `cap_drop: ALL`, and tmpfs mounted at `/tmp`. The compose file also starts a `postgres` service for the agent store; set `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY`, `GOVOICE_CREDENTIAL_KEYS` and `POSTGRES_PASSWORD` before `up`. `deploy/smoke.sh` runs this stack end to end: it adds a credential, creates an agent and issues a voice token.

---

## 3. Deployment Path B: Systemd Native Service

The systemd unit is a reference file and is not required to run V1.

### Step 1: Install Binaries and Models
```bash
# Create service user
sudo useradd -r -s /usr/sbin/nologin -d /opt/govoice govoice

# Install binaries
sudo install -m 755 -o root -g root bin/server /usr/local/bin/govoice-server
sudo install -m 755 -o root -g root bin/vadworker /usr/local/bin/govoice-vadworker

# Install VAD ONNX model
sudo mkdir -p /opt/govoice/vad/models
sudo cp vad/models/silero_vad.onnx /opt/govoice/vad/models/
sudo chown -R root:root /opt/govoice
```

### Step 2: Install Configuration and Systemd Unit
```bash
sudo mkdir -p /etc/govoice
sudo cp config/production.yaml /etc/govoice/config.yaml

# Copy and enable systemd unit
sudo cp deploy/govoice.service /etc/systemd/system/govoice.service
# /etc/govoice/env (mode 600) must set TOKEN_SECRET, GOVOICE_MANAGEMENT_API_KEY,
# GOVOICE_DATABASE_DSN and GOVOICE_CREDENTIAL_KEYS (see Path A, Step 2).
sudo systemctl daemon-reload
sudo systemctl enable --now govoice.service
```

### Step 3: Inspect Service Status
```bash
sudo systemctl status govoice.service
journalctl -u govoice.service -f
```

---

## 4. Reverse Proxy & TLS Termination (Nginx)

The public edge (`:8080`) must terminate TLS via a reverse proxy (e.g., Nginx or Envoy).

### Install Nginx Configuration
```bash
sudo cp deploy/nginx.conf /etc/nginx/sites-available/govoice.conf
sudo ln -s /etc/nginx/sites-available/govoice.conf /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

### Security Highlights of `deploy/nginx.conf`:
1. **WSS Upgrades**: Passes `Upgrade` and `Connection "Upgrade"` headers properly.
2. **Access Log Redaction**: Automatically redacts query tokens (`token=[REDACTED]`).
3. **Admin Port Isolation**: Port `9090` is bound to `127.0.0.1` and blocked from external ingress.

---

## 5. Routine Operations & Maintenance

### Checking Health & Readiness
```bash
# Liveness (Should return HTTP 200)
curl -s http://127.0.0.1:9090/health

# Readiness (Should return HTTP 200 with checks: true)
curl -s http://127.0.0.1:9090/ready | jq .
```

### Graceful Drain-Then-Cancel Shutdown
When rolling out updates or draining a host:
```bash
# Send SIGTERM to server
sudo systemctl stop govoice.service
# Or in docker:
docker stop -t 45 govoice
```
**Drain Sequence** (`limits.drain_timeout`, default 30s; 45s in production.yaml):
1. SIGTERM/SIGINT: admin `/ready` returns 503 immediately (`voice_draining 1`).
2. New calls are refused: a new `/ws` upgrade gets HTTP 503 `server_draining` before the token is checked (the token stays valid for a retry on another instance); a client already upgraded but not yet `client-ready` gets RTVI error `server_draining` and close 1001. The public listener stays open (public `/health` keeps answering) so stragglers routed during LB endpoint lag get a clear 503, not connection refused.
3. Active calls continue until they end or `drain_timeout` expires (`voice_drain_sessions_remaining` shows how many are left).
4. At the deadline each remaining call receives a fatal RTVI error `server_draining` followed by close 1001 (going away), then is force-closed. The client should reconnect.
5. The public listener, VAD workers and admin listener are closed. Exit `0` when all calls ended inside the deadline; exit `1` if calls had to be force-closed (or the shutdown budget ran out). Orchestrators ignore the code on a requested stop (Kubernetes, `docker stop`; systemd only logs `status=1/FAILURE` and does not restart on `systemctl stop`), so it is a signal for alerting on dropped calls, not a restart trigger.

### Rolling Deploys and Session Affinity
- **Readiness vs liveness**: point the load balancer / Kubernetes `readinessProbe` at admin `/ready` (503 during drain). Point `livenessProbe` at `/health` (admin `:9090` or public `:8080`; both stay up for the whole drain). Never use `/ready` for liveness: it is 503 for the whole drain and would kill the pod mid-drain.
- **Grace period**: `terminationGracePeriodSeconds` (Docker `stop_grace_period`, systemd `TimeoutStopSec`) must be at least `drain_timeout` + `preStop` sleep + 5s process margin (the server budgets `drain_timeout + 5s`). Production: 45s drain + 5s preStop + 5s margin => 60s or more (compose uses 60s).
- **preStop sleep**: add `preStop: sleep 5` so endpoint removal propagates to the LB before drain starts; SIGTERM is delivered after the hook. Anything still routed afterwards gets 503 `server_draining`; have the LB retry 503 on another target (nginx: `proxy_next_upstream error http_503`).
- **LB connection draining**: enable it (deregistration delay >= 0 is fine; do not set it shorter than the preStop sleep) and never have the LB cut established WebSockets when a target goes unhealthy. nginx: a reload keeps existing connections on old workers.
- **Affinity**: a call is one WebSocket pinned to one instance, and its pipeline, context and turn state live in that process's memory. There is no cross-node migration, and no sticky-session config is needed beyond the connection itself. New calls land on whichever instance is ready.
- **Reconnect semantics**: a force-closed client gets `server_draining` + 1001 and must start a new call (new voice token; tokens are single-use). Conversation context is not carried over; the app should replay it through the agent or accept a fresh start. Size `drain_timeout` to your typical call length to keep forced closes rare, and roll one instance at a time.
- **Kubernetes sketch**: `terminationGracePeriodSeconds: 60`, `lifecycle.preStop.exec.command: ["sleep","5"]`, `readinessProbe.httpGet: {path: /ready, port: 9090}`, `livenessProbe.httpGet: {path: /health, port: 9090}`.

---

## 6. Incident Troubleshooting & Failure Recovery

### Incident 1: Native VAD Worker Crash
- **Symptoms**: Alert on `vad_worker_restarts_total` increasing or `vad_worker_errors_total > 0`.
- **Underlying Mechanism**: A Silero VAD C/ONNX native routine crashed (e.g. SIGSEGV).
- **Automated Mitigation**: The parent process detects the child exit, reaps the PID via `cmd.Wait()`, updates capacity, and automatically spawns a healthy replacement worker with exponential backoff.
- **Action Required**: Inspect `journalctl -u govoice.service` for child stderr diagnostics (bounded to 64KB). Verify that the host is not experiencing OOM kills (`dmesg -T | grep -i oom`).

### Incident 2: Leaked Goroutines / Leaked Work
- **Symptoms**: Alert on `voice_session_tasks_leaked_total > 0`.
- **Underlying Mechanism**: A session was terminated, but a managed child task failed to exit within `TeardownTimeout: 2s`.
- **Action Required**: Query `/timeline?session_id=<ID>` for the affected session. Check if an external provider call hung without respecting context cancellation.

### Incident 3: Cloud Provider Circuit Breakers Tripped
- **Symptoms**: `provider_circuit_state{provider="..."} == 2`; `/ready` lists the circuit as `open` under `circuits` (it stays 200, because only agents using that provider are affected).
- **Underlying Mechanism**: 5 consecutive failures occurred against Google Cloud STT, LLM, or TTS within 60 seconds.
- **Action Required**:
  1. Check GCP Service Health status for Speech-to-Text or Vertex AI.
  2. Verify the stored credential (`POST /api/v1/providers/credentials/{id}/validate`) and IAM permissions.
  3. Circuits will automatically probe the provider in Half-Open state after 30 seconds of cooldown.
  4. Agents with STT fallbacks move the open utterance to the next provider and replay its buffered audio (`voice_stt_failover_total`); each new utterance tries the primary again once its breaker allows. `stt.replay_buffer` (default `10s`, max `60s`, ~32 KB/s per call with fallbacks) bounds how much of a long utterance survives; `voice_stt_replay_dropped_bytes_total` counts utterance audio that did not fit.

## Idle handling (`idle:`)

Off by default; no processor is added when off. `idle.user_timeout` (e.g. 15s) prompts a silent user after the bot stops speaking (and from call start, so a caller who connects and never speaks is released too), `idle.max_prompts` (default 2) times, one timeout apart, then ends the call with reason `user_idle` (optional spoken `idle.goodbye`); any speech, DTMF or typed input resets the count, the controller's own prompts do not. Prompts are skipped while a tool runs, the user is muted, a reply is generating or playing, or either side speaks. An idle hang-up sends a non-fatal RTVI `error` whose text is the reason (`user_idle` / `bot_idle`) and closes the WebSocket with 1000 (normal): clients must treat it as final, unlike the drain's `server_draining` + 1001 which means reconnect elsewhere. `idle.prompt_mode: llm` (default) injects a labelled non-user notice so the model phrases the nudge; `say` speaks `idle.prompt` verbatim. `idle.bot_timeout` (e.g. 30s) is the stuck-bot watchdog: a finished user turn with no bot speech sends the client an error, cancels the generation and optionally speaks `idle.bot_fallback`; `idle.bot_max_stuck` (default 3) in a row ends the call (`bot_idle`). A reply that finishes with no speech (tool-only) is not stuck. The watchdog is the caller's patience bound and cancels whatever is in flight, including an LLM failover (which may start a fallback up to 60 s into a turn); set `bot_timeout` above that budget if failovers must always run to completion. Debug events `idle.user_prompt`, `idle.user_timeout`, `idle.bot_stuck`; counters `voice_user_idle_prompts_total`, `voice_user_idle_hangups_total`, `voice_bot_idle_stuck_total`, `voice_bot_idle_hangups_total` (added when a call ends). Server-wide for now; per-agent settings come with the agent config. The existing `limits.idle_timeout` is only the WebSocket read deadline and is unrelated.

## Connection warm-up and keepalive (P2-9)

What costs first-turn time, per provider (measured on loopback with an injected 80 ms dial delay in `core/netpool` and each provider's `warm_test.go`; the real gain is 1 RTT for TCP, 1-2 RTT for TLS and 0-1 for DNS, i.e. typically 50-250 ms, and can only be measured live):

| Stage | Before | Now |
|---|---|---|
| xAI / OpenAI-compatible LLM (HTTP/2) | per-session `http.Client`; OpenAI cloned a transport per session, so every call's first request paid DNS+TCP+TLS | one shared transport per provider (and per SSRF-guard mode for `openai`), 32 idle conns/host, 90 s idle timeout, TLS session cache, HTTP/2 health pings (30 s idle, 10 s timeout). Second session: 0 new dials |
| Deepgram STT, xAI STT, Cartesia/ElevenLabs TTS | websocket dialed per turn/unit on `http.DefaultClient`: DNS+TCP+TLS+upgrade each time | same dial, through a shared HTTP/1.1 pool. Only TCP+TLS can be pre-dialed (see below) |
| Google STT/TTS (gRPC) | one channel per call, lazy: first RPC paid connect | channel per call is unchanged; `Warm` starts the connect so it is READY before the first utterance; gRPC keepalive (60 s ping/20 s timeout, only while an RPC is in flight; never below 60 s, the floor Google's own Go clients use, because Google front ends answer too-frequent pings with GOAWAY too_many_pings). Sharing channels across calls needs a credential-keyed refcounted cache and is not done |
| Google LLM | API-key mode already used `http.DefaultTransport` (shared); Vertex builds an authenticated transport per client | API-key mode uses the shared pool and can be warmed; Vertex is unchanged |

Pre-dial decisions (all in `core/netpool`): a spare is an anonymous TCP+TLS connection with no request, no credentials, no provider session. It cannot bill and never reports to a breaker, and it is closed after `warmup.spare_ttl` (default 10 s) if unused or if the peer closed it (checked with a non-consuming socket peek). It goes through the same dialer, so the `openai` SSRF dial guard applies to pre-dials. No speculative websocket is opened for any provider: Deepgram closes idle sockets after about 10 s and a stream is bound to one turn, Cartesia/ElevenLabs bind a context/session to the socket. One spare per host exists at a time, so only the first dial after a warm-up benefits; per-turn re-warm after a stream ends is not done (follow-up if live numbers justify it). Per-turn websockets have nothing to keep alive. `http.Transport` already retries a request on an idle connection the server closed.

Config (`warmup`, see `config/*.yaml`): `enabled` (default true, shared transports only; false restores one private pool per session), `predial` (default false), `on_start` (default false, needs `predial`; pre-dials default endpoints with a placeholder key, which also primes DNS and TLS session tickets), `timeout` (3 s), `spare_ttl` (10 s). Warm-up runs in a goroutine bounded by `timeout`, for every candidate in a failover chain, and is skipped (counted) when that provider's breaker is open.

Metrics (labels `provider`, `stage` only): `voice_provider_warm_dials_total{result=hit|miss}`, `voice_provider_warmups_total{result=ok|error|skipped}`, `voice_provider_connect_seconds` (histogram of fresh TCP+TLS connects, and gRPC time-to-READY for Google).

Manual live check (not run in CI, needs real keys): `RUN_LIVE_DEEPGRAM_TEST=1 DEEPGRAM_API_KEY=... go test -tags integration -run LiveWarm -v ./providers/deepgram` alternates cold and pre-dialed first connects and logs each. For the whole pipeline set `warmup.predial: true` on one canary, place calls, and compare `voice_latency_seconds` (first-turn TTFB stages) and `voice_provider_connect_seconds` against a canary without it; flip the default only if the p50 first-turn gain is real and `warmups_total{result="error"}` stays low.
