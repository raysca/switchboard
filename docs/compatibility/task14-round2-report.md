# Task 14 correction round 2

Task 14 and Gate F remain open for independent review. This round replaces test-owned Pipecat turn transitions with `TurnEventFrame` decisions emitted by the production turn manager inside a live session. The pinned reference revision, file, and named tests are validated before comparison, including turn two ending interrupted after EndFrame. The Whisker barge-in case now observes assistant-generated interruption and transport-generated flush acknowledgement, then verifies old output is fenced. The flush case checks a blocked in-flight write, queued drop, barrier acknowledgement, and new-generation recovery on the real paced writer.

The eval checks a late transcript candidate, completion, and advance after invalidation without a commit. Its tool interruption case starts a generation-one tool while an assistant turn is active and cancels it on the session's emitted interruption after the generation advances. The Whisker tool case observes both successful and failed executor outcomes. Teardown events follow the session's completed `Done` signal with zero leaked tasks. These are compatibility-probe observations; a general processor shutdown notification and runtime observer subscription still belong to Task 35.

The RTVI debug encoder emits only the mapped safe error category, fatal flag, and correlation fields for errors. A serialized-byte test proves an admitted sentinel in the original category, reason, and component does not leak. The official client integration now rejects every unexpected error callback, while a Node callback test injects fatal, duplicate, and post-disconnect errors.

Verification on 2026-10-05:

- `go test -race -count=1 ./tools/compat/... ./tools/eval/... ./transport/rtvi ./transport/ws` and matching `go vet` passed.
- `npm test --prefix tools/compat` and `node tools/compat/verify_fixtures.js` passed. The race run includes the official-client loopback integration.
- Eval passed 25 consecutive CLI runs. `TestFailedScenarioMakesExitCodeNonzero` passed.
- Focused core race tests passed with `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk`. The linker warned that some objects target macOS 27, but exited successfully.
- The playback probe still reports `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`; this is not a client-buffer-clear pass.
- Pipecat pytest remains unrun because `python3 -m pytest --version` reports no pytest module. The paired Go test validates the pinned local source and selected outcomes.
