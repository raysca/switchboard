# Voice Framework V1 — Tech Stack & Dependencies

Sep 27, 2026

## 1. Purpose and Scope

This document pins concrete library, protocol, and tooling choices for the V1 implementation. It exists because the Specification and Implementation Plan deliberately describe behavior and architecture, not specific packages, and an implementing agent working phase by phase needs one place to check rather than making a fresh, possibly inconsistent choice in each phase.

This document does not repeat the repository layout (spec section 5), the architecture (spec sections 6-20), or the phase sequencing (Implementation Plan). It only adds what those documents deliberately left unpinned: which library, which version policy, which wire format, which build tool.

Where a choice depends on information that changes quickly (SDK availability, model names, current stable versions), this document says so explicitly in section 11 rather than presenting a guess as settled fact. The implementing team should confirm any such item before relying on it and update this document once confirmed.

## 2. Go Toolchain and Module Setup

Go version: use the current stable Go release at the time implementation starts; pin the exact version in go.work and each module's go.mod, and pin the same version in CI. Do not float on "latest" once pinned, since a mid-project toolchain bump can silently change race-detector or cgo behavior.

Module layout: go.work at the repository root referencing core/, providers/google/, providers/xai/, and server/ as separate modules, per spec section 5. Each module gets its own go.mod and its own semantic version tags, so a provider module can be released independently of core.

Versioning policy: core stays pre-1.0 (v0.x) through all of V1, since its API may still shift phase to phase; provider modules likewise. Do not tag a v1.0.0 release until the V1 release criteria (spec section 46) are fully met.

Linting and formatting: gofmt and go vet are mandatory gates (already required by the Implementation Plan's general rules); golangci-lint is recommended as an additional CI gate but is not release-blocking on its own.

## 3. Core Runtime Dependencies

Deliberately minimal. Core (Frame, Processor, Pipeline, Session, Supervisor, ResourceBudget, per spec sections 6-14) should depend only on the standard library wherever possible, since it is the piece other modules build on and the piece the POC already proved out with essentially no external dependencies.

Context and cancellation: standard library context, no third-party alternative.

Concurrency primitives: standard library sync, sync/atomic; no third-party goroutine-pool or actor-framework library. The POC deliberately avoided these and the spec's section 6 default execution model does not need them.

Testing: standard library testing plus go test -race, as already required. testify or a similar assertion library may be used for readability but is not required; keep it out of core if used elsewhere, to keep core's dependency graph minimal.

## 4. VAD and ONNX Runtime Dependencies

Carry the POC's proven choices forward rather than re-deriving them:

ONNX Runtime bindings: github.com/yalue/onnxruntime\_go, the same cgo bindings the POC validated. Confirm the exact ORT shared-library version against the POC's own pre-flight notes and re-pin for the target Linux architecture (the POC ran on macOS arm64; V1 targets Linux server hardware per spec section 42, so the ORT tarball, its SDK/linker environment quirks, and any SDKROOT-equivalent build flags must be re-verified on Linux, not assumed to carry over).

Model: Silero VAD v5 (silero\_vad.onnx), same model the POC validated, same input/output tensor shapes (input \[B,576\], state \[2,B,128\], sr int64). Do not silently upgrade the model version without re-running the POC's streaming-state correctness check (batched output matches unbatched within the POC's established tolerance).

ONNX Runtime threading: intra-op=1, inter-op=1, sequential execution, letting the Go executor own concurrency, per the POC's finding that ORT-internal threading loses to Go-owned concurrency at the executor level.

Executor implementations: in-process sharded executor and local worker-process executor, both per spec section 15, both ported from or closely modeled on the POC's Strategy A/B/C executor interface.

## 5. Google Provider Dependencies

STT: cloud.google.com/go/speech/apiv2, the official Go client for Cloud Speech-to-Text V2, using StreamingRecognize for interim and final transcription. The adapter owns the provider's finite stream lifetime: it opens a replacement before the limit, bridges a bounded audio overlap, switches generation, and deduplicates results. Capacity budgeting reserves a temporary second stream per rotating call. The adapter keeps each audio message below the provider's pinned request limit and sends audio at approximately real-time cadence. Phase 0 pins the module version, project/location endpoint, recognizer resource or implicit-recognizer policy, immutable model ID, stream-rotation threshold, overlap, and quota budget.

