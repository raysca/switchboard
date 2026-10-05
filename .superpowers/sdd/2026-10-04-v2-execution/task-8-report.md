# Task 8 implementation report

Status: review-round-1 fixes prepared for independent re-review; Task 8 and Gate B remain open.

Baseline: `451c103` on `codex/v2-implementation`.

## RED

Before production changes, the following focused runs failed:

```text
cd core && go test -run 'TestSpeakingTracker' ./...
go test -run 'Test(PacedWriter|RTVISpeaking)' ./transport/ws
cd providers/google && go test -run TestTTS_HeaderlessPCM20msChunking ./...
cd providers/xai && go test -tags xaispeech -run TestXAITTS_Success ./...
```

The compiler reported missing `NewSpeakingTracker`, speaking states/reasons,
`OnSpeakingState`, `OutputAudioFrame.TurnFinal`, and provider propagation. Full
outputs were captured in `/tmp/task8-core-red.log`, `/tmp/task8-ws-red.log`,
`/tmp/task8-google-red.log`, and `/tmp/task8-xai-red.log` during implementation.

The first integration GREEN attempt exposed a real whole-turn boundary race:
punctuation-aligned audio could already be synthesizing before the final LLM
frame. The narrower regressions then failed for missing
`OutputTurnFinalFrame` and `PacedWriter.MarkTurnFinal`; outputs were captured in
`/tmp/task8-marker-core-red.log` and `/tmp/task8-marker-ws-red.log`. A proposed
“hold the trailing sentence” approach was rejected after it reproduced an
existing streaming-latency/context test regression.

## GREEN

- `go test -race -count=20 -run 'Test(PacedWriter|RTVISpeaking)' ./transport/ws ./transport/rtvi`
- `cd core && SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race -count=1 ./...`
- `go test -race -count=1 ./transport/rtvi ./transport/ws`
- `cd providers/google && go test -race -count=1 ./...`
- `cd providers/xai && go test -race -tags xaispeech -count=1 ./...`
- `go vet ./...` in core and Google; `go vet ./transport/rtvi ./transport/ws` at root; `go vet -tags xaispeech ./...` in xAI.

The focused regressions cover normalized queue byte/duration limits and
rollback, caller-buffer ownership, pacing, empty/error/final behavior,
multi-unit no-flicker state, typed RTVI agreement, old-generation
uninterruptible preservation, in-flight flush barriers, late completion
fencing, repeated/overlapping acknowledgement identity, unresponsive writers,
callback re-entry/panic isolation, close/cancel wakeup through the full suites,
and new-generation recovery. Existing core lifecycle tests retain ordered
text/TTS/audio generation fencing and provider interruption cancellation.

## Decisions and assumptions

- `IsFinal` remains synthesis-unit finality; `TurnFinal` is whole-turn
  finality. An explicit ordered `OutputTurnFinalFrame` covers the case where
  LLM completion follows an already-streamed last unit. Pending markers are
  capped at 64 and keyed by turn, generation, and unit.
- Speaking starts after a successful non-empty socket write and stops after
  paced whole-turn drain, flush, fatal write failure, or close. RTVI receives
  this exact typed transition. It does not prove remote playback.
- Flush removes only interruptible queued/in-flight work through its target,
  waits for in-flight quiescence, and fences late write success. Explicitly
  uninterruptible old-generation media remains ordered ahead of later output.
- Queue entries copy samples and store one normalized duration used for both
  admission and removal. Overflow wraps both the explicit queue-limit error
  and the V1-compatible slow-client error.
- Callbacks are invoked without pacing/state locks, serialized through a
  re-entry-safe event drain, and panic-isolated.

## Playback limitation

`node tools/compat/probe_playback_clear.js` again returned the recorded verdict
`UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`. The product
decision permits verification to continue but is not a playback-clear pass.
The server guarantee excludes bytes already written to or buffered by the
client.

## Review round 1

The independent reviewer required changes and the tester failed the initial
implementation. Before the fixes, focused REDs reproduced the gaps:

```text
go test -run 'TestPacedWriter(FinalMarker|LateFinal|FlushWins|Synchronous|Noop|Cancellation|Defaults|RealWebSocket)' ./transport/ws
cd providers/xai && go test -tags xaispeech -run TestXAITTS_MultiUnitAudibleThenEmptyFinal ./...
```

The WebSocket suite failed to compile for the absent frame-count bound,
pending-marker observability, and deterministic write-admission hooks. The xAI
regression timed out because a silent final unit emitted no final frame. Outputs
were captured in `/tmp/task8-r1-ws-red.log` and
`/tmp/task8-r1-xai-red.log`. A narrower speaking-tracker regression also failed
to compile until exact output-identity stop support was added.

The fixes establish these additional invariants:

- A whole-turn marker reconciles the matching final unit before admission,
  while queued, selected, writing, or after successful completion. Admission
  overflow retains the marker for retry, and duplicate/stale markers are
  bounded no-ops.
- The final generation/drop/closed validation and transport-write registration
  share one pacer critical section. A deterministic barrier proves flush wins
  the pre-admission race without calling the writer.
- Selected/pacing and transport-bound states are distinct. Callback-visible
  quiescence is cleared before synchronous speaking/completion callbacks, so a
  callback may flush without self-waiting and a start callback can suppress
  later old-generation work.
- Flush preserves the media clock when it does not change a selected/pacing
  schedule. Cancellation and close clear queue accounting and balance speaking
  once. A 64-frame default bounds zero-byte frames; non-positive limits receive
  bounded defaults.
- Real coder/websocket writes are non-abortable by default: flush waits for an
  already-started write to return because canceling its context closes the
  connection. An opt-in remains for explicitly recoverable writers. The real
  `httptest` WebSocket regression proves wait-then-ack and same-connection
  new-generation recovery; timeout never reports success.
- xAI emits an empty final frame for a silent final unit, matching Google's
  explicit whole-turn-final propagation and preventing a multi-unit turn from
  remaining stuck speaking.

Review-round verification:

```text
go test -race -count=20 -run 'TestPacedWriter(FinalMarker|LateFinal|FlushWins|Synchronous|Noop|Cancellation|Defaults)' ./transport/ws
go test -race -count=1 -run TestPacedWriterRealWebSocketWaitsForStartedWriteAndRecovers ./transport/ws
cd core && SDKROOT=.../MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race -count=1 ./... && go vet ./...
go test -race -count=1 ./transport/rtvi ./transport/ws && go vet ./transport/rtvi ./transport/ws
cd providers/google && go test -race -count=1 ./... && go vet ./...
cd providers/xai && go test -race -tags xaispeech -count=1 ./... && go vet -tags xaispeech ./...
```

All passed. The playback probe was rerun and again returned
`UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`; continuation
under the recorded decision is not evidence that client playback was cleared.
