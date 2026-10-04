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
| A — Core Contracts Stable | Open | Task 1 public contract tests and independent review passed; Tasks 2–5 and the independent Phase 1 tester remain. |
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
- [ ] Task 2: P0 lifecycle, mute, and flush; independent command tester passed and first reviewer fix round is implemented with focused regressions and core race/vet evidence; awaiting review confirmation. Typed settings envelope declared; application and urgent priority remain deferred.
- [ ] Task 3: Storage foundation and dialect migrations.
- [ ] Task 4: Management API/auth skeleton.
- [ ] Task 5: UI shell and typed API client.
- [ ] Independent Phase 1 tester gate; record Gate A.

### Task 1 closeout

- Commit range: `60a5e8f..b71dbd1` (`751187a` implementation, `64123b9` Round 1 fixes, `b71dbd1` Round 2 fixes).
- Initial focused contract tests, full core race suite, vet, diff, dependency-boundary, and privacy checks passed. Final independent tester PASS at `b71dbd1` repeated focused correlation/budget/privacy tests, full core race, vet, diff, dependency boundary, and clean-worktree checks. Independent reviewer approved all findings after Rounds 1–2.
- Rulings: public typed IDs remain additive while V1 `uint64`/`string` runtime fields await P0 migration; trace metadata defaults to a fail-closed allowlist; custom structured metadata admits only finite numeric/boolean values with depth, collection, key-count, and shared 1,024-node copy budgets; tool-call correlation is a scalar field; tool JSON byte slices remain caller-owned. Costs and alternatives are recorded in the Task 1 report.
- Gate A remains open pending Tasks 2–5 and the independent Phase 1 tester.

## Phase 2 — P0 audio/call parity

- [ ] Task 6: Audio conditioning, filters, conversion, buffers.
- [ ] Task 7: Silence, turn detection, manager.
- [ ] Task 8: Output pacing, speaking state, barge-in/flush.
- [ ] Task 9: STT commit/finalization.
- [ ] Independent Phase 2 tester gate; record Gate B.

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

Task 1 is complete. The remaining Phase 1 foundations are unblocked. Phase 0 completion is distinct from Gate A: Gate A remains open pending Tasks 2–5 and the independent Phase 1 tester. Gates A–G remain open; product implementation tasks are not complete.
