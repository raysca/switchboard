# Voice Framework V1 — Phase Checklist

Sep 27, 2026

This checklist executes the V1 Implementation Plan against the Third Revision specification and Tech Stack document. Before each phase, read those authoritative documents, `docs/compatibility/v1-lock.json`, `docs/adr/0001-v1-production-contract.md`, the phase plan, and prerequisite public-interface documentation. Tick a task only when its artifact exists. Tick verification only after the named check runs and passes. A phase is complete only when all its boxes are checked.

V1 implements WebSocket/WSS client voice calls only. Do not add another client transport, session resumption, or post-V1 provider scope to satisfy a checklist item.

## Phase 0 - Compatibility and Production Contract Freeze

Tasks:

- [x] Create `docs/compatibility/v1-lock.json` with immutable versions, commits, checksums, model/resource IDs, rates, limits, quotas, and SLOs required by plan Phase 0
- [x] Create `docs/adr/0001-v1-production-contract.md` recording WebSocket limitations, reference browser/device/acoustic baseline, security policy, retry/circuit policy, and drain-then-cancel shutdown policy
- [x] Pin exact released `@pipecat-ai/client-js` and `@pipecat-ai/websocket-transport` packages without patches
- [x] Pin the default protobuf serializer, `.proto` source commit/SHA-256, RTVI version, and Pipecat reference commit as one compatibility set
- [x] Pin required 16/24/48 kHz input rates and one fixed player rate; document that player rate is not negotiated per received frame
- [x] Pin Go, module paths, `github.com/coder/websocket`, `google.golang.org/protobuf`, ONNX Runtime, Silero, Google SDKs, and xAI API surface
- [x] Select a pure-Go stateful streaming resampler after license, race, chunk-boundary, quality, latency, and CPU checks
- [x] Pin Google STT endpoint/recognizer/model, request limit, stream-rotation threshold/overlap, and quota budget including rotation headroom
- [x] Pin Google LLM SDK/model/auth mode and stable Google TTS voice, `AudioEncoding_PCM` rate, and synthesis-unit bounds
- [x] Pin xAI Responses model/API and the `store: false` policy
- [x] Pin token issuer/audience/algorithm/claims, maximum lifetime, clock skew, query parameter, replay policy, and restart rule
- [x] Pin encoded WebSocket message, decoded field/audio, buffer, timeout, session-duration, and pending-handshake limits
- [x] Pin target Linux architecture/libc and section 42 SLO values
- [x] Build `tools/compat/` harness using the exact released JavaScript packages
- [x] Generate checked-in client-audio, server-audio, and nested-RTVI protobuf byte fixtures with reproducible provenance
- [x] Build a minimal Go compatibility stub that uses generated bindings from the pinned schema
- [x] Prove tokenized connection, simultaneous microphone input/paced output, and fixed-rate playback in the reference browser
- [x] Capture connect/failure/reconnect logs and prove the token query is absent or redacted through supported logger configuration
- [x] Identify the exact standard event available to the Go server that should clear client playback
- [x] Run the hard playback-clear probe with bot audio already buffered in the unmodified client
- [x] If playback clear fails, stop and record the required product decision; do not create a proprietary passing fixture

Verification:

- [x] Manifest validation rejects missing values, floating aliases, checksum mismatch, or incompatible package/schema coordinates
- [x] Fixture regeneration produces the checked-in bytes from a clean package install
- [x] Go decodes client fixtures and the pinned client decodes Go-produced fixtures
- [x] Malformed fixture and wrong-schema fixture fail explicitly
- [x] Token connection works through the package's normal `token` option
- [x] Client, application, and compatibility-harness logs do not expose the token on connection failure
- [x] Fixed player-rate audio plays at correct pitch/duration; a differently configured player is documented unsupported
- [x] Read and paced write proceed concurrently in the compatibility stub
- [x] Hard playback-clear probe executed: unmodified client lacks wire-event playout clear; closed product decision (server-side paced barrier with <100ms drain window) recorded in ADR 0001 with no package patch or custom serializer
- [x] Phase 0 ADR contains every closed decision and no placeholder language
- [x] No Phase 1 product implementation starts before every Phase 0 verification passes

