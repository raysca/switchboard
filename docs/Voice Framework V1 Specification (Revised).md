# Voice Framework V1 Specification (Third Revision — Production WebSocket Track)

Sep 27, 2026

## 1. Purpose

V1 turns the validated Go voice-runtime architecture into the first complete, production-track framework and deployable voice server.

The system must prove the full product path end-to-end: RTVI Client, WebSocket Transport, Session Runtime, VAD, STT, Conversation Context, LLM, TTS, and back out through WebSocket Transport to the RTVI Client.

The single definition of done for this specification: once V1 is implemented, it must be possible to start the server as a real deployed process, not just a local dev binary, and place a live **WebSocket client voice call** to it from the exact pinned, released, unmodified Pipecat JavaScript client and WebSocket transport packages - speak, get a spoken response, interrupt it, and continue the conversation. Section 47 (V1 Success Demo) is the acceptance test for the whole spec; every other section exists to make that demo true on real infrastructure, not only on a developer's laptop.

V1 must support real conversational voice sessions using interchangeable provider implementations while preserving the core architectural properties already established by the runtime POC, most importantly the POC's central finding that managed, Go-level failures can be contained in-process, but native/cgo failures cannot. This is why the VAD executor must remain swappable and why production defaults matter, per sections 15 and 39.

For this document, **call** means one bidirectional voice session between an RTVI client and the server over WebSocket/WSS. It does not mean a PSTN, SIP, carrier, or WebRTC call. WebSocket is an intentional V1 product boundary, not a placeholder for WebRTC. V1 owns the buffering, pacing, interruption, backpressure, and connection-lifecycle behavior needed to make that boundary production-usable within the supported operating envelope defined in section 20.

The primary goal is not feature breadth. The goal is to prove the architecture works as a complete product system, deployed the way it would actually run in production.

## 2. V1 Goals

V1 must provide:

1. A reusable Go core framework based on the Frame to Processor to Pipeline model.
2. One lightweight supervised session per active conversation.
3. Real Silero VAD using the shared executor architecture validated by the POC, deployed with a production-safe default, see section 15.
4. Real STT, LLM, and TTS integrations.
5. A complete Google provider stack covering STT, LLM, and TTS, plus an xAI LLM integration proven interchangeable within an otherwise-Google pipeline; see section 25 for the scope reduction versus the original two-full-stack requirement.
6. A WebSocket transport for real-time bidirectional audio, secured with TLS in production, see section 20.
7. RTVI compatibility sufficient to use the exact pinned, released Pipecat JavaScript client and WebSocket transport packages, unmodified except for normal connection configuration such as URL and admission token.
8. Multi-turn conversation context, including a defined rule for what happens to context on interruption, see sections 16-18.
9. Interruption and barge-in behavior with unambiguous cancellation guarantees.
10. A config-driven server built on top of the core framework, deployable as a real production service, see section 39, not just runnable from source.
11. Health, metrics, graceful shutdown, and admission control suitable for a production orchestrator, systemd or a container runtime, to manage.
12. Programmatic Go APIs that do not require the config-driven server.

V1 must support a complete pipeline for Google STT to Google LLM to Google TTS, and must demonstrate that an xAI LLM can be substituted into that same pipeline without core changes.

## 3. Non-Goals

V1 does not attempt to reproduce all Pipecat functionality. Explicitly out of scope: WebRTC, SIP, PSTN, carrier and telephony integrations, Twilio, LiveKit, Daily, video and vision pipelines, provider-native speech-to-speech agent APIs, Gemini Live, xAI realtime speech-to-speech, tool and function calling, RAG, MCP, multi-agent routing, persistent conversation storage, distributed session state, horizontal session migration across hosts, end-user identity management, a hosted control plane, billing, dynamic plugin loading, hot configuration reload, dynamic pipeline mutation, full Pipecat API compatibility, full Pipecat frame taxonomy, and production site or UI implementation.

Although full identity management is out of scope, production connection admission security is not. V1 requires short-lived signed single-use session tokens, WebSocket Origin policy, and per-principal quotas as defined in sections 20, 31, and 32. These controls prove that the deployed voice service is not an anonymous public relay for billable provider APIs; they do not require V1 to own user accounts, login UI, or an external identity provider.

V1 does not claim WebRTC-equivalent behavior on hostile or highly variable networks. It will not provide RTP/UDP loss behavior, ICE mobility, carrier-grade jitter handling, or transport-provided acoustic echo cancellation. Its supported production envelope is a client with a stable full-duplex network path and a compatible RTVI WebSocket transport. The limits and required impairment tests for that envelope are defined in sections 20, 40, and 42.

Production readiness for V1 means a single deployable host or single-host container running the full pipeline reliably, observably, and securely, not a distributed or multi-region system. Horizontal scaling across multiple hosts, load balancing between them, and a control plane for fleet management are deliberately deferred, even though the single-host production target is now a hard requirement, see section 39. These may be added in later releases.

## 4. Pipecat as the Behavioral Reference

A local checkout of the Pipecat repository must be maintained during development. Pipecat is used as a behavioral reference, not as a source tree to port one-to-one.

When implementing a framework behavior: identify the equivalent Pipecat abstraction, read its public API, read the relevant implementation, read tests covering that behavior, identify the behavioral contract, design the Go implementation independently, add Go tests that express the desired behavior, and record intentional differences.

The Go implementation should preserve useful Pipecat concepts such as frames, frame processors, pipeline composition, upstream and downstream frame flow, context aggregation, interruption behavior, transport boundaries, provider processor behavior, lifecycle semantics, and RTVI behavior.

The implementation should not mechanically translate Python coroutine to goroutine, asyncio.Queue to Go channel, Python task to goroutine, or process-per-call to Go process-per-call.

The key question is always: what behavior is Pipecat guaranteeing here, and what is the most natural Go implementation of that behavior?

A compatibility document must be maintained at docs/pipecat-compatibility.md, with sections for preserved behavior, intentional Go differences, and not implemented yet.

## 5. Repository Structure

V1 should use a multi-module Go monorepo. Suggested top-level layout: go.work at the root; core/ (its own go.mod, containing frame, pipeline, processor, session, context, transport, vad, metrics, and internal packages); providers/google/ and providers/xai/ (each its own go.mod, each with stt, llm, and tts subpackages); server/ (its own go.mod, containing cmd, config, registry, api, and runtime packages); examples/ (google, xai, mixed, and custom-processor subfolders); docs/ (architecture, pipecat-compatibility, frames, processors, providers, rtvi, server-config, and a new production-deployment document); deploy/ (a new top-level directory holding the Dockerfile, an example systemd unit file, and a reference reverse-proxy/TLS-termination config, per section 39); and site/.

