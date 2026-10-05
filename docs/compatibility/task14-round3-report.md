# Task 14 correction round 3

Task 14 and Gate F remain open for independent review. The Whisker teardown case now observes two real callbacks: a probe processor's `core.Closer.Close`, and a supervised worker's exit after session cancellation. The worker starts through the attached session controller's `Go` capability, waits for `Close`, and records resource completion before returning. The scenario requires `Close` observed, worker exit observed, session `Done`, zero leaked tasks, and ordered `session.ended`, `processor.shutdown`, and `session.resources.completed` events. A negative test omits the `Close` observation and confirms the scenario fails; the runner no longer appends teardown events merely because `Done` fired.

This remains a narrow compatibility probe. General processor shutdown observation and debug-event subscription belong to Task 35. The playback-clear probe remains unsupported for the pinned unmodified client. Pipecat pytest remains unavailable in this environment, as recorded in the earlier Task 14 reports.

Verification on 2026-10-05:

- `go test -race -count=25 ./tools/compat/whisker ./tools/eval` passed.
- `go test -race -count=1 ./tools/compat/... ./tools/eval/... ./transport/rtvi ./transport/ws` and matching `go vet` passed.
- `npm test --prefix tools/compat` and `node tools/compat/verify_fixtures.js` passed; the Task 14 race run includes the pinned official-client loopback test.
- `TestFailedScenarioMakesExitCodeNonzero` passed as the intentional failure check.
- Focused core race tests passed with `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk`; linker version warnings remained nonfatal.
- The playback probe reported `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT` under the recorded product decision.
