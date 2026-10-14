# Switchboard — Production Deployment & Operations Runbook

This runbook provides step-by-step instructions for deploying, operating, monitoring, and troubleshooting Switchboard in production environments.

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

Measured with `tools/loadtest` (see `docs/benchmarks/capacity.md`; Apple M4 Pro, echo provider, so these are framework-only figures):

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

The server also requires the flow store and its secrets. It refuses to start without them:

| Variable | Value |
|---|---|
| `TOKEN_SECRET` | Voice token signing secret (32+ characters). |
| `GOVOICE_MANAGEMENT_API_KEY` | Management API bearer key, at least 32 bytes, different from `TOKEN_SECRET`. |
| `GOVOICE_DATABASE_DSN` | Flow store DSN: a `postgres://` URL (`config/production.yaml` uses `storage.dialect: postgres`) or a SQLite file path. |
Provider keys are environment variables too, read on demand and never stored: `GOOGLE_APPLICATION_CREDENTIALS` (path to a mounted service-account JSON file), `GOOGLE_API_KEY`, `XAI_API_KEY`, `OPENAI_API_KEY`, `NVIDIA_API_KEY`, `DEEPGRAM_API_KEY`, `CARTESIA_API_KEY`, `ELEVENLABS_API_KEY` and `MCP_TOKEN_<NAME>` for MCP servers; [providers.md](providers.md) has the full table. Set the ones you use, then create flows with `POST /api/v1/flows` (see `docs/quickstart.md`); `GET /api/v1/providers` shows which are detected. To rotate a key, change the variable and restart. Upgrading from a build with stored credentials: the schema changed in place and nothing is migrated; start from a fresh database and recreate flows.

### Step 3: Run with Docker Compose
Use the reference compose configuration in `deploy/docker-compose.yaml`:
```bash
docker compose -f deploy/docker-compose.yaml up -d
```
The container runs with `read_only: true`, `cap_drop: ALL`, and tmpfs mounted at `/tmp`. The compose file also starts a `postgres` service for the flow store; set `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY` and `POSTGRES_PASSWORD` before `up`, and put the provider variables in `deploy/govoice.env` (mode 600, read through `env_file`; mount a Google service-account file read-only and point `GOOGLE_APPLICATION_CREDENTIALS` at it). `deploy/smoke.sh` is the keyless release check (no provider key, costs nothing): it builds the image, starts it next to a throwaway Postgres and probes `/ready` and `/metrics` from outside the container, checks that xAI speech is in the build and that SIGTERM exits 0. `deploy/smoke-google.sh` runs the compose stack end to end with a Google service account (`GOOGLE_APPLICATION_CREDENTIALS` on the host, mounted read-only): it creates a flow, checks it is ready and issues a voice token.

**Listeners.** `public_listen_addr` (`:8080`) serves `/ws` and `/health`. `admin_listen_addr` (`127.0.0.1:9090`, container loopback) serves the management API (`/api/v1`, bearer auth) and `/debug/timeline`; compose does not publish it, so run management calls with `docker exec govoice-server curl -H "Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY" http://127.0.0.1:9090/api/v1/...` (Kubernetes: `kubectl port-forward` or `exec`). `probe_listen_addr` (`0.0.0.0:9092`) serves only `/health`, `/ready` and `/metrics` without auth, so kubelet probes and Prometheus can reach the container; compose publishes it on `127.0.0.1` only. Never bind the admin listener beyond loopback, and keep the probe port off public ingress (cluster network only).

---

## 3. Deployment Path B: Systemd Native Service

The systemd unit is a reference file and is not required to run V1.

The layout mirrors the container image (`deploy/Dockerfile`). The server finds `vadworker` and `turnworker` next to its own executable, so all three binaries go in the same directory. Build with `make build` (it uses `-tags xaispeech`; `make ort` fetches the ONNX Runtime for your platform).