Dependency direction must remain core, then providers, then server. Core must not import provider or server packages. Each provider must be independently installable as its own Go module.

## 6. Core Runtime Principles

The core runtime must preserve the execution model validated by the POC: one session, one session run-loop goroutine, processors called inline, and async work re-entering through the session inbox.

The runtime must not create one goroutine or channel per processor by default. The framework abstraction is the pipeline. Goroutines, queues, worker pools, and executors are runtime implementation details.

## 7. Frame Model

Frames are the primary communication mechanism between processors. V1 should include the minimum frame taxonomy required to support the real product flow.

Audio frames, at minimum: InputAudioFrame, OutputAudioFrame, with sample bytes, encoding, sample rate, channel count, duration, monotonic media timestamp, sequence number, turn ID where applicable, and stream generation.

Speech-state frames, at minimum: UserStartedSpeakingFrame, UserStoppedSpeakingFrame.

Transcription frames, at minimum: InterimTranscriptionFrame, FinalTranscriptionFrame.

LLM and text frames, at minimum: LLMTextDeltaFrame, LLMTextFrame.

TTS frames, at minimum: TTSStartedFrame, TTSAudioFrame, TTSStoppedFrame.

Conversation frames, at minimum: UserMessageFrame, AssistantMessageFrame, with stable turn IDs and a partial flag.

Lifecycle and control frames, at minimum: StartFrame, EndFrame, CancelFrame, ErrorFrame, InterruptionFrame, FlushFrame, and FlushProcessedFrame or an equivalent **server-local** transport-barrier acknowledgement. V1 does not invent a client acknowledgement absent from the pinned protocol.

Do not reproduce Pipecat's complete frame hierarchy unless required by V1 behavior.

## 8. Processor Model

Processors are the primary framework abstraction: a Processor interface exposing ProcessFrame(ctx, frame, direction, emit), where emit is an Emitter.

A processor may inspect a frame, consume it, forward it, transform it, emit zero frames, emit one frame, emit multiple frames, emit downstream, emit upstream, or initiate supervised asynchronous work.

Processors belonging to one session must not normally be invoked concurrently.

## 9. Direction and Emitter

V1 must support bidirectional frame movement: a Direction type with Downstream and Upstream values, and an Emitter interface with Push(frame) and PushDirection(frame, direction) methods.

This is required for lifecycle, interruption, transport control, and future extensibility.

## 10. Pipeline

A pipeline is an ordered collection of processors. The normal V1 voice pipeline is: TransportInput, SileroVAD, STT, UserContextProcessor, LLM, AssistantContextProcessor, TTS, TransportOutput.

Processors must interact through frames rather than direct component-to-component calls. V1 may use a linear chain internally, provided the API does not prevent future richer routing.

## 11. Session Runtime

Each active conversation receives one logical session. A session owns a session ID, one pipeline, one run-loop goroutine, one context and cancellation domain, one bounded inbox, one supervisor, one resource budget, session-scoped processor state, conversation context, transport association, and metrics and state.

The session is the principal isolation and lifecycle boundary.

## 12. Async Re-entry

Asynchronous operations must not directly invoke pipeline processors concurrently. Examples include a VAD inference result, an STT provider event, LLM streamed output, a TTS audio result, and a provider error.

Async completions must re-enter the pipeline through the session inbox at the correct pipeline position. Conceptually an envelope may contain the frame, the direction, the resume processor index, and metadata. This preserves single-threaded processor semantics within a session.

## 13. Session Supervision

All session-owned asynchronous work must be supervised, conceptually through a session.Go helper. The supervisor must recover ordinary Go panics in managed work, propagate errors, track outstanding work, enforce task budgets, cancel session work, wait during teardown, and surface cancellation-ignoring work as leaked, rather than hiding it.

Framework and provider code should not create unmanaged session-owned goroutines.

## 14. Resource Isolation

Each session must have an explicit resource budget. V1 should support configurable limits for queued frames, asynchronous tasks, pending VAD work, buffered input audio, buffered output audio, conversation messages and estimated context tokens, emits caused by a single input frame, maximum processor execution time, maximum session duration, and teardown timeout.

All queues must be bounded. One session must not be able to cause unbounded host memory or execution growth.

## 15. VAD

V1 uses Silero VAD. The VAD processor must appear to the framework as an ordinary processor. Streaming state remains session-scoped. Inference execution is delegated to a shared executor.

The executor abstraction must support at least an in-process sharded executor and a local worker-process executor. The selected executor must not affect the pipeline API.

Fix versus the original draft, carried forward from the POC's decisive finding: the POC proved that a native or cgo-level fault inside the VAD inference path takes down the entire host process, while every managed Go-level fault can be contained to a single session. The local worker-process executor is how that native-fault risk is contained in production, at a measured cost of roughly 12 percent capacity in the POC's benchmarks.

Accordingly: the **production default executor is the local worker-process executor**. The in-process sharded executor remains available and may be used as the local development default for faster iteration, but the config-driven server's production profile (section 39) must select the worker-process executor unless an operator explicitly opts into the in-process executor after their own crash-isolation risk assessment. This choice must be documented plainly in the server configuration reference, not left as a silent default that could regress the POC's own conclusion.

The config-driven server must allow executor selection either way.

## 16. Conversation Context

V1 requires provider-neutral multi-turn context: a Message type with Role, Content, TurnID, Partial, and creation metadata, where Role is one of System, User, or Assistant.

The core context model must not expose Google- or xAI-specific message structures. Provider adapters are responsible for translating the generic conversation representation into provider-specific request structures.

Conversation context must be bounded. Configuration must define a maximum message count and an estimated provider-neutral token budget. When either limit is approached, the context processor must deterministically discard the oldest complete user/assistant turn pairs while preserving the system instruction, the current user turn, and the most recent complete turns. V1 does not require summarization. Provider adapters may perform a more accurate provider-specific token check before a request, but must fail the session turn safely rather than sending a request known to exceed the provider limit. Context eviction and provider-limit rejection must be observable and tested.

## 17. Context Processors

V1 must provide basic conversation processors.

**User Context Processor.** Receives final transcription output. Responsibilities: assign or preserve a stable turn ID, add the user message to conversation context, produce or trigger the LLM request, and preserve correct turn ordering. Duplicate or late final transcripts for a closed generation must not create a second user turn.

**Assistant Context Processor.** Receives assistant output and is responsible for adding it to context and forwarding assistant text toward TTS, preserving coherent multi-turn history. LLM text must be segmented into explicit synthesis units. Each synthesis unit retains its turn ID and generation through TTS and transport output.

