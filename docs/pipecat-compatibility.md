# Pipecat, RTVI, and Whisker Compatibility

**Status:** V2 compatibility contract
**Reference:** local `pipecat-impl/` checkout at Git HEAD `2967e1c09ad484076089bc88427d305fe8f79fa9`
**Pinned RTVI client:** `@pipecat-ai/client-js@1.13.1`, `@pipecat-ai/websocket-transport@1.7.2`
**Whisker target:** semantic observer/debug adapter; no official package or protocol version is pinned.

## Compatibility surfaces are independent

| Surface | What a pass proves | What a pass does not prove | V1/V2 evidence |
|---|---|---|---|
| RTVI client conformance | The pinned Pipecat client can handshake, exchange the V1/V2 client-facing wire messages, send/receive audio, and observe supported lifecycle events. | It does not prove Whisker debug semantics or all Pipecat audio/call behavior. | `node tools/compat/test_rtvi_client.js`, `node tools/compat/verify_fixtures.js`, and Go transport tests. |
| Whisker debug conformance | Semantic pipeline/debug events have the required names, order, fields, direction, correlation, and lifecycle boundaries for an adapter to render them. | It does not prove RTVI wire behavior or general Pipecat runtime parity. | A dedicated observer/debug adapter suite and reference scenarios; wire-level certification remains unclaimed until a package/protocol version is pinned. |
| Pipecat behavioral parity | Selected Go behaviors match the local Pipecat implementation/tests under the same scenarios, or an intentional difference is recorded. | It does not prove RTVI client compatibility or Whisker acceptance. | Focused paired behavior fixtures and Go tests against the pinned local checkout. |

The three results are reported separately at Gate F. Passing one suite never implies a pass in another.

## Pinned reference coordinates

| Coordinate | Frozen value | Evidence |
|---|---|---|
| Local Pipecat checkout | `pipecat-impl/`, Git HEAD `2967e1c09ad484076089bc88427d305fe8f79fa9` | `git -C pipecat-impl rev-parse HEAD` |
| JavaScript client | `@pipecat-ai/client-js@1.13.1` | `tools/compat/package.json`, `tools/compat/package-lock.json`, installed package metadata |
| WebSocket transport | `@pipecat-ai/websocket-transport@1.7.2` | `tools/compat/package.json`, `tools/compat/package-lock.json`, installed package metadata |
| RTVI envelope label | `rtvi-ai` | `transport/rtvi/rtvi.go` |
| RTVI protocol version | `2.1.0` (V1 accepts `2.1` as a compatibility spelling) | `transport/rtvi/rtvi.go`, compatibility tests |
| Protobuf source | `proto/frames.proto`, SHA-256 `adcd346af21673d3385d075c5a662a8407584b4b04f177320609596f4dea5c37` | `docs/compatibility/v1-lock.json` validator |
| Silero VAD model | `third_party/silero_vad.onnx`, SHA-256 `1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3` | `vad/silero.go`, `docs/compatibility/v1-lock.json` validator |

The lock file records Go module versions, runtime/audio coordinates, provider settings, admission/limit defaults, and target platform details for the V1 baseline. `validate_manifest.js` checks that required top-level values exist, rejects selected floating/empty strings while traversing object fields, verifies the `proto/frames.proto` and Silero model SHA-256 values, and compares the installed versions of the two packages in `client_packages`. It does not compare every other recorded value with source code or verify values nested in arrays; those remaining pins are inventory evidence, not validator-certified coordinates.

Remaining pin provenance: Go module paths and Go directives are in the five `go.mod` files; requested JavaScript package versions are in `tools/compat/package.json` and `tools/compat/package-lock.json`; observed Node/protoc versions are environment captures. RTVI label/version and accepted spelling are in `transport/rtvi/rtvi.go` and its tests; protobuf field meanings are in `proto/frames.proto`; audio formats and pacing defaults are in `transport/ws/wire.go`, `transport/ws/pacing.go`, and their tests; resampler implementation and benchmark are in `core/transport/resample/` and `core/transport/resample/resampler_test.go`; VAD model/runtime coordinates are in `vad/`, `third_party/silero_vad.onnx`, and `deploy/Dockerfile`. Provider defaults are in `config/development.yaml`, `config/production.yaml`, and `server/config.go`; admission and timeout limits are in `server/config.go`, `server/admission.go`, and `transport/ws/transport.go`; supported image/platform details are in `deploy/Dockerfile`, `deploy/security_scans.md`, and `docs/operations_runbook.md`.