### Step 1: Install Binaries, Models and ONNX Runtime
```bash
# Create service user
sudo useradd -r -s /usr/sbin/nologin -d /opt/govoice govoice

# Binaries: server, vadworker and turnworker side by side
sudo install -d -m 755 /opt/govoice/bin /opt/govoice/models /opt/govoice/lib /opt/govoice/third_party /var/log/govoice
sudo install -m 755 bin/server bin/vadworker bin/turnworker /opt/govoice/bin/

# Models: Silero VAD (the unit's GOVOICE_MODEL_PATH) and the Smart Turn model (turn.analyzer)
sudo install -m 644 third_party/silero_vad.onnx /opt/govoice/models/silero_vad.onnx
sudo install -m 644 third_party/smart_turn_v3.2_cpu.onnx /opt/govoice/third_party/smart_turn_v3.2_cpu.onnx

# ONNX Runtime 1.29.0 for Linux (the unit's GOVOICE_ORT_LIB). The tarball URL and sha256 are in deploy/Dockerfile.
sudo install -m 644 onnxruntime-linux-x64-1.29.0/lib/libonnxruntime.so /opt/govoice/lib/libonnxruntime.so
sudo chown -R root:root /opt/govoice && sudo chown govoice:govoice /var/log/govoice
```

### Step 2: Install Configuration and Systemd Unit
```bash
sudo mkdir -p /etc/govoice
sudo cp config/production.yaml /etc/govoice/production.yaml   # the unit reads production.yaml
# On a host (not a container) probe_listen_addr "0.0.0.0:9092" binds every host interface: set it to
# "127.0.0.1:9092" (or the private interface Prometheus scrapes) or firewall 9092 from the internet.

# Copy and enable systemd unit
sudo cp deploy/govoice.service /etc/systemd/system/govoice.service
# /etc/govoice/env (mode 600) must set TOKEN_SECRET, GOVOICE_MANAGEMENT_API_KEY and
# GOVOICE_DATABASE_DSN, plus the provider variables you use (see Path A, Step 2).
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
3. **Admin Isolation**: the public vhost returns 404 for `/metrics` and `/debug/`. The management API listener (`9090`) is container loopback and never proxied; the internal-only vhost on `9091` proxies just `/health`, `/ready` and `/metrics` to the probe listener (`9092`).

---

## 5. Routine Operations & Maintenance

### Checking Health & Readiness
```bash
# Liveness (Should return HTTP 200)
curl -s http://127.0.0.1:9092/health

