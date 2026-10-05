Supplemental execution plan: [`docs/superpowers/plans/2026-10-04-v2-execution.md`](docs/superpowers/plans/2026-10-04-v2-execution.md)

# V2 Execution Ledger

## Branch and baseline

- Branch: `codex/v2-implementation`
- Base/starting commit: `9d99f80f519e5f20f567e1c6ac8ef52aea9d5559`
- Phase 0 analyst roles: framework contract/domain analyst; compatibility and V1 evidence auditor; task decomposition/dependency planner; independent self-reviewer of the six-document set.
- Baseline sources: repository test/vet/benchmark evidence recorded for the Phase 0 freeze. The manifest validator verifies required keys, selected non-floating/empty string rules, protobuf and VAD file hashes, and installed versions of the two client packages; it does not verify every inventory value against implementation sources. Remaining pin provenance is mapped in `docs/pipecat-compatibility.md`.
- Baseline: 155 passing race-enabled test events, zero failures, across 10 test-bearing packages. Vet passed in root, `core`, `providers/google`, `providers/xai`, and `server`.
- Baseline benchmark: resampler, 892,284 operations; 1,202 ns/op; 3,648 B/op; 7 allocs/op on Apple M4 Pro.
- Live provider tests were not run because credentials were unavailable. Pipecat pytest was not run because pytest was unavailable.
- macOS SDK note: Makefile's default `/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` candidate may be absent. Working override: `/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk`.

## Phase 0 — Contracts and compatibility freeze

- [x] Inspect binding `docs/v2-spec.md`, sequencing `docs/v2-plan.md`, current repository, nested Go modules, compatibility scripts, pinned npm packages, and Pipecat HEAD.
- [x] Review and correct framework/server contracts in `docs/v2-contracts.md`.
- [x] Review and correct independent RTVI, Whisker, and Pipecat targets in `docs/pipecat-compatibility.md`.
- [x] Review and correct V1 lock and playback-clear decision JSON against script schemas and current evidence.
- [x] Write task-level plan `docs/superpowers/plans/2026-10-04-v2-execution.md`.
- [x] Finish full verification, placeholder scan, and independent self-review of all six deliverables.
- [x] Commit all six Phase 0 deliverables as `docs(v2): freeze contracts and execution plan`.

### Phase 0 gates

| Gate | Status | Evidence/remaining condition |
|---|---|---|
| A — Core Contracts Stable | Passed | Independent Phase 1 tester PASS at `00175a9`: core/server race and vet, focused API/auth, UI tests/typecheck/build, SQLite/PostgreSQL 18 repository contract, and `make verify`; dependency and Task 1 contract boundaries inspected. |
| B — P0 Audio Path Stable | Passed | Independent Phase 2 tester PASS at `48a8170`: core/Google/tagged-xAI/Server race and vet, root tagged test/build, RTVI/WebSocket race tests, focused audio/turn/pacing/flush/transcript repetitions, and mocked provider integrations. The pinned client still cannot clear already-buffered playback; this documented already-delivered-audio limitation is unsupported rather than represented as working. |
| C — Tool Loop Stable | Open | Contract frozen; framework/provider implementation remains open. |
| D — Persistent Agent Runtime | Open | Domain/snapshot contract frozen; server implementation remains open. |
| E — Trace End-to-End | Open | Framework/server/UI persistence path not implemented. |
| F — Compatibility | Open | V1 evidence pinned; V2 RTVI, Whisker semantic, and P0 parity suites remain to be implemented and passed separately. |
| G — Product Release | Open | Product implementation and seven vertical scenarios remain open. |

### Phase 0 assumptions and rulings

- UUIDv7 strings are recommended for durable Server IDs; framework IDs remain opaque and typed; generation is a cancellation fence only.
- Immutable agent/runtime and MCP discovery/schema snapshots use normalized canonical JSON with SHA-256 hashes outside hash input.
- Storage uses `database/sql`, modernc SQLite, pgx stdlib, embedded dialect migrations, and thin repositories.
- Metadata-only tracing is the production default; raw audio and credentials are never traced.
- MCP uses the official Go SDK behind a Server-owned adapter; tool identity is `(MCPServerID, remote tool name)` and selected versions pin schema hashes.
- Whisker remains semantic observer/debug compatibility until an official package/protocol version is pinned.
- Playback-clear probe is an observed failure for pinned unmodified packages; V1 server residual queue remains pacing-bounded to 100 ms / 4,800 bytes, but client-buffer clearing and the V2 flush gate remain unresolved.
- Full ruling rationale and cost-if-wrong statements are frozen in `docs/v2-contracts.md`.

### Phase 0 review and verification record

- RED evidence for missing compatibility-file checks: not available in the recovered worktree because the partial JSON files were already present at handoff; the pre-existing files validated on first invocation. Do not report a missing-file RED run as performed.
- Missing-file RED replay in this recovery turn: validator exited 1 with “Manifest not found”; playback probe exited 2 with missing decision file after reproducing the unsupported playback-clear observations. Files were restored immediately.
- Current manifest validation: passed; required fields and selected string constraints passed, protobuf/VAD hashes matched, and installed versions for the two client packages matched. Other V1 inventory values are sourced from repository files listed in `docs/pipecat-compatibility.md`, not certified by this script.
- Current playback-clear probe: passed through the recorded decision only; observed unsupported interruption and no client clear remain explicit. V2 gate remains unresolved.
- Fixture verification: passed; client/server audio and nested RTVI valid, malformed and wrong-schema fixtures rejected explicitly.
- `make verify`: passed with the Makefile SDK candidate. It built server, VAD worker and examples; root/core/Google/xAI/server vet and race suites passed; formatting passed; manifest, fixtures, and playback-decision checks passed.
- Markdown placeholder scan: passed across all new Markdown.
- Six-deliverable self-review: passed; checked requirements, cross-document consistency, pins and evidence. No material contract discrepancies found.
- Report: `.superpowers/sdd/v2-plan/task-0-report.md` (contains initial evidence and completed Round 1 review fixes/verification).

### Independent review and Round 1 corrections