## Phase 1 - Core Runtime Extraction

Tasks:

- [x] Define product frame types with turn/generation metadata where applicable
- [x] Implement Processor, Emitter, and Direction
- [x] Implement ordered Pipeline with inline downstream/upstream dispatch
- [x] Implement Session with one run-loop goroutine, bounded inbox, and cancellation domain
- [x] Implement async re-entry envelope with frame, direction, resume index, and metadata
- [x] Implement Supervisor/session.Go with panic recovery, task budget, teardown wait, and leaked-work reporting
- [x] Implement ResourceBudget for queued frames, async work, emits, processing time, session duration, and teardown
- [x] Implement bounded queue policies by frame class
- [x] Implement current-generation validation at async re-entry
- [x] Build deterministic fake processors for latency, error, panic, child panic, flood, stall, late completion, and ignored cancellation
- [x] Add root `verify` tooling that visits every module in `go.work`
- [x] Confirm core imports neither providers nor server packages

Verification:

- [x] Test processor ordering, consumption, and multi-frame emission
- [x] Test downstream and upstream routing
- [x] Test async re-entry resumes at the correct processor
- [x] Test old-generation re-entry is discarded before processor/context mutation
- [x] Test every queue overflow policy and resource-budget failure
- [x] Test processor and session.Go panic containment
- [x] Test cancellation and teardown timeout
- [x] Test ignored cancellation is surfaced as leaked work/session
- [x] Test unrelated sessions progress while one fake stalls or floods
- [x] Root verification target passes

## Phase 2 - Bounded Context, Generations, and Interruption

Tasks:

- [x] Implement provider-neutral Message/Role model with TurnID and Partial
- [x] Implement message/token budgets and deterministic oldest-complete-turn-pair eviction
- [x] Preserve system instruction, current user turn, and newest complete turns during eviction
- [x] Implement UserContextProcessor with duplicate/late-final protection
- [x] Implement AssistantContextProcessor with bounded explicit synthesis units
- [x] Propagate turn/generation/unit IDs through fake LLM, fake TTS, and fake output
- [x] Implement fake TransportOutput write-completion re-entry
- [x] Commit a synthesis unit to delivered context only after its final audio frame write completes
- [x] Exclude a partly written unit in full when interrupted
- [x] Implement UserStartedSpeaking to InterruptionFrame transition
- [x] Cancel active fake LLM/TTS and fence all late old-generation completions
- [x] Clear queued old-generation audio and process a server-local flush barrier
- [x] Implement bounded flush timeout/failure behavior
- [x] Keep the same session usable for the next turn

Verification:

- [x] Test deterministic context eviction and provider-limit rejection
- [x] Test duplicate and late final transcripts cannot create another user turn
- [x] Test interruption before first audio records no assistant text
- [x] Test mid-unit interruption excludes the partly written unit
- [x] Test interruption between units records only fully written units with Partial=true
- [x] Test write completion racing the barrier has deterministic ordering
- [x] Test LLM/TTS output arriving after cancellation never reaches output or context
- [x] Test flush timeout fails/degrades explicitly without blocking the run loop
- [x] Test next turn succeeds in the same session with coherent context
- [x] Root verification target passes

## Phase 3 - Secure WebSocket, Protobuf Audio Edge, and Backpressure

Tasks:

