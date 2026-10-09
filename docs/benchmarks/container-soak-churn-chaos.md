# Container Soak, Churn & Fault Injection Test Report

## Executive Summary

To validate production readiness, resource sizing, and fault tolerance without incurring cloud provider charges, a comprehensive automated performance suite was executed against the **go-voice** server running inside a resource-constrained Docker container with zero paid cloud dependencies (`-tags fakeproviders`).

### Sizing & Budget Constraints
- **Container Environment:** Linux container on macOS host (`arm64`), managed via Docker Compose (`deploy/docker-compose.perf.yaml`).
- **Resource Constraints (Enforced):**
  - **CPU Limit:** 2.0 Cores (`limits.cpus: "2.0"`, reservation 1.0)
  - **Memory Limit:** 2048 MB RAM (`limits.memory: "2048M"`, reservation 512M)
- **Cloud Spend:** **\$0.00** (100% offline fake STT, LLM, TTS providers with real audio streaming and Silero VAD running in process-isolated worker).
- **Execution Script:** `scripts/run-container-perf.sh`

---

## 1. Capacity Stepping Sweep (Connections vs. Resources)

- **Test Objective:** Measure throughput, voice-to-voice (V2V) latency, and process tree growth across increasing concurrency steps (4, 8, 16, 24 concurrent calls) under the 2 CPU / 2 GB RAM envelope.
- **Budget Target:** Voice-to-Voice p95 $\le 1117\text{ ms}$.

### Capacity Step Results

| Sessions | Client V2V p50 (ms) | Client V2V p95 (ms) | Server V2V p95 (ms) | Error % | Rejects | CPU Used (Cores) | Pacing Lag p99 (ms) | Verdict |
|:--------:|:-------------------:|:-------------------:|:-------------------:|:-------:|:-------:|:----------------:|:-------------------:|:-------:|
| **4**    | 944.0               | 966.6               | 295.0               | 0.00%   | 0       | 0.05             | 5.0                 | **PASS** |
| **8**    | 943.5               | 965.3               | 295.0               | 0.00%   | 0       | 0.25             | 5.0                 | **PASS** |
| **16**   | 943.8               | 965.4               | 295.0               | 0.00%   | 0       | 0.36             | 5.0                 | **PASS** |
| **24**   | 944.1               | 965.1               | 295.0               | 0.00%   | 0       | 0.58             | 2.0                 | **PASS** |

### Capacity Findings
- **Max Safe Sessions at Budget:** 24 concurrent sessions (utilizing only **0.58 cores** of the 2.0 core budget).
- **Density:** **41.4 concurrent sessions per CPU core**.
- **Latency Stability:** V2V p95 remained rock-solid between 965 ms and 966 ms across all load steps, with zero admission rejections and zero timeouts.
- **Leak-Free Teardown:**
  - Goroutines: 19 idle $\rightarrow$ 331 peak $\rightarrow$ **19 after drain** (0 residual).
  - Open File Descriptors: 84 idle $\rightarrow$ 113 peak $\rightarrow$ **84 after drain** (0 residual).

---

## 2. Simultaneous Call Churn (30s – 60s Call Lifespans)

- **Test Objective:** Simulate high-churn traffic where 16 slots constantly establish calls, exchange conversational turns, terminate after 30s to 60s, and immediately spawn replacement calls with fresh tokens.
- **Duration:** 90 seconds continuous churn.

### Churn Results

| Metric | Result |
|:---|:---|
| **Target Concurrent Sessions** | 16 |
| **Sessions Started** | 67 |
| **Sessions Completed Gracefully** | 51 (remaining 16 active at cutoff) |
| **Turns Attempted / Completed** | 355 / 350 |
| **Turn Timeouts** | **0** |
| **Error Rate** | **0.00%** |
| **V2V Latency (Client p50 / p95)** | 942.9 ms / 963.6 ms |
| **RSS Steady / Peak** | 207 MB / 212 MB |
| **RSS Growth Over Steady Window** | **+1.4%** |
| **Goroutines Post-Drain** | **19 (0 residual growth)** |
| **FDs Post-Drain** | **84 (0 residual growth)** |

### Churn Findings
- Rapid cycle churn does not trigger memory fragmentation or session descriptor leaks.
- Token issuance, session initialization, and WebSocket teardown pipelines clean up immediately on disconnect.

---

## 3. Fault Injection & Chaos Testing

- **Test Objective:** Inject deliberately malicious or abruptly failing clients into 20% of sessions while normal traffic flows concurrently, ensuring the server isolates failures without panicking, crashing, or affecting healthy sessions.
- **Chaos Injections Tested:**
  1. **Abrupt TCP Abort (`tcp_abort`):** Forceful TCP socket drop (`CloseNow()`) mid-stream without WebSocket close handshake.
  2. **Malformed Binary Frame (`malformed_frame`):** Transmission of illegal binary frames to test protocol decoder rejection.
  3. **Stalled Consumer (`stalled_consumer`):** Reader abruptly stops reading incoming audio, testing backpressure and slow-client termination.

### Chaos Results

| Metric | Result |
|:---|:---|
| **Concurrent Sessions** | 12 |
| **Total Injected Faults** | **13** across 69 calls |
| **Fault Breakdown** | `tcp_abort`: 6, `stalled_consumer`: 4, `malformed_frame`: 3 |
| **Protocol Errors Recorded by Server** | 3 (exact 1:1 match with malformed frames) |
| **Healthy Calls Affected** | **0** (healthy calls completed turns with 0.00% error rate) |
| **Server Panics / Crashes** | **0** |
| **Post-Chaos Container Health Probe** | **HTTP 200 OK (`status: ready`, all circuits closed)** |
| **Goroutines / FDs Post-Drain** | 19 / 84 (exact baseline return) |

### Chaos Findings
- Protocol error handling cleanly isolates invalid frames to the offending connection.
- TCP drops and stalled readers trigger standard connection cleanup routines without blocking worker threads or orphaning VAD streams.

---

## 4. Summary & Verification Commands

All test scenarios and infrastructure are fully automated and reproducible:

```bash
# Run complete container performance, churn, and chaos suite:
./scripts/run-container-perf.sh

# Or run individual loadtest commands against a container:
docker compose -f deploy/docker-compose.perf.yaml up -d
go run ./tools/loadtest -target-url 127.0.0.1:8080 -admin-url http://127.0.0.1:9090 -metrics-url http://127.0.0.1:9092 -container govoice-perf -sessions 16 -duration 60s
docker compose -f deploy/docker-compose.perf.yaml down -v
```