- Independent Phase 0 tester: PASS on initial deliverables; compatibility scripts, diff check, placeholder scan, and `make verify` all exited 0. Playback remains unsupported and passed only by the explicit recorded decision.
- Independent reviewer: identified gate-ordering, contract-dependency, premature P1, task granularity, stale task-reference, progress wording, and validator-provenance findings; all findings are addressed in this revision.
- Phase 0 document reviewer/fixer: completed Round 1 corrections; Gate A/E/G sequencing corrected; Task 1 now includes compile-time tool contracts; P0 gate no longer depends on the later eval harness; functional P1/P2 work is deferred and split into reviewable tasks; progress distinguishes the Phase 0 review from Gate A.
- Reverification after these fixes: `node tools/compat/validate_manifest.js`, `node tools/compat/probe_playback_clear.js`, `node tools/compat/verify_fixtures.js`, `git diff --check`, placeholder scan, and `make verify` all exited 0. Playback remains unsupported; probe exit 0 is only via the recorded decision.
- Final bookkeeping: independent tester PASS is recorded at initial deliverables commit `02af5c9`; review Rounds 1–2 are complete; all Critical and Important findings were addressed by `09150ac`; the remaining Task 12 wording has been corrected in the closeout commit. Phase 0 is complete. Gates A–G all remain open for implementation evidence.

### Independent review Round 2 corrections

- Phase 5 tracker now records persistence/API evidence and explicitly leaves Gate E open until Phase 6 UI timeline correlation.
- Task summaries now match their detailed plans: Task 2 is P0 lifecycle/mute/flush with prerequisite envelope declarations only; Task 11 consumes Task 1 tool types and owns only bounded ToolLoop behavior; Task 12 is provider tool adapters only, with usage assigned to P1 Task 32; Task 13 no longer promises usage tests or a usage commit.
- Task 14 uses a narrow protocol-specific RTVI encoder and Whisker debug-event adapter for Gate F, distinct from the reusable general serializer abstraction in Task 34.
- Focused revalidation after Round 2: manifest validator, playback probe, fixture verifier, `git diff --check`, and Markdown placeholder scan passed. No JSON, executable code, dependencies, or machine-readable inputs changed; the prior independent tester `make verify` and Round 1 full verification remain applicable, so full `make verify` was not rerun.

## Phase 1 — Foundations in parallel

- [x] Task 1: Distinct identity types, trace/observer/debug value contracts, no-op spans, privacy envelope, and public tool types. Independent tester PASS and reviewer approval after two fix rounds; runtime integration and serializer behavior remain in their assigned tasks.
- [x] Task 2: P0 lifecycle, mute, and deterministic core/fake-transport flush. Independent reviewer APPROVED after two fix rounds; final independent tester PASS. Typed settings envelope declared; application and urgent priority remain deferred.
- [x] Task 3: Storage foundation and dialect migrations. Independent reviewer APPROVED after two fix rounds; final independent tester PASS on SQLite and PostgreSQL 18.
- [x] Task 4: Management API/auth skeleton. Independent reviewer APPROVED after one fix round; final independent tester PASS.
- [x] Task 5: UI shell and typed API client. Independent reviewer APPROVED after one fix round; final independent tester PASS.
- [x] Independent Phase 1 tester gate; Gate A passed at `00175a9`.

### Gate A independent verification

- At `00175a9`, `cd core && go test -race -count=1 ./... && go vet ./...` and the equivalent Server commands passed with `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` and `CGO_LDFLAGS=-Wl,-w`. Focused `Test(API|Auth|Token|Config)` Server tests passed.
- `cd ui && npm test -- --run && npm run typecheck && npm run build` passed (10/10 UI tests). Root `make verify` passed the root/core/provider/server checks, formatting, manifest, and fixtures. Its playback-clear probe still observes that the pinned unmodified client cannot clear buffered playback; the target exits successfully only under the recorded Phase 0 decision.
- The race-enabled shared repository contract passed against SQLite and a disposable PostgreSQL 18 container, along with PostgreSQL integration, config-hash round trip, and migration upgrade/rollback checks. The isolated migration schema was absent after testing, and the container was removed.
- `go list -deps` and core import inspection found no Server, UI, storage, or MCP dependency. Focused core contract tests passed for distinct session/turn/generation types, trace/span parentage and no-op behavior, privacy and immutable observer/debug values, debug correlation, and provider-neutral tool executor signatures. The serializer boundary is a protocol-neutral debug value contract; general serializer implementation and wire mapping remain deferred to their planned tasks.
- Final `git status --porcelain=v1` and `git diff --check` were empty. No product code changed during Gate A testing.

### Task 1 closeout

- Commit range: `60a5e8f..b71dbd1` (`751187a` implementation, `64123b9` Round 1 fixes, `b71dbd1` Round 2 fixes).
- Initial focused contract tests, full core race suite, vet, diff, dependency-boundary, and privacy checks passed. Final independent tester PASS at `b71dbd1` repeated focused correlation/budget/privacy tests, full core race, vet, diff, dependency boundary, and clean-worktree checks. Independent reviewer approved all findings after Rounds 1–2.
- Rulings: public typed IDs remain additive while V1 `uint64`/`string` runtime fields await P0 migration; trace metadata defaults to a fail-closed allowlist; custom structured metadata admits only finite numeric/boolean values with depth, collection, key-count, and shared 1,024-node copy budgets; tool-call correlation is a scalar field; tool JSON byte slices remain caller-owned. Costs and alternatives are recorded in the Task 1 report.
- Gate A remains open pending Tasks 2–5 and the independent Phase 1 tester.

### Task 2 closeout

- Implementation range: `9a1eab7..3094453` (`fa5f8d4` implementation, `cafdede` Round 1 fixes, `3094453` Round 2 fixes). The independent reviewer APPROVED the final implementation after two fix rounds.
- Final independent tester PASS at `3094453`: focused lifecycle/control suite, new regressions repeated 100 times, transport preservation repeated 1,000 times, full core race suite, `go vet ./...`, diff check, and clean-worktree check. RED/GREEN details and review findings are in `.superpowers/sdd/2026-10-04-v2-execution/task-2-report.md`.
- Rulings: default assistant output is interruptible; admission and applied mute state are separate, with inline controls queued in admission order; a flush requires an explicit matching generation and advances the fence once; stale controls cannot cancel a new turn; partial already-delivered context and explicitly uninterruptible media survive the core/fake flush. Costs and alternatives are recorded in the Task 2 report. Real WebSocket disposition/pacing remains Task 8; client playback clearing is not claimed.
- At Task 2 closeout, Gate A remained open pending Tasks 3–5 and the independent Phase 1 tester.

### Task 3 closeout

