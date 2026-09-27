# Voice Framework V1 — Implementation Plan

Sep 27, 2026

## 0. Purpose, Authority, and Execution Rules

This plan implements the **Third Revision — Production WebSocket Track** specification. The authoritative inputs for every phase are:

1. `docs/Voice Framework V1 Specification (Revised).md`
2. `docs/Voice Framework V1 — Tech Stack & Dependencies.md`
3. The Phase 0 compatibility manifest and production-contract ADR created below
4. This plan and the matching section of `docs/Voice Framework V1 — Phase Checklist.md`

WebSocket/WSS is V1's only client voice transport. WebRTC, SIP, PSTN, carrier transports, session resumption, and other V2 transport work must not enter this plan.

An agent may execute one phase at a time, but must read the four inputs above and the public interfaces produced by prerequisite phases. Cross-cutting contracts must not be inferred from one phase in isolation. Each phase ends with an independently runnable verification gate. Do not defer a failed criterion to a later phase.

The root build tooling must provide a `verify` target that runs formatting checks, `go vet ./...`, `go test -race ./...`, and relevant generated-file/fixture checks in every module in `go.work`. Use that target at every phase exit. Live-provider, browser-audio, native-fault, and deployment tests may be separately tagged, but every release-gating tagged suite must run before a release candidate.

## Phase 0 - Compatibility and Production Contract Freeze (spec M0; tech stack section 11)

**Goal:** close every fast-changing or feasibility-sensitive contract before product implementation begins.

**Build:** create a checked-in compatibility manifest and production-contract ADR. Pin the Go toolchain; module paths; exact released `@pipecat-ai/client-js` and `@pipecat-ai/websocket-transport` versions; default protobuf serializer, `.proto` source commit and SHA-256; RTVI protocol and Pipecat reference commit; required 16/24/48 kHz input rates and one fixed player rate; admission-token claims/transport/lifetime/skew; WebSocket and decoded-field limits; pure-Go streaming resampler; Google STT endpoint, recognizer, model, stream/request/quota settings; Google LLM SDK/model/auth mode; stable Google TTS voice, `AudioEncoding_PCM` rate and synthesis-unit bounds; xAI Responses model/API; ONNX Runtime/Silero artifacts; Linux target; and section 42 SLOs.

Maintain a small compatibility harness that installs the exact released JavaScript packages without patches and generates byte fixtures for client audio, server audio, and nested RTVI messages. Use a minimal Go stub to prove protobuf interoperability, the supported token connection parameter, simultaneous microphone input and paced output, and the fixed player-rate contract.

The hard gate is playback clearing: while the unmodified pinned client has buffered bot audio, emit the exact standard serializer/RTVI event available to the Go server and prove that the client stops playback. Record the concrete event and observed behavior. If this fails, stop. The product owner must choose a released version that supports clearing, relax the unmodified-client requirement, or relax V1 barge-in. A proprietary client message is not a passing result.

**Exit criteria:** all manifest fields are immutable values, not aliases or “latest”; schema and fixtures reproduce from a checked-in command; Go decodes and re-encodes the pinned protobuf fixtures byte-compatibly; tokenized connection and fixed-rate audio work; the unmodified-client playback-clear test passes; and the ADR records accepted WebSocket limitations, browser/device/acoustic baseline, retry/circuit policy, shutdown policy, and every closed Phase 0 decision. No production phase begins before this gate passes.

## Phase 1 - Core Runtime Extraction (spec sections 6-14; M1)

**Goal:** produce a provider- and transport-neutral core preserving the POC's execution model.

**Build:** implement frames, Processor/Emitter/Direction, Pipeline, one-run-loop Session, bounded inbox and async re-entry envelopes, Supervisor/session.Go, panic recovery, task budgets, leaked-work reporting, and ResourceBudget. Product frames carry turn and generation IDs where applicable. Add deterministic fake processors for latency, error, panic, flood, stall, late completion, and cancellation-ignoring behavior.

**Exit criteria:** tests cover ordering, bidirectional routing, consumption, multi-frame emission, re-entry position, stale-generation rejection, bounded overflow by frame class, budgets, panic containment, cancellation, and timed teardown. Core imports neither providers nor server packages. The root verification target passes.

## Phase 2 - Bounded Context, Generations, and Interruption (spec sections 16-18; M2)

**Goal:** prove coherent multi-turn and interruption semantics with deterministic fakes before networking or providers.

**Build:** implement bounded provider-neutral context with deterministic complete-turn-pair eviction; User and Assistant Context Processors; stable turn/generation IDs across STT, LLM, synthesis units, TTS audio, and output; and an interruption barrier. Fake TransportOutput reports asynchronous write completions through session re-entry.