## Current V1 behavior

- The protobuf `Frame` oneof has text field 1, audio field 2, transcription field 3, nested message field 4, and interruption field 5. The checked-in Go protocol and fixtures cover audio, nested RTVI messages, and malformed input.
- The server normalizes incoming audio to mono PCM16LE at 16 kHz and client playback to mono PCM16LE at 24 kHz. The official client quick start uses 16 kHz microphone capture and 24 kHz playback. Audio output is paced against media time; the default server-side residual queue is bounded to 100 ms / 4,800 bytes at 24 kHz PCM16 mono.
- The server emits V1 RTVI lifecycle, user speech, transcription, assistant text, bot speaking, error, and disconnect messages at the `rtvi-ai` label and `2.1.0` protocol version.
- V1 internally uses generation fencing and a paced output queue. This protects server-side work and queued frames from stale output, but it does not clear audio already buffered in the pinned browser player.
- The hard probe against the exact unmodified packages found that protobuf interruption field 5 is rejected as `Unknown frame kind`; the RTVI `user-started-speaking` message deserializes and calls the client callback, but neither `WebSocketTransport` nor `PipecatClient` calls the media player's interruption/clear method. The observed verdict is `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`.

### Playback-clear limitation and V2 gate

Do not claim full playback-clear compatibility for V1 or for the pinned unmodified client. Current server-side residual queued playback is bounded by pacing to 100 ms / 4,800 bytes. That bound does not describe audio already delivered to or buffered by the client. The V2 output-flush gate stays unresolved until a compatible client/API or another mechanism is demonstrated by a test that observes the actual playback buffer being cleared. A recorded product decision lets `make verify` proceed; it is not a passing playback-clear result.

## RTVI client conformance

Run the pinned client against the production WebSocket/RTVI path. Verify protocol negotiation, client-ready/bot-ready, valid binary audio in both directions, user-started/user-stopped-speaking, interim/final user transcription, bot text, bot speaking start/stop, server error, disconnect, malformed protobuf handling, audio normalization, and rejection of audio before client-ready. Validate fixtures with the installed serializer. Keep this suite at the transport boundary; do not count trace/debug adapter events as RTVI evidence.

Commands:

```sh
node tools/compat/test_rtvi_client.js
node tools/compat/verify_fixtures.js
go test -race -count=1 ./tools/compat/...
go test -race -count=1 ./transport/rtvi ./transport/ws
```

## Whisker semantic observer/debug conformance

Until an official Whisker package and protocol version are pinned, the V2 target is a semantic observer/debug adapter. The adapter translates `core.DebugEvent` values and does not add Whisker-specific concepts to core processors or the RTVI message vocabulary. Tests assert required semantics and event ordering, not a guessed byte-for-byte protocol. Pin an official package/protocol coordinate before any claim of wire compatibility.

Task 1 adds the protocol-neutral `core.DebugEvent` and immutable observer value contracts. `DebugEvent` directly carries pipeline/processor position, frame kind, direction, sequence, and tool call ID with session/turn/generation correlation. Namespaced structured metadata is restricted to copied numeric/boolean values. The Whisker adapter and its eleven conformance scenarios remain pending; the new core contract tests are not a Whisker conformance pass.

The conformance scenario set is:

1. Session and pipeline start: stable session ID, ordered start events, processor inventory/relationship.
2. Audio frame flow: input and output frame kind, direction, format metadata, processor position, sequence, and session correlation.
3. User speech start/stop: ordered state transitions with the same session and turn identity.
4. STT interim/final: interim and final event distinction, transcript policy, and turn correlation.
5. LLM output: request/start, text/output, completion, and provider-error ordering.
6. TTS lifecycle/audio: request/start, audio frames, first-audio/completion, and bot playback state.
7. Barge-in: interruption is visible before generation cancellation and stale output is not reported as current output.
8. Flush: queued/in-flight output flush start/completion and generation fence are visible; client-buffer clear is asserted only when a tested client mechanism supports it.
9. Tool call/result: requested, started, completed/failed, and result events share call, turn, and session correlation.
10. Provider/pipeline errors: safe error category, fatality, source, direction, and ordering without credentials or content leakage.
11. Session teardown: end/cancel/failure, processor shutdown, and resource completion are distinguishable and ordered.