# Readiness: HTTP 200 {"status":"ready","circuits":{...}}; 503 {"status":"unready","reason":...} while starting,
# draining, without VAD capacity or when the flow store does not answer a ping
curl -s http://127.0.0.1:9092/ready | jq .
```

### Graceful Drain-Then-Cancel Shutdown
When rolling out updates or draining a host:
```bash
# Send SIGTERM to server
sudo systemctl stop govoice.service
# Or in docker:
docker stop -t 60 govoice-server   # at least drain_timeout + 5 s (50 s in production.yaml); the default 10 s would SIGKILL mid-drain
```
**Drain Sequence** (`limits.drain_timeout`, default 30s; 45s in production.yaml):
1. SIGTERM/SIGINT: `/ready` (probe listener) returns 503 immediately (`voice_draining 1`).
2. New calls are refused: a new `/ws` upgrade gets HTTP 503 `server_draining` before the token is checked (the token is not consumed, but it is bound to the issuing instance; see Affinity); a client already upgraded but not yet `client-ready` gets RTVI error `server_draining` and close 1001. The public listener stays open (public `/health` keeps answering) so stragglers routed during LB endpoint lag get a clear 503, not connection refused.
3. Active calls continue until they end or `drain_timeout` expires (`voice_drain_sessions_remaining` shows how many are left).
4. At the deadline each remaining call receives a fatal RTVI error `server_draining` followed by close 1001 (going away), then is force-closed. The client should reconnect.
5. The public listener, VAD workers and admin listener are closed. Exit `0` when all calls ended inside the deadline; exit `1` if calls had to be force-closed (or the shutdown budget ran out). Orchestrators ignore the code on a requested stop (Kubernetes, `docker stop`; systemd only logs `status=1/FAILURE` and does not restart on `systemctl stop`), so it is a signal for alerting on dropped calls, not a restart trigger.

### Rolling Deploys and Session Affinity
- **Readiness vs liveness**: point the load balancer / Kubernetes `readinessProbe` at `/ready` on the probe port (`:9092`, 503 during drain). Point `livenessProbe` at `/health` (probe `:9092` or public `:8080`; both stay up for the whole drain). Never use `/ready` for liveness: it is 503 for the whole drain and would kill the pod mid-drain.
- **Grace period**: `terminationGracePeriodSeconds` (Docker `stop_grace_period`, systemd `TimeoutStopSec`) must be at least `drain_timeout` + `preStop` sleep + 5s process margin (the server budgets `drain_timeout + 5s`). Production: 45s drain + 5s preStop + 5s margin => 60s or more (compose uses 60s).
- **preStop sleep**: add `preStop: sleep 5` so endpoint removal propagates to the LB before drain starts; SIGTERM is delivered after the hook. Anything still routed afterwards gets 503 `server_draining`. A retry on another instance only works if that instance can validate the token (see Affinity), so the client normally requests a fresh voice token instead. `deploy/nginx.conf` has a single upstream and no `proxy_next_upstream`; a multi-instance edge needs an upstream block with `proxy_next_upstream error http_503` and token issuance pinned as described below.
- **LB connection draining**: enable it (deregistration delay >= 0 is fine; do not set it shorter than the preStop sleep) and never have the LB cut established WebSockets when a target goes unhealthy. nginx: a reload keeps existing connections on old workers.
- **Affinity**: a call is one WebSocket pinned to one instance, and its pipeline, context and turn state live in that process's memory. There is no cross-node migration. Voice tokens are NOT portable between instances: every API-issued token carries the issuing process's random `issuer_epoch`, and any other process rejects it with 401 (`ErrTokenEpoch`, `docs/api/v1.md`). With more than one replica, `POST /api/v1/voice-tokens` and the `/ws` upgrade for that token must reach the same replica (sticky routing by client, or issue the token through the replica you will connect to); otherwise about (N-1)/N of calls fail with `invalid token`. A client that gets the drain 503 must request a new token (from a ready replica) and reconnect.
- **Admission rejects**: a refused call gets an RTVI error `capacity_exceeded` | `principal_limit` | `flow_limit` | `rate_limited` and close 1013 (try again later), counted in `voice_admission_rejects_total{reason}` (metric labels: `session_limit`, `vad_unavailable`, `principal_limit`, `flow_limit`, `start_rate_limit`; the first two both reach the client as `capacity_exceeded`). The client sees only the code, never the principal or counts. 1011 `session_setup_failed` is a real setup failure, so alert on it separately. `limits.handshake_rate_per_ip` (default 20/s, burst max(5, 2x); 0 means the default, it cannot be disabled) bounds WebSocket upgrades per source IP, refused with HTTP 429 before the upgrade. The source IP is the TCP peer unless that peer is listed in `tls.trusted_proxies`, in which case it is the right-most untrusted `X-Forwarded-For` entry (else `X-Real-IP`). Behind a load balancer, list it in `tls.trusted_proxies`, or every caller shares the balancer's bucket; raise the rate only for a NAT that really hides many clients. IPv6 sources are keyed by their /64 (for this limiter, the pending-handshake cap and `max_sessions_per_ip`). The table holds at most 10k sources (entries idle over 1 min are evicted when full); while 10k distinct sources are active, new sources share one overflow bucket (10x a single IP's rate) instead of all being refused, so a flood cannot lock out every new caller.
- **Ping liveness**: the server sends a WebSocket ping every `limits.ping_interval` (default 15 s); a peer that misses 2 pings in a row (each waits `limits.ping_timeout`, 10 s) is dropped. Browsers and ws libraries answer automatically, so a muted or text-only client stays connected; `PingMaxFailures` (2) has no YAML key. `limits.idle_timeout` is the legacy data-silence deadline and applies only when `ping_interval: 0`. An abandoned tab that still answers pings holds its slot until `limits.max_call_duration`, so set `idle.user_timeout` (e.g. 30s) in production. Each connection runs one extra ping goroutine and adds a small ping/pong frame every interval (see the perf gate notes in `docs/benchmarks/README.md`).
- **Telemetry env**: `ORT_DISABLE_TELEMETRY=1` is set by the server before ONNX Runtime starts and passed to the VAD and Smart Turn workers (`vad.WorkerEnv`); do not override it. Workers otherwise receive only an allowlisted environment, never provider credentials.
- **Reconnect semantics**: a force-closed client gets `server_draining` + 1001 and must start a new call (new voice token; tokens are single-use). Conversation context is not carried over; the app should replay it through the flow or accept a fresh start. Size `drain_timeout` to your typical call length to keep forced closes rare, and roll one instance at a time.
- **After a crash or kill**: the next start ends the calls the old process left `active` in storage. With SQLite (one process owns the file) that is all of them; with Postgres it is those older than `limits.max_call_duration` plus 5 minutes, because another replica may still own younger ones (a call that crashed more recently stays `active` until a later restart). They show as ended at their last finished turn with the note "Ended when the server restarted.", and an MCP server or tool that only they were holding back can then be deleted. The server logs `ended sessions left active by a previous process` with the count. Two cautions: never run two processes on one SQLite file (the new process would end the old one's live calls, so stop the old one first), and with Postgres keep `limits.max_call_duration` the same on every replica (a replica that starts with a smaller value than an older one could end that one's long calls).
- **Provider test**: `POST /api/v1/providers/{id}/test` (the Test connection button) sends one tiny request per service of that provider: a one-word reply, the word "ok" spoken, 200 ms of silence. It runs only when pressed, at most once per 5 s per provider and one at a time, and each service is cut off after 15 s. It uses the same circuit breakers as calls, so repeated server-side failures can open one (a bad key does not); a paid provider is billed those few units per press.
- **Greetings**: a step's `greeting` is an additive config field. Before rolling back to a binary older than this feature, remove greetings from the flows (an older server rejects the unknown field when it reads such a version). `voice_greeting_failed_total` counts greetings that could not be queued (the session was closing); a greeting that was queued but failed to synthesize shows in the TTS error metrics instead, and the call carries on silently.
- **Draft test calls**: a draft (`POST /api/v1/voice-tokens` with `draft`) is held in memory until its token connects, at most 32 at once. A restart loses them; the editor asks for a new token. Their sessions show `draft: true` and are otherwise ordinary.
- **Rolling upgrades and missing providers**: in the production profile, startup refuses to boot (other profiles only log the error) when an enabled, non-archived stored flow uses a provider or stage that has no factory in this binary (for example a flow with `xai` STT/TTS on a build without `-tags xaispeech`, or a provider id from a newer release). During a rolling upgrade or rollback the old binary therefore crash-loops if the shared flow store already holds a flow that only the new binary understands. Roll forward first and create flows that use new providers only after every replica runs the new build; to roll back, disable, archive or edit those flows first. The failure names each `flow <id> <stage>=<provider>`.
- **Kubernetes sketch**: `terminationGracePeriodSeconds: 60`, `lifecycle.preStop.exec.command: ["sleep","5"]`, `readinessProbe.httpGet: {path: /ready, port: 9092}`, `livenessProbe.httpGet: {path: /health, port: 9092}`; scrape `:9092/metrics` with Prometheus and load `deploy/alerts/govoice.rules.yml`.

---

## 6. Incident Troubleshooting & Failure Recovery

### Incident 1: Native VAD Worker Crash
- **Symptoms**: Alert `GovoiceVADWorkerRestarts` (`increase(voice_vad_worker_restarts_total[10m]) > 5`) or `GovoiceVADUnavailable` (admission rejects with `reason="vad_unavailable"`).
- **Underlying Mechanism**: A Silero VAD C/ONNX native routine crashed (e.g. SIGSEGV).
- **Automated Mitigation**: The parent process detects the child exit, reaps the PID via `cmd.Wait()`, updates capacity, and automatically spawns a healthy replacement worker with exponential backoff.
- **Action Required**: Inspect `journalctl -u govoice.service` for child stderr diagnostics (bounded to 64KB). Verify that the host is not experiencing OOM kills (`dmesg -T | grep -i oom`).

### Incident 2: Leaked Goroutines / Leaked Work
- **Symptoms**: Alert `GovoiceLeakedWork` (`increase(voice_leaked_work_total[15m]) > 0`; `voice_leaked_sessions_total` counts the affected sessions).
- **Underlying Mechanism**: A session was terminated, but a managed child task failed to exit within `TeardownTimeout: 2s`.
- **Action Required**: Query the admin listener's `/debug/timeline?session=<ID>` for the affected session (inside the container, port 9090). Check if an external provider call hung without respecting context cancellation.

### Incident 3: Cloud Provider Circuit Breakers Tripped
- **Symptoms**: alert `GovoiceProviderCircuitOpen` (`voice_{stt,llm,tts}_circuit_state{provider="..."} == 1`; 0 = closed, 1 = open, 2 = half-open; Nvidia has one series per endpoint with an `endpoint` label); `/ready` lists the circuit as `open` under `circuits` (it stays 200, because only flows using that provider are affected). `voice_provider_errors_total{provider,stage}` shows which provider the errors came from (`failover` when the stage had fallbacks).
- **Underlying Mechanism**: 5 consecutive failures occurred against the provider (Google, xAI or an Nvidia endpoint) STT, LLM, or TTS.
- **Action Required**:
  1. Check GCP Service Health status for Speech-to-Text or Vertex AI.
  2. Verify the provider key in the server's environment (`GET /api/v1/providers` shows whether it is detected; a wrong value shows up as provider errors) and IAM permissions.
  3. Circuits will automatically probe the provider in Half-Open state after 30 seconds of cooldown.
  4. Flows with STT fallbacks move the open utterance to the next provider and replay its buffered audio (`voice_stt_failover_total`); each new utterance tries the primary again once its breaker allows. `stt.replay_buffer` (default `10s`, max `60s`, ~32 KB/s per call with fallbacks) bounds how much of a long utterance survives; `voice_stt_replay_dropped_bytes_total` counts utterance audio that did not fit.

## Idle handling (`idle:`)

Off by default; no processor is added when off. `idle.user_timeout` (e.g. 15s) prompts a silent user after the bot stops speaking (and from call start, so a caller who connects and never speaks is released too), `idle.max_prompts` (default 2) times, one timeout apart, then ends the call with reason `user_idle` (optional spoken `idle.goodbye`); any speech, DTMF or typed input resets the count, the controller's own prompts do not. Prompts are skipped while a tool runs, the user is muted, a reply is generating or playing, or either side speaks. An idle hang-up sends a non-fatal RTVI `error` whose text is the reason (`user_idle` / `bot_idle`) and closes the WebSocket with 1000 (normal): clients must treat it as final, unlike the drain's `server_draining` + 1001 which means reconnect elsewhere. `idle.prompt_mode: llm` (default) injects a labelled non-user notice so the model phrases the nudge; `say` speaks `idle.prompt` verbatim. `idle.bot_timeout` (e.g. 30s) is the stuck-bot watchdog: a finished user turn with no bot speech sends the client an error, cancels the generation and optionally speaks `idle.bot_fallback`; `idle.bot_max_stuck` (default 3) in a row ends the call (`bot_idle`). A reply that finishes with no speech (tool-only) is not stuck. The watchdog is the caller's patience bound and cancels whatever is in flight, including an LLM failover (which may start a fallback up to 60 s into a turn); set `bot_timeout` above that budget if failovers must always run to completion. Debug events `idle.user_prompt`, `idle.user_timeout`, `idle.bot_stuck`; counters `voice_user_idle_prompts_total`, `voice_user_idle_hangups_total`, `voice_bot_idle_stuck_total`, `voice_bot_idle_hangups_total` (added when a call ends). Server-wide for now; per-flow settings come with the flow config. The WebSocket liveness policy is separate: the server pings every `limits.ping_interval` (default 15s) and waits `limits.ping_timeout` (default 10s) for the pong; after 2 consecutive misses it counts `voice_ws_ping_failures_total`, sends a non-fatal RTVI error `peer_unresponsive` and drops the socket (no close frame; counted as a client disconnect). Reads have no data deadline, so muted or text-only clients stay connected while they answer pings; `limits.idle_timeout` now applies only when `ping_interval: 0` (legacy: silent for that long = disconnected). A dead peer is dropped between `ping_interval + ping_timeout` and `2 x ping_interval + ping_timeout` after it goes silent (25-40 s by default); misses while the server is still handling a message (e.g. session setup) are not counted. Validation: `0 < ping_timeout < ping_interval`. A client that answers pings but never speaks (an abandoned tab) holds its session slot until `limits.max_call_duration` unless `idle.user_timeout` is set; set it in production.

## Turn manager: `min_words` and Smart Turn

`turn.min_words` starts turns (and barge-in) from transcripts, so it needs an STT that sends interim results. With one that sends only finals (Google `chirp_2`; `latest_long` sends interims) the turn starts only when the final arrives, about 1.5 s after the speech, and the bot is not interrupted before that. The server logs `turn.min_words is on but the STT sent no interim results` and counts `voice_turn_min_words_no_interims_total` once per call where the first transcript is already final; if it rises, switch the flow's STT model or disable `min_words`. An unstarted turn waits up to 2.5 s after the VAD stop for its final (fixed, `core/turn` `unstartedNoFinalExpiry`). Smart Turn (`turn.stop: [turn-analyzer, speech-timeout]`) holds a turn the model calls incomplete for `turn.analyzer.stop_secs` (default 3 s, Pipecat's value); in live tests it held a plain recorded question that long, so lower `stop_secs` if that cost matters (`docs/benchmarks/turn-manager-live.md`).

A client hang-up while the bot is speaking is not a session failure: audio still in flight after the WebSocket closed, and a Google STT `Send` on a stream the hang-up already ended (`io.EOF`), are dropped instead of logging `session failed`.

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

## Live-call control

`POST /api/v1/sessions/{id}/context` and `/run` (management key only; see `docs/api/v1.md`) let a backend add operator notes to a call and make the bot respond. Operational facts:
- A call is reachable only on the instance that holds it. With several instances, route by session affinity or try each; a 404 means "not here or already ended".
- Notes wait until the caller and the bot are both quiet, or join the caller's next turn if it comes first (after the caller's message in the history; that turn is then the reply), so a note can take a few seconds to land. When a caller's text (spoken or typed) mentions one of the server notice phrases (operator note, idle notice, turn-completion notice, system notice, the summary of earlier turns, the greeting after a route, in any spacing or case), its brackets are turned into parentheses so it loses the exact shape of a server message. This is a heuristic: "(note from the operator) ..." still reads like a note to a model, look-alike letters from other scripts are not folded, and caller text stays untrusted. A burst is bounded: 4 requests then 1 per second per call, 8 notes per request, at most 8 notes and run requests waiting per call (beyond that, or while the call is momentarily too busy, 429 `too_many_pending`: retry, nothing queued), and the context budget still evicts old messages. Notes still waiting when a call ends are counted in `voice_async_tool_results_dropped_total`.
- Replies started by `run` count in `voice_async_tool_turns_total`, the same counter as async tool replies; a run that joins the caller's own turn is that turn's reply and is not counted separately.
- Requests are logged with the session id, the count and `run`, never the note text.
- Trust: the management key can steer any live call, the same level of trust as editing a flow.

## TTS streaming

xAI TTS always streams over one WebSocket per reply. Google streams only Chirp 3 HD voices (`<locale>-Chirp3-HD-<Name>`, one `StreamingSynthesize` stream per sentence); every other Google voice uses the unary request. A stream that cannot open, or fails before any audio is sent, retries as the unary request, so the call keeps its speech. Once audio has started, a failure ends the sentence with an error (no replay).

Kill switches (config, default `false`): `providers.google_tts.disable_streaming` and `providers.xai_tts.disable_streaming`. Set `providers.xai_tts.disable_streaming: true` if a proxy blocks WebSockets to the xAI endpoint.

Google quotas, per project: 100 concurrent streaming sessions and 200 Chirp 3 requests per minute. A speaking call holds one stream at a time, so about 100 calls can speak at once per project. Beyond that the stream open fails and that unit is served unary.

Seeing fallbacks: `voice_tts_retries_total` counts retry attempts across all TTS providers and has no provider label. For Google Chirp 3 HD and xAI a retry is a unary fallback, so a rise while those providers carry the traffic means streams are failing and units are being served unary; the TTS error log lines name the provider. A stream that stays silent for the provider timeout (10 s default) is abandoned and also falls back.

## Failover timing

How long a stage waits on its primary before a fallback takes over. Applies only to flows with `fallbacks` for the stage; a stage without fallbacks keeps the providers' own retries unchanged (LLM: 3 x 30 s, so a hung primary only errors at about 90 s).

| Stage | Primary with a fallback | Worst case before the fallback starts | Without fallbacks (unchanged) | Tune |
|---|---|---|---|---|
| LLM | first-token deadline `failover.llm_first_token` (default 8 s): a candidate that produced no text or tool call by then is cancelled and its breaker told. A generation that has started streaming is never cut by this deadline. | 8 s per candidate (about 16 s with two fallbacks) | 3 attempts x 30 s + backoff, about 90 s | `failover.llm_first_token` |
| LLM budget | no new candidate is started later than `failover.llm_budget` (default 60 s) after the generation began | n/a | n/a | `failover.llm_budget`; must exceed `llm_first_token` (config is rejected otherwise) |
| STT open | Deepgram and ElevenLabs 2 x 5 s (each try reports to the breaker); Google, xAI, Cartesia and NVIDIA 2 x 5 s (always, fallbacks or not; one breaker report per open, not per try) | about 10 s | Deepgram 3 x 5 s (each try counts against its breaker) | provider `STTConfig.RetryPolicy` |
| TTS request | Cartesia, ElevenLabs, xAI, Google: 2 attempts, each keeping the provider's request timeout (it bounds the whole sentence, dial to last audio byte, so it is not shortened: a long sentence must not be cut) | Cartesia/ElevenLabs about 30 s, xAI/Google about 20 s | Cartesia/ElevenLabs 3 x 15 s (45 s), xAI/Google 3 x 10 s (30 s) | `failoverPolicy` in `server/tts_failover.go`; provider `Timeout` |
| TTS unit watchdog | 30 s + 20 ms per character, a backstop for a provider that hangs without erroring | 30 s+ | same | `core.TTSFailoverConfig` |

Notes:
- The last candidate of a chain keeps its full retries: nothing is left to fail over to, so cutting it short would only lose answers.
- The provider request timeout still bounds a whole LLM stream (30 s), with or without fallbacks, so the first-token deadline is what rescues a hang and the provider timeout is what ends a runaway generation.
- A first-token timeout counts against the provider's circuit breaker, so later calls start on the healthy provider (open breakers are tried last).
- `idle.bot_timeout` cancels an LLM failover in flight; keep it above `failover.llm_first_token` times the chain length.
- "First token" is the first non-empty text delta or the first tool execution (thinking/reasoning and usage frames do not count; no provider emits tool-call frames before running the tool). A model that thinks, or decides on a tool call, for longer than `llm_first_token` is abandoned like a hung one: raise it for reasoning models. Once a tool ran or text streamed, later rounds (slow tool, second model round) are never cut by it.
- Each first-token timeout is one breaker failure; 5 in a row (no success between, across calls) open that provider's breaker for 30 s, so a primary that is merely slow (overloaded, huge context) is shed to the fallback for a cooldown. The abandoned request is cancelled; its prompt tokens may still be billed.

```yaml
failover:
  llm_first_token: 6s   # default 8s
  llm_budget: 45s       # default 60s
