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
- The server emits V1 RTVI lifecycle, user speech, transcription, assistant text, bot speaking, error, and disconnect messages at the `rtvi-ai` label and `2.1.0` protocol version. Assistant text is bracketed per reply by `bot-llm-started`/`bot-llm-stopped` (a reply cut off by an interruption gets no stop; the next reply still opens with a start), and each TTS synthesis unit by `bot-tts-started`/`bot-tts-stopped`. `bot-tts-text`, `bot-interrupted`, function-call and `vad-*` messages are not emitted.
- V1 internally uses generation fencing and a paced output queue. This protects server-side work and queued frames from stale output, but it does not clear audio already buffered in the pinned browser player.
- Admission rejects (go-voice extension, not Pipecat) arrive after `client-ready` as a fatal RTVI `error` whose text is a stable reason code, then close 1013 (try again later): `capacity_exceeded` (global cap or no VAD capacity), `principal_limit`, `agent_limit`, `rate_limited` (per-principal start rate). Retry later or on another instance. `session_setup_failed` + 1011 now means a genuine setup failure (provider/pipeline build). `server_draining` + 1001 is unchanged. Tests: `server/admission_reject_test.go`.

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

Task 1 adds the protocol-neutral `core.DebugEvent` and immutable observer value contracts. `DebugEvent` directly carries pipeline/processor position, frame kind, direction, sequence, and tool call ID with session/turn/generation correlation. Namespaced structured metadata is restricted to copied numeric/boolean values. The Task 14 semantic adapter deep-copies nested metadata and rejects malformed identifiers, processor indexes, and directions. Its eleven cases use live session/probe processing or the actual paced writer, then adapt observed transitions to `DebugEvent` values. Interruption and transport flush acknowledgement come from the assistant/transport processors; queued and in-flight flush completion comes from the paced writer. Tool start and failure come from live executor outcomes. This validates the narrow adapter; a general runtime observer subscription/delivery API belongs to Task 35.

The teardown probe implements `core.Closer` and records `processor.shutdown` only from its actual `Close` callback. Its session-supervised worker waits for cancellation and the close callback, then records `session.resources.completed` as it exits. The scenario requires both observations before session `Done` and zero leaked tasks. Suppressing the close observation fails the negative regression. This does not add a general processor shutdown event to core; Task 35 owns general observer delivery.

Task 35 is delivered by `core/observer.go` (plan task P0-1). `Session.Subscribe(buffer, fn)` runs each observer on its own session-supervised task with a bounded queue. `Session.EmitDebug` never blocks: a full queue drops the event for that observer only, and the drop is counted (`Subscription.Dropped`, `Session.DebugDropped`). An observer panic is recovered and never fails the session. Cancel is idempotent, and teardown leaves no tasks behind unless an observer blocks forever, which Close reports as a leaked task.

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

## P0 eval harness and independent suite results

`go run ./tools/eval/cmd/govoice-eval` emits one JSON object with a result for
each deterministic P0 scenario. `duration_ns` is the scenario's declared
logical duration, so repeated runs have stable machine-readable output;
`steps` records the checked transitions and `diagnostics` contains safe failure
details. Any failed scenario produces a nonzero process exit. The runner uses
framework turn/transcript/tool/pacing/session APIs and does not request live
provider credentials.

Compatibility remains three separately verifiable suites:

| Suite | Command/evidence | Result interpretation |
|---|---|---|
| Pinned RTVI client | Go loopback integration runs `node tools/compat/test_rtvi_client.js`; `node tools/compat/verify_fixtures.js`; `npm test --prefix tools/compat` | The exact pinned client/transport exchange and fixture decoding pass. The client integration checks interim/final transcript payload and order, a nonfatal server error, user speech, assistant text/speaking, audio, handshake, and disconnect. Error-callback tests reject unexpected, fatal, duplicate, and late callbacks. |
| Whisker semantic | `go test -race ./tools/compat/whisker` and eleven `whisker_*` eval results | The semantic adapter preserves observed event identity, ordering fields, correlation, and copied safe attributes. No wire protocol or general observer-delivery claim is made. |
| Pinned Pipecat paired behavior | `go test -race ./tools/compat -run TestPinnedPipecatTurnInterruptionPairedBehavior` (turn events) and `go test -race ./tools/compat -run TestParity` (56 paired scenarios, below) against `pipecat-impl/` | The fixture validators check the exact checkout HEAD and cited modules. Turn events: equivalent Go frames pass through a live session and the production turn manager; turn two ends interrupted after EndFrame. Paired scenarios: the same scripted input runs through the Pipecat component and go-voice; equal output, or a declared `intentional_difference`. Neither implies complete Pipecat parity. |

The JSON runner labels the official RTVI and Pipecat suites as `separate_suite`
because those require their own package/reference test runs; it does not turn
an unexecuted suite into a pass. The Whisker status is computed from the eleven
runtime semantic scenario results; any failed case makes eval exit nonzero. The playback-clear probe remains
`UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`; it is reported
as unsupported and never counted as a pass or a client clear.