For every scenario assert event/message type, required fields, sequence, direction where applicable, stable identifier/correlation, processor/pipeline relationship, and privacy-policy behavior. Run the same behavior against Pipecat reference scenarios where a matching behavior exists; document Go-specific internal events as intentional differences.

## Selected Pipecat behavioral parity

The table tracks every V2 P0/P1/P2 capability from `docs/v2-spec.md`. Status describes the V2 target as of this contract freeze. Acceptance evidence must be attached to the owning phase task; a planned test is not a passing result.

| Priority and capability | Status | Acceptance evidence |
|---|---|---|
| P0 — Input mute/unmute control | P0 gate | Core tests prove mute keeps the session alive, blocks VAD/STT by default, emits state, and unmute resumes input without reconnecting. |
| P0 — Audio filter/noise-suppression abstraction | implemented; re-review pending | Task 6 uses vendored BSD-licensed SpeexDSP 1.2.1 preprocessing, not a noise gate or model-weight substitute. Core tests cover explicit formats, deterministic mid-stream cancellation/reuse, a sub-frame state bound, reset isolation, honest one-frame/10 ms latency and finite-stream drain, and gain-normalized suppression on a voiced/noise fixture. That metric requires at least 3 dB SNR improvement and 0.5 wanted gain; bypass, fixed attenuation, silence, and a threshold gate fail it. Gate B remains open. |
| P0 — Audio resampling and normalization | implemented; re-review pending | Task 6 adds the SpeexDSP quality-5 band-limited streaming resampler plus deterministic PCM16/float and mono/stereo conversion. It admits only 8/16/24/48 kHz and bounds each call at 48,000 input and 288,064 allocated output samples. Tests cover exact finite-stream sample count, nonzero wanted amplitude, alias rejection, pathological rate/input rejection, cancellation, reset, and close. The existing V1 transport resampler is not silently replaced in this task. |
| P0 — Bot speaking state | current | Transport tests prove speaking transitions derive from output audio/playback lifecycle, balance on empty/error/interrupted output, and agree with RTVI and observer events. |
| P0 — Interruptible vs uninterruptible output | P0 gate | Tests show interruption drops interruptible output and preserves explicitly uninterruptible output in the defined order. |
| P0 — Output flush semantics | P0 gate | Queue/text/TTS/in-flight transport tests prove deterministic server flush ordering; a separate browser/player test is required before claiming client-buffer clearing. |
| P0 — Turn management controller | implementation; independent review pending, P0 gate open | `core/turn` tests cover caller-timestamped initial no-speech and inter-turn silence, debounced start/stop and minimum speech, continuous-idle stop-origin stability, maximum active duration, one-shot completion/interruption, mute/unmute, reset/close rejection, timestamp regression, old-turn retirement and bounded fail-closed capacity, higher-generation fencing/interruption, and hybrid semantic early completion with invalid/panicking/stale fallback. Injected detection is snapshot-based and invoked outside the state lock; reentrant/concurrent state changes cannot commit stale suggestions. The package emits typed `core.TurnEventFrame` values. No STT/context commit, provider VAD, transport, output flush, or session-termination integration is claimed; those remain in Tasks 8/9 and Gate B. |
| P0 — STT commit/finalization policy | P0 gate | Tests cover interim stability, delayed final, timeout, late final, duplicate suppression, and configured end-of-turn grace. |
| P0 — Turn-scoped audio buffer | implemented; re-review pending | Task 6 adds byte-bounded, in-memory-only pre-roll, rechunking, and opt-in turn capture keyed by `core.TurnID`, with reset, disabled retention, and caller-selected drop-oldest/reject-newest behavior. Rejection is observable and preserves prior bytes. Gate B remains open. |
| P0 — Behavioral eval harness | P0 gate | Machine-readable scenarios pass for barge-in, turn completion, silence/no-speech timeout, flush, transcript stabilization, tool interruption, pacing, teardown, RTVI, and Whisker. |
| P1 — Runtime service settings updates | P1 tracked | Tests prove supported changes apply atomically at control boundaries, increment revision, and reject invalid updates without partial mutation. |
| P1 — Context summarization/compaction hook | P1 tracked | Context tests prove compaction is provider-neutral, preserves required system/tool state, and leaves the session usable when an application hook declines. |
| P1 — Audio recording/capture processor | P1 tracked | Sink tests cover user/assistant/mixed/separate capture, opt-in behavior, bounded buffering, cancellation, and format metadata without storage in core. |
| P1 — Urgent control frames/priority lane | P1 tracked | Congestion tests prove cancel/interruption/flush/end/critical error pass queued media while preserving control ordering. |
| P1 — Processor failure/unusable policy | P1 tracked | Tests cover fail-session, disable, degrade, safe retry, error propagation, and side-effect safety for each supported policy. |
| P1 — Usage metrics | P1 tracked | Provider contract tests record available token/audio/character/request units, omit unavailable values, and correlate them to session/turn without billing logic. |
| P1 — Bot output/playback state observer | P1 tracked | Observer/eval tests distinguish generating, buffered, playing, drained, interrupted, and completed states with ordered events. |
| P1 — Serializer abstraction | P1 tracked | Round-trip/negative tests map semantic events and frames to selected serializers while preserving RTVI and debug boundaries. |
| P1 — Frame/pipeline observer API | P1 tracked | Tests prove immutable non-mutating observations, correct direction/position/timing, order, and observer-error isolation. |
| P2 — DTMF/tone frames | P2 tracked | Core tests validate transport-neutral tone/DTMF values; transport adapters do not imply PSTN support. |
| P2 — Audio mixer | P2 tracked | Tests prove bounded multi-source mix, deterministic format/timing, and interruption policy. |
| P2 — Background audio/earcon processor | P2 tracked | Tests cover injection, start/stop, pacing, output classification, and interruption behavior. |
| P2 — Audio source abstraction | P2 tracked | Tests cover generated/file/application streams through shared output format, pacing, buffering, and cancellation semantics. |
| P2 — Advanced audio filter chain | P2 tracked | Composed-filter tests prove declared format transitions, bounded copies/state, order, and cancellation. |
| P2 — Runtime pipeline settings introspection | P2 tracked | Read-only snapshots expose current supported settings/state without mutable internals or content-policy leaks. |
| P2 — Processor capability metadata | P2 tracked | Metadata tests validate optional audio, interruption, runtime-update, async, and stateful declarations and safe absence for older processors. |
| P2 — Provider/processor readiness events | P2 tracked | State-transition tests cover ready/degraded/unavailable/recovered and prove optional components do not block startup by default. |
| P2 — Turn recording/replay hooks | P2 tracked | Replay-hook tests preserve turn/event ordering, obey consent/privacy policy, and delegate storage to the application. |
| P2 — Extended behavioral parity cases | P2 tracked | Eval scenarios cover false speech start, repeated barge-in, late STT, reconnect, TTS/tool completion races, mute update, congestion, overflow, and degradation. |