- Implementation range: `8a55cac..5b46752` (`34655c0` implementation, `fc30920` Round 1 fixes, `5b46752` Round 2 fixes). Independent reviewer APPROVED after two fix rounds; final independent tester PASS at `5b46752`.
- Final tester ran focused storage tests, the SQLite suite and concurrent-open cases 25 times, disposable PostgreSQL 18 shared repository contract and 001→002 upgrade/failed-migration rollback tests, full Server race suite, `go vet ./...`, diff check, clean-worktree check, and container/schema cleanup. RED/GREEN details and review fixes are in `.superpowers/sdd/2026-10-04-v2-execution/task-3-report.md`.
- Ruling: Server owns `database/sql` and thin repositories; modernc SQLite v1.60.1 and pgx/v5 v5.11.0 supply CGO-free and PostgreSQL drivers. Goose v3.28.0 owns embedded dialect migration parsing/versioning and PostgreSQL session locking; gofrs/flock v0.13.1 coordinates SQLite startup, and google/uuid v1.6.0 issues UUIDv7 IDs. Immutable config/schema hashes use canonical JSON bytes stored as text on both dialects. SQLite rejects caller DSN overrides of mandatory settings. Costs include a larger Server dependency graph, an adjacent SQLite migration lock file, and reserved SQLite connection options; see `docs/operations/storage.md`.
- macOS Command Line Tools SDK 27 could not link the existing Server test binary (`tapi error: malformed file`); verification used the documented Xcode macOS 26 `SDKROOT` override and completed with non-failing target-version linker warnings. Gate A remains open pending Tasks 4–5 and the independent Phase 1 tester.

### Task 4 closeout

- Implementation range: `5d536ab..21e0450` (`b5b5482` implementation, `21e0450` review fixes). Independent reviewer APPROVED after one fix round; final independent tester PASS at `21e0450` with repeated security regressions, full root and Server race suites, vet, diff check, and clean-worktree check. RED/GREEN and fix evidence are in `.superpowers/sdd/2026-10-04-v2-execution/task-4-report.md`.
- The admin-only `/api/v1` boundary uses storage-independent DTOs, a deployment-provided management key, JSON error/request-ID envelopes, bounded request bodies, a sanitized read-only config projection, and scoped single-use voice token issuance. The Server reuses `golang-jwt/v5` and Go `net/http`. The production signing and validation key ID is resolved once. JWT lifetime is bounded at whole-second precision, and noncanonical API paths retain JSON errors.
- Scoped tokens carry a validator-generated random issuance epoch to reject replay after validator replacement. Issuance and `/ws` admission therefore need replica affinity until a shared replay/issuance service is provided. Legacy unscoped V1 voice tokens retain their explicit compatibility exception; management credentials remain separate from voice signing. The bootstrap management key requires deployment-side distribution/rotation. See `docs/api/v1.md` and `docs/v2-contracts.md`.
- Final verification used the documented Xcode macOS 26 `SDKROOT` override. The default Command Line Tools SDK 27 linker could not parse its `.tbd` architecture metadata; the Xcode SDK produced only non-failing target-version warnings. Gate A remains open pending Task 5 and the independent Phase 1 tester.

### Task 5 closeout

- Implementation commits: `3d8191f` (`feat(ui): establish v2 admin shell`) and `8e29616` (`fix(ui): harden authentication recovery`). Independent reviewer APPROVED after Round 1 identified a stale 401 that could clear a newer key and a current 401 that lost its message/request ID and input focus. The fix fences requests by auth generation and preserves a sanitized rejection outside the query cache. Review and RED/GREEN evidence are in `.superpowers/sdd/2026-10-04-v2-execution/task-5-report.md`.
- Final independent tester PASS at `8e29616`: `cd ui && npm test -- --run` passed 10/10, focused stale/current 401 recovery tests passed, and `npm run typecheck` and `npm run build` passed. The initial lockfile audit found zero vulnerabilities. The worktree and diff checks passed. The browser visual review was unverified because the bundled browser runtime file was missing; CSS responsiveness and keyboard behavior have source/test evidence, not a visual browser finding.
- The shell exposes six routes with an in-memory management bearer and typed `/api/v1` client. Design and architecture are in `docs/ui/architecture.md`. React Router 7.18.4 was chosen because Node 22.18.0 cannot run React Router 8.4.0. Only the read-only Server configuration route has live API data; collection views await later management endpoints. Gate A remains open pending the independent Phase 1 tester gate.

## Phase 2 — P0 audio/call parity

- [x] Task 6: Audio conditioning, filters, conversion, buffers.
- [x] Task 7: Silence, turn detection, manager. Independent reviewer APPROVED and final independent tester PASS at `3563c8b`.
- [x] Task 8: Output pacing, speaking state, barge-in/flush. Independent reviewer APPROVED and final independent tester PASS at `f117b51`.
- [x] Task 9: STT commit/finalization. Independent reviewer APPROVED and final independent tester PASS at `e9b96c2`.
- [x] Independent Phase 2 tester gate; Gate B passed at `48a8170`.

### Gate B independent verification

- At `48a8170`, full core, Google, tagged xAI, and Server race/vet suites
  passed; root tagged test/build and RTVI/WebSocket race suites also passed.
  Focused race repetitions covered audio normalization and conditioning,
  silence/turn semantics, lifecycle/mute, pacing and speaking state, barge-in
  and flush barriers, transcript finalization/invalidation, and mocked Google
  and xAI multi-turn integrations.
- `node tools/compat/probe_playback_clear.js` exited successfully only through
  the recorded compatibility decision and reproduced
  `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`. Client
  buffer clearing remains explicitly unsupported; Gate B covers the bounded
  server path and permits the documented already-delivered-audio exception.
- Live credentialed providers were not run. Task 14's machine-readable eval
  harness is a Phase 3 deliverable and was not required for this gate. The
  tester left the worktree clean and `git diff --check` passed.

### Task 9 closeout

- Independent review APPROVED the bounded final correction range
  `6835637..e9b96c2`; the reviewer also reran the promoted Round 2–5
  regressions under the race detector and found no concrete regression in the
  corrected Google repetition/rotation or xAI invalidation/cleanup paths.
- The final independent tester passed the tracked Round 5 race suite for 20
  repetitions, all promoted Review/Round3/Round4/Round5 regressions, Google
  repetition/rotation focus, and tagged xAI buffered-write, invalidation,
  cleanup, barge-in, and late-owner focus. The temporary overlay files had
  expired before this last tester run; their tracked equivalents passed and
  the reviewer had already replayed the exact Round 5 overlay successfully.