The RTVI `DebugEvent` encoder maps the admitted `error.category` metadata to a
bounded safe category. `session.error` is fatal and `provider.error` is
nonfatal; arbitrary category text is emitted as `internal_error`. The wire
error payload omits the original category and every other source attribute.
The encoder
rejects malformed identifiers, processor indexes, and directions. The RTVI `DebugEvent` encoder rejects transcript/text events when their
content is absent under Task 1's default metadata privacy policy. Callers that
have explicitly authorized content use the existing typed
`Handler.FormatUserTranscription` or `Handler.FormatBotLLMText` path. This
prevents the compatibility adapter from fabricating empty content or widening
debug-event privacy policy.

## Pipecat behavioral parity: matrix and paired fixtures

The per-feature matrix (Pipecat module, go-voice package and config key, status, whether it is on by default, differences, evidence) is `docs/pipecat-parity-matrix.md`; it also lists known gaps and risks. This is the third surface only: nothing here is evidence for RTVI client or Whisker conformance, and those suites do not count toward it.

Paired fixtures live in `tools/compat/testdata/parity/*.json` (committed, about 40 KB). `tools/compat/parity/gen_goldens.py` runs the pinned `pipecat-impl` components on scripted inputs (asyncio on a virtual clock: no network, audio hardware or wall clock) and writes the goldens, including each declared `intentional_difference` with its reason and the go-voice output. `tools/compat/parity_fixtures_test.go` runs the same inputs through go-voice and needs no Python.

| Area | Scenarios | Pipecat component | go-voice |
|---|---|---|---|
| `turn` | 14 | user start strategies (min words, transcription, wake phrase), speech-timeout stop, `UserTurnController` | `core/turn` Manager |
| `idle` | 6 | `UserIdleController` | `core.IdleController` on a fake clock |
| `mute` | 6 | four user mute strategies | `core/turn` Processor mute |
| `dtmf` | 6 | `DTMFAggregator` | `core/audio/dtmf` Aggregator |
| `text` | 12 | `SimpleTextAggregator` | `core.AssistantContextProcessor` synthesis units |
| `assistant` | 6 | `LLMAssistantAggregator` on interruption | `core.HeardText` via the assistant-context stage |
| `summary` | 5 | `get_messages_to_summarize` | `core.ContextSummarizer` |
| `constants` | 1 | turn-completion markers | `core` markers |

```sh
go test -race -count=1 ./tools/compat/...   # compare go-voice with the committed goldens (also part of make test)
make parity-regen                           # re-execute Pipecat from pipecat-impl/.venv, rewrite the goldens (needs: cd pipecat-impl && uv sync)
```

Regeneration is deterministic (a second run is byte-identical). A fixture without a declared difference must match Pipecat exactly; a declared difference must still hold and must differ from Pipecat, so a stale one fails. `TestParityMatrix` also fails when the matrix cites a missing path, test or fixture, or leaves a declared difference unexplained. Only the turn-event fixture above pins `tests/test_turn_tracking_observer.py`; the new goldens pin the checkout revision and each cited module. A paired fixture drives one component with scripted frames; it is not evidence that the feature is enabled in a live call (most turn-taking features need `turn.manager: true`).

## Selected V2 capability status

The table tracks every V2 P0/P1/P2 capability from `docs/v2-spec.md`. Status describes the V2 target as of this contract freeze; rows marked "rebuilt" were deleted then and re-added by the Pipecat parity plan (current state in `docs/pipecat-parity-matrix.md`). Acceptance evidence must be attached to the owning phase task; a planned test is not a passing result.