## Intentional differences and deferred scope

| Topic | Status | Contract |
|---|---|---|
| Go framework API and internal execution | intentional difference | Behavioral and semantic compatibility is the objective. Go uses native contexts, goroutines, and its own public types; Python class names and internal implementation are not API requirements. |
| Audio DSP dependency | intentional difference | Go uses a minimal CGO adapter over vendored SpeexDSP 1.2.1 sources for reproducible preprocessing and resampling. It does not import Pipecat provider classes, install a system DSP package, or bundle unresolved RNNoise model weights. The supported release image already enables CGO. |
| Whisker wire protocol | P0 gate | Semantic adapter/conformance is required, but a byte-level wire claim waits for an official protocol/package version to be pinned. |
| Playback clear in the pinned V1 client | intentional difference | The probe records a failure for both tested wire events. V2 must satisfy its output-flush gate through a compatible client/API or another playback-buffer mechanism with direct test evidence. |
| PSTN, SIP, Twilio, and call-center behavior | deferred beyond V2 | Telephony remains out of scope; generic tones/DTMF do not imply telephony support. |

## Evidence commands and release interpretation

Run the focused suites first, then the complete `make verify` gate. Because the repository contains nested Go modules, root `go test ./...` alone is not full verification; `make verify` runs root, `core`, `providers/google`, `providers/xai`, and `server`, plus formatting, pinned-manifest, fixture, and playback-decision checks. Live provider tests require credentials and are reported separately when unavailable.

Gate F passes only when RTVI conformance, Whisker semantic conformance, and P0 behavior parity are each reported with their own evidence and intentional differences are listed here. A recorded playback decision is an accepted product ruling for the current V1 probe, not evidence that a client cleared its buffer.
