# Switchboard

Switchboard is a high-performance, low-latency, resilient voice agent runtime built in Go. It connects bidirectional WebSocket voice clients to Google Cloud and xAI neural speech and language models with deterministic turn-taking, sub-200ms barge-in interruption, and bounded resource safety.

---

## Key Capabilities

- **Unmodified Pipecat Compatibility**: Integrates seamlessly with official, unmodified `@pipecat-ai/client-js@1.13.1` and `@pipecat-ai/websocket-transport@1.7.2` using binary Protobuf frames and the RTVI 2.1.0 protocol.
- **Sub-200ms Barge-In Interruption**: On user speech the server stops sending assistant audio. The pinned Pipecat client does not clear audio it has already buffered; residual playback is bounded by the 100 ms server pacing queue.
- **Fault-Isolated Neural VAD**: Runs Silero VAD inside supervised worker child processes, ensuring that native C/C++ or ONNX faults or segmentation faults cannot crash the Go server.
- **Stored Agents**: Every call runs a stored agent (STT, LLM and TTS provider, model, voice, prompt, MCP tools) managed through the `/api/v1` management API. Provider keys come from the server's environment only (never stored or returned; see [docs/providers.md](docs/providers.md)), and each call pins the agent version it started with.
- **Agent Graphs (multi-agent handoff)**: An agent can be a graph of agents drawn on a canvas in the admin UI: each node has its own prompt, MCP tools, LLM and voice, and the model hands the call to another node through a generated `transfer_to_<node>` tool. Only the node holding the call is built; a left node is closed. A configurable sound can cover the gap. See [Management API v1](docs/api/v1.md) and [audio](docs/audio-filters.md).
- **Provider Interchangeability**: Agents can use Google Cloud for every stage (Chirp 2 STT + Gemini LLM + Chirp 3 HD TTS) or swap in xAI Grok for the LLM (Scenario C) with zero core runtime modifications. xAI STT/TTS are only available in builds made with `-tags xaispeech`. Also available: Nvidia (NIM LLM plus Riva STT and TTS over gRPC; live-checked, see [docs/benchmarks/nvidia-live.md](docs/benchmarks/nvidia-live.md)), Cartesia and ElevenLabs STT and TTS, and Deepgram STT.
- **Ping Liveness**: The server pings every WebSocket (`limits.ping_interval`, default 15 s) and drops a peer that misses two pings in a row, so dead connections free their slot while muted or text-only clients stay connected.
- **Bounded Resource Safety**: Enforces message and token context budgets, async task budgets, priority control queues, and data shedding under backpressure.
- **Production-Ready Observability**: Provides separation of public (`:8080`) and administrative (`:9090`) listeners, cheap local `/health`, compound `/ready`, Prometheus metrics conforming to Spec §35 without high-cardinality labels, and a per-session circular diagnostic timeline.
- **Pipecat feature coverage** (defaults, config keys and status per feature in [docs/pipecat-parity-matrix.md](docs/pipecat-parity-matrix.md)): observer and latency metrics, LLM/STT/TTS failover, turn strategies (speech-timeout, eager, LLM completion, mute), Smart Turn v3 analyzer, idle handling, DTMF, call recording, OpenTelemetry export, PII redaction, context summarization, input denoise and background audio, plus a perf gate (advisory in CI until a runner-class baseline is committed; policy in [docs/benchmarks/README.md](docs/benchmarks/README.md)).

---

## Quick Start

`make help` lists every target, grouped by cost: free (fake providers), read-only previews, and PAID live checks that refuse to run without `GOVOICE_LIVE=1`.

### Fastest path: `make dev`
```bash
make dev                       # server + admin UI, ready to call
make example-web               # in another terminal: browser client
make example-cli TEXT="Hello"  # or a CLI call (omit TEXT for a voice clip)
make example-go NAME=google_pipeline   # standalone Go examples
```

`make dev` builds everything, installs UI dependencies, generates the management
key and token secret, and starts the server and admin UI. The server creates a
local SQLite store in `.govoice-dev/` (git-ignored; delete it to start over).
Provider keys come from `examples/.env` (template: `examples/.env.example`), which
the dev server reads as its environment; nothing is stored. If any provider is set,
`make dev` creates a ready "Dev Assistant" agent from the detected providers that
the examples call, and a two-agent "Dealership desk" graph (front desk hands buyers
to sales) for trying handoffs on the agent's call stage. The management key
for the admin UI is in `.govoice-dev/dev.env`. Ports default to 8080 (public),
9090 (admin), 5173 (UI) and 3000 (web client). Running `make dev` again stops the
previous dev stack from the same checkout first. If another program holds a default
port, the next free one is used and printed (the examples follow it). A port you name,
e.g. `PUBLIC_PORT=18080 make dev`, is used exactly or the run fails naming the holder.

The steps below do the same by hand.

### 1. Build Binaries and Examples
```bash
make build
```

This compiles:
- `bin/server`: Production voice server
- `bin/vadworker`: Isolated child worker process for Silero VAD
- `bin/turnworker`: Isolated child worker process for the Smart Turn model
- `examples/...`: Programmatic pipeline examples

### 2. Run Complete Verification Suite
```bash
make verify
```

Validates code formatting, runs all tests with `-race` across all modules (root, `core`, every `providers/*`, `server`, including `vad`, `smartturn`, `transport`, `tools/*`), verifies Protobuf binary fixtures, and confirms the compatibility manifest.