```

## Public admin mode (single port)

By default the management API (`/api/v1`) lives on the loopback admin listener and the admin UI runs separately. With `public_admin: {enabled: true}` the public listener also serves the management API and the embedded admin UI (build it in with `make ui-embed`), so one port carries the UI, the API and the voice socket. It is off by default.

- Every management route still needs the management key. `/metrics`, `/ready` and `/debug/*` are not mounted on the public port (they answer non-200 there): metrics expose internals and stay on the admin listener. `/health` stays public.
- `origin_allow_same_host: true` admits a browser whose `Origin` host equals the request `Host`, so the page can open the voice socket without listing its own origin in `origin_allowlist`. A foreign browser origin is still rejected.
- Production requires `tls.trusted_proxy_mode` for both keys: the `Host` header and the client address must come from your proxy, not the caller. See `config/cloudrun.yaml`.
- Failed management-key attempts share ONE global allowance (burst 10, one more every 6 s). A correct key is checked first and is never throttled, so no flood of bad keys, from any address, can lock the admin out. Once the allowance is spent, a wrong or missing key gets 429 `too_many_attempts` ("too many failed attempts; try again shortly", with `Retry-After`) instead of 401, and the UI shows that message. A missing key counts as a failure. `voice_management_auth_failures_total` counts every failed attempt and `voice_management_auth_throttled_total` those answered 429.
- `GET /api/v1/server-config` (key required, never cached) reports `client_ip` and `secure`; check it after each deploy. If `client_ip` is a Google front-end/load-balancer address or `secure` is false, add the proxy's CIDR to `tls.trusted_proxies`, otherwise every caller shares one bucket in the per-IP rate limits and the handshake limiter (the auth throttle is global and unaffected).
- End-to-end check: `make e2e-oneport` (echo provider, no secrets).