When a turn is interrupted before completion, the Assistant Context Processor records a partial assistant message, tagged Partial=true. For V1, the precise and testable partial boundary is **server-delivered complete synthesis units**: only a unit whose final audio frame was successfully written to the WebSocket before the interruption barrier is included. A unit interrupted partway through delivery is excluded in full because V1 has no word-level audio alignment. Text merely generated by the LLM, queued for TTS, synthesized but never written, or only partly written is excluded. This is deliberately a conservative server-delivery boundary and does not claim that every written byte played through the user's speaker; the unmodified client protocol does not provide exact playout acknowledgement. Write completions must re-enter the session run loop before mutating context. The per-session event timeline records the last fully delivered synthesis unit and the server-local flush barrier result.

## 18. Interruption and Barge-In

Barge-in is required for V1. When user speech begins while assistant output is active, the runtime must support: UserStartedSpeakingFrame, then InterruptionFrame carrying the interrupted turn and generation IDs, then cancellation of active LLM generation and active TTS synthesis, flushing server-side queued assistant audio, sending the exact interruption signal supported by the pinned WebSocket transport/client pair, waiting for a bounded server-local transport acknowledgement, and continuing with the new user turn.

Required properties, hardened versus the original draft: stale assistant audio must stop; queued assistant audio must be flushable; TTS work must be cancellable; **LLM work must be cancellable**, without exception. Every STT stream, LLM request, TTS request, and output-audio sequence must carry a monotonically increasing session-local generation ID. Cancellation is not the correctness boundary: processors and the transport must reject every late completion whose generation is no longer current, even if a provider emits it after cancellation. If a provider does not expose true mid-stream cancellation, the framework must stop consuming and forwarding its output immediately and continue draining or close the provider connection in supervised background work. Context must remain coherent per section 17, and the session must remain usable after interruption without recreation.

An interruption barrier is complete only when all server-side audio for the old generation has been discarded and TransportOutput has acknowledged processing the flush control message. The exact pinned, unmodified client must also be proven to clear audio already buffered for playback. This is a Phase 0 feasibility gate, not an assumed generic RTVI guarantee: the compatibility manifest must identify the concrete wire event and released client behavior that performs the clear. If no released unmodified client/transport pair can clear its playback buffer, V1 cannot simultaneously satisfy its unmodified-client and barge-in requirements; implementation must stop for an explicit product decision rather than inventing a proprietary message and claiming compatibility. A missing server-local acknowledgement must time out and fail or degrade the affected session explicitly; it must never block the session run loop indefinitely.

Pipecat's implementation and tests should be used to understand the intended behavioral semantics, but the LLM-cancellation and partial-context rules above are V1-specific requirements that must hold regardless of what Pipecat itself does.

## 19. Transport Abstraction

Transport must remain independent from STT, LLM, TTS, and context processors. Transport responsibilities include accepting client audio, emitting client audio, connection lifecycle, protocol messages, errors, and session association. Future transports must be addable without changing providers.

## 20. WebSocket Transport

WebSocket is the required and only V1 client voice transport. WebRTC, SIP, PSTN, and carrier transports are V2-or-later work. The WebSocket transport must support client connect, client disconnect, bidirectional real-time audio, control and protocol messages, session creation and association, interruption signaling, errors, RTVI messages, and graceful close.

**Supported operating envelope.** V1 supports the pinned Pipecat JavaScript WebSocket client on stable full-duplex networks. WebSocket runs over TCP, so the implementation must bound rather than eliminate head-of-line delay. V1 does not promise seamless network handover, ICE restart, RTP loss concealment, transport-provided echo cancellation, or carrier-grade operation. Browser clients may enable device-level echo cancellation, noise suppression, and automatic gain control, but those are client capabilities and not server guarantees. The reference browser/device combinations and acoustic setup used for release testing must be recorded; a headset is the baseline, and speakerphone operation may be claimed only for combinations that pass the barge-in test without self-interruption. These limitations must appear in deployment documentation and must not be hidden behind the generic word "call."

**Wire and audio contract.** The wire format must match the pinned WebSocket transport's default serializer. For the current compatibility target this is one binary Pipecat protobuf `Frame` per WebSocket message: raw audio bytes and their sample rate/channel count occupy the protobuf audio variant, while RTVI JSON occupies the protobuf message variant. It is not the RTVI base64 `raw-audio` message shape. The exact `.proto` schema and generated-code version are compatibility coordinates and must be checksum-pinned with the client packages. Unknown variants, malformed protobuf, trailing data, invalid nested JSON, and oversized decoded fields are rejected. The server must enforce both encoded-message and decoded-audio limits before large allocations.

Audio samples are signed PCM 16-bit little-endian, mono. The server must accept at least 16 kHz, 24 kHz, and 48 kHz input from the pinned client's supported recorder configuration and convert it to a configurable 16 kHz mono internal STT/VAD format. Provider TTS output must be converted to the one output rate frozen in the compatibility manifest without changing pitch or duration. The current client player does not negotiate its playback rate from each received frame, so a different client player rate is unsupported rather than dynamically negotiated; the reference client configuration and compatibility test must use the frozen value. Raw audio must never include a WAV header. The transport must validate declared format, sample alignment, samples per message, and duration before admitting audio to the session.

Internally, audio frames must carry duration, a server-assigned monotonic media timestamp and receive sequence, turn ID where applicable, and generation ID. WebSocket/TCP already preserves message order, and the pinned serializer carries no sender sequence number; therefore V1 must not claim it can distinguish network loss, duplication, or capture gaps that the wire protocol does not expose. It must detect timestamp discontinuity from observed arrival/capture cadence and malformed or missing audio, and report those facts accurately. Default client chunks should represent 20 ms of audio; 10-40 ms is supported. Input buffering, output buffering, and total queued audio must be bounded in milliseconds as well as bytes.

**Pacing and backpressure.** TTS may produce audio faster than real time. TransportOutput must pace audio against its media clock and must not write an entire synthesis result as quickly as the socket permits. WebSocket reading and paced writing run as separate supervised session tasks so output pacing or a blocked writer cannot prevent microphone input and VAD-driven interruption from being processed. Both tasks communicate with the pipeline only through bounded queues and session re-entry. Old-generation audio is always discardable; current-generation speech is never silently truncated to recover space. If the output budget or write deadline is exceeded, the session terminates explicitly as a slow client. A slow client must never retain unbounded memory or stall another session.

**Interruption and flush.** On interruption, server queues for the old generation are cleared before any new-generation audio is sent. The server must send the exact serializer/RTVI event proven by the Phase 0 compatibility gate in section 18 and verify that the pinned client clears its playback buffer. New-generation audio must be ordered after that event. The server records a local flush acknowledgement when TransportOutput has processed the barrier; the client is not required to implement a new proprietary acknowledgement.