TTS: cloud.google.com/go/texttospeech/apiv1, using the stable SynthesizeSpeech API with `AudioEncoding_PCM` headerless 16-bit little-endian output and bounded sentence or phrase synthesis units. `LINEAR16` is not used for the internal raw-audio path because the synchronous API wraps it in a WAV container. Preview-only bidirectional streaming and Gemini-native TTS are not release-gating V1 dependencies. They may be evaluated behind the same provider interface after V1. The Phase 0 compatibility lock pins one production-supported voice family, default voice, output sample rate, synthesis-unit maximum text/duration policy, and module version.

LLM: google.golang.org/genai, Google's GA GenAI Go SDK. Do not start new work on the deprecated cloud.google.com/go/vertexai/genai package. The adapter uses streamed GenerateContent, explicit provider request deadlines, and the SDK's Vertex AI mode or Gemini Developer API mode as selected by configuration. The authentication mode, region, model ID, and SDK version are pinned in Phase 0 and must not change implicitly through a floating model alias.

Authentication: Cloud Speech-to-Text and Cloud Text-to-Speech use Application Default Credentials through GOOGLE\_APPLICATION\_CREDENTIALS or the target platform's workload identity. Google GenAI uses either Vertex AI with ADC or the Gemini Developer API with GOOGLE\_API\_KEY, selected explicitly in configuration. The server must not assume one credential type is valid for all three capabilities.

All three must be wrapped so their SDK-specific types never cross the providers/google boundary into core, per spec section 23.

## 6. xAI Provider Dependencies

xAI does not publish an official first-party Go SDK (only official Python and TypeScript SDKs exist as of this writing; several unofficial community Go clients exist on GitHub but carry the usual unofficial-dependency risk: unclear maintenance, no guaranteed API-compatibility commitment).

Recommendation: xAI's API is REST-based and OpenAI-compatible. Build a small, self-owned internal REST/SSE client against https://api.x.ai using only net/http and encoding/json, targeting the Responses API for the LLM capability required by spec section 25. Chat Completions is a compatibility fallback, not the default for a new V1 integration. Responses are stored by the provider by default, so V1 must send `store: false` and resend its bounded provider-neutral context on each turn; it must not silently create provider-side conversation persistence that conflicts with the spec's in-memory context boundary. The client must implement bounded SSE event parsing and line/event limits, total response-body limits, context cancellation, Retry-After handling, terminal-event validation, and usage extraction. This avoids an unofficial third-party Go dependency for a release-gating capability.

If the team later builds the fast-follow xAI STT/TTS modules and finds an official xAI Go SDK has since shipped, prefer it over hand-rolled REST at that point; this is exactly the kind of fact that can change and should be re-checked before that work starts (see section 11).

Authentication: XAI\_API\_KEY per spec section 30, sent as a bearer token, never logged.

## 7. Transport and RTVI Dependencies

WebSocket library: github.com/coder/websocket. Its context-aware API matches the required read, write, close, and handshake deadlines. V1 does not carry two interchangeable WebSocket libraries; the exact version is pinned in server/go.mod during Phase 0. Compression is disabled by default for voice connections.

TLS: standard library crypto/tls and net/http, no third-party TLS library needed. For local certificate generation during development and testing, mkcert is a convenient external tool (not a Go dependency).

Wire serializer and RTVI: no off-the-shelf Go library implements the complete pinned client edge. The released `@pipecat-ai/websocket-transport` defaults to a Pipecat protobuf serializer: each WebSocket binary message contains one protobuf `Frame`; the audio variant contains raw PCM bytes plus sample rate/channel count, and the message variant contains RTVI JSON. It does **not** use the RTVI base64 `raw-audio` shape for its normal microphone path. Generate Go bindings with google.golang.org/protobuf from the exact pinned `.proto` schema; check in the schema and generated bindings with their source commit and SHA-256. Do not hand-maintain lookalike structs or use dynamic/gob decoding. The RTVI JSON mapping at the message edge remains hand-built.

Phase 0 records one atomic compatibility set in a checked-in manifest: released `@pipecat-ai/client-js` version, released `@pipecat-ai/websocket-transport` version, default serializer and protobuf schema checksum/source commit, recorder and player sample rates, RTVI protocol version, and Pipecat reference commit. Byte fixtures in both directions include those coordinates and are regenerated only through an explicit compatibility update. Phase 0 must also prove that the released unmodified client clears already-buffered playback using a standard event the Go server can emit. This is a feasibility gate: current source inspection is not a substitute for a passing test against the exact released package.