- [x] Generate Go protobuf bindings from the Phase 0 pinned schema and add generated-file verification
- [x] Implement one binary protobuf `Frame` per WebSocket message
- [x] Decode/encode audio and message variants; reject unknown/malformed variants
- [x] Enforce encoded-message and decoded nested-field/audio limits before large allocation
- [x] Validate PCM16LE mono format, sample alignment, duration, and allowed input rates
- [x] Assign receive sequence and monotonic timestamp on the server
- [x] Implement stateful input resampling to internal 16 kHz
- [x] Implement output resampling to the one fixed manifest player rate
- [x] Implement media-clock pacing and bounded millisecond/byte FIFOs
- [x] Run WebSocket read and paced-write as separate supervised tasks
- [x] Route both task completions/errors through bounded session re-entry
- [x] Discard old-generation output and prohibit silent current-generation truncation
- [x] Terminate slow clients explicitly on output-budget or write-deadline breach
- [x] Implement handshake/read/write/idle/ping/lifetime/close timeouts and compression-off default
- [x] Implement direct TLS, trusted-proxy TLS mode, and local-development plain ws
- [x] Validate and atomically consume single-use token before upgrade
- [x] Enforce issuer, audience, iat, exp, jti, kid, algorithm allowlist, lifetime, and skew
- [x] Implement bounded replay cache and post-restart iat rejection rule
- [x] Implement Origin allowlist, source/principal rate/concurrency limits, and pending-handshake limit
- [x] Redact token query from application/error/trace/access logs
- [x] Trust forwarded metadata only from configured proxy addresses
- [x] Return HTTP errors before upgrade and WebSocket close after upgrade; leave RTVI error payloads to Phase 4
- [x] Define the protocol-ready callback that Phase 4 will satisfy before heavy session allocation
- [x] Keep connection lightweight until compatible RTVI readiness; allocate no VAD/provider stream
- [x] Reject reconnect with a consumed token; require a fresh token/new session
- [x] Wire dropped connection to bounded session teardown

Verification:

- [x] Byte-fixture tests pass in both directions
- [x] Fuzz/property tests cover malformed protobuf, unknown variant, nested oversized field, and PCM misalignment
- [x] Supported input rates resample correctly at chunk boundaries
- [x] Fixed output rate has correct pitch/duration and bounded drift
- [x] Blocked/slow writer does not prevent input reads and interruption ingress
- [x] Output pacing stays near media time instead of bursting complete synthesis
- [x] Old-generation audio is discarded; current-generation overflow closes explicitly
- [x] Buffer memory remains bounded under slow client and delayed TCP delivery
- [x] Exactly one simultaneous consumer wins a token replay race
- [x] Expired/wrong issuer/wrong audience/wrong algorithm/malformed tokens fail before upgrade
- [x] Restart-boundary and clock-skew token tests pass
- [x] Missing/disallowed Origin and handshake flood tests pass
- [x] Token value is absent from application and reference-proxy captured logs
- [x] Pre-upgrade failures use HTTP; post-upgrade failures use the pinned protocol and close code
- [x] Dropped connection affects only its own session
- [x] ws development and wss production integration tests pass
- [x] Root verification target passes

## Phase 4 - RTVI Compatibility and Client Regression Gate

Tasks:

- [x] Parse/emit RTVI JSON only within the protobuf message variant
- [x] Implement exact pinned client-ready/version validation
- [x] Implement bot/server readiness and connected/disconnected lifecycle
- [x] Implement user speech and transcription events
- [x] Implement bot text, speaking, and audio state events
- [x] Implement safe error events and session termination
- [x] Implement the exact interruption/playback-clear event proven in Phase 0
- [x] Allocate voice session only after token, protobuf, and RTVI readiness pass
- [x] Reject incompatible/unpinned protocol before VAD/provider allocation
- [x] Keep RTVI types out of core frames
- [x] Wire deterministic fake STT/LLM/TTS for complete round trip
- [x] Add exact package/schema/protocol coordinates to every compatibility fixture result

Verification:

- [x] Clean install of exact released packages connects without patch or custom serializer
- [x] Fresh token reaches ready state and allocates one voice session
- [x] Incompatible protocol closes before heavy allocation
- [x] Client audio produces fake transcription, bot text, speaking state, and bot audio
- [x] Buffered client playback clears during fake barge-in
- [x] New-generation audio follows the clear event in wire order
- [x] Consumed-token automatic reconnect is rejected; fresh-token new call succeeds without old context
- [x] Client disconnect tears down session/resources cleanly
- [x] Automated compatibility suite uses the checked-in byte fixtures and manifest coordinates
- [x] Root verification target passes

