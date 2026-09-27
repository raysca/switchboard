# Voice Framework V1

Voice Framework V1 is a high-performance, low-latency, resilient voice agent runtime built in Go. It connects bidirectional WebSocket voice clients to Google Cloud and xAI neural speech and language models with deterministic turn-taking, sub-200ms barge-in interruption, and bounded resource safety.

---

## Key Capabilities

- **Unmodified Pipecat Compatibility**: Integrates seamlessly with official, unmodified `@pipecat-ai/client-js@1.13.1` and `@pipecat-ai/websocket-transport@1.7.2` using binary Protobuf frames and the RTVI 2.1.0 protocol.
- **Sub-200ms Barge-In Interruption**: Solves the client-side audio buffer clear limitation via server-side media-clock pacing and bounded queues, ensuring immediate assistant silence upon user speech.
- **Fault-Isolated Neural VAD**: Runs Silero VAD inside supervised worker child processes, ensuring that native C/C++ or ONNX faults or segmentation faults cannot crash the Go server.
- **Provider Interchangeability**: Supports complete Google Cloud pipelines (Chirp 2 STT + Gemini 2.5 Flash LLM + Journey TTS) and drop-in xAI Grok LLM interchangeability (Scenario C) with zero core runtime modifications.
- **Bounded Resource Safety**: Enforces message and token context budgets, async task budgets, priority control queues, and data shedding under backpressure.
- **Production-Ready Observability**: Provides separation of public (`:8080`) and administrative (`:9090`) listeners, cheap local `/health`, compound `/ready`, Prometheus metrics conforming to Spec §35 without high-cardinality labels, and a per-session circular diagnostic timeline.

---

## Quick Start

### 1. Build Binaries and Examples
```bash
make build
```

This compiles:
- `bin/server`: Production voice server
- `bin/vadworker`: Isolated child worker process for Silero VAD
- `bin/bench`: Capacity and benchmark harness
- `examples/...`: Programmatic pipeline examples

### 2. Run Complete Verification Suite
```bash
make verify
```

Validates code formatting, runs all tests with `-race` across all modules (`core`, `providers/google`, `providers/xai`, `server`, `vad`, `transport`), verifies Protobuf binary fixtures, and confirms the compatibility manifest.

### 3. Launch Development Server
```bash
./bin/server -config config/development.yaml
```

Check health:
```bash
curl http://127.0.0.1:9090/health
# {"status":"ok"}

curl http://127.0.0.1:9090/ready
# {"status":"ready","uptime":"2s","checks":{...}}
```

---

## Documentation Index

- [Quick Start Guide](docs/quickstart.md): Step-by-step guide to minting an admission token, starting the server, and connecting with the Pipecat JS client.
- [Architecture Overview](docs/architecture.md): Core runtime model, 7-stage pipeline, generations, interruption barriers, and memory bounds.
- [Wire Protocol & RTVI](docs/wire_protocol.md): Protobuf framing, RTVI 2.1.0 event dictionary, audio formats, resampling, and media-clock pacing.
- [Providers & Lifecycle](docs/providers.md): Google Cloud STT/LLM/TTS, xAI Grok LLM, stream rotation, retries, and circuit breakers.
- [Security & Admission](docs/security.md): Single-use JWT tokens, replay prevention, query token redaction, origin allowlists, and admin port isolation.
- [Observability & Telemetry](docs/observability.md): Configuration profiles, `/health` vs `/ready`, Prometheus metrics (Spec §35), timelines, and incident troubleshooting.
- [Pipecat Compatibility Record](docs/pipecat-compatibility.md): Pinned coordinates, preserved behaviors, intentional differences, and non-goals.
- [Operations Runbook](docs/operations_runbook.md): Production container & systemd deployment, Nginx TLS reverse proxy, graceful drain shutdown, and failure recovery.
- [Capacity & SLO Report](docs/benchmarks/capacity_slo_report.md): Host capacity derivation (250 calls/host), release SLO verification table, and synthetic VAD disclaimer.
- [Release Criteria Evidence Matrix](docs/release_evidence.md): Audit table mapping every Spec §46 release criterion to passing test evidence.

---

## Programmatic Examples

Voice Framework V1 provides standalone, executable examples independent of the HTTP/WebSocket server:

- [`examples/google_pipeline`](examples/google_pipeline/main.go): Complete Google Cloud pipeline (Silero VAD + Google STT + Gemini LLM + Cloud TTS).
- [`examples/xai_pipeline`](examples/xai_pipeline/main.go): Scenario C interchangeable LLM pipeline (Google STT + xAI Grok LLM + Cloud TTS).
- [`examples/custom_processor`](examples/custom_processor/main.go): Adding a custom application processor (profanity filter / audit logger) without changing the core runtime.

---

## License

Voice Framework V1 is released under the Apache 2.0 License.