On interruption, cancel fake LLM/TTS, reject every late old-generation completion, discard queued old-generation audio, process a server-local flush barrier, and keep the session usable. Partial context contains only complete synthesis units whose final audio frame was successfully written before the barrier. A partly written unit is excluded; V1 does not claim client playout knowledge.

**Exit criteria:** deterministic tests cover duplicate/late final transcripts, bounded context eviction, interruption before first audio, mid-unit and between-unit interruption, provider output after cancellation, flush timeout, write completion racing the barrier, and a successful next turn in the same session. No test describes generated or merely queued text as delivered context.

## Phase 3 - Secure WebSocket, Protobuf Audio Edge, and Backpressure (spec sections 19-20, 31-32; M3)

**Goal:** implement the production WebSocket boundary independently of RTVI semantics and real providers.

**Build:** use `github.com/coder/websocket` and generated Go bindings from the Phase 0 protobuf schema. Accept one binary protobuf `Frame` per WebSocket message; handle the audio variant as PCM16LE mono and expose the message variant to Phase 4. Validate encoded and decoded sizes before large allocation. Assign receive sequence/timestamps on the server; do not invent sender sequence, loss, or duplicate claims.

Implement the Phase 0 input rates, fixed output player rate, stateful bounded resampling, media-clock pacing, and millisecond/byte queue limits. Run independent supervised read and paced-write tasks so output cannot block microphone/VAD input. Drop old-generation audio; never silently truncate current-generation speech. Terminate explicitly on output-budget/write-deadline breach.

Authenticate before upgrade with atomically consumed single-use tokens through the pinned client's `token` query parameter. Enforce issuer/audience/iat/exp/jti/kid, bounded replay cache, post-restart iat rule, Origin policy, source/principal limits, pending-handshake limit, redaction, and trusted-proxy rules. Use HTTP failures before upgrade and WebSocket close behavior after upgrade; Phase 4 adds RTVI error messages once message semantics exist. Keep an authenticated connection lightweight behind a protocol-ready callback until Phase 4 readiness; allocate no VAD/provider resources. Reject automatic reconnect with a consumed token. Implement ws for development, wss/direct TLS and trusted proxy mode for production, timeouts, compression-off default, and graceful close.

**Exit criteria:** fixture, fuzz, and integration tests cover malformed/unknown protobuf, nested-field limits, PCM alignment/rates, fixed-rate playback, independent read during blocked write, pacing, bounded buffers, stale-generation discard, explicit slow-client termination, token replay races, restart boundary, Origin, redaction, pre/post-upgrade errors, dropped connection isolation, ws/wss, and controlled network impairment. The root verification target passes.

## Phase 4 - RTVI Compatibility and Client Regression Gate (spec sections 21-22; M4)

**Goal:** implement the exact pinned RTVI subset and re-prove the unmodified-client contract before provider work.

**Build:** parse and emit RTVI JSON only inside the protobuf message variant. Implement client readiness/version rejection, bot/server readiness, lifecycle, user speech/transcription, bot text/speaking/audio state, error, termination, and the exact interruption/clear event proven in Phase 0. Allocate the full session only after authentication, protobuf validation, and compatible RTVI client-ready. Keep RTVI types at the edge.

**Exit criteria:** the exact released packages from the manifest, installed without patches or a custom serializer, connect with a fresh token, complete a fake audio/text round trip, reject incompatible versions before heavy allocation, clear buffered playback on interruption, reject a consumed-token reconnect, and disconnect cleanly. Byte fixtures and browser compatibility tests run in CI/release suites as appropriate.

## Phase 5 - Google Providers and Provider Lifecycle (spec sections 23-24, 30, 33-34; M5)

**Goal:** replace provider fakes with the complete release-gating Google stack while retaining deterministic VAD for this phase.

**Build:** create the Google provider module. Implement Speech-to-Text V2 streaming with pinned recognizer/model, provider request-size and near-real-time cadence limits, pre-limit rotation, bounded overlap, transcript deduplication, and reserved concurrency headroom for the temporary second stream. Implement streamed Google LLM generation with bounded context and immutable model ID. Implement stable synchronous Cloud TTS using headerless `AudioEncoding_PCM`, bounded synthesis units, explicit rate metadata, cancellation/fencing, and conversion to the fixed client rate. Add deadlines, retry classification, Retry-After, jitter, no unsafe retry after committed output, circuit state, credential handling, and safe provider-neutral errors.

**Exit criteria:** unit/contract tests cover mapping, cancellation, late output, retry/circuit behavior, TTS PCM without WAV bytes, synthesis-unit bounds, STT request size/cadence, forced rotations and deduplication. A credential-gated real test completes Google-only multi-turn through the pinned client using deterministic VAD. A release-gating 30-minute real call and production-limit rotation run before release; the accelerated suite crosses at least five forced rotations with no duplicate UserMessageFrame.

## Phase 6 - Silero VAD and Production Worker Isolation (spec sections 15, 40-41; M6)