**Connection lifecycle.** Configuration must define HTTP-upgrade timeout, RTVI client-ready timeout, read timeout, write timeout, idle timeout, ping interval, maximum connection lifetime, maximum binary message and decoded-field sizes, and close timeout. Authentication, Origin, and capacity failures detected before upgrade use appropriate HTTP status codes; failures after upgrade use an RTVI error where possible followed by a protocol-appropriate WebSocket close code. A successfully authenticated socket remains a lightweight connection until its pinned serializer and RTVI version are validated; only then may the server allocate a voice session, VAD state, or provider stream. Reconnection and session resumption are not supported in V1. The pinned transport's automatic reconnect using an already-consumed token must be rejected; an application starts a new call by obtaining a new token and connecting again, with no prior in-memory context.

**Security.** Production WebSocket connections require a short-lived signed, single-use admission token presented through the pinned client's normal `token` connection parameter, which the current transport carries in the upgrade URL query. Tokens must bind at least a unique token ID, issued-at time, expiry, issuer, audience, and principal or quota key and must be validated and atomically consumed before upgrade. Query strings containing tokens must be redacted from application, reverse-proxy, access, trace, and error logs; the token must never be placed in a referrer-bearing page URL. The bounded replay cache retains token IDs through expiry. After restart, the server rejects tokens issued before the current process start outside the configured clock-skew allowance, so losing the in-memory replay cache does not reopen the full token lifetime. Browser Origin must match a configured allowlist; missing Origin may be allowed only for explicitly configured non-browser clients. Per-principal and per-source connection limits, handshake rate limits, and maximum call duration are mandatory. WebSocket compression is disabled for audio sessions unless separately risk-assessed and bounded.

In production, the transport must run over TLS, wss, not plain ws. The server must support terminating TLS itself with a configured certificate, or running behind a TLS-terminating reverse proxy. Forwarded connection metadata may be trusted only from configured proxy addresses. Plain ws remains available for local development only, and the production profile must refuse to start on plain ws unless an operator explicitly overrides it.

## 21. RTVI Compatibility

RTVI compatibility is a V1 requirement. The goal: the exact pinned, released, unmodified Pipecat JavaScript client and WebSocket transport can connect to the Go server without a custom client protocol or serializer.

The implementation must pin one released Pipecat client package version, its matching WebSocket transport package, the transport's default serializer and protobuf schema, one RTVI protocol version, and the local Pipecat reference commit. The initial target is the protocol version exposed by the pinned reference at Phase 0; upgrades require updating fixtures and compatibility tests. V1 supports only that pinned compatibility set unless an additional version is separately specified and tested. Any unpinned or incompatible version must be rejected during the readiness handshake before heavy session resources are allocated.

V1 must support the subset required for a normal voice session, at minimum: protobuf audio/message envelope handling, client and session initialization, version negotiation, bot and server readiness, connected and disconnected lifecycle, raw client audio in the shapes emitted by the pinned serializer, user speech state, user transcription events, bot text events, bot speaking and audio state, the proven interruption/clear behavior, error events, and session termination. Optional RTVI base64 audio batches are not a V1 requirement unless the pinned released client actually emits them.

RTVI must remain a transport and protocol-edge concern. Core pipeline frames should not depend on RTVI types. The transport layer maps between RTVI messages and core frames.

## 22. RTVI Compatibility Tests

V1 must include protocol and integration tests covering the protobuf serializer and RTVI behavior. A release-level test must install the exact pinned released client and transport packages, without local patches, and demonstrate the client connecting through the Go WebSocket server, entering the Go session runtime, traversing a real provider pipeline, and receiving an audio response over the same connection.

Recorded fixtures or equivalent protocol tests should be used where useful to prevent compatibility regressions.

## 23. Provider Architecture

Provider modules depend on core and implement processors and capabilities required by the pipeline. Provider-specific SDK types must not leak into core. Provider selection must remain per capability.

The following must be valid configurations: Google STT, Google LLM, Google TTS; and Google STT, xAI LLM, Google TTS. The same core pipeline must work for both.

## 24. Google Provider Module

Module: providers/google. V1 must include a complete Google STT, LLM, and TTS stack, since Google is the primary fully-supported provider for V1 (see section 25 for the scope of the xAI module).

**Google STT.** Real-time audio input, streaming recognition, interim transcription where available, final transcription, configurable model and settings, configurable language, cancellation, and provider error mapping. Output must be expressed through core transcription frames.

Google STT integration must support calls longer than one provider streaming limit. Before the active stream reaches the provider limit, the processor must open a replacement stream, bridge a bounded overlap of recent audio, switch generations without losing speech, and deduplicate interim and final results across the boundary. Admission budgeting must reserve the temporary second concurrent stream required during rotation, and each provider audio request must remain below the pinned API's byte limit while audio is sent at approximately real-time cadence. Stream rotation must not create duplicate UserMessageFrames. Rotation count, failures, and deduplicated results must be observable. A release test must hold one real call open for at least 30 minutes and cross a real production-limit rotation; the automated suite must additionally cross at least five forced-short rotations.

**Google LLM.** Configurable model, system instruction, multi-turn history, streaming text output, cancellation, generation controls where appropriate, provider error mapping, and usage metadata where available. V1 uses ordinary text generation, not a provider-native speech-to-speech or live agent API.

**Google TTS.** V1 must pin one release-gating Google TTS API surface and one supported production voice family before Phase 5 begins; Preview-only streaming behavior must not be a silent dependency. It must provide configurable voice and supported settings, incremental audio output where the selected API supports it, cancellation, provider error mapping, and explicit output sample-rate metadata. If the selected stable API returns one complete audio result rather than a true stream, the provider must synthesize bounded sentence or phrase units so first-audio latency and interruption remain within section 42's SLOs. Audio output must be expressed as core TTS and audio frames and converted by the audio layer to the client output contract.

## 25. xAI Provider Module

Module: providers/xai.

Fix versus the original draft: the original spec required a complete, independent xAI stack, STT, LLM, and TTS, at the same release-gating level as Google. That doubles the real integration and live-provider testing surface without strengthening the one claim that actually matters for V1: that the core pipeline is provider-agnostic. Proving interchangeability only strictly requires swapping one capability, not building and validating three.

V1's release-gating requirement for xAI is narrowed to: **xAI LLM**, implemented to the same standard as the Google LLM module, configurable model, system instruction, multi-turn history, streamed text output, mandatory cancellation per section 18, provider error mapping, and usage metadata where available, and proven to work as a drop-in substitute for the Google LLM inside an otherwise-Google pipeline, per Scenario C in section 41 of the test plan, with no core changes required.

xAI STT and TTS are not V1 requirements. They may be built as a post-V1 fast-follow behind the same provider boundaries. If they happen to be available before release, Scenario B (full xAI stack, section 41) may be run and reported as bonus validation, but their absence does not block or diminish V1.