| Priority and capability | Status | Acceptance evidence |
|---|---|---|
| P0 — Input mute/unmute control | Gate B passed | Core race tests prove mute keeps the session alive, blocks VAD/STT by default, emits state, and unmute resumes input without reconnecting. |
| P0 — Audio filter/noise-suppression abstraction | rebuilt (P4-2) | `core/audio/filter`, off by default. |
| P0 — Audio resampling and normalization | transport only | `core/transport/resample` normalizes input to 16 kHz mono; no general processor. |
| P0 — Bot speaking state | Gate B passed | Task 8 starts typed speaking state only after a successful non-empty paced write and stops once on whole-turn drain, flush, fatal write failure, or close. A late whole-turn marker reconciles already-completed output exactly once. Empty/error/stale output and per-unit TTS boundaries do not flicker state. The same transition maps to RTVI; it is not client playback acknowledgement. |
| P0 — Interruptible vs uninterruptible output | Gate B passed | Task 8 tests show flush drops queued/selected interruptible output through the target generation while old-generation explicitly uninterruptible media survives in relative order and new-generation output recovers. A transport-bound real WebSocket write finishes before acknowledgement because canceling it would close the connection. |
| P0 — Output flush semantics | Gate B passed; client clear unsupported | Task 8 bounds/copies the paced queue by frame/byte/duration, atomically fences write admission, preserves uninterruptible final drains, and schedules late finality at recorded media time. The real-WebSocket integration proves barrier waiting plus same-connection reuse. The pinned browser probe still cannot clear already-buffered playback, so no client-buffer claim is made; Gate B permits this documented already-delivered-audio exception. |
| P0 — Turn management controller | Gate B passed | `core/turn` tests cover caller-timestamped initial no-speech and inter-turn silence, debounced start/stop and minimum speech, maximum active duration, one-shot completion/interruption, mute/unmute, reset/close rejection, timestamp regression, bounded retirement, and higher-generation fencing/interruption. The independent Gate B tester reran focused turn and lifecycle suites under the race detector. |
| P0 — STT commit/finalization policy | Gate B passed | `core/transcript` separates raw candidates from marked commits, orders policy by an injected monotonic receipt clock, and has one session-owned deadline driver. Google and xAI preserve immutable turn ownership, bounded invalidation, multi-result completeness, rotation/drain lifecycle, and exactly-once cleanup. Provider-local wall-clock/text dedupe is absent, V2 UserContext suppresses committed-frame replay by turn/generation, and the independent Gate B tester passed focused transcript and mocked multi-turn provider integrations under the race detector. |
| P0 — Turn-scoped audio buffer | removed | Deleted from core: it had no production users. |
| P0 — Behavioral eval harness | P0 gate | Machine-readable scenarios pass for barge-in, turn completion, silence/no-speech timeout, flush, transcript stabilization, tool interruption, pacing, teardown, RTVI, and Whisker. |
| P1 — Runtime service settings updates | removed | Deleted from core: it had no production users. |
| P1 — Context summarization/compaction hook | rebuilt (P3-4) | `core/context_summary.go`, per-agent opt-in. |
| P1 — Audio recording/capture processor | rebuilt (P4-4) | `core/record`, server and agent opt-in. |
| P1 — Urgent control frames/priority lane | P1 tracked | Congestion tests prove cancel/interruption/flush/end/critical error pass queued media while preserving control ordering. |
| P1 — Processor failure/unusable policy | removed | Deleted from core: it had no production users. |
| P1 — Usage metrics | P1 tracked | Provider contract tests record available token/audio/character/request units, omit unavailable values, and correlate them to session/turn without billing logic. |
| P1 — Bot output/playback state observer | removed | Deleted from core: it had no production users. |
| P1 — Serializer abstraction | removed | Deleted from core: it had no production users. |
| P1 — Frame/pipeline observer API | rebuilt (P0-1) | `Session.Subscribe` on debug events, not frames. |
| P2 — DTMF/tone frames | implemented (P4-1) | `core/audio/dtmf`: Goertzel detector, RTVI `dtmf` input, `DTMFAggregator`-style user turns, `send_dtmf` tool; off by default (`dtmf:` config). Tones are generated in-process rather than sent as transport DTMF events. |
| P2 — Audio mixer | rebuilt (P4-3) | `core/audio/mixer`, off by default. |
| P2 — Background audio/earcon processor | rebuilt (P4-3) | Looping ambience only; no earcons. |
| P2 — Audio source abstraction | removed | Deleted from core: it had no production users. |
| P2 — Advanced audio filter chain | rebuilt (P4-2) | Ordered `audio.input_filters`; one filter (denoise). |
| P2 — Runtime pipeline settings introspection | removed | Deleted from core: it had no production users. |
| P2 — Processor capability metadata | removed | Deleted from core: it had no production users. |
| P2 — Provider/processor readiness events | removed | Deleted from core: it had no production users. |
| P2 — Turn recording/replay hooks | removed | Deleted from core: it had no production users. |
| P2 — Extended behavioral parity cases | P2 tracked | Eval scenarios cover false speech start, repeated barge-in, late STT, reconnect, TTS/tool completion races, mute update, congestion, overflow, and degradation. |

## Intentional differences and deferred scope

| Topic | Status | Contract |
|---|---|---|
| Go framework API and internal execution | intentional difference | Behavioral and semantic compatibility is the objective. Go uses native contexts, goroutines, and its own public types; Python class names and internal implementation are not API requirements. |
| Audio DSP dependency | removed | Deleted from core: it had no production users. |
| Whisker wire protocol | P0 gate | Semantic adapter/conformance is required, but a byte-level wire claim waits for an official protocol/package version to be pinned. |
| Playback clear in the pinned V1 client | intentional difference | The probe records a failure for both tested wire events. V2 must satisfy its output-flush gate through a compatible client/API or another playback-buffer mechanism with direct test evidence. |
| PSTN, SIP, Twilio, and call-center behavior | deferred beyond V2 | Telephony remains out of scope; generic tones/DTMF do not imply telephony support. |

## Evidence commands and release interpretation

Run the focused suites first, then the complete `make verify` gate. Because the repository contains nested Go modules, root `go test ./...` alone is not full verification; `make verify` runs root, `core`, `providers/google`, `providers/xai`, and `server`, plus formatting, pinned-manifest, fixture, and playback-decision checks. Live provider tests require credentials and are reported separately when unavailable.

Gate F passes only when RTVI conformance, Whisker semantic conformance, and P0 behavior parity are each reported with their own evidence and intentional differences are listed here. A recorded playback decision is an accepted product ruling for the current V1 probe, not evidence that a client cleared its buffer.