- Task 9 is complete at `e9b96c2`. Live credentialed provider tests were not
  run. The client playback-clear limitation remains unresolved and is not
  represented as supported. Gate B remains open for its separate phase-wide
  independent tester run.

### Task 9 implementation history

- Implementation is prepared for independent review; Task 9 deliberately
  remains unchecked and Gate B remains open. `core/transcript` now owns raw
  candidate stabilization, receipt-time grace/timeout, late-final selection,
  exactly-once commit, interruption/generation fencing, and bounded fail-closed
  retirement. Its one session-owned deadline driver uses an injectable
  monotonic policy clock; provider/media timestamps are retained only as source
  metadata. V2 context assembly accepts only finalizer-marked commits and
  boundedly suppresses replay by `(TurnID, Generation)`.
- Google and tagged xAI adapters emit raw candidates. Google preserves the SDK's
  audio-portion `IsFinal` and interim-only `Stability`, consumes every result in
  a response, and assembles offset-ordered final segments while reconciling
  rotation overlap. xAI preserves the difference between provider-final
  `is_final`, utterance `speech_final`, and `transcript.done`; done text is
  authoritative when present. Provider streams retain immutable turn owners.
- The production server and demos insert the finalizer before UserContext. The
  production path now keeps the nonzero grace/timeout defaults; the lifecycle
  driver advances typed completion even when no further frame arrives. Focused
  server tests cover delayed-final, interim-only, and empty-result turns.
- Focused RED/GREEN evidence, verification commands, and assumptions are in
  `.superpowers/sdd/2026-10-04-v2-execution/task-9-report.md`. Independent
  review and the Phase 2 tester are still required before Task 9 or Gate B can
  close.
- Correction round 2 hardens the same boundary without closing it: candidate
  frames can no longer seize the speech-start-owned V1 identity; rejected
  higher-generation inputs cannot clear accepted state; Google applies text
  overlap only when media intervals actually overlap, resets rotation media
  origin per turn, updates pending generation on barge-in, and fails explicitly
  at assembly bounds. xAI typed completion/interruption closes only its matching
  immutable stream, bounds draining, restarts cleanly, and propagates receiver
  task admission failure. All independent review probes are now tracked tests;
  Task 9 and Gate B remain open for independent re-review.
- Correction round 3 closes six additional integration gaps without claiming
  the gate: due-state expiration rechecks retirement before applying late input;
  real Google barge-in control reconciles the transcript processor generation;
  bounded xAI draining preserves a prior V1 or typed turn's late final; Google
  assembly overflow emits correlated turn invalidation before its observable
  error; each Google response emits one complete consecutive-result snapshot;
  and automatic rotation replays only previously sent audio. Both preserved
  review overlays are promoted or covered by tracked regressions and pass under
  the race detector. Task 9 and Gate B remain open for independent re-review.
- Correction round 4 hardens terminal transcript transitions: Google now admits
  correlated capacity invalidation in data order or fails the session closed,
  retains scoped sentinel errors, accepts interim tail changes after a settled
  final prefix, and reconciles rotated interim overlap using media intervals.
  xAI reconciles barge-in generation for new streams, releases blocked buffered
  sends without lifecycle-lock contention, and closes receiver sockets before
  dropping ownership, including during session cancellation and in-flight open.
  The six round-4 probes and additional lifecycle/policy regressions are tracked
  and pass under race. Task 9 and Gate B remain open for independent re-review.
- Correction round 5 preserves repeated words in consecutive Google audio
  portions by starting each interim at that stream's last final offset, including
  final-plus-interim responses. xAI treats a failed buffered audio send as a
  terminal error for its immutable turn: it admits correlated invalidation and
  an observable error in data order, then closes the stream without accepting
  a partial `transcript.done`. Round-5 probes are tracked and pass under race;
  Task 9 and Gate B remain open for independent re-review and the Phase 2 tester.

### Task 8 closeout

- Implementation and review commits: `cffcedc`, `1b4c1d3`, `059110d`,
  `fa14406`, and `f117b51`. Independent reviewer APPROVED and final independent
  tester PASS at `f117b51`. Task 8 is complete; Task 9 and the independent
  Phase 2 tester remain open, so Gate B remains open. Next action is Task 9,
  STT commit/finalization. The implementation and review-round entries below
  are historical handoffs; their pending-review statuses are superseded here.
- Four review rounds closed final-marker phase reconciliation, silent finals,
  queue/admission races, callback re-entry, media-time drains and critical
  retention, retirement/replay bounds, overlapping timeout outcomes, and
  FakeTTS ordering. Round 4 escalated the two remaining defects: retained
  critical markers lost around flush and completed units retaining admission
  permission. Both received exact RED/GREEN regressions before final approval.
- Final independent verification: critical-marker/replay regressions passed
  race ×100 (400 cases), with marker counts 1 after flush and 0 after final
  admission, 800 speaking transitions, 700 physical writes, all 200 completed
  audio replays rejected with `ErrInvalidUnitID`, and 200 replayed markers
  producing no effect. Broader Task 8 focused tests passed race ×25. Full
  core/root/WebSocket/RTVI/Google/tagged-xAI race suites and vet passed, using
  the documented macOS SDK workaround where needed. Detailed implementation
  evidence remains in `.superpowers/sdd/2026-10-04-v2-execution/task-8-report.md`.
- Durable design: one hardened `PacedWriter` owns copied, bounded queued output
  and flush/write admission; speaking starts on successful non-empty writes
  and ends once on whole-turn `TurnFinal` drain or terminal interruption.
  Finality uses nonzero session-global monotonic `UnitID` values, bounded
  pending/completed metadata (64 each), and unfinished admission identities
  (128). Completion removes admission permission while preserving late-marker
  metadata. Critical output, its media clock, and pending finality survive
  flush; interruptible output remains fenced. Overlapping flush waiters share
  one terminal failure result, and close/cancel clears all identity ledgers.
- Real coder/websocket writes wait for completion and are not aborted by
  default. The integration holds the writer wrapper after coder `Write`
  returns, proving barrier waiting and same-connection reuse; it does not prove
  recovery from cancellation of an actual in-progress coder write. The
  separately tested timeout is terminal with no successful acknowledgement.
  The playback probe remains unresolved:
  `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`. Server flush
  excludes bytes already accepted by the socket or buffered by the client;
  neither RTVI stopped-speaking nor the recorded decision proves client clear.

### Task 8 initial implementation evidence