Audio encoding: V1 accepts raw PCM16LE mono at 16, 24, or 48 kHz inside the pinned protobuf audio variant and uses 16 kHz PCM16LE internally for VAD/STT. Opus and other codecs are out of scope. Output is PCM16LE at the one client player rate frozen in the compatibility manifest, initially expected to be 24 kHz but not assumed until the manifest is frozen. The client player does not derive its playback rate from each received audio frame, so V1 does not claim runtime output-rate negotiation; a client configured for another player rate is outside the compatibility set. A stateful, bounded streaming resampler is required in both input and output paths. Phase 0 must select and pin a pure-Go implementation after license, quality, latency, chunk-boundary, race, and CPU benchmarks; an in-process cgo DSP dependency is prohibited because it would reintroduce the native-crash domain the VAD process executor is intended to contain. Until that gate passes, the resampler choice is intentionally not named as an approved dependency.

Audio framing helpers remain internal: protobuf envelope validation, PCM byte/sample conversion, server-assigned receive sequencing, media-clock pacing, and bounded millisecond-based FIFOs. They must not depend on OS audio devices or a media framework.

## 8. Server, Config, and Observability Dependencies

Config parsing: gopkg.in/yaml.v3 for YAML config (spec section 28's example format), plus a small internal validation layer; no heavier config framework (Viper, etc.) is needed for V1's scope and would add configuration-source complexity (env, flags, remote config) the spec explicitly does not require.

Admission tokens: github.com/golang-jwt/jwt/v5, restricted to an explicit algorithm allowlist. The reference single-host V1 deployment uses short-lived HS256 tokens with issuer, audience, subject/quota key, issued-at, expiry, unique token ID, and key ID claims. The verifier rejects algorithm confusion, missing claims, excessive lifetime, clock skew outside a small allowance, and replayed token IDs. Production tokens are single-use: validation and consumption are atomic, and a bounded in-memory replay cache retains IDs through expiry. On restart, tokens with `iat` older than the current process start minus the configured clock-skew allowance are rejected; tests cover the small documented skew boundary. Token minting belongs in a trusted application backend; the voice server ships verification and an example integration, not a public unauthenticated mint endpoint. Rotation supports an active and previous key selected by key ID.

The pinned JavaScript WebSocket transport accepts a normal `token` connection parameter and appends it to the WebSocket URL query. Use that supported path rather than requiring a browser-only custom header. Every application and reference-proxy logger must redact the token query parameter, and the token must never appear in the containing page URL. A reconnect after token consumption is rejected; the application obtains a fresh token for a new call.

Structured logging: log/slog, the standard library's structured logger (stable since Go 1.21), avoids taking a dependency on zap or zerolog for a requirement that slog already satisfies natively: JSON output, structured fields, level control.

Metrics: github.com/prometheus/client\_golang for the /metrics endpoint (spec sections 31, 35), the de facto standard and explicitly named as consumable by a standard scraper per spec section 39.

Per-session event log (spec section 36): implement as typed internal events emitted to a dedicated slog handler and a bounded per-session ring. The ring has configurable maximum events, maximum bytes, and TTL; eviction and exporter drops increment metrics. Retrieval is exposed only on the separately bound administrative listener. A streaming OpenTelemetry exporter may be added without changing session code, but a full tracing backend is not required for V1.

HTTP routing: standard library net/http (Go 1.22+'s enhanced ServeMux is sufficient for the small surface in spec section 31); no third-party router required.

## 9. VAD Worker-Process IPC Protocol

Carried forward from the POC's cmd/vadworker design (spec section 15's local worker-process executor), pinned as a concrete wire format so it isn't re-derived per phase:

Transport: each worker process communicates with the parent over its own stdin/stdout pipe, one worker process per pipe pair, no shared socket. This keeps the failure domain of one worker's pipe isolated from others and avoids a network-socket dependency for what is purely local IPC.

Framing: length-prefixed binary messages (a fixed-size uint32 length header followed by the payload), not line-delimited text, to safely carry raw audio sample data without escaping.

Protocol handshake: the parent starts a worker, reads a fixed magic value, protocol version, worker instance ID, model SHA-256, ONNX Runtime version, and ready status, and only then adds the worker to capacity. A mismatch keeps the server unready and produces a diagnostic without routing jobs to that worker.

Message shape: a request carries a globally unique request ID, session ID, session-local sequence number, relative timeout, audio format, raw samples, and streaming state; a response echoes the request ID and session/sequence tuple and carries the inference result, updated state, and a typed status. All integers have fixed widths and little-endian encoding. The parent enforces one outstanding state-mutating VAD job per session so responses cannot reorder session state. Encode with encoding/binary and a fixed layout rather than gob. Unknown versions or message types are rejected.

Every frame has a configured maximum length checked before allocation. Reads and writes use complete-read/write loops and deadlines; truncated frames, oversized lengths, invalid dimensions, non-finite inference output, and responses for unknown requests fail that worker. stdout is reserved exclusively for framed protocol bytes; worker diagnostics go to stderr. Absolute wall-clock deadlines are not serialized because clock changes can invalidate them. The parent owns the monotonic deadline and kills an unresponsive worker after the bounded grace period; the relative timeout sent to the worker is only for admission/expiry checks around inference and must not imply that an in-progress native ONNX call is safely cancellable.

Failure handling: a worker process crash or unresponsive worker is detected by the parent; in-flight jobs on that worker are marked dropped, affected session detector state is reset or reconstructed from a bounded parent-owned checkpoint, and the worker is respawned with capped exponential backoff and a crash-loop threshold. Capacity and readiness reflect missing workers. Respawn succeeds only after the full handshake. Crash loops never spin without bound or produce unbounded logs or processes.

## 10. Build, CI, and Deployment Tooling

Build: a root Makefile (mirroring the POC's own Makefile) with targets for build, test, test-race, bench, and docker-build; must set whatever Linux-equivalent SDK/toolchain environment variables cgo needs for the ONNX Runtime link step, determined fresh on the target Linux distribution rather than assumed from the POC's macOS notes (see section 4).

CI: run on every PR: gofmt check, go vet and go test -race across every module independently and through go.work, byte-for-byte protobuf and RTVI fixture tests with the pinned JavaScript client, malformed/oversized wire-input tests, and a build and smoke test of the production Docker image. Run on Linux matching the production target. CI verifies the protobuf schema/source coordinate, ONNX Runtime, and Silero SHA-256 values, generates an SBOM, and runs Go dependency and container vulnerability scans against a documented severity gate. Live-provider and real-browser playback-clear integration tests remain credential/environment-gated; scheduled release CI runs them and the long-call scenario before release-candidate approval.

Containerization: a multi-stage Dockerfile (build stage compiles the Go binary and links against the checksum-verified ONNX Runtime shared library; final stage is a pinned minimal base image with a non-root user, init or child-reaping behavior, the compiled server and VAD worker, the ONNX Runtime shared library, the checksum-verified Silero model, and CA certificates). The reference runtime uses a read-only root filesystem, an explicit writable temporary directory, no unnecessary Linux capabilities, and health checks that distinguish liveness from readiness.

Process management: the reference systemd unit (spec section 39) should set Restart=on-failure, a reasonable resource limit block (MemoryMax, TasksMax), and route stdout/stderr to journald, consistent with the structured JSON logging format so the two integrate without extra parsing.

Reverse proxy reference: an nginx or Caddy example config terminating TLS and proxying wss traffic to the server's plain-ws listener, for teams that prefer proxy-terminated TLS over the server terminating it directly. Its access-log format must omit or redact the `token` query parameter while retaining a safe path and request correlation ID.

## 11. Open Decisions for the Implementing Team

These are the items in this document that depend on fast-changing external facts rather than architecture, and should be re-checked at the start of implementation rather than trusted as settled:

- The exact stable Google Cloud TTS voice family, default voice, `AudioEncoding_PCM` output rate, synthesis-unit bounds, and module version to lock in the Phase 0 compatibility manifest. V1 has selected the stable Cloud TTS API rather than Preview streaming.
- The exact google.golang.org/genai SDK version, Gemini model ID, region, and authentication mode. V1 has selected the GA SDK rather than the deprecated Vertex genai package.
- Whether an official xAI Go SDK has shipped by the time Phase 7 (or the xAI STT/TTS fast-follow) begins (section 6); none existed as of this document's writing.
- The exact github.com/coder/websocket version; the library choice itself is closed.
- The pure-Go streaming resampler selected after the Phase 0 quality, latency, license, race, and CPU gate.
- The released Pipecat JavaScript client and WebSocket transport versions, default protobuf serializer/schema, recorder/player rates, RTVI protocol version, and Pipecat reference commit recorded together in the compatibility manifest.
- Whether that exact released, unmodified WebSocket client clears already-buffered playback on the standard interruption event available to the server. This is a hard Phase 0 feasibility gate, not a choice that may drift into implementation. If it fails, the product owner must choose between waiting for/pinning a released client that supports the behavior, relaxing the unmodified-client requirement, or relaxing V1 barge-in; the server must not silently add a proprietary event and still claim compatibility.
- The current stable Go release to pin (section 2).
- Exact ONNX Runtime tarball version and Linux build/link requirements (section 4), which must be established fresh on target hardware, not assumed from the POC's macOS notes.

These decisions do not reopen the architecture, but Phase 0 must close and record them before implementation phases begin. They are updated in place and in the checked-in compatibility manifest rather than left to drift.