**Goal:** install real VAD and prove the production native-fault boundary before adding more product surface.

**Build:** implement Silero VAD behind the shared executor interface, with in-process sharded development executor and local worker-process production executor. Implement the pinned length-prefixed IPC handshake and messages, maximum lengths before allocation, one state-mutating request per session, parent-owned checkpoints, stdout protocol/stderr diagnostics, parent monotonic deadline, kill-after-grace for an unresponsive native call, crash-loop backoff, capacity/readiness feedback, and child reaping. Wire it into the Google pipeline.

**Exit criteria:** streaming-state correctness matches the POC tolerance; malformed IPC and handshake mismatches fail only the worker; hard kill, hang, and deliberate native fault cause bounded in-flight loss, capped respawn, correct internal capacity/fault signals, and no impact to sessions on other workers. The isolation portion of Scenario F passes with real concurrent Google sessions and an identical pipeline API for both executors. Phase 8 proves those signals through readiness, metrics, and timelines; Phase 9 reruns the complete deployed scenario.

## Phase 7 - xAI LLM Interchangeability (spec section 25; M7)

**Goal:** prove provider independence with the required LLM swap only.

**Build:** create the xAI provider module and a bounded REST/SSE Responses client using the pinned immutable model. Send `store: false`, resend bounded local context, enforce SSE line/event/total limits, validate terminal events, extract usage, honor cancellation/Retry-After, fence late output, and map safe errors. Do not implement xAI STT/TTS as V1 work.

**Exit criteria:** unit tests cover bounded parsing, malformed/oversized SSE, storage disabled, cancellation, retry, terminal event, and late-generation behavior. Scenario C passes with Google STT, xAI LLM, Google TTS and zero core changes.

## Phase 8 - Config-Driven Server, Readiness, and Observability (spec sections 27-36, 39; M8)

**Goal:** compose the programmatic components into an operable single-host service without introducing a second execution path.

**Build:** implement validated YAML config, registry/factories, development/production profiles, separate public/admin listeners, protected metrics/debug surface, provider budgets including STT rotation headroom, pending-handshake/session admission, credential checks, liveness/readiness semantics, retry/circuit exposure, structured logs, all section 35 metrics, and a bounded/TTL per-session event timeline whose exporter cannot block the session. Wire the Phase 3 token, TLS, proxy, and redaction controls through configuration. Implement server lifecycle shutdown as: unready, stop admission/upgrades, drain existing calls to the configured deadline, emit close/error where possible, cancel remaining sessions, close providers/workers, and return a documented status; retain an explicit immediate-cancel mode.

**Exit criteria:** Google-only and mixed configurations construct the same objects as the programmatic API; invalid/insecure configs fail clearly; health remains local/cheap; readiness reflects startup, shutdown, VAD capacity, admission state, and usable pipeline circuits; admin data is unreachable from the public listener; metrics avoid session-ID labels; event rings/exporter failure remain bounded; and faults produce an ordered diagnostic timeline without transcript/audio leakage.

## Phase 9 - Production Packaging and Release Scenarios (spec sections 37, 39-42, 47; M9-M10)

**Goal:** prove the complete system in both supported production process models.

**Build:** provide checksum-pinned multi-stage container image, non-root/read-only runtime, explicit writable paths, child reaping, capability/resource guidance, SBOM/scans, reference systemd unit, and TLS reverse-proxy config whose logs redact token queries. Wire SIGTERM from both process models into Phase 8's drain-then-cancel lifecycle.

**Exit criteria:** container and systemd deployment smoke tests and Scenario A both pass on target Linux; the remaining release scenarios may run on the declared primary package after package-equivalence checks. Run Scenarios C, D, E, F, G, H, I, and J plus the section 47 success demo against deployed WSS: mixed-provider calls, client-buffer barge-in, 30-minute rotation, concurrency isolation, provider/native failures, network/slow client, atomic token replay/redaction, and bounded resources. SIGTERM drain and forced-timeout behavior pass in both process models. Product SLO/capacity results use the minimum host/VAD/provider/FD/client limit and never reuse VAD-only POC capacity.

## Phase 10 - Documentation, Compatibility Record, and Release Closure (spec sections 43-46; M11)

**Goal:** close the release with reproducible evidence, not discover missing functional work.

**Build:** complete quick start, architecture/API/provider/RTVI/audio/security/config/observability documentation, compatibility and intentional-differences record, examples, production runbook, target Linux support matrix, immutable release manifest/digests, and links from every release criterion to evidence.

**Exit criteria:** every section 46 criterion points to a passing test, scenario, artifact, or report; someone other than the implementer follows the runbook end to end; all modules pass the root verification target; release-tagged provider/browser/native/deployment suites pass; and the compatibility manifest matches shipped artifacts. Any functional gap returns to its owning phase and that phase's gate is rerun rather than being patched silently in stabilization.