- Implementation is prepared for independent review; Task 8 remains unchecked
  and Gate B remains open. The existing `transport/ws.PacedWriter` is the only
  pacing engine. Core adds typed speaking state and an explicit bounded
  whole-turn marker; Google, xAI, and deterministic fakes propagate finality.
- RED: the focused core, WebSocket, Google, and xAI commands failed to compile
  for the missing `SpeakingTracker`, typed transition, `TurnFinal`, and pacer
  callback. A second recorded RED failed for the missing explicit
  `OutputTurnFinalFrame` / `MarkTurnFinal` path after the punctuation-aligned
  streaming race was reproduced. Exact commands and outputs are recorded in
  `.superpowers/sdd/2026-10-04-v2-execution/task-8-report.md`.
- GREEN: focused pacing/RTVI race tests passed 20 repetitions. Full core race
  tests passed with the documented macOS 26 SDK workaround; root RTVI/WebSocket
  race tests and Google/xAI (with `xaispeech`) race tests passed. Core, root
  transport, and affected-provider vet passed. The playback-clear probe was
  rerun and again reported
  `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`; this remains
  unresolved and is not a pass.
- Durable limits: server queue defaults remain 100 ms / 4,800 bytes; pending
  whole-turn markers are capped at 64; flush guarantees only server-side
  queued/in-flight interruptible quiescence. Bytes already accepted by the
  socket or buffered by a client remain outside the guarantee. Task 8 and Gate
  B await independent review/test; Task 9 also remains open.

### Task 8 review round 1

- Independent review required changes and the tester failed the initial
  implementation. Fixes are prepared for re-review; Task 8 remains unchecked
  and Gate B remains open.
- New REDs reproduced missing reconciliation for late/phase-dependent final
  markers and xAI's silent final unit, then failed to compile for the required
  frame bound, pending-marker visibility, and deterministic admission hooks.
  Exact commands and captured outputs are recorded in the Task 8 report.
- Final markers now reconcile before enqueue, while queued, selected, writing,
  or just after completion; overflow does not consume a marker. The queue has a
  64-frame default in addition to byte/duration bounds. Flush/write admission
  is atomic, callback re-entry cannot self-wait, cancellation/close clears
  accounting, and stale/no-op flush leaves retained pacing intact.
- coder/websocket writes are not canceled by default because context
  cancellation closes the connection. A real `httptest` WebSocket regression
  proves flush waits for a started write, then new-generation output succeeds
  on the same connection. Recoverable non-WebSocket writers may explicitly opt
  into write cancellation; timeout remains terminal and never a successful ack.
- Focused review regressions passed 20 race repetitions; the real-WebSocket
  integration passed under race. Full core, root RTVI/WebSocket, Google, and
  tagged xAI race suites and vet passed. The playback probe again returned
  `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`; its exit 0
  is only the recorded product decision, not a playback-clear pass.

### Task 8 review round 2

- Independent re-review required further finality lifecycle changes. Task 8
  remains unchecked and Gate B remains open pending another independent review.
- RED reproduced four defects: an uninterruptible turn-final callback flush
  suppressed the drain, a completed empty final could not accept a late marker,
  a completed audible unit stopped before its scheduled media end, and more
  than 129 terminal turns allowed forgotten duplicate markers to fill the
  pending ledger. Core RED also showed unit IDs restarting at one each turn.
- Final drains now retain output disposition and recorded media end. Late
  markers schedule one abortable nonblocking drain; flush preserves
  uninterruptible finality, while close/cancel cleans scheduled work. Empty
  unit-finals use the completed ledger. Core unit IDs are session-global, and a
  monotonic retirement high-water makes old marker replay bounded and permanent.
  External finality producers must provide nonzero session-monotonic unit IDs.
- The real-WebSocket evidence is deliberately narrow: its barrier holds the
  wrapper after coder `Write` has returned and proves wait plus same-connection
  reuse, not cancellation/recovery of an actual in-progress coder write.
  `AbortInProgressWrite` remains false for coder/websocket; the separately
  tested slow-writer timeout closes the pacer and never acknowledges success.

### Task 8 review round 3

- Re-review found ordering gaps between older completed markers and newer
  drains, monotonic retirement and retained uninterruptible chunks, overlapping
  timeout waiters, and concurrent FakeTTS units. Task 8 remains unchecked and
  Gate B remains open for independent review.
- RED reproduced B's drain being replaced by A, a later critical unit-1 chunk
  rejected after marker 2 advanced retirement, a second flush waiter returning
  success after the first timed out, and FakeTTS order `1,2,1`.
- The pacer now keeps a bounded active-unit ledger, reconciles retained
  uninterruptible identities before stale rejection, retires older markers
  without replacing newer drains, and stores one terminal flush result shared
  by every waiter. Close/cancel cleanup has a dedicated pending-late-drain test.
  FakeTTS units use a cancellation-safe admission-order chain.

### Task 8 review round 4

- The escalation fixes the remaining retained-critical marker and completed-unit
  replay findings. Task 8 remains unchecked and Gate B remains open for review.
- Exact REDs reproduced a lost marker both before flush and after flush before
  the last chunk, plus accepted replay of a completed first unit after the
  second unit drained (empty and non-empty first-unit finals).
- Unfinished critical identities now retain pending markers across flush and
  accept markers despite a newer retirement/order boundary. Successful final
  completion retires admission immediately; bounded completed metadata remains
  available for late markers without permitting audio replay.
- Lifecycle audit: unfinished admission is capped at 128 identities, pending
  and completed metadata at 64 each. Flush removes interruptible identities;
  terminal retirement removes admission; Run's close path clears all ledgers on
  cancellation, encoder/write failure, and shutdown. Verification and the
  unresolved playback-clear result are recorded in the Task 8 report.

### Task 6 closeout

- Implementation commits: `10d9c26` (`feat(core): add bounded streaming audio
  conditioning`), `26b2613` (`fix(core): harden streaming audio conditioning`),
  and `6cb134a` (`test(core): prove real noise suppression`). The independent
  reviewer APPROVED and the final independent tester passed at `6cb134a`.
- Review round 1 found six issues: full-frame cancellation could strand state;
  Speex's one-frame delay and tail drain were not represented honestly;
  resampler rates, allocation, and C-narrowing needed bounds; DSP assertions
  could accept silence or gating; reject-newest overflow was not observable;
  and turn buffers used strings instead of `core.TurnID`. Round 2 found the
  residual metric could accept uniform attenuation; it now uses a deterministic
  voiced/noise fixture with gain-normalized SNR plus a wanted-gain floor and
  explicit bypass, attenuation, silence, and threshold-gate negative controls.
