# Task 14 — P0 evaluation and independent compatibility suites

Base: `0fe8893` on `codex/v2-implementation`.

## Scope delivered

- `tools/eval` provides a credential-free JSON harness for barge-in, flush,
  turn completion, silence timeout, no-speech timeout, transcript stabilization,
  late-final rejection/invalidation, paced speaking, tool cancellation, session
  teardown, mute, RTVI, and Whisker semantics. Scenario duration is a declared
  logical duration; this keeps repeated reports byte-stable. Steps and safe
  diagnostic strings accompany every result, and a failed scenario exits 1.
- `transport/rtvi/wire_encoder.go` maps the supported semantic debug-event
  names to the existing RTVI 2.1 envelope and carries bounded scalar identity
  and sequence fields. Unknown event names fail explicitly.
- `tools/compat/whisker` maps immutable core debug events into a semantic event
  record. The adapter makes no wire-compatibility claim because no official
  Whisker package/version is pinned.
- The pinned client integration checks the client-visible user speech and
  transcript events, assistant text and speaking events, output audio, ready
  handshake, and clean disconnect. Existing malformed/unknown-schema fixture
  checks remain part of the installed serializer run.
- `tools/compat/fixtures/pipecat-turn-parity.json` freezes two paired turn
  interruption/cancel outcomes against named tests in the pinned local
  Pipecat checkout. The Go suite compares the framework lifecycle result to
  those reference outcomes.
- `docs/pipecat-compatibility.md` describes commands, scope and independent
  result interpretation. `progress.md` is unchanged; Task14 and Gates C/F
  remain open pending independent review and test-gate integration.

## RED / GREEN

RED: before adapters/harness implementation,
`go test ./tools/eval/... ./tools/compat/whisker ./transport/rtvi` failed to
compile on the missing `RunAll`, `ExitCode`, `Report`, `Result`, `Adapter`, and
`EncodeDebugEvent` APIs. New tests assert deterministic JSON, unique named
results, independent compatibility names, nonzero failure exit, strict
unknown-event rejection, metadata ownership, and malformed-event rejection.

GREEN: the RED packages compiled and passed, the JSON CLI emitted 13 named
passing results, and 25 race-enabled eval repetitions passed. The RTVI
loopback integration, fixture/manifest npm checks, paired Pipecat Go suite,
Whisker adapter tests, root compatibility/transport race suites, and targeted
core turn/transcript/tool cancellation race tests passed. The hard playback
probe returned the documented unsupported verdict and exit 0 under its
recorded product decision; it is not a playback-clear pass.

## Assumptions and limits

- The eval `duration_ns` is deterministic scenario/media duration, not observed
  wall-clock runtime. The latter would make report diffs unstable and is not
  useful for semantic regression comparisons; test execution time remains
  visible from CI itself.
- `RunAll` reports the pinned RTVI and Pipecat suites as `separate_suite`, not
  pass. The official client suite needs its loopback server/token integration;
  Pipecat parity is executed by a paired Go test. The harness cannot promote
  either separate command to pass without its result.
- The Whisker adapter records semantic values only. A byte-level adapter would
  guess an unpinned protocol and would not have authoritative conformance
  evidence.
- Task 1's `DebugEvent` defaults to metadata-only attributes. The RTVI adapter
  therefore rejects transcript/text events if their authorized text payload is
  absent; the existing typed RTVI formatter handles explicitly authorized
  content. This preserves privacy and avoids emitting `null` transcript data.
- Pipecat parity is intentionally narrow: the fixture pairs the pinned
  `TurnTrackingObserver` interruption and cancel outcomes. It does not claim
  every P0 capability is behaviorally identical. Other intentional framework
  differences and the unsupported playback clear stay documented.
- Tool interruption uses the framework ToolLoop executor cancellation path;
  teardown uses a real started session and checks `Done` plus the supervised
  leaked-task count.

## Verification

- PASS: `go test -race -count=1 ./tools/eval/... ./tools/compat/... ./transport/rtvi ./transport/ws`.
- PASS: `go vet ./tools/eval/... ./tools/compat/... ./transport/rtvi ./transport/ws`.
- PASS: `go test -race -count=25 ./tools/eval/...` and a fresh CLI output check.
- PASS: `npm test --prefix tools/compat` (installed serializer fixtures and manifest).
- PASS: `go test -race -count=1 ./tools/compat -run TestCompat_UnmodifiedClientPackage_RegressionGate`; this loopback test supplies the URL/token flags to `node tools/compat/test_rtvi_client.js` and exercises the exact installed client/transport packages.
- PASS: `go test -race -count=1 ./tools/compat -run TestPinnedPipecatTurnInterruptionPairedBehavior` against the checked-in paired expected outcomes.
- PASS: `node tools/compat/probe_playback_clear.js` recorded `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`, with `client_cleared_buffered_playback: false`.
- PASS: from `core`, `go test -race -count=1 ./turn ./transcript ./tools -run 'Test(ToolLoopHonorsCancellationInterruptionAndStaleGeneration|Turn|Finalizer|Processor)'` and `go vet ./turn ./transcript ./tools`.
- CAVEAT: `python3 -m pytest --version` reported that pytest is not installed, so Pipecat's Python source tests were inspected at the pinned checkout but not executed. The Go paired suite exercises the expected recorded states; it does not claim the Python reference test itself ran.
- CAVEAT: a broader concurrent `cd core && go test -race -count=1 ./turn ./transcript ./tools/...` run found `TestToolLoopTraceCarriesNeutralCallCorrelationOnly` failing with `trace events = 2`. This is in concurrently changing Task13 trace instrumentation, outside Task14-owned files; parent has been informed. Task14-specific and targeted tool interruption tests pass.
- PASS: `git diff --check`; formatting checks passed for all Task14 Go files.

## Review status

Not independently reviewed. Task14, Gate C, and Gate F remain open.
