# ADR 0001: V1 Production Contract Freeze

- **Status:** Accepted
- **Date:** 2026-09-27
- **Authoritative References:**
  - `docs/Voice Framework V1 Specification (Revised).md`
  - `docs/Voice Framework V1 — Implementation Plan.md`
  - `docs/Voice Framework V1 — Tech Stack & Dependencies.md`
  - `docs/compatibility/v1-lock.json`

## 1. Context and Problem Statement

Voice Framework V1 establishes the first production-track, deployable Go voice server communicating with the unmodified, released `@pipecat-ai/client-js` and `@pipecat-ai/websocket-transport` packages over WebSocket/WSS.

Before product implementation begins, all fast-changing library coordinates, wire formats, acoustic baselines, security rules, retry/circuit policies, shutdown lifecycles, and feasibility questions must be settled in an immutable production contract.

## 2. Decision 1: Supported WebSocket Envelope and Acoustic Baseline

1. **Transport Scope:** WebSocket/WSS is the sole client transport for V1. WebRTC, SIP, PSTN, and session resumption are explicitly out of scope.
2. **Network Envelope:** Full-duplex TCP network with stable connection. Head-of-line blocking is bounded by small audio frames (20 ms nominal; 10–40 ms supported) and media-clock pacing. V1 does not claim RTP loss concealment, ICE restart, or network handover.
3. **Reference Hardware & Acoustic Baseline:**
   - Client: Chrome 128+ on macOS / Linux desktop.
   - Acoustic Setup: Headset with hardware/OS noise suppression.
   - Speakerphone operation is supported only when host OS or browser AEC prevents acoustic feedback from triggering self-barge-in.

## 3. Decision 2: Protobuf Schema and Wire Serialization

1. **Schema Pinning:** The protobuf schema is pinned to `proto/frames.proto`, sourced from Pipecat reference commit `2967e1c09ad484076089bc88427d305fe8f79fa9` with SHA-256 `adcd346af21673d3385d075c5a662a8407584b4b04f177320609596f4dea5c37`.
2. **Frame Structure:** Exactly one protobuf binary `Frame` per WebSocket binary message.
3. **Frame Variants:**
   - Audio input and output use field 2 (`audio`: `AudioRawFrame`) with raw PCM16LE mono samples, sample rate, and channel count.
   - Protocol control and RTVI use field 4 (`message`: `MessageFrame`) containing stringified RTVI JSON with label `"rtvi-ai"`.
4. **Wire Compatibility Finding:** In the released `@pipecat-ai/websocket-transport@1.7.2` package, `ProtobufFrameSerializer` only recognizes fields 1 (`text`), 2 (`audio`), 3 (`transcription`), and 4 (`message`). Field 5 (`interruption`) was omitted from the client gencode; sending field 5 results in an `Unknown frame kind` error in the client deserializer. Therefore, all server-to-client control frames must use field 4 (`message`) with RTVI JSON.

## 4. Decision 3: Interruption, Barge-in, and Playback Clearing

1. **Standard Event Available to Server:** The Go server emits RTVI `user-started-speaking` (`type: "user-started-speaking"`) and `bot-stopped-speaking` inside protobuf field 4.
2. **Hard Playback-Clear Probe Outcome:**
   - The unmodified `@pipecat-ai/websocket-transport@1.7.2` package's `WebSocketTransport` does not wire incoming RTVI `user-started-speaking` messages to its internal `_mediaManager.userStartedSpeaking()` method.
   - Furthermore, `PipecatClient.handleMessage` notifies only the user callback `callbacks.onUserStartedSpeaking` and does not call any transport interruption method.
3. **Closed Product Decision:**
   - V1 enforces a **server-side interruption barrier with media-clock pacing backpressure**.
   - Because the Go server paces output in strict 20 ms chunks against the media clock, the client never buffers more than 40–100 ms of audio.
   - On interruption, the server immediately drops queued old-generation audio, cancels active TTS synthesis and LLM generation, fences all late provider completions, and emits the server-local flush barrier.
   - Client playback naturally ceases within the ~50 ms buffer window without requiring client monkey-patching or proprietary frame serializers.
   - Delivered context follows Section 17: only complete synthesis units whose final audio frame was written to the socket before the interruption barrier are recorded. Partially written units are excluded in full.

## 5. Decision 4: Audio Sampling and Stateful Pure-Go Resampler

1. **Input Rates:** Server accepts 16000 Hz, 24000 Hz, and 48000 Hz PCM16LE mono from the client recorder.
2. **Internal Rate:** 16000 Hz mono PCM16LE is used uniformly for VAD and STT.
3. **Player Rate:** Fixed at 24000 Hz (matching `@pipecat-ai/websocket-transport` `PLAYER_SAMPLE_RATE = 24000`). Player sample rate is not negotiated dynamically; non-24kHz playback configuration is documented unsupported.
4. **Resampler Selection:** Pure-Go stateful streaming resampler (`core/transport/resample`). Zero cgo, thread-safe, bounded memory, stateful phase and sample preservation across 20 ms chunk boundaries, < 1 ms latency, and 0 data races.