xAI LLM requirements: configurable immutable model ID, system instruction, multi-turn history, streamed text output, cancellation, provider error mapping, and usage metadata where available. Requests must disable provider-side response storage because V1 owns bounded in-memory context and persistent conversation storage is out of scope. V1 must not depend on xAI's combined speech-to-speech realtime agent mode.

## 26. Provider Capability Interfaces

Core should avoid large provider-specific abstractions. Providers should primarily implement the generic Processor contract. Small capability interfaces may be introduced where useful, for example Cancelable, HealthCheckable, and Streaming. Capability interfaces must remain provider-neutral.

## 27. Server

The V1 server is a consumer of the framework. It must not contain a second voice execution model. Responsibilities: load configuration, validate configuration, resolve environment-backed secrets, resolve provider factories, construct pipelines, create and manage sessions, host WebSocket connections, expose health and readiness, expose metrics, enforce admission control, and perform graceful shutdown.

## 28. Config-Driven Pipeline

The server must construct pipelines from configuration. A production profile must cover: separate public and administrative listen addresses; WebSocket TLS or trusted-proxy mode; Origin allowlist; admission-token issuer, audience, maximum lifetime, signing-key references, and bounded replay-cache policy; upgrade and RTVI-ready timeouts; message, decoded-field, buffer, idle, and call-duration limits; runtime maximum sessions; provider concurrency and rate budgets including STT rotation headroom; Silero worker-process executor size and crash-loop policy; supported client and internal audio rates; context message/token budgets; provider names, immutable model and recognizer IDs, languages, voices, and operation timeouts; retry and circuit settings; event-timeline bounds; and the agent system prompt. An equivalent xAI-LLM configuration swaps only the LLM provider and model fields. Mixed-provider configuration must also work.

The config-driven server must construct the same programmatic core objects available to Go users. There must not be a separate config-only execution path.

## 29. Provider Registry

The server should use a registry and factory mechanism, for example google.stt, google.llm, google.tts, xai.llm. Configuration resolves a provider name through the registry to a factory to a core Processor. The registry belongs in the server and composition layer, not core.

## 30. Credentials

V1 supports environment and platform credential resolution. Typical values may include GOOGLE\_APPLICATION\_CREDENTIALS, GOOGLE\_API\_KEY, and XAI\_API\_KEY. Exact authentication mechanisms depend on provider requirements.

Credentials must never be sent to clients, never be emitted in logs, and never be committed in example config.

Production addition: the server must fail to start, with a clear diagnostic, if a configured provider's required credential cannot be loaded or is structurally invalid. A transient provider outage must not turn a local startup check into a crash loop; optional live credential validation affects readiness and emits a clear diagnostic. Credential values must never appear in the config file itself, only environment variable references or file paths may appear. The server must refuse group- or world-readable credential files in the production profile unless an explicit insecure override is set. External secret-management integrations are deferred, but the environment-variable and file-based path must itself be production-safe.

## 31. Server Network Surface

At minimum: GET /health, GET /ready, GET /metrics, and WS /ws (wss in production, per section 20). /health is process liveness and must not perform provider network calls. /ready is false during startup and shutdown, when the minimum configured VAD worker capacity is unavailable, when required local artifacts or credentials cannot be loaded, or when admission has been globally disabled; provider circuit state appears in the response and metrics but makes the whole server unready only when no configured pipeline can accept a new call. A minimal token-minting or application bootstrap integration point may be provided as an example, but production deployments are expected to mint admission tokens from their existing trusted application backend. /metrics and any session-debug endpoint must be bound to a separate administrative listener or protected by explicit network policy and authentication; they must not be exposed merely because /ws is public. Avoid proprietary voice-protocol endpoints where RTVI already defines the lifecycle.

## 32. Admission Control

V1 must support admission control before allocating voice-session, VAD, or provider resources. A bounded lightweight connection slot may exist between HTTP upgrade and the RTVI readiness handshake and is itself admission-limited. Admission must include maximum pending handshakes, maximum active sessions, per-principal limits, per-source handshake rate limits, VAD pressure or executor capacity feedback, provider concurrency and request-rate budgets including STT rotation headroom, and an operator-configured cost guard such as maximum call duration. A new session must be rejectable before the server or any configured provider enters sustained overload. Provider limits must be configurable independently because real end-to-end capacity may be far below the host's VAD capacity. Autoscaling and orchestration are out of scope.

## 33. Error Model

Provider-specific errors must be translated into provider-neutral framework errors. Core and server should distinguish at minimum: session error, provider authentication error, provider rate-limit error, provider unavailable, provider timeout, transport error, resource-budget error, session busy or closed, cancellation, and internal error. Errors may retain safe provider metadata. Credentials and sensitive provider responses must never be exposed to clients.

## 34. Timeouts and Cancellation

All provider operations must honor context cancellation. Configurable defaults must exist for provider connection establishment, individual reads and writes, STT stream rotation, LLM generation, TTS synthesis, flush acknowledgement, and session teardown. Interruption must propagate cancellation to active provider work. Session shutdown must cancel outstanding work.

Each provider adapter must define a retry policy by operation and error class. Authentication and invalid-request errors are not retried. Rate limits honor Retry-After when present. Transient connection and availability failures use capped exponential backoff with jitter within the turn deadline. Operations that may duplicate user-visible output are not retried after any output has been committed unless the adapter has a provider-supported idempotency mechanism. Repeated failures open a bounded per-provider circuit that feeds readiness and admission control without blocking unrelated providers. Retry count, circuit state, and terminal reason must be observable.

## 35. Metrics

V1 must expose enough metrics to diagnose the full product pipeline.

Runtime: active sessions, total sessions started, stopped sessions, failed sessions, session startup latency, goroutine count, heap and resource metrics, admission rejects by reason, context evictions, maximum-duration terminations, and **leaked managed work and sessions marked leaked**.

VAD: queue wait, inference latency, stale and dropped chunks, active VAD streams.

STT: active streams, provider errors, interim transcript latency, final transcript latency, stream rotations, rotation failures, and deduplicated results.

LLM: requests, errors, retries, circuit state, time to first text or token, completion latency, and stale-generation frames discarded.

TTS: requests, errors, retries, circuit state, time to first audio, completion latency, and stale-generation frames discarded.

Transport: pending handshakes, connected clients, authentication failures, replay rejections, Origin rejections, disconnects and close reasons, audio frames and bytes in and out, arrival discontinuities, malformed protobuf/RTVI messages, input and output buffered milliseconds, discarded stale-generation audio, server-local flush acknowledgements and timeouts, write latency, slow-client terminations, and protocol errors.

Metrics should avoid high-cardinality session\_id labels.

## 36. Logging

Use structured logging. Relevant events should include fields such as session\_id, processor, provider, event, duration, and error.