- Final tester evidence passed the suppression controls 25 consecutive times
  under the race detector, full `core` race tests and vet, and the audio race
  suite in the supported Linux/arm64 Debian builder. SpeexDSP is vendored from
  official tag `SpeexDSP-1.2.1`, commit
  `1b28a0f61bc31162979e1f26f3981fc3637095c8`, under BSD 3-Clause; provenance,
  generated-config expansion, harmless whitespace differences, RED/GREEN
  commands, and review evidence are recorded in
  `.superpowers/sdd/2026-10-04-v2-execution/task-6-report.md`. No unresolved
  RNNoise weights or global package installation are used.
- Durable limits: native audio builds require CGO and a C compiler; the initial
  denoiser accepts mono PCM16LE only; denoising/resampling rates are limited to
  8/16/24/48 kHz; resampling accepts at most 48,000 input samples per call and
  caps output allocation at 288,064 samples. The existing V1 transport
  resampler was not migrated. Task 6 does not change the pinned browser
  player's inability to clear already-buffered playback. Tasks 7–9 and the
  independent Phase 2 Gate B remain open.

### Task 7 closeout

- Implementation and fixes: `31a4818` (`feat(core): add pluggable turn detection and management`), `376a787` (`fix(core): harden turn manager ordering`), `c801183` (`fix(core): drain concurrent turn decisions`), `ef4ef50` (`fix(core): bound turn detector coalescing`), and `3563c8b` (`fix(core): preserve queued turn detection`). The original implementer was interrupted at its timebox and the task restarted; after three fix rounds, the remaining ordering issue went to a fresh escalation implementer. Independent review APPROVED and final independent tester PASS at `3563c8b`.
- Review rounds addressed repeated stop/silence-origin drift, generation fencing, detector lock reentry and invalid decisions, old-turn retirement, dropped concurrent completions, stale pre-callback frames, unsafe confidence values, same-input suppression, reset timestamp leakage, and unbounded callback rescheduling. The escalation resolved no-op/rejected inputs invalidating queued work: preflight and commit now validate actual semantic state and epoch; every accepted eligible state change supplies a current successor, and identical positive or negative inputs reuse their result.
- Final independent evidence passed the entire turn package under race ×100, the new 14-case queued-state table under race ×100, focused tests, full core race tests, and vet. Formatting/diff checks and the dependency/timer boundary passed: no provider, Server, storage, STT, transport, wall-clock timer, or manager-created goroutine dependency was introduced. Native audio verification used the documented Xcode macOS 26 SDK override for the SDK 27 linker issue.
- Durable assumptions: deterministic caller-supplied timestamps and typed turn/generation identity; silence detection plus guarded hybrid semantic evidence only, with no model integration; at most two plugin callbacks per outer evaluation before `SilenceDetector` fallback; positive `MaxRetiredTurns` bounds retired IDs per generation and fails closed at capacity, with generation advance/reset clearing the ledger. No provider, session-termination, STT/context, transport, or output-flush integration is claimed.
- Task 7 is complete. At its closeout, Tasks 8–9 and the independent Phase 2 tester remained open; Task 8 is now closed above. Task 9 and the independent Phase 2 tester remain open, so Gate B remains open. Historical RED/GREEN and review-round details follow and are also recorded in `.superpowers/sdd/2026-10-04-v2-execution/task-7-report.md`; their pending-review statuses describe those earlier handoffs.

#### Task 7 initial implementation evidence

- RED: the required focused command first failed because `core/turn` and the typed `TurnEventFrame` API did not exist. Its all-package run also hit the known Command Line Tools SDK 27 `tapi error: malformed file` while linking `core/audio`; the documented Xcode macOS 26 SDK override was used for subsequent verification.
- GREEN: `cd core && SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -run 'Test(TurnDetector|TurnManager|Silence|NoSpeech)' ./...` passed. Focused turn-manager/hybrid race tests passed 25 repetitions; `go test -race -count=1 ./...` and `go vet ./...` passed in `core` with the same SDK override. `gofmt` and `git diff --check` passed.
- The implementation adds `core/turn` deterministic detection/management and a typed normal framework event frame. Configured caller timestamps drive no-speech, start/stop debounce, silence completion, active-turn maximum, mute pauses, interruption, and reset. Hybrid evidence has turn/generation/time/confidence/freshness/silence guards and always falls back to silence. Full details and rulings are in `.superpowers/sdd/2026-10-04-v2-execution/task-7-report.md`.
- At the initial handoff, Task 7 remained unchecked pending independent review. Gate B remained open pending Tasks 8–9 and the Phase 2 independent tester; this task has no provider, STT/context, transport, output-flush, or session-termination integration.

#### Task 7 review round 1 fixes (historical handoff)

- RED regressions reproduced four findings: continuous idle reset the silence origin/re-emitted `Stopped`; a higher-generation idle snapshot failed to fence a pending old candidate; injected detector execution under the manager lock deadlocked reentrant/concurrent state operations and accepted invalid decisions; and retaining only one terminal ID allowed an older turn to be revived after another turn completed.
- Fixes add one-shot speaking-to-idle stop detection; generation-first fencing and automatic interruption; snapshot-based detector invocation outside the state lock with copied semantic evidence, revision/epoch fencing, nonblocking in-flight coordination, panic/decision validation, and safe silence fallback; and bounded per-generation retired IDs. `Config.MaxRetiredTurns` defaults to 128, must be positive, and exhaustion returns `ErrRetiredTurnLimit` without evicting an ID. Generation advance and `Reset` clear retired IDs. Added regressions cover continuous idle completion, higher-gen idle/active transitions, close rejection, reentrant controls/`Observe`/`Advance`, blocked detector invalidation, defensive evidence copying, invalid/panicking detectors, retirement across A→B→late-A, and capacity/reset behavior.
- Initial assumption, superseded by later review fixes: when a concurrent/reentrant update invalidated an in-flight detector snapshot, no recursive or background retry occurred; the next caller-driven `Observe`/`Advance` reevaluated. Gate B remained open and Task 7 unchecked pending independent review. No Task 8/9/14/27 behavior was included.
- RED/GREEN and test evidence for this review round is maintained in `.superpowers/sdd/2026-10-04-v2-execution/task-7-report.md`. The focused suite, 25 race repetitions of the review regressions, full core race suite, vet, `gofmt`, and `git diff --check` passed with the documented SDK workaround; the follow-up commit is `fix(core): harden turn manager ordering`.