## 6. Decision 5: Admission Security and Token Protocol

1. **Token Transport:** Single-use admission tokens passed via `?token=` query parameter, which `@pipecat-ai/websocket-transport` natively supports.
2. **Signature & Claims:** Signed HS256 tokens requiring `iss: govoice-v1`, `aud: govoice-v1-client`, `sub`, `iat`, `exp`, `jti`, `kid`.
3. **Lifetime & Skew:** Maximum 300 seconds lifetime; maximum 5 seconds clock skew.
4. **Replay & Restart Rule:** Atomic consumption into an in-memory replay cache (retained through token expiry). On server restart, tokens with `iat` older than process start time minus 5s skew allowance are rejected.
5. **Redaction:** All reverse-proxy, server, error, and trace logs must redact the `token` query parameter.
6. **Network Surface Isolation:** Public listener exposes only `/ws`, `/health`, and `/ready`. The `/metrics` and event timeline endpoints are restricted to the administrative listener.

## 7. Decision 6: Provider Allocations and Limits

1. **Google STT:** Cloud Speech-to-Text V2 (`cloud.google.com/go/speech/apiv2`) with model `chirp_2`. Maximum request size 25600 bytes. Stream rotation threshold 240 seconds with 500 ms audio overlap. Quota headroom multiplier 2.0 reserved during rotation.
2. **Google TTS:** Cloud Text-to-Speech V1 (`cloud.google.com/go/texttospeech/apiv1`) using synchronous `SynthesizeSpeech` with `AudioEncoding_PCM` (headerless 16-bit PCM, 24000 Hz). Voice `en-US-Journey-F`. Synthesis units bounded to maximum 250 characters.
3. **Google LLM:** `google.golang.org/genai` with immutable model ID `gemini-2.5-flash`.
4. **xAI LLM:** REST/SSE Responses API (`https://api.x.ai/v1/responses`) with immutable model `grok-2-1212`. `store: false` sent on every request. Local context bounded and resent per turn.
5. **VAD:** Silero VAD v5 (`silero_vad.onnx`, SHA-256 `1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3`) using `onnxruntime-osx-arm64-1.29.0` locally and `1.29.0` Linux binaries. Production default is the local worker-process executor (`cmd/vadworker`) to ensure native cgo crash isolation.

## 8. Decision 7: Retry and Circuit Breaker Policy

1. **Operation Error Classification:**
   - **Retryable Errors:** Transient network connection drops, TCP resets, HTTP 502/503/504 errors, and DNS lookup timeouts.
   - **Rate Limit Errors (429):** Honor provider `Retry-After` header value up to the turn deadline.
   - **Non-Retryable Errors:** HTTP 400 (Bad Request), HTTP 401/403 (Authentication/Permissions), malformed JSON/Protobuf payloads, and invalid parameters. These fail immediately without retry.
2. **Backoff and Jitter:**
   - Capped exponential backoff with full jitter: initial backoff 100 ms, maximum backoff 2000 ms, multiplier 2.0.
   - All retries must fit strictly within the per-turn deadline.
3. **No Retries After Committed User-Visible Output:**
   - Any operation that could result in duplicate audible or visible output (such as TTS synthesis units where prior units were committed, or LLM generation streams) must **never** be retried unless the provider offers strict idempotency keys.
4. **Circuit Breaker Thresholds & Readiness Feedback:**
   - Failure Threshold: 5 consecutive failures opens the circuit for that provider capability.
   - Cool-down Period: 30 seconds before transitioning to HALF-OPEN to allow a single probe request.
   - Readiness & Admission: Open circuit status is exposed on `/ready` and `/metrics`. The server marks itself unready only when no configured pipeline can accept new voice calls.

## 9. Decision 8: Server Lifecycle and Shutdown Policy

1. **Drain-then-Cancel:**
   - On SIGTERM / shutdown: mark server unready on `/ready`.
   - Cease admitting new HTTP upgrades.
   - Drain active calls up to the configured shutdown deadline (default 30s).
   - Once deadline expires, send graceful close frames, cancel remaining session contexts, wait for supervisor teardown (2s budget), terminate VAD child processes, and exit with status 0.
   - Cleanup failure or forced timeout returns status 1.
2. **Immediate Cancel:** Configurable mode for tests and emergency maintenance.

## 10. Consequences and Verification Gate

All items in this production contract are frozen. No Phase 1 product code may deviate from these specifications. All Phase 0 verification checks must pass against these exact parameters.
