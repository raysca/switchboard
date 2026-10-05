# Task 14 correction round 1

Task 14 and Gate F remain open for independent review. This round adds eleven separately reported Whisker semantic probes through a live session processor, deep metadata copies and strict adapter validation, a frame-derived Pipecat turn comparison pinned to `2967e1c09ad484076089bc88427d305fe8f79fa9`, and official-client coverage for interim/final transcripts and a nonfatal server error.

The eval now checks three ordered writes against media time and speaking transitions, waits for an in-flight flush barrier, verifies queued output is dropped and the next generation resumes, observes runtime barge-in before old output rejection, and exercises input mute at session admission. Each Whisker case contributes its own JSON result; the suite status follows those results.

The narrow Whisker probe maps observed runtime frames and tool executor starts into `DebugEvent` values. It is not a general runtime observer subscription. That delivery surface is owned by Task 35. The scripted provider-error and TTS frames check adapter semantics but do not establish provider-specific instrumentation. Tool failure, full processor shutdown reporting, and client-buffer clear are not certified by this round. The latter remains `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`.

Verification on 2026-10-05:

- `go test -race -count=1 ./tools/compat/... ./tools/eval/... ./transport/rtvi ./transport/ws` passed.
- `go vet ./tools/compat/... ./tools/eval/... ./transport/rtvi ./transport/ws` passed.
- `npm test --prefix tools/compat`, `node tools/compat/verify_fixtures.js`, and the pinned official-client loopback Go test passed.
- `go run ./tools/eval/cmd/govoice-eval` passed 25 consecutive runs; `TestFailedScenarioMakesExitCodeNonzero` passed.
- Focused core race tests passed using `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk`. The default MacOSX27.0 SDK caused a linker error due to unsupported `arm64e.x1` architecture metadata; the explicit 26 SDK resolved it. The selected core test regex does not cover every core test.
- The playback probe still reports `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`.
- `python3 -m pytest --version` reported `No module named pytest`; Pipecat pytest was not run. The Go paired test validates the pinned local source and compares the documented selected outcomes.
