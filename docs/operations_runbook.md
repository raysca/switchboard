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

### Step 3: Run with Docker Compose
Use the reference compose configuration in `deploy/docker-compose.yaml`:
```bash
docker compose -f deploy/docker-compose.yaml up -d
```
The container runs with `read_only: true`, `cap_drop: ALL`, and tmpfs mounted at `/tmp`.

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
**Drain Sequence**:
1. Server marks itself unready (`/ready` returns 503).
2. Existing WebSocket calls continue normally up to `shutdown_timeout: 30s`.
3. New handshakes are rejected with 503.
4. After 30s, any remaining sessions are canceled cleanly.
5. All worker processes and network sockets are closed.
6. Process exits with status `0`.

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
- **Symptoms**: `/ready` returns 503; `provider_circuit_state{provider="..."} == 2`.
- **Underlying Mechanism**: 5 consecutive failures occurred against Google Cloud STT, LLM, or TTS within 60 seconds.
- **Action Required**:
  1. Check GCP Service Health status for Speech-to-Text or Vertex AI.
  2. Verify credentials and IAM permissions.
  3. Circuits will automatically probe the provider in Half-Open state after 30 seconds of cooldown.
