Supplemental execution plan: [`docs/superpowers/plans/2026-10-04-v2-execution.md`](docs/superpowers/plans/2026-10-04-v2-execution.md)

# V2 Execution Ledger

## Branch and baseline

- Branch: `codex/v2-implementation`
- Base/starting commit: `9d99f80f519e5f20f567e1c6ac8ef52aea9d5559`
- Phase 0 analyst roles: framework contract/domain analyst; compatibility and V1 evidence auditor; task decomposition/dependency planner; independent self-reviewer of the six-document set.
- Baseline source: repository test/vet/benchmark evidence recorded for the Phase 0 freeze; machine-readable compatibility coordinates validated against the current worktree and pinned Node packages.
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
| A — Core Contracts Stable | Open | Planning contract frozen; implementation contract tests not yet run. |
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
- Current manifest validation: passed; immutable coordinates, checksums, and npm package versions verified.
- Current playback-clear probe: passed through the recorded decision only; observed unsupported interruption and no client clear remain explicit. V2 gate remains unresolved.
- Fixture verification: passed; client/server audio and nested RTVI valid, malformed and wrong-schema fixtures rejected explicitly.
- `make verify`: passed with the Makefile SDK candidate. It built server, VAD worker and examples; root/core/Google/xAI/server vet and race suites passed; formatting passed; manifest, fixtures, and playback-decision checks passed.
- Markdown placeholder scan: passed across all new Markdown.
- Six-deliverable self-review: passed; checked requirements, cross-document consistency, pins and evidence. No material contract discrepancies found.
- Report: `.superpowers/sdd/v2-plan/task-0-report.md` (to be completed with output and final review).

## Phase 1 — Foundations in parallel

- [ ] Task 1: Turn, trace, observer, serializer contracts.
- [ ] Task 2: Lifecycle, priority lane, mute, flush, runtime settings.
- [ ] Task 3: Storage foundation and dialect migrations.
- [ ] Task 4: Management API/auth skeleton.
- [ ] Task 5: UI shell and typed API client.
- [ ] Independent Phase 1 tester gate; record Gate A.

## Phase 2 — P0 audio/call parity

- [ ] Task 6: Audio conditioning, filters, conversion, buffers.
- [ ] Task 7: Silence, turn detection, manager.
- [ ] Task 8: Output pacing, speaking state, barge-in/flush.
- [ ] Task 9: STT commit/finalization.
- [ ] Independent Phase 2 tester gate; record Gate B.

## Phase 3 — Context, tools, tracing, compatibility

- [ ] Task 10: Context aggregation/reconciliation.
- [ ] Task 11: Provider-neutral ToolLoop/ToolExecutor.
- [ ] Task 12: Google/xAI tool adapters and usage.
- [ ] Task 13: Runtime trace instrumentation/content policy.
- [ ] Task 14: P0 eval harness and separate compatibility suites.
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
- [ ] Independent Phase 5 tester gate; record Gate E.

## Phase 6 — Admin UI product

- [ ] Task 21: Onboarding, providers, agent configuration.
- [ ] Task 22: Agent voice test and Pipecat debug panel.
- [ ] Task 23: MCP and sessions/trace UI.
- [ ] Task 24: Dashboard and read-only server operations view.
- [ ] Independent Phase 6 tester gate.

## Phase 7 — Vertical integration

- [ ] Task 25: Seven product scenarios: fresh install, mixed providers, MCP, agent versioning, MCP deletion safety, barge-in, failure isolation.
- [ ] Independent Phase 7 tester gate; record Gate G.

## Phase 8 — Production hardening

- [ ] Task 26: Security, persistence reliability, operations, packaging, Linux performance.
- [ ] Independent Phase 8 tester gate.

## Phase 9 — P1 parity (after P0)

- [ ] Task 27: Runtime updates, context compaction, capture, failure policy, usage, observers/serializer.
- [ ] Independent Phase 9 tester gate.

## Phase 10 — P2 parity (after P1)

- [ ] Task 28: Audio sources/mixing, metadata/readiness, replay, extended evaluation.
- [ ] Independent Phase 10 tester gate.

## Next unblocked work

After an independent Gate A contract review, begin Phase 1 tasks. Keep implementation tasks open; this ledger marks no product implementation phase complete.