#### Task 7 review round 2 fixes (historical handoff)

- RED regressions showed two overlapping advances could both return without completion, reentrant epoch changes could leak an outer `Stopped` frame, and NaN/Inf confidence with `HasConfidence=false` could enter a terminal frame and fail JSON encoding.
- The detector owner now drains a single coalesced newest-input slot after a stale callback; identical deterministic snapshots are not rerun, preventing reentrant self-rescheduling. Same-epoch time-only advancement preserves valid pre-callback frames, while reset, close, interruption, and generation changes invalidate them. Terminal confidence is always finite and absent confidence is normalized to zero.
- Review regressions passed 50 repetitions under the race detector; the all-package focused suite, full core race suite, vet, `gofmt`, and `git diff --check` also passed. Task 7 remained unchecked for independent re-review and Gate B remained open.

#### Task 7 review round 3 fixes (historical handoff)

- RED regressions proved the equal-snapshot shortcut could suppress a due validated result, reset could leave a previous epoch's `pendingDetectAt` and complete the new turn beyond its caller timestamp, and continually advancing detector reentry was not independently bounded.
- Pending work now stores a complete newest snapshot with input/revision/epoch and is cleared at every epoch, generation, reset, close, mute, speech-resume, and terminal boundary. A validated result crosses a revision only for semantically identical current state; all other queued snapshots run. Two plugin callbacks are allowed per outer evaluation before deterministic silence fallback handles the latest snapshot. Invalid semantic confidence is dropped before storage/equality.
- Exact same-timestamp, reset A→B, and adversarial +1ns/NaN regressions passed 100 repetitions under the race detector; the all-package focused suite, full core race suite, vet, `gofmt`, and `git diff --check` also passed. Task 7 remained unchecked for independent re-review and Gate B remained open.

#### Task 7 review round 4 escalation (approved at closeout)

- RED reproduced dropped queued completion after no-op mute and identity-free observations at equal/later timestamps, plus unmute failing to queue fresh silence while a callback remained in flight.
- One semantic state/epoch validation now governs preflight and commit, replacing raw revision counters. Rejected observations are filtered before timestamp mutation; no-op controls preserve eligible work. True mute/speech-resume transitions fence the epoch; unmute supplies a successor snapshot when detection remains eligible. Identical positive and negative inputs reuse their result, preserving the two-callback fallback bound.
- Added a concurrent 14-case state-change table, negative-result reuse, and rejected-observation timestamp isolation regressions. Independent re-review approved the final state and the final tester passed at `3563c8b`; Gate B remains open. Final verification is recorded in the Task 7 report.

## Phase 3 — Context, tools, tracing, compatibility

- [x] Task 10: Context aggregation/reconciliation. Independent reviewer APPROVED and final independent tester PASS at `1425276`.

### Task 10 closeout

- The final independent reviewer approved `daf5fa6..1425276` after five
  review cycles. The reviewed result enforces all-unit assistant delivery,
  cancellation/generation fencing, contiguous interrupted prefixes,
  JSON-safe copied metadata, atomic budget rollback, and bounded tool replay
  identities whose call/result insertions cannot self-evict.
- The final independent tester passed the focused context race suite for 50
  repetitions, the self-evicted call regression for 100 repetitions, related
  replay/result cases for 50 repetitions, and the full core race/vet suite.
  Earlier rounds also passed provider, tagged-root, and transport integration
  suites after the shared contract changes.
- Task 10 is complete at `1425276`. Provider ToolLoop behavior remains Task 11;
  provider adapters remain Task 12; context summarization/compaction remains
  P1 Task 28.

### Task 10 implementation history

Task 10 implementation candidate: provider-neutral system/tool frames now feed
an ordered context aggregator; public tool contracts share one core type surface
with the `core/tools` import path. Context copies mutable inputs, correlates and
deduplicates tool exchanges, fails closed on malformed/unmatched results and
budget overflow, and excludes tool-call messages from turn-completion eviction.
Assistant commits wait for every delivery acknowledgement, commit once, retain
only a fully delivered contiguous prefix on interruption, and discard cancelled
or stale output. Focused race tests pass; independent review/test remain pending.

Task 10 correction round 1 closes the initial review findings: late finality now
uses the same all-unit barrier; cancelled/retired turns cannot resume without a
new user turn; tool context has one correlation-checked mutation path and keeps
result status; metadata cloning is bounded and rejects unsupported values;
failed budget transactions restore eviction accounting; and rejected partial
commits emit no assistant message. Focused race coverage passes 50 repetitions;
independent re-review remains pending.

Task 10 correction round 2 normalizes generationless V1 user turns to the
session generation, retains bounded tool replay identities across history
eviction with fail-closed saturation, and rejects invalid raw JSON or non-finite
metadata atomically. The system-replacement rollback regression now proves an
eligible old turn is tentatively evicted before rollback. Re-review remains
pending.

Task 10 correction round 3 makes tool exchange insertion atomic with context
retention: evicted pending calls cannot authorize first results, self-evicting
result transactions roll back messages and eviction accounting, and only
completed exchanges remain exact-replay fences after eviction. Re-review remains
pending.

Task 10 correction round 4 rejects a new tool call when budget enforcement
evicts that same call. The rejected insertion restores messages, eviction
accounting, and replay-ledger capacity; exact retries remain rejects. Focused
context race x50, full core/provider/root-tagged race and vet, and transport
race checks pass. This historical handoff was superseded by the approved
closeout above.
- [x] Task 11: Bounded ToolLoop behavior consuming Task 1 public types. Independent reviewer APPROVED and final independent tester PASS at `c38919d`.
- [x] Task 12: Google/xAI tool adapters only; usage is P1 Task 32. Independent cross-provider reviewer APPROVED and final independent tester PASS at `ed3b930`.
- [ ] Task 13: Runtime trace instrumentation/content policy; no usage implementation.
- [ ] Task 14: P0 eval harness and separate compatibility suites with protocol-specific debug adapters.
- [ ] Independent Phase 3 tester gate; record Gates C and F.

### Task 11 closeout

- Independent review APPROVED `9f569fb..c38919d`. The final policy rechecks
  generation around execution and replay, exposes only safe trace outcome
  categories, enforces current byte/depth bounds on cached replay without
  re-execution, and reports incomplete/error runs with `Done=false`.