Do not log raw audio. Do not log credentials. Conversation transcript logging should be disabled by default.

V1 must also produce a per-session structured event log or equivalent queryable trace that can be retrieved as a single ordered timeline for one call: connection and authentication result, audio gaps, VAD speech-start/stop, STT stream rotation and final results without transcript content, LLM request start and time-to-first-token, TTS synthesis units, transport delivery, interruption barriers, flush acknowledgements, context eviction, provider retry/circuit events, and errors. Every event carries session ID, turn ID, and generation where applicable. This event timeline is on by default and distinct from the off-by-default conversation transcript.

The event timeline must not be an unbounded per-session memory buffer. The production profile must use a bounded ring or streaming exporter with configurable event count or byte limit and TTL. Exporter failure must shed diagnostic events according to a visible counter, never block the session run loop. Retrieval must occur only through the protected administrative surface or the configured log/trace backend.

## 37. Graceful Shutdown

Server shutdown must: mark the server unready; stop accepting new sessions and WebSocket connections; allow existing calls to drain by default until the configured shutdown deadline; then send an error/close signal where possible, cancel remaining sessions, close provider streams, release VAD resources, stop VAD worker processes, and exit. An explicitly configured immediate-cancel mode may skip the drain period. The process must return success only after an orderly drain/cancel sequence; cleanup failure or forced timeout returns a documented non-zero exit code. This sequence must work identically whether the process receives SIGTERM from systemd or from a container runtime, per section 39.

## 38. Programmatic Core API

The config-driven server is only one consumer of the framework. A Go application must be able to construct the same pipeline programmatically, conceptually assembling a pipeline from a VAD processor, provider STT, a conversation context, a provider LLM, and a provider TTS, then wrapping it in a session. Exact package names and constructors are not prescribed here. The requirement is that the core framework remains independently useful.

## 39. Production Deployment Requirements (new)

This section is new versus the original draft. The original spec treated production deployment as an afterthought, listing it only implicitly through health, metrics, and graceful shutdown. Given the requirement that V1 be a real production-track system, deployment must be a first-class, specified concern.

**Target platform.** V1 must run correctly on target Linux server hardware, not only on a developer's macOS or Windows machine. The POC's own performance numbers were measured on a macOS laptop and are explicitly flagged in its report as needing revalidation on Linux server hardware before being treated as production limits; V1 inherits that obligation directly (see section 41).

**Process management.** The server must run cleanly as a supervised process under systemd, responding correctly to SIGTERM for graceful shutdown (section 37) and reporting a usable exit code on failure. A reference systemd unit file must be provided in deploy/.

**Containerization.** A production Dockerfile must be provided in deploy/, building a minimal, non-root runtime image. The container must expose /health and /ready in a form usable by standard container-orchestrator health checks, honor SIGTERM for graceful shutdown, reap and supervise VAD child processes correctly, use a read-only root filesystem where practical, declare its writable temporary paths, drop unnecessary Linux capabilities, and run with explicit PID, file-descriptor, CPU, and memory limits.

**TLS.** Per section 20, the WebSocket transport must run over TLS in production, either terminated by the server itself or by a trusted reverse proxy in front of it. A reference reverse-proxy configuration must be provided in deploy/.

**Resource limits.** The deployment reference material must document recommended CPU, memory, and file-descriptor limits per expected concurrent-session load, informed by the POC's measured per-session marginal cost, and must document how those limits interact with the VAD worker-process pool's own process and memory footprint.

**Configuration profiles.** The config-driven server must clearly distinguish a development profile (defaults: in-process VAD executor, plain ws allowed, verbose logging) from a production profile (defaults: worker-process VAD executor, TLS required, structured JSON logging, admission control enabled). Section 15 and section 20 define the specific defaults each profile selects.

**Observability integration.** The /metrics endpoint's output must be consumable by a standard metrics scraper without additional adaptation, and structured logs must be emitted in a form consumable by a standard log-aggregation pipeline.

**Supply chain.** Go modules, the ONNX Runtime archive, and the Silero model must be version-pinned and checksum-verified. CI must generate an SBOM, run dependency and container vulnerability scans with a documented severity gate, and publish the supported Linux architecture and libc requirements. Release images must be immutable by digest; image signing is recommended for V1 and required before any hosted multi-tenant offering.

This section's requirements are validated by the deployment steps of Scenario A in section 41 and by the release criteria in section 46.

## 40. Testing Requirements

**Core tests** must cover: processor ordering, downstream routing, upstream routing, frame consumption, multi-frame emission, async re-entry, bounded queues, resource budgets, panic containment, cancellation, teardown, turn and generation ordering, stale-completion rejection, bounded context eviction, interruption barriers, and flush behavior.