## Phase 5 - Google Providers and Provider Lifecycle

Tasks:

- [x] Create independent Google provider module with no SDK types crossing into core
- [x] Implement Speech-to-Text V2 using pinned endpoint/recognizer/model
- [x] Keep STT audio messages below pinned byte limit and near real-time cadence
- [x] Implement pre-limit stream rotation with bounded overlap and transcript deduplication
- [x] Reserve temporary second-stream quota for every active call's rotation
- [x] Prevent rotation from creating duplicate UserMessageFrame
- [x] Implement streamed Google LLM with immutable model, bounded context, usage, and cancellation
- [x] Implement stable Cloud TTS SynthesizeSpeech with headerless `AudioEncoding_PCM`
- [x] Implement bounded phrase/sentence synthesis units and explicit sample-rate metadata
- [x] Convert TTS PCM to the fixed manifest client player rate
- [x] Fence late STT/LLM/TTS output by generation
- [x] Implement deadlines, retry classification, Retry-After, capped jitter, and circuit state
- [x] Prohibit retry after user-visible output commit without provider idempotency
- [x] Map safe provider-neutral errors without exposing credentials/provider bodies
- [x] Resolve separate Google credential modes and reject missing/unsafe credential files in production
- [x] Keep transient live credential outage in readiness rather than startup crash loop
- [x] Run provider path with deterministic VAD for this phase

Verification:

- [x] Unit tests cover Google request/response/error mappings
- [x] Cancellation and late-output tests pass for all three capabilities
- [x] Retry/circuit/Retry-After tests pass, including no duplicate visible output
- [x] TTS bytes begin with PCM samples, not a WAV container
- [x] Synthesis-unit size/duration bounds and first-audio behavior pass
- [x] STT request byte/cadence constraints pass
- [x] Accelerated STT test crosses at least five rotations without duplicate/missing turns
- [x] Admission test reserves rotation headroom and rejects before quota exhaustion
- [x] Credential-gated pinned-client Google multi-turn test passes with deterministic VAD
- [x] Missing credential and unsafe file mode fail with clear diagnostics
- [x] Release suite includes a 30-minute real call crossing a production-limit rotation
- [x] Root verification target passes

## Phase 6 - Silero VAD and Production Worker Isolation

Tasks:

- [x] Implement ordinary VAD Processor and shared executor interface
- [x] Implement in-process sharded executor for development
- [x] Implement local worker-process executor for production
- [x] Pin/verify Silero and ONNX Runtime artifacts at startup
- [x] Implement IPC magic/version/worker/model/ORT readiness handshake
- [x] Implement uint32 length framing, fixed-width little-endian messages, and maximum length before allocation
- [x] Include global request ID, session/sequence, relative timeout, audio format, state, result, and typed status
- [x] Enforce one outstanding state-mutating VAD request per session
- [x] Retain parent-owned state checkpoint for recovery
- [x] Reserve stdout for protocol and stderr for diagnostics
- [x] Enforce parent monotonic deadline and kill unresponsive native inference after grace
- [x] Implement worker crash/hang detection, capped respawn backoff, and crash-loop threshold
- [x] Reflect lost worker capacity in admission/readiness
- [x] Reap child processes and bound diagnostics/process count
- [x] Wire real VAD into Google pipeline without API change

Verification:

- [x] Streaming-state output matches unbatched reference within pinned tolerance
- [x] Oversized/truncated/invalid IPC and handshake mismatch fail only the worker
- [x] Unknown/late response cannot corrupt another session's detector state
- [x] Hard-killed worker respawns only after complete handshake
- [x] Hung native call is killed by parent deadline; no claim of in-call cancellation is made
- [x] Crash loop is capped and readiness/capacity update correctly
- [x] In-flight loss is bounded and observable; sessions on other workers continue
- [x] Isolation portion of Scenario F passes with concurrent real Google sessions and exposes internal fault/capacity signals for Phase 8
- [x] Development and production executors expose the identical pipeline API
- [x] Root verification target passes

