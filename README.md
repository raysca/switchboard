# Switchboard

Switchboard is a low-latency, resource-efficient voice agent runtime built in Go. It connects bidirectional WebSocket voice clients to Google Cloud, xAI and other speech and language providers with deterministic turn-taking, sub-200ms barge-in interruption, and bounded resource use.

---

## Key Capabilities

- **Unmodified Pipecat clients**: works with the official `@pipecat-ai/client-js@1.13.1` and `@pipecat-ai/websocket-transport@1.7.2` over binary Protobuf frames and RTVI 2.1.0. See [docs/pipecat-compatibility.md](docs/pipecat-compatibility.md).
- **Barge-in under 200 ms**: on user speech the server stops sending assistant audio; the pinned client does not clear audio it already buffered, so residual playback is bounded by the 100 ms pacing queue.
- **Fault-isolated VAD**: Silero VAD (and the Smart Turn model) run in supervised child processes in the production profile (`vad.mode: process`), so a native ONNX fault cannot crash the Go server; development runs them in-process.
- **Stored agents**: every call runs a stored agent (providers, models, voice, prompt, MCP tools) managed through the [`/api/v1` management API](docs/api/v1.md); each call pins the agent version it started with. Agents can have several steps joined by handoffs.
- **Environment-only secrets**: provider keys are read from the server's environment and never stored or returned ([docs/providers.md](docs/providers.md), [docs/security/credentials.md](docs/security/credentials.md)).
- **Provider choice**: Google, xAI (STT and TTS need `-tags xaispeech`), OpenAI-compatible, NVIDIA, Deepgram, Cartesia, ElevenLabs, and a fully local offline stack ([offline/README.md](offline/README.md)), with failover between them.
- **Bounded by design**: context, task and queue budgets, admission limits, ping liveness, and graceful drain.
- **Observability**: `/health`, `/ready`, Prometheus metrics without high-cardinality labels, per-session timelines, optional OpenTelemetry ([docs/observability.md](docs/observability.md)).
- **Pipecat feature coverage**: turn strategies, Smart Turn, idle handling, DTMF, recording, redaction, summarization, background audio and more; status per feature in [docs/pipecat-parity-matrix.md](docs/pipecat-parity-matrix.md).

---

## Five-Minute Start

`make help` lists every target, grouped by cost: free (fake providers), read-only previews, and PAID live checks that refuse to run without `GOVOICE_LIVE=1`.

```bash
make ort                       # fresh clone only: fetch the pinned ONNX Runtime
make dev                       # server + admin UI, ready to call
make example-web               # in another terminal: browser client
make example-cli TEXT="Hello"  # or a CLI call (omit TEXT for a voice clip)
```

`make dev` builds everything, generates the management key and token secret, and keeps a local SQLite store in `.govoice-dev/` (git-ignored). Provider keys come from `examples/.env` (template: `examples/.env.example`). Without keys, the free targets in `make help` run on the `echo` fake provider. The admin UI is at `http://127.0.0.1:5173`. Seeded agents, ports and `make admin-link`: [quickstart section 8](docs/quickstart.md#8-local-development-with-make-dev); the full walkthrough (agents, tokens, the Pipecat client) is the rest of [docs/quickstart.md](docs/quickstart.md).

---

## Deploy

[docs/deployment.md](docs/deployment.md) covers building the image, configuration, environment variables, Docker Compose, a reference systemd unit, Google Cloud Run (the demo deployment), TLS, monitoring and a security checklist, and says what the repository does not provide.

---

## Documentation Map

| If you want to | Read |
|---|---|
| Try it | [Quick start](docs/quickstart.md), [Smile Dental demo](docs/demo/dentistry.md), [example client](examples/rtvi_client/README.md), [offline speech stack](offline/README.md) |
| Deploy it | [Deployment guide](docs/deployment.md), [Cloud Run rehearsal](deploy/rehearsal/README.md) |
| Run it | [Operations](docs/operations.md), [Agent evals](docs/operations/agent-evals.md), [Observability](docs/observability.md), [Storage](docs/storage.md), [Privacy and retention](docs/privacy.md) |
| Integrate with it | [Management API v1](docs/api/v1.md), [MCP API](docs/api/mcp.md), [Providers](docs/providers.md), [Audio filters](docs/audio-filters.md), [Output pacing](docs/output-pacing-and-flush.md) |
| Secure it | [Provider credentials](docs/security/credentials.md), [MCP transport and security](docs/security/mcp.md) |
| Check compatibility | [Pipecat compatibility](docs/pipecat-compatibility.md), [parity matrix](docs/pipecat-parity-matrix.md), [decisions](docs/decisions/pipecat-evals.md) |
| Plan upcoming work | [Next implementation phases](docs/roadmap-next-phases.md) |
| Measure it | [Benchmarks and perf gates](docs/benchmarks/README.md) |
| Contribute | [AGENTS.md](AGENTS.md), [browser e2e suites](e2e/README.md), `make help` |

---

## Programmatic Examples

Standalone, executable examples independent of the HTTP/WebSocket server:

- [`examples/google_pipeline`](examples/google_pipeline/main.go): Silero VAD + Google STT + Gemini LLM + Cloud TTS.
- [`examples/xai_pipeline`](examples/xai_pipeline/main.go): the same pipeline with the xAI Grok LLM.
- [`examples/custom_processor`](examples/custom_processor/main.go): adding a custom processor (profanity filter / audit logger) without changing the core runtime.

---

## License

Switchboard is released under the Apache 2.0 License.