- The final independent tester passed focused ToolLoop race tests for 50
  repetitions plus full core/root race and vet, provider suites, transport
  race tests, and the tagged xAI compile probe. No detached worker goroutines
  are used; executors and continuation callbacks must honor their contexts.
- Task 11 is complete at `c38919d`. Gate C remains open for Task 12's Google
  and xAI adapters and the Phase 3 independent tester. Task 12 must claim a
  call through ToolLoop before projecting it through ContextAggregator, or
  explicitly distinguish an already recorded call.

### Task 11 implementation history

Task 11 implementation handoff (historical): added a
native sequential multi-round loop, atomic context-backed tool-call claims,
bounded argument/result JSON handling, context-derived cancellation and
timeouts, generation fencing, panic/error normalization, and a continuation
surface for adapters. The official-library evaluation and policy are in
`docs/decisions/tool-loop.md`; focused RED/GREEN and verification evidence is
in `.superpowers/sdd/2026-10-04-v2-execution/task-11-report.md`. Task 11 and
Gate C remain open pending independent review and test.

Task 11 correction round 1 addresses the independent review's four findings:
stale generation is rechecked around execution/replay, span end no longer sees
raw executor errors, the minimum result payload size is validated, and limit
errors leave Done false. Task 12 must order ToolLoop's atomic claim before a
duplicate ContextAggregator projection or distinguish previously recorded
calls; Task11/GateC remain open pending corrected commit review/test.

Correction verification passed focused ToolLoop race x50, full core race/vet
with the documented MacOS26 SDK override, Google/xAI provider race/vet, and
root xAI-tagged race/vet including RTVI/WebSocket transports. Independent
correction review and tester are still pending; Task11/GateC remain open.

Task 11 correction round 2 addresses trace classification and replay policy:
spans expose only normalized safe categories; cached results outside stricter
current byte/depth bounds fail closed without execution or history mutation.
The oversized result regression now exceeds its configured cap. Focused x50,
full core race/vet, provider race/vet, root xAI-tagged race/vet, and transport
race passed with the documented MacOS26 SDK override where needed. Independent
re-review and tester review remain pending; Task11/GateC remain open.

### Task 12 closeout

- Google and xAI consume the shared Task 11 ToolLoop and Task 10 context
  contracts without duplicate execution. Both claim calls before context
  projection, compose application/provider generation fences, preserve ordered
  multiple calls, normalize safe result statuses, and retain provider-native
  continuation history.
- Google preserves complete response-occurrence boundaries, ordered signed
  native parts, stream-wide fallback call identities, arbitrary JSON results,
  and verified successful stream termination before tool side effects. xAI
  correlates added/delta/done identities, filters private argument/reasoning
  deltas from assistant text, preserves native later-turn tool history, and
  honors the configured core result-size policy without numeric precision loss.
- Independent cross-provider review APPROVED `a042dff..ed3b930`. The final
  independent tester passed full Google/xAI tagged and untagged race/vet,
  full core/root tagged checks, ToolLoop/fence stress, three generations of
  promoted native-protocol regressions, and mocked end-to-end continuation.
- Task 12 is complete at `ed3b930`. Usage metrics remain P1 Task 32;
  readiness remains P2 Task 43; MCP remains Task 17. Gate C remains open for
  the Phase 3 independent tester after Tasks 13–14.

## Phase 4 — Persistent Server product core

- [ ] Task 15: Agents, immutable versions, runtime snapshots.
- [ ] Task 16: Credentials, capability registry, discovery.
- [ ] Task 17: MCP client, immutable discovery, validation.
- [ ] Task 18: Agent/MCP association and safe lifecycle.
- [ ] Independent Phase 4 tester gate; record Gate D.

## Phase 5 — Trace persistence and observability

- [ ] Task 19: Framework-to-Server trace adapter/query API.
- [ ] Task 20: Retention/privacy and optional OTel export.
- [ ] Independent Phase 5 tester gate; record persistence/API evidence and leave Gate E open pending Phase 6 UI timeline correlation.

## Phase 6 — Admin UI product

- [ ] Task 21: Onboarding, providers, agent configuration.
- [ ] Task 22: Agent voice test and Pipecat debug panel.
- [ ] Task 23: MCP and sessions/trace UI.
- [ ] Task 24: Dashboard and read-only server operations view.
- [ ] Independent Phase 6 tester gate.

## Phase 7 — Vertical integration

- [ ] Task 25: Seven product scenarios: fresh install, mixed providers, MCP, agent versioning, MCP deletion safety, barge-in, failure isolation.
- [ ] Independent Phase 7 tester gate; record the seven vertical scenario results as Gate G inputs.

## Phase 8 — Production hardening

- [ ] Task 26: Security, persistence reliability, operations, packaging, Linux performance.
- [ ] Independent Phase 8 tester gate; pass Gate G only after combining Phase 7 scenario results with security and production packaging evidence.

## Phase 9 — P1 parity (after P0)

- [ ] Task 27: Atomic runtime settings updates.
- [ ] Task 28: Context summarization/compaction hook.
- [ ] Task 29: Opt-in audio recording/capture sink.
- [ ] Task 30: Urgent control lane.
- [ ] Task 31: Processor failure/unusable policies.
- [ ] Task 32: Provider usage metrics.
- [ ] Task 33: Bot output/playback-state observer.
- [ ] Task 34: Serializer abstraction.
- [ ] Task 35: Frame/pipeline observer API.
- [ ] Independent Phase 9 tester gate.

## Phase 10 — P2 parity (after P1)

- [ ] Task 36: DTMF/tone frames.
- [ ] Task 37: Bounded audio mixer.
- [ ] Task 38: Background audio/earcons.
- [ ] Task 39: Generic audio source.
- [ ] Task 40: Advanced audio filter chain.
- [ ] Task 41: Read-only runtime introspection.
- [ ] Task 42: Processor capability metadata.
- [ ] Task 43: Provider/processor readiness events.
- [ ] Task 44: Turn recording/replay hooks.
- [ ] Task 45: Extended behavioral parity scenarios.
- [ ] Independent Phase 10 tester gate.

## Next unblocked work

Tasks 1–9 and the independent Phase 1–2 tester gates are complete. Gate A
passed at `00175a9`; Gate B passed at `48a8170` with client playback clearing
explicitly unsupported. Next action is Phase 3 Task 10: context aggregation
and interruption reconciliation. Gates C–G remain open.