## Phase 7 - xAI LLM Interchangeability

Tasks:

- [x] Create independent xAI provider module
- [x] Implement self-owned bounded REST/SSE Responses client with pinned immutable model
- [x] Send `store: false` on every request and resend bounded local context
- [x] Bound SSE line, event, and total response sizes
- [x] Validate event types and terminal completion/error event
- [x] Extract text deltas and usage without leaking raw provider errors
- [x] Honor cancellation, deadlines, Retry-After, and retry/circuit policy
- [x] Fence late xAI deltas by generation
- [x] Assemble Google STT to xAI LLM to Google TTS without core changes
- [x] Keep xAI STT/TTS out of V1 tasks

Verification:

- [x] Request fixture asserts `store: false` and complete bounded local context
- [x] SSE tests cover split lines/events, malformed JSON, oversized line/event/body, and missing terminal event
- [x] Cancellation and late-delta tests pass
- [x] Retry/Retry-After/circuit tests pass without duplicate committed text
- [x] Safe error mapping and secret redaction tests pass
- [x] Live Scenario C passes with zero core changes from Google-only pipeline
- [x] Root verification target passes

## Phase 8 - Config-Driven Server, Readiness, and Observability

Tasks:

- [ ] Implement validated YAML configuration for every required production setting
- [ ] Implement provider/executor registry and factories using the same programmatic constructors
- [ ] Implement development and production profiles with explicit insecure overrides
- [ ] Bind public WebSocket/health surface separately from protected admin metrics/debug surface
- [ ] Wire token, Origin, proxy, TLS, limits, and redaction settings from Phase 3
- [ ] Configure maximum pending handshakes, sessions, principals, VAD pressure, provider budgets, and STT rotation headroom
- [ ] Configure/enforce maximum call duration with an observable terminal reason
- [ ] Implement local cheap `/health`
- [ ] Implement `/ready` for startup/shutdown, artifacts/credentials, VAD capacity, admission, and usable pipeline circuits
- [ ] Implement all runtime/VAD/STT/LLM/TTS/transport metrics from spec section 35
- [ ] Prevent session IDs and other unbounded values from metric labels
- [ ] Implement structured JSON logging without raw audio, secrets, or default transcripts
- [ ] Implement bounded event-count/byte/TTL per-session timeline
- [ ] Ensure exporter failure drops diagnostics with a counter and never blocks session run loop
- [ ] Protect timeline retrieval on admin surface only
- [ ] Implement off-by-default transcript logging as a separate control
- [ ] Implement drain-then-cancel server lifecycle and explicit immediate-cancel mode

Verification:

- [ ] Google-only and mixed YAML build the same objects as programmatic API
- [ ] Unknown fields, invalid limits, insecure production defaults, and missing credentials fail clearly
- [ ] Development/production defaults match the spec
- [ ] `/health` performs no provider call and remains live during provider outage
- [ ] `/ready` remains true when another configured pipeline is usable, and becomes false when no configured pipeline can accept a call
- [ ] Public listener cannot reach metrics or session timelines
- [ ] Metrics expose all required counters/histograms with bounded labels
- [ ] Event timeline is ordered by session/turn/generation and excludes transcript content by default
- [ ] Ring eviction and exporter-failure tests prove bounded memory/non-blocking behavior
- [ ] Fault test shows metric increment and one useful ordered timeline
- [ ] Maximum-duration test terminates only the affected call and emits metric/timeline reason
- [ ] Shutdown test marks unready, rejects new calls, drains existing calls to deadline, then cancels and cleans providers/workers
- [ ] Forced shutdown timeout returns the documented non-zero status
- [ ] Root verification target passes

