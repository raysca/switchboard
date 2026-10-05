# Task 8 implementation report

Status: implementation prepared for independent review; Task 8 and Gate B remain open.

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