### 3. Launch Development Server
The agent store is required. Provide its DSN, a management API key and the provider variables you use:
```bash
mkdir -p data
export GOVOICE_DATABASE_DSN="$PWD/data/govoice.db"            # sqlite path, or a postgres:// URL with storage.dialect: postgres
export GOVOICE_MANAGEMENT_API_KEY="$(openssl rand -hex 32)"     # >= 32 bytes
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa-key.json      # Google STT, TTS, LLM
export XAI_API_KEY="..."                                        # optional: xAI LLM, STT, TTS
./bin/server -config config/development.yaml
```

Check health:
```bash
curl http://127.0.0.1:9090/health
# {"status":"live"}

curl http://127.0.0.1:9090/ready
# {"circuits":{...},"status":"ready"}
```

### 4. Set the Variables, Create an Agent, Connect
YAML defines no pipelines, models, voices or provider keys. Provider keys are environment variables, read on demand and never stored:

| Variable | Unlocks |
|---|---|
| `GOOGLE_APPLICATION_CREDENTIALS` (path to a service-account JSON) | Google STT, TTS, LLM |
| `GOOGLE_API_KEY` | Google LLM (Gemini API) |
| `XAI_API_KEY` | xAI LLM, STT, TTS |
| `OPENAI_API_KEY`, optional `OPENAI_BASE_URL` | OpenAI-compatible LLM |
| `NVIDIA_API_KEY`, `NVIDIA_STT_FUNCTION_ID`, `NVIDIA_TTS_FUNCTION_ID` | NVIDIA LLM, Riva STT, TTS |
| `DEEPGRAM_API_KEY` / `CARTESIA_API_KEY` / `ELEVENLABS_API_KEY` | Deepgram STT / Cartesia STT, TTS / ElevenLabs STT, TTS |
| `MCP_TOKEN_<NAME>` | MCP server bearer token (`auth_env`) |

1. `GET /api/v1/providers` shows which providers are detected (variable names only).
2. `POST /api/v1/agents` with a `config` naming the `stt`, `llm` and `tts` providers.
3. `POST /api/v1/voice-tokens` with `{"principal": "...", "agent_id": "..."}`.
4. Connect to `ws://127.0.0.1:8080/ws?token=<token>`.

To rotate a key, change the variable and restart.

> **Release note (environment-only credentials):** delete `.govoice-dev` (or any development database): the schema changed in place; nothing is migrated. The credential API, `GOVOICE_CREDENTIAL_KEYS` and `credential_id` are gone; set the provider variables and recreate agents. Self-hosted NVIDIA NIM and Riva endpoints are not supported in this release: `nvidia` always uses the hosted NIM (`integrate.api.nvidia.com`) and NVCF Riva endpoints, and `providers.nvidia.allow_private` is removed (a config that still sets it fails to load). They come back as catalog environment fields when needed.

[docs/quickstart.md](docs/quickstart.md) has the full commands.

---

## Documentation Index

**Getting started**
- [Quick Start Guide](docs/quickstart.md): start the server, set the provider variables, create an agent, issue a voice token, connect with the Pipecat JS client.
- [Providers](docs/providers.md): environment variables per provider, the catalog format, and the overlay directory for extra models and voices.

**API and security**
- [Management API v1](docs/api/v1.md): agents, providers, voice tokens, MCP servers and sessions.
- [MCP management API](docs/api/mcp.md) and [MCP transport and security policy](docs/security/mcp.md).
- [Provider credential security](docs/security/credentials.md): secrets live only in the server's environment.
- [Privacy policy](docs/privacy.md): what is traced, stored and recorded.

**Operations**
- [Operations Runbook](docs/operations_runbook.md): container and systemd deployment, TLS reverse proxy, graceful drain, failure recovery.
- [Observability](docs/observability.md): `/health` vs `/ready`, Prometheus metrics, timelines, alerts.
- [Production hardening](docs/operations/hardening.md), [storage](docs/operations/storage.md) and [retention](docs/operations/retention.md).
- [Performance gates and benchmarks](docs/benchmarks/README.md): the baselines CI compares against, and the measured capacity and latency reports.

**Behaviour and compatibility**
- [Pipecat compatibility record](docs/pipecat-compatibility.md) and [parity matrix](docs/pipecat-parity-matrix.md): pinned versions, preserved behaviours, intentional differences, non-goals.
- [Inbound audio filters](docs/audio-filters.md) and [output pacing and flush](docs/output-pacing-and-flush.md).

**Design**
- [Design specs](docs/superpowers/specs): the reasoning behind agent graphs, the credentials catalog, streaming TTS, turn latency and the Switchboard console.

---

## Programmatic Examples

Switchboard provides standalone, executable examples independent of the HTTP/WebSocket server:

- [`examples/google_pipeline`](examples/google_pipeline/main.go): Complete Google Cloud pipeline (Silero VAD + Google STT + Gemini LLM + Cloud TTS).
- [`examples/xai_pipeline`](examples/xai_pipeline/main.go): Scenario C interchangeable LLM pipeline (Google STT + xAI Grok LLM + Cloud TTS).
- [`examples/custom_processor`](examples/custom_processor/main.go): Adding a custom application processor (profanity filter / audit logger) without changing the core runtime.

---

## License

Switchboard is released under the Apache 2.0 License.