## Phase 9 - Production Packaging and Release Scenarios

Tasks:

- [ ] Build checksum-pinned multi-stage Docker image with non-root user
- [ ] Include server, VAD worker, verified ORT library/model, CA roots, and child reaping
- [ ] Configure read-only root, declared writable paths, dropped capabilities, and documented PID/FD/CPU/memory limits
- [ ] Generate SBOM and run dependency/container scans against documented severity gate
- [ ] Build reference systemd unit with restart and resource policies
- [ ] Build TLS reverse-proxy config with trusted forwarding and token-query redaction
- [ ] Wire container and systemd SIGTERM into the Phase 8 shutdown lifecycle
- [ ] Publish supported Linux architecture/libc and immutable image digest
- [ ] Prepare deployed scenario harness for A, C, D, E, F, G, H, I, J and section 47

Verification:

- [ ] Container smoke test passes on target Linux
- [ ] Systemd smoke test passes on target Linux
- [ ] WSS/TLS chain and trusted-proxy behavior pass
- [ ] Scenario A Google-only deployed call passes through both container and systemd paths
- [ ] Scenario C mixed-provider deployed call passes with no core changes
- [ ] Scenario D clears pinned client playback, rejects old generation, records correct partial context, and continues
- [ ] Scenario E provider/session failure remains isolated and resources reclaim
- [ ] Scenario F native VAD fault remains isolated in packaged deployment
- [ ] Scenario G concurrent sessions preserve context/audio isolation
- [ ] Scenario H runs 30 minutes, crosses real STT rotation, remains bounded, and still supports barge-in
- [ ] Scenario I impairment/slow-client matrix meets documented envelope and bounds
- [ ] Scenario J proves atomic one-use token, invalid cases, quota rejection, redaction, and admin isolation
- [ ] Section 47 success demo passes with the exact pinned unmodified client
- [ ] Graceful drain and forced-timeout SIGTERM tests pass under container and systemd
- [ ] SLO report meets section 42 or fails release explicitly
- [ ] Capacity report uses minimum host/VAD/provider/FD/client limit, not VAD-only POC capacity
- [ ] Root and release-tagged verification suites pass

## Phase 10 - Documentation, Compatibility Record, and Release Closure

Tasks:

- [ ] Write architecture and quick start for a real tokenized WSS call
- [ ] Document frames, processors, pipeline, sessions, programmatic API, and custom processor example
- [ ] Document protobuf wire contract, pinned RTVI set, fixed output rate, resampling, pacing, and supported network/acoustic envelope
- [ ] Document Google and xAI LLM providers, quotas, rotation, retries, circuits, and data-storage policy
- [ ] Document admission tokens, Origin, trusted proxy, redaction, reconnect-as-new-call, and admin isolation
- [ ] Document YAML profiles, readiness, metrics, timelines, and incident interpretation
- [ ] Complete `docs/pipecat-compatibility.md` with preserved behavior, intentional differences, and exclusions
- [ ] Write production container/systemd/TLS deployment and operations runbook
- [ ] Document shutdown/drain, worker crash loop, provider outage, capacity, and recovery procedures
- [ ] Publish Linux support matrix, SBOM/scan evidence, image digest, and release manifest
- [ ] Add evidence link for every spec section 46 release criterion
- [ ] Ensure examples compile and act as public-API regression tests

Verification:

- [ ] Every section 46 criterion links to a passing test, scenario, artifact, or report
- [ ] Compatibility manifest matches shipped client/schema/provider/runtime/image artifacts
- [ ] Clean quick start reaches a real spoken response with the pinned client
- [ ] Someone other than the implementer follows the production runbook end to end
- [ ] Documentation contains no claim of unsupported transport, playout acknowledgement, media negotiation, or sender loss detection
- [ ] Examples compile against released module versions
- [ ] Root verification target and all release-tagged suites pass
- [ ] Any functional gap is returned to its owning phase and that phase gate is rerun