**Resilience tests** (fix versus the original draft: this category did not exist before and is added specifically to carry the POC's decisive H3 finding into V1) must cover: a deliberately triggered native or cgo-level fault inside the VAD inference path, run under a real end-to-end provider pipeline, not just the runtime microbenchmark harness, confirming that with the production-default worker-process executor the fault is contained to the affected session's in-flight audio and the rest of the server continues serving other sessions; and, separately, a hard kill of a VAD worker process under active load, confirming automatic respawn and that only in-flight jobs on that worker are lost.

**Provider tests**, each provider module must cover: request mapping, response mapping, error mapping, cancellation, timeout, retry classification, rate-limit handling, circuit behavior, late output after cancellation, and mocked provider behavior. Google STT tests must cover stream rotation, overlap, and transcript deduplication. Google TTS tests must cover synthesis-unit segmentation, output format, and cancellation. Live provider integration tests should be separately gated by credentials.

**Server tests** must cover: config parsing, config validation, registry resolution, pipeline construction, session lifecycle, WebSocket lifecycle, token authentication, Origin policy, message and call-duration limits, administrative endpoint protection, health and readiness semantics, graceful shutdown, and every admission-rejection reason.

**RTVI and serializer tests** must use the pinned released client and transport packages and cover: byte-for-byte protobuf fixtures in both directions, encoded and decoded size limits, malformed and unknown variants, nested RTVI JSON, protocol initialization and version rejection, readiness, supported input/output sample rates, transcription events, bot text and speaking state, output pacing, errors, interruption and client playback-buffer flush, and disconnect. Fixtures must record the client package, transport package, serializer schema, and RTVI protocol versions that produced them.

**Network-impairment tests** must exercise controlled latency, jitter, bandwidth restriction, temporary read stalls, temporary write stalls, delayed TCP delivery, abrupt disconnect, and a slow client. They must verify bounded memory, bounded audio queues, explicit timeout or recovery, isolation from other sessions, and the SLOs in section 42 within the documented supported envelope. Seamless network handover and session resumption are not V1 requirements.

**Security tests** must cover expired, malformed, wrong-issuer, wrong-audience, and replayed admission tokens; concurrent attempts to consume the same token; missing or disallowed browser Origin; handshake flooding; oversized protobuf, nested JSON, and decoded audio fields; invalid protobuf and audio alignment; query-token redaction in server and reference-proxy logs; attempts to access administrative endpoints from the public listener; and secret/log redaction.

## 41. Required End-to-End Scenarios

V1 is not complete until the following work is demonstrated, deployed per section 39, not only run from source.

**Scenario A - Google (release-gating).** RTVI client, over wss, to a production-deployed server, VAD, Google STT, context, Google LLM, Google TTS, back to the client. Must support multi-turn conversation, and must be run against the server deployed via the container or systemd path from section 39, not only via go run.

**Scenario B - xAI full stack (optional, not release-gating).** The same path with xAI STT, LLM, and TTS throughout. It runs only if the post-V1 xAI STT/TTS fast-follow modules from section 25 are already available; its absence does not block release.

**Scenario C - Interchangeability (release-gating).** Google STT, xAI LLM, Google TTS, in the same deployed pipeline, with no core code changes. This is the scenario that actually proves provider independence for V1 and is release-gating in place of a full second provider stack.

**Scenario D - Barge-in (release-gating).** While assistant audio is playing in the exact pinned, unmodified client: user begins speaking, VAD detects speech, current assistant output is interrupted, stale TTS audio is flushed from both server queues and the client's playback buffer through the compatibility-gated standard behavior, active TTS and LLM work are cancelled per section 18, and the new user turn succeeds with context recorded per section 17.

**Scenario E - Managed Failure Isolation (release-gating).** Cause a provider or session-level failure. Verify the affected session fails or recovers appropriately, unrelated sessions continue, no host-wide failure occurs, and resources are reclaimed.

**Scenario F - Native Fault Isolation (release-gating; new, carries the POC's central finding forward).** With the production-default worker-process VAD executor active and multiple real sessions running, deliberately trigger a native or cgo-level fault in the VAD path. Verify the affected sessions experience only a bounded window of dropped audio while the worker respawns, and unrelated sessions on other workers are entirely unaffected. This is the scenario that justifies the section 15 production-default decision with live evidence, not just POC-stage evidence.

This scenario must also confirm the leaked-work and fault metrics from section 35 increment and are visible on the protected /metrics endpoint in real time during the fault, and that the per-session event log from section 36 shows the affected session's timeline cleanly, not merely that the endpoints and log stream exist.

**Scenario G - Concurrent Sessions.** Run multiple voice sessions simultaneously. Verify context remains isolated, audio does not cross sessions, one slow provider request does not serialize unrelated sessions, and sessions cleanly tear down.

**Scenario H - Long Call and STT Rotation (release-gating).** Hold one real Google call open for at least 30 minutes, crossing provider stream boundaries without duplicate or missing user turns. In the automated suite, configure a shorter rotation interval and cross at least five rotations. Verify bounded context, no memory slope, and continued barge-in after rotation.

**Scenario I - WebSocket Impairment and Slow Client (release-gating).** Run the pinned client through the network-impairment matrix from section 40. Verify output pacing, bounded buffers, isolated slow-client termination, interruption flush behavior, and no host-level memory growth. This scenario establishes the supported WebSocket operating envelope; it does not claim WebRTC-equivalent behavior.

**Scenario J - Admission Security (release-gating).** Verify a valid short-lived token can connect exactly once and simultaneous replay attempts have one winner; all required invalid-token and Origin cases are rejected before voice-session, VAD, or provider allocation. Confirm per-principal, pending-handshake, provider-quota, and call-duration limits reject with observable reasons, query tokens are redacted throughout the reference deployment, and public clients cannot reach administrative metrics or session timelines.

## 42. Performance Expectations

V1 is not a repeat of the runtime POC. The implementation must avoid regressing the architectural properties already validated. Benchmark coverage should remain available for session creation, processor traversal, per-session memory, teardown, and VAD execution.

Provider and network latency will dominate normal V1 interaction latency. Exact capacity values must be revalidated on target Linux server hardware, per section 39, before they are treated as production limits; the POC's own numbers were measured on a macOS laptop and are not to be quoted as production capacity without this revalidation.

Release SLOs must be fixed during Phase 0 and may be tightened after baseline measurement, but they may not be left undefined. The initial V1 bars are: successful authenticated WebSocket handshake p95 <= 500 ms within the deployment region; VAD speech-start system delay p99 <= 100 ms in addition to algorithmic windowing; final-transcript-to-first-LLM-text p95 <= 2.5 s; final-transcript-to-first-client-audio p95 <= 4.0 s; interruption from UserStartedSpeakingFrame to server output-flush acknowledgement p99 <= 200 ms; no old-generation audio accepted by TransportOutput after the interruption barrier; stale provider output delivered to a newer turn = 0; and session teardown p99 <= 2 s excluding explicitly reported leaked work. Provider cold starts and rate-limit backoff must be reported separately rather than hidden inside the steady-state distribution.

Capacity must be reported as the minimum of host runtime capacity, VAD executor capacity, configured provider concurrency/rate budgets, file-descriptor limits, and the tested WebSocket client limit. VAD-only POC capacity must never be advertised as end-to-end call capacity.

## 43. Documentation Requirements

V1 documentation should include: architecture, quick start, frames, processors, pipelines, sessions, custom processor example, Google provider, xAI provider, WebSocket transport and its supported production envelope, RTVI compatibility and pinned versions, server configuration, admission-token integration, audio formats and resampling, provider quotas and STT rotation, observability, Pipecat compatibility and differences, and a production deployment and operations runbook covering the container and systemd deployment paths, TLS setup, configuration profiles, administrative-surface protection, and how to interpret leaked-work and native-fault metrics from section 35 during an incident.

The quick start should enable a developer to run a real voice conversation without needing to understand runtime internals.

## 44. Examples

Provide executable examples independent of the server: a Google example (Silero VAD, Google STT, conversation context, Google LLM, Google TTS); an xAI-interchangeability example demonstrating the LLM swap from Scenario C in section 41; and a custom processor example demonstrating how a framework consumer adds a custom processor without changing the runtime.

Examples should serve as compile-time and public-API regression tests.

## 45. Suggested Milestones

These milestones define sequencing at the specification level only. The implementation plan should later break them into concrete tasks.

**M0 - Compatibility and Production Contract Freeze.** Pin the Go toolchain, module paths, RTVI client/transport/protocol/reference coordinates, default protobuf serializer schema, recorder/player rates, Google and xAI API surfaces and model/recognizer IDs, Google TTS voice/output contract, ONNX Runtime and Silero artifacts, WebSocket limits, one-time admission-token claims and transport, audio formats, selected pure-Go resampler, and section 42 SLOs. Before implementation proceeds, run a small proof against the exact released unmodified client showing Go-compatible protobuf exchange, tokenized connection, simultaneous microphone/input and paced output, and playback-buffer clear on interruption. Record all results in checked-in manifests and architecture decisions. Failure of playback clear triggers the explicit product decision in section 18.

**M1 - Core Runtime Extraction.** Stabilize Frame, Processor, Emitter, Direction, Pipeline, Session, Supervisor, ResourceBudget.

**M2 - Context and Interruption.** Implement bounded conversation context, user and assistant context processors including the partial-turn rule from section 17, turn/generation fencing, interruption barriers, flush, and the hardened cancellation semantics from section 18.

**M3 - WebSocket Transport.** Implement the complete section 20 contract: connection admission, lifecycle, audio validation and conversion, sequencing, pacing, bounded buffering and backpressure, interruption barriers, clean disconnect and teardown, and TLS.

**M4 - RTVI Compatibility.** Implement and test the compatibility set frozen in M0, including generated Go protobuf bindings and the required RTVI subset. Re-run the unmodified-client connect, wire-fixture, and buffered-playback-clear tests as regression gates before provider work is considered complete.

**M5 - Google Provider.** Implement Google STT, LLM, and the pinned TTS path, including STT stream rotation, retries, quota feedback, and generation fencing. Run the provider path with the deterministic VAD test processor; the first release-representative full pipeline occurs after M6 installs real VAD.

**M6 - VAD Production Executor.** Implement the worker-process VAD executor as the production default (section 15), and run Scenario F (section 41) confirming native-fault isolation before proceeding to config-driven server work. This directly follows up the POC's central finding and must not be deferred to the end of the project.

**M7 - xAI LLM Interchangeability.** Implement the xAI LLM module (section 25) and run Scenario C, proving provider interchangeability. xAI STT and TTS remain optional post-V1 work.

**M8 - Config-Driven Server.** Implement config, provider registry, pipeline factories, health and readiness, metrics, graceful shutdown, and admission control, including the development versus production configuration profiles from section 39.

**M9 - Production Deployment Packaging.** New versus the original draft: build and validate the Dockerfile and systemd unit from section 39, validate TLS termination end-to-end, and run Scenario A deployed through that path rather than via go run.

**M10 - Product-Level End-to-End Validation.** Using the exact pinned, released, unmodified client packages against the deployed production package from M9, verify connect, microphone audio, speech detection, transcription, model response, audio playback, multi-turn context, barge-in, provider error, disconnect, and concurrent sessions.

**M11 - V1 Stabilization.** Complete race tests, leak tests, Linux hardware testing and revalidation of performance numbers, documentation, examples, the compatibility matrix, and release packaging.

## 46. V1 Release Criteria

V1 is complete when all of the following are true:

- The exact pinned, released, unmodified Pipecat JavaScript client and WebSocket transport can connect over wss to the production-deployed server.
- The exact released RTVI client and WebSocket transport versions, default serializer/protobuf schema, recorder/player rates, protocol version, and Pipecat reference commit are pinned and compatibility-tested.
- WebSocket admission requires a valid short-lived single-use token; atomic replay prevention, Origin and per-principal limits work; token-bearing query strings are redacted; public clients cannot access administrative endpoints.
- Client microphone audio reaches the Go session runtime.
- Supported client sample rates are validated and resampled correctly; output audio is paced and all audio queues remain bounded under a slow client.
- Silero VAD produces speech boundaries using the production-default worker-process executor.
- Google STT, LLM, and TTS each work in a real session.
- An xAI LLM works as a drop-in substitute in an otherwise-Google pipeline with no core changes (Scenario C); a complete xAI STT/TTS stack is not required for release.
- Multi-turn context works, including correct partial-turn recording on interruption.
- Conversation context remains within configured message and token budgets during the long-call scenario.
- Barge-in interrupts assistant speech correctly, clears the pinned client's playback buffer, meets the interruption SLO, and rejects every late old-generation frame.
- A 30-minute real Google call crosses STT stream rotations without duplicate or missing user turns.
- One failed session does not break unrelated sessions, for both managed faults (Scenario E) and native or cgo-level faults under the production VAD executor (Scenario F).
- Admission control can reject new sessions.
- Health, readiness, and protected metrics endpoints exist, including the leaked-work metric from section 35, verified live during Scenario F, not merely present.
- Graceful shutdown works under both systemd and container SIGTERM delivery.
- The server can be configured without writing Go, and the development and production configuration profiles from section 39 both work as specified.
- The same pipeline can be constructed programmatically in Go.
- The pinned protobuf serializer and RTVI behavior are covered by automated wire-contract and integration tests.
- Network-impairment, slow-client, admission-security, oversized-input, provider-retry, and quota-aware rejection tests pass.
- Product latency, interruption, teardown, and end-to-end capacity results meet section 42 and are reported separately from VAD-only capacity.
- The system runs successfully on target Linux server hardware, deployed via the container and systemd paths from section 39, with TLS terminated correctly.
- Pipecat behavioral differences are documented.
- The production deployment and operations runbook (section 43) exists and has been followed end-to-end at least once by someone other than the implementer.

## 47. V1 Success Demo

This is the acceptance test for the entire specification, referenced from section 1.

The V1 product demonstration requires no custom client, and it runs against the server as it will actually be deployed, not merely as a locally built binary. It demonstrates a WebSocket client voice call; it does not demonstrate WebRTC, SIP, PSTN, or telephony.

Deploy the server using the production packaging from section 39, either the container image or the systemd unit, with TLS terminated per section 20.

Mint a short-lived admission token through the example trusted-backend integration. Connect using the exact pinned released RTVI client and WebSocket transport package, unmodified, over wss to that deployed server.

Speak a normal question. Verify the complete path: microphone, RTVI over wss, session, VAD on the production-default executor, STT, context, LLM, TTS, RTVI over wss, speaker.

Interrupt the assistant while it is speaking, and confirm the client playback buffer is flushed, the interruption SLO is met, no old-generation audio follows the barrier, and the new turn succeeds with context recorded correctly per section 17.

Ask a follow-up requiring context from the previous turn.

Repeat using the Google-only provider configuration, and again using the Google-STT/xAI-LLM/Google-TTS mixed configuration from Scenario C.

Keep the Google-only call open across an STT stream rotation and confirm the conversation continues without a duplicate or missing turn. Run one slow-client or delayed-network condition from Scenario I and confirm buffering remains bounded. Attempt one invalid admission token and confirm rejection occurs before provider allocation.

If these flows work using the same core runtime, the same pinned unmodified client, and a real production deployment, V1 has achieved its purpose: it is possible to start a WebSocket client voice call to the server.
