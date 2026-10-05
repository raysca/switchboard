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
| B — P0 Audio Path Stable | Open | Pinned client cannot clear already-buffered playback; P0 framework implementation remains open. |
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
- [ ] Task 7: Silence, turn detection, manager (implementation committed; independent review pending).
- [ ] Task 8: Output pacing, speaking state, barge-in/flush.
- [ ] Task 9: STT commit/finalization.
- [ ] Independent Phase 2 tester gate; record Gate B.

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

### Task 7 implementation evidence (independent review pending)

- RED: the required focused command first failed because `core/turn` and the typed `TurnEventFrame` API did not exist. Its all-package run also hit the known Command Line Tools SDK 27 `tapi error: malformed file` while linking `core/audio`; the documented Xcode macOS 26 SDK override was used for subsequent verification.
- GREEN: `cd core && SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -run 'Test(TurnDetector|TurnManager|Silence|NoSpeech)' ./...` passed. Focused turn-manager/hybrid race tests passed 25 repetitions; `go test -race -count=1 ./...` and `go vet ./...` passed in `core` with the same SDK override. `gofmt` and `git diff --check` passed.
- The implementation adds `core/turn` deterministic detection/management and a typed normal framework event frame. Configured caller timestamps drive no-speech, start/stop debounce, silence completion, active-turn maximum, mute pauses, interruption, and reset. Hybrid evidence has turn/generation/time/confidence/freshness/silence guards and always falls back to silence. Full details and rulings are in `.superpowers/sdd/2026-10-04-v2-execution/task-7-report.md`.
- Task 7 remains unchecked pending independent review. Gate B remains open pending Tasks 8–9 and the Phase 2 independent tester; this task has no provider, STT/context, transport, output-flush, or session-termination integration.

#### Task 7 review round 1 fixes (independent review still pending)

- RED regressions reproduced four findings: continuous idle reset the silence origin/re-emitted `Stopped`; a higher-generation idle snapshot failed to fence a pending old candidate; injected detector execution under the manager lock deadlocked reentrant/concurrent state operations and accepted invalid decisions; and retaining only one terminal ID allowed an older turn to be revived after another turn completed.
- Fixes add one-shot speaking-to-idle stop detection; generation-first fencing and automatic interruption; snapshot-based detector invocation outside the state lock with copied semantic evidence, revision/epoch fencing, nonblocking in-flight coordination, panic/decision validation, and safe silence fallback; and bounded per-generation retired IDs. `Config.MaxRetiredTurns` defaults to 128, must be positive, and exhaustion returns `ErrRetiredTurnLimit` without evicting an ID. Generation advance and `Reset` clear retired IDs. Added regressions cover continuous idle completion, higher-gen idle/active transitions, close rejection, reentrant controls/`Observe`/`Advance`, blocked detector invalidation, defensive evidence copying, invalid/panicking detectors, retirement across A→B→late-A, and capacity/reset behavior.
- Assumption: when a concurrent/reentrant update invalidates an in-flight detector snapshot, no recursive or background retry occurs; the next caller-driven `Observe`/`Advance` reevaluates. Gate B remains open and Task 7 unchecked pending independent review. No Task 8/9/14/27 behavior is included.
- RED/GREEN and test evidence for this review round is maintained in `.superpowers/sdd/2026-10-04-v2-execution/task-7-report.md`. The focused suite, 25 race repetitions of the review regressions, full core race suite, vet, `gofmt`, and `git diff --check` passed with the documented SDK workaround; the follow-up commit is `fix(core): harden turn manager ordering`.

#### Task 7 review round 2 fixes (independent review still pending)

- RED regressions showed two overlapping advances could both return without completion, reentrant epoch changes could leak an outer `Stopped` frame, and NaN/Inf confidence with `HasConfidence=false` could enter a terminal frame and fail JSON encoding.
- The detector owner now drains a single coalesced newest-input slot after a stale callback; identical deterministic snapshots are not rerun, preventing reentrant self-rescheduling. Same-epoch time-only advancement preserves valid pre-callback frames, while reset, close, interruption, and generation changes invalidate them. Terminal confidence is always finite and absent confidence is normalized to zero.
- Review regressions passed 50 repetitions under the race detector; the all-package focused suite, full core race suite, vet, `gofmt`, and `git diff --check` also passed. Task 7 remains unchecked for independent re-review and Gate B remains open.

#### Task 7 review round 3 fixes (independent review still pending)

- RED regressions proved the equal-snapshot shortcut could suppress a due validated result, reset could leave a previous epoch's `pendingDetectAt` and complete the new turn beyond its caller timestamp, and continually advancing detector reentry was not independently bounded.
- Pending work now stores a complete newest snapshot with input/revision/epoch and is cleared at every epoch, generation, reset, close, mute, speech-resume, and terminal boundary. A validated result crosses a revision only for semantically identical current state; all other queued snapshots run. Two plugin callbacks are allowed per outer evaluation before deterministic silence fallback handles the latest snapshot. Invalid semantic confidence is dropped before storage/equality.
- Exact same-timestamp, reset A→B, and adversarial +1ns/NaN regressions passed 100 repetitions under the race detector; the all-package focused suite, full core race suite, vet, `gofmt`, and `git diff --check` also passed. Task 7 remains unchecked for independent re-review and Gate B remains open.

#### Task 7 review round 4 escalation (independent review still pending)

- RED reproduced dropped queued completion after no-op mute and identity-free observations at equal/later timestamps, plus unmute failing to queue fresh silence while a callback remained in flight.
- One semantic state/epoch validation now governs preflight and commit, replacing raw revision counters. Rejected observations are filtered before timestamp mutation; no-op controls preserve eligible work. True mute/speech-resume transitions fence the epoch; unmute supplies a successor snapshot when detection remains eligible. Identical positive and negative inputs reuse their result, preserving the two-callback fallback bound.
- Added a concurrent 14-case state-change table, negative-result reuse, and rejected-observation timestamp isolation regressions. Task 7 remains unchecked for independent re-review; Gate B remains open. Final verification is recorded in the Task 7 report.

## Phase 3 — Context, tools, tracing, compatibility

- [ ] Task 10: Context aggregation/reconciliation.
- [ ] Task 11: Bounded ToolLoop behavior consuming Task 1 public types.
- [ ] Task 12: Google/xAI tool adapters only; usage is P1 Task 32.
- [ ] Task 13: Runtime trace instrumentation/content policy; no usage implementation.
- [ ] Task 14: P0 eval harness and separate compatibility suites with protocol-specific debug adapters.
- [ ] Independent Phase 3 tester gate; record Gates C and F.

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

Tasks 1–6 and the independent Phase 1 tester gate are complete; Gate A passed at `00175a9`. Task 7 implementation is committed and awaits independent review. Gates B–G remain open; after that review, Phase 2 continues with Tasks 8–9 and its independent tester gate.
