# Voice Platform V2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver Framework V2 and Server V2 as an independently reusable voice runtime and a persistent, operable agent product.

**Architecture:** Keep framework APIs in `core` and provider modules, with `server` consuming only public framework APIs and owning persistence, auth, MCP, and management APIs. Keep `ui/` as a React/TypeScript/Vite client of `/api/v1`; trace, tool, turn, agent-snapshot, and MCP identities are frozen in `docs/v2-contracts.md` before parallel implementation.

**Tech Stack:** Go modules at repository root, `core/`, `providers/google/`, `providers/xai/`, and `server/`; `database/sql`, modernc SQLite, pgx stdlib, embedded dialect migrations; official MCP Go SDK; React, TypeScript, Vite; Node compatibility harnesses.

**Spec:** `docs/v2-spec.md` (sequenced by `docs/v2-plan.md`; cross-workstream contracts are in `docs/v2-contracts.md`).

## Global Constraints

- `core` has no Server, persistence, UI, product-auth, or MCP dependency; providers depend on core; server consumes public framework APIs; UI consumes management APIs.
- `SessionID`, `TurnID`, and `Generation` remain distinct; generation is a cancellation fence, never an identity.
- Server durable IDs are opaque UUIDv7 strings; `AgentVersion`, MCP discovery, and selected tool schemas are immutable snapshots with explicit hashes.
- Trace defaults to metadata-only; raw audio and credentials are never traced; text and tool content require explicit policy and redaction.
- Land and integrate P0 behavior before functional P1 work, and P1 before P2; only prerequisite contract types/envelopes may be published earlier, with their implementation explicitly deferred below. Optional P2 work does not block V2 release unless promoted by product evidence.
- Preserve independent RTVI conformance, Whisker semantic conformance, and Pipecat behavioral parity results; a recorded playback-clear decision is not a pass.
- Use `database/sql`, `modernc.org/sqlite`, `github.com/jackc/pgx/v5/stdlib`, embedded dialect-specific migrations, thin repositories, and shared repository-contract tests.
- Management APIs use versioned `/api/v1` DTOs; public voice admission is separate from administrator authentication; secrets never reach browser reads.
- Run verification in every affected nested module; root `go test ./...` alone is insufficient. `make verify` is the repository-wide acceptance command.
- Respect existing Go module and UI package boundaries; each task updates its owning docs, adds focused regression coverage, runs the complete affected module suite, receives independent review, and ends at a reviewable commit.

## Review Focus

1. Interrupt or flush racing late STT/LLM/TTS output must preserve turn identity and prevent stale frames, context commits, or misleading trace events; pin in Tasks 2, 4, 9, and 13.
2. Sentinel transcript, prompt, tool payload, credential, and provider-error content must not leak under metadata-only policy; pin in Tasks 13, 16, 19, and 20.
3. MCP schema drift, tool deletion, and active sessions must preserve immutable snapshots and historical evidence; pin in Tasks 17–18 and 25.
4. SQLite/PostgreSQL behavior and upgrade paths must agree under concurrent writes and interrupted migrations; pin in Tasks 3 and 26.
5. A trace/history store outage must not terminate an active voice session, while invalid configuration must prevent a new session; pin in Tasks 19 and 26.

---

## Ownership and shared workflow

### Preflight overlap and dependency table

| Shared surface | Single contract owner | Consumers that must not invent a parallel model | Earliest dependent gate |
|---|---|---|---|
| Turn and cancellation semantics | FW-A | VAD/turn detection, STT commit, context, providers, trace UI | A, then B |
| Trace, observer, semantic debug events, content policy | FW-D | framework processors, Server persistence, Whisker adapter, UI | A, then E/F |
| ToolDefinition/Call/Result, ToolExecutor, ToolLoop | FW-C | Google, xAI, MCP wrappers, UI schemas | A, then C |
| AgentVersion and runtime snapshot hashing | SV-A | API/auth, credentials, MCP associations, runtime, UI | D |
| MCPServer/tool/schema identity and deletion | SV-B with SV-A domain approval | agent associations, runtime, history, UI | C, then D |
| Management DTOs and auth | SV-C | UI and all server feature work | D for product test flows |

Before implementation begins, record each task owner and reviewers in `progress.md`, check the target branch for overlapping edits, and assign one owner per shared surface. Parallel tasks may consume a published interface but may not rename or redefine it. Producers update `docs/v2-contracts.md` and compatibility docs with intentional changes in the same commit; consumers stop at the relevant gate if a contract changes.

### Task completion protocol

For every task below: add the named focused regression tests first and run them to observe the expected failure; implement the smallest passing change; update the named contract/operations/compatibility docs; run the focused test and the full affected module race suite plus that module's `go vet ./...`; request an independent review of the diff and evidence; resolve findings; then commit only that task's files. The final task in each phase is an independent phase tester who did not implement that phase; they run the listed phase gate and record pass/fail in `progress.md`. Keep P0 phases ahead of P1 and P2. Product API, storage, and UI shell tasks can overlap with framework foundations after Phase 0 contracts are frozen.

## Phase 1 — Foundations in Parallel

### Task 1: Turn, trace, observer, serializer, and tool contract surfaces (FW-A/FW-C/FW-D)

**Files:** `core/identity.go`, `core/trace.go`, `core/observer.go`, `core/debug.go`, `core/tools/types.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Phase 0 contracts. Produces distinct `SessionID`, `TurnID`, `Generation`; no-op `Tracer`/`Span`; immutable `TraceEvent`, `ObserverEvent`, `DebugEvent`; and the provider-neutral `ToolDefinition`, `ToolCall`, `ToolResult`, and `ToolExecutor` signatures from `docs/v2-contracts.md`. This task publishes type/interface contracts and compile-time contract tests only: serializer mapping, observer delivery, ToolExecutor execution, and ToolLoop behavior are deferred until after P0. No exporter or persistence types enter core.

- [ ] Add compile-time identity and tool-signature, no-op, parent/child span, immutable observer/debug-value, and privacy-envelope contract tests; run `cd core && go test -run 'Test(Identity|TraceContract|ObserverContract|DebugContract|ToolContract)' ./...` and confirm failure.
- [ ] Implement only public contract types and docs/API examples; do not implement P1 observer/serializer/tool behavior; rerun focused tests, then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): add v2 identity and observability contracts`.

### Task 2: P0 runtime lifecycle, mute, and deterministic flush (FW-A)

**Files:** `core/pipeline.go`, `core/session.go`, `core/turn.go`, `core/control.go`, `core/*_test.go`; `docs/v2-contracts.md`.

**Interfaces:** Consumes Task 1 identities/events. Produces turn lifecycle state, input mute, cancellation fence, interruptible-output classification, and deterministic server-side flush semantics. The task defines ordering needed for P0 interruption but does not add a P1 urgent-control queue or runtime-settings update API; those implementations are Tasks 27 and 30.

- [ ] Add race tests for duplicate completion, false start, mute/unmute, late frame after interruption, and flush versus queued media; verify failures with `cd core && go test -run 'Test(TurnLifecycle|Mute|Flush)' ./...`.
- [ ] Implement P0 lifecycle/control behavior and document boundaries; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): harden turn lifecycle and control semantics`.

### Task 3: Server storage foundation (SV-A)

**Files:** `server/storage/`, `server/migrations/sqlite/`, `server/migrations/postgres/`, `server/*_test.go`, `server/go.mod`; `docs/v2-contracts.md`, `docs/operations/storage.md`.

**Interfaces:** Consumes frozen durable-ID and record contracts. Produces `database/sql` opener, embedded versioned dialect migrations, transaction boundary, and thin repositories for agent/version, credential, MCP/discovery, session/turn, and trace records.

- [ ] Add shared repository-contract and empty/upgrade/concurrent-write migration tests; run `cd server && go test -run 'Test(Storage|Migration|Repository)' ./...` and verify initial failures.
- [ ] Implement SQLite and PostgreSQL contracts with UUIDv7 opaque IDs; document backup and migration expectations; run focused tests then `cd server && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(server): establish v2 storage and migrations`.

### Task 4: Management API/auth skeleton (SV-C)

**Files:** `server/api/`, `server/auth/`, `server/*_test.go`, `server/go.mod`; `docs/v2-contracts.md`, `docs/api/v1.md`.

**Interfaces:** Consumes Task 3 repository boundary and Phase 0 auth ruling. Produces `/api/v1` DTO/error envelope, management auth middleware, scoped single-use voice token boundary, and read-only server-config endpoint.

- [ ] Add route, auth-separation, expiry/scope, DTO, secret-redaction, and error-envelope tests; run `cd server && go test -run 'Test(API|Auth|Token|Config)' ./...` and verify failure.
- [ ] Implement only the skeleton; document route/version rules; run focused tests then `cd server && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(server): add versioned management api and auth boundary`.

### Task 5: UI shell and typed API client (SV-D)

**Files:** `ui/package.json`, `ui/src/app/`, `ui/src/api/`, `ui/src/components/`, `ui/src/routes/`; `docs/ui/architecture.md`.

**Interfaces:** Consumes Task 4 DTO/auth contract. Produces React/TypeScript/Vite navigation for Dashboard, Agents, Providers, MCP Servers, Sessions, Server; typed `/api/v1` client and shared form/table/status primitives.

- [ ] Add route/API-client tests and browser capability checks; run `cd ui && npm test -- --run` and confirm the new tests fail.
- [ ] Implement shell and docs; run `cd ui && npm test -- --run && npm run build`.
- [ ] Obtain independent UI review; commit `feat(ui): establish v2 admin shell`.

**Phase 1 independent tester gate:** A reviewer outside Tasks 1–5 runs `cd core && go test -race -count=1 ./... && go vet ./...`, `cd server && go test -race -count=1 ./... && go vet ./...`, `cd ui && npm test -- --run && npm run build`; inspect that core imports no server/UI/storage/MCP packages and that Task 1's public turn, trace, observer/debug, serializer-boundary, and provider-neutral tool type contracts all have contract tests. Gate A passes only on this contract/API evidence; Task 11's ToolLoop implementation remains required for Gate C. Record the Gate A result in `progress.md`.

## Phase 2 — P0 Framework Audio/Call Parity (complete before P1/P2)

### Task 6: Audio conditioning, filters, buffers, and conversion (FW-B)

**Files:** `core/audio/`, `core/audio/*_test.go`, `core/go.mod`; `docs/pipecat-compatibility.md`, `docs/v2-contracts.md`.

**Interfaces:** Consumes audio-filter contract. Produces explicit-format streaming filter with cancellation/reset/close and one real denoiser, plus bounded resampling, conversion, channel normalization, pre-roll, rechunking, and turn buffer.

- [ ] Add format rejection, chunk continuity, state bound, cancellation, reset-isolation, and exact turn-boundary tests; run `cd core && go test -run 'Test(AudioFilter|Resampler|AudioBuffer|PreRoll)' ./...` and confirm failure.
- [ ] Implement and record latency behavior; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): add bounded streaming audio conditioning`.

### Task 7: Silence, turn detection, and turn management (FW-B/FW-A)

**Files:** `core/turn/`, `core/audio/`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 1 turn identity and Task 2 lifecycle. Produces pluggable silence/hybrid detector, no-speech handling, debounce/false-start policy, minimum speech duration, and deterministic manager decisions.

- [ ] Add silence/no-speech, false-start, timeout, duplicate decision, mute, and interrupt-race tests; run `cd core && go test -run 'Test(TurnDetector|TurnManager|Silence|NoSpeech)' ./...` and confirm failure.
- [ ] Implement detector/manager behavior; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): add pluggable turn detection and management`.

### Task 8: Output pacing, speaking state, and barge-in flush (FW-A/FW-B)

**Files:** `core/audio/output*`, `core/turn/`, `transport/rtvi/`, `transport/ws/`, owning tests; `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 2 cancellation/control contract and V1 TTS/transport. Produces paced bounded output, balanced bot-speaking state, interruptible/uninterruptible handling, ordered text/TTS/audio flush, and observer state transitions.

- [ ] Add queue-bound, empty/error/interrupted speaking-state, repeat-barge-in, server queue flush, and stale-output tests; run focused `go test` in `core`, root `transport`, then confirm failures.
- [ ] Implement and update compatibility evidence; run `cd core && go test -race -count=1 ./... && go vet ./...` and `go test -race -count=1 ./transport/rtvi ./transport/ws`.
- [ ] Obtain independent review; commit `feat(core): make output pacing and barge-in deterministic`.

### Task 9: STT commit/finalization and transcript stabilization (FW-B/FW-E)

**Files:** `core/transcript/`, `core/context.go`, `providers/google/`, `providers/xai/`, owning tests; `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes stable `TurnID`, turn decisions, and provider streams. Produces interim filter, finalization grace/timeout, deduplication, late-final reconciliation, and one committed transcript per turn.

- [ ] Add interim/final, duplicate/late final, timeout/grace, interruption, and cross-provider contract tests; run focused `go test` in core and both provider modules and confirm failure.
- [ ] Implement the common policy and provider adapters; run focused tests then race/vet suites in `core`, `providers/google`, and `providers/xai`.
- [ ] Obtain independent review; commit `feat(core): stabilize speech transcript commits`.

**Phase 2 independent tester gate:** This gate is executable before the eval harness exists and does not depend on Task 14. Run `cd core && go test -race -count=1 ./... && go vet ./...`, `cd providers/google && go test -race -count=1 ./... && go vet ./...`, `cd providers/xai && go test -race -count=1 ./... && go vet ./...`, and `go test -race -count=1 ./transport/rtvi ./transport/ws`; inspect focused unit tests for mute, silence/no-speech, turn completion, normalization, pacing, barge-in, server flush, and transcript finalization. Phase 3 Task 14 then packages these behaviors as machine-readable scenarios. Separately run the playback-clear probe and preserve its unresolved result; do not infer client buffer clearing. Update Gate B in `progress.md` before Phase 3 begins.

## Phase 3 — Context, Tools, Tracing, Compatibility

### Task 10: Context aggregation and interruption reconciliation (FW-C)

**Files:** `core/context.go`, `core/context_test.go`, `core/context/`; `docs/v2-contracts.md`.

**Interfaces:** Consumes stable turns, committed transcripts, and output completion barriers. Produces user/assistant/tool context aggregation, partial assistant reconciliation, and deterministic commit. The P1 provider-neutral compaction hook is Task 28.

- [ ] Add context ordering, partial write, cancelled output, tool-result, and bounded-window tests; run `cd core && go test -run 'Test(Context|AssistantContext|InterruptedAssistant)' ./...` and confirm failure.
- [ ] Implement semantics and docs; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): harden turn-scoped context commits`.

### Task 11: Provider-neutral ToolLoop and ToolExecutor (FW-C)

**Files:** `core/tools/`, `core/tools/*_test.go`, `core/go.mod`; `docs/v2-contracts.md`, `docs/decisions/tool-loop.md`.

**Interfaces:** Consumes Phase 0 tool contract and Task 10 context. Produces `ToolDefinition`, `ToolCall`, `ToolResult`, `ToolExecutor`, and bounded multi-round `ToolLoop`. Evaluate current Go libraries for schema/loop reuse only; reject ownership of voice lifecycle, pipeline, or providers.

- [ ] Add multiple-call, malformed/unknown tool, max-round, timeout, cancellation, interruption, and trace-correlation tests; run `cd core && go test -run 'Test(ToolLoop|ToolExecutor)' ./...` and confirm failure.
- [ ] Implement the minimal provider-neutral loop and publish the library evaluation; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): add bounded provider-neutral tool loop`.

### Task 12: Google/xAI tool adapters (FW-E)

**Files:** `providers/google/llm.go`, `providers/google/tools.go`, `providers/xai/llm.go`, `providers/xai/tools.go`, provider tests; `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 11 tool types. Produces symmetric schema mapping, call/result parsing, continuation, and cancellation/retry rules. Usage metrics are deferred to P1 Task 32; readiness-event behavior is deferred to P2 Task 43.

- [ ] Add identical shared contract cases to both provider modules; run focused provider tests and confirm failures.
- [ ] Implement adapters; run each focused suite then `cd providers/google && go test -race -count=1 ./... && go vet ./...` and the equivalent in `providers/xai`.
- [ ] Obtain independent cross-provider review; commit `feat(providers): support shared v2 tool calls`.

### Task 13: Framework trace instrumentation and content policy (FW-D)

**Files:** `core/trace.go`, `core/observer.go`, instrumentation in `core/`, `providers/google/`, `providers/xai/`; `docs/v2-contracts.md`, `docs/privacy.md`.

**Interfaces:** Consumes Task 1 trace API and completed turn/tool/audio interfaces. Produces correlated spans/events for session through transport, metadata default, and selected text redaction. Usage emission is P1 Task 32, readiness events are P2 Task 43, and observer delivery/fault isolation is P1 Task 35.

- [ ] Add sentinel-secret/content tests and ordering/correlation tests across session, turn, tools, interruption, and usage; confirm failure in affected module tests.
- [ ] Instrument and document; run full race/vet suites in core and both providers.
- [ ] Obtain independent privacy and API review; commit `feat(core): instrument v2 runtime traces and usage`.

### Task 14: P0 eval harness and separate compatibility suites (FW-D)

**Files:** `tools/eval/`, `tools/compat/`, `transport/rtvi/`, `transport/ws/`, tests and fixtures; `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes finalized semantic events, serializer and runtime behaviors. Produces deterministic machine-readable P0 scenarios plus independent RTVI client, Whisker semantic adapter, and pinned Pipecat paired-behavior suites.

- [ ] Add scenarios for barge-in, turn completion, silence/no-speech, flush, transcript stability, tool interruption, pacing, teardown, RTVI and Whisker; verify expected failures with `go test`/`node` focused commands.
- [ ] Implement suites, event fixture evidence, and differences; run `node tools/compat/test_rtvi_client.js`, `node tools/compat/verify_fixtures.js`, `go test -race -count=1 ./tools/compat/...`, and `go test -race -count=1 ./transport/rtvi ./transport/ws`.
- [ ] Obtain independent compatibility review; commit `test(compat): add independent v2 conformance and parity suites`.

**Phase 3 independent tester gate:** Confirm Gate C only after shared ToolLoop, Google, and xAI contract suites pass; run all core/provider race and vet suites plus eval and each compatibility suite independently. Confirm Gate F reports three separate results and documents known playback-clear limits. Record phase evidence in `progress.md`.

## Phase 4 — Persistent Server Product Core

### Task 15: Agents, immutable versions, and runtime snapshots (SV-A/SV-C)

**Files:** `server/agent/`, `server/storage/`, `server/api/agents*`, server tests; `docs/v2-contracts.md`, `docs/api/v1.md`.

**Interfaces:** Consumes storage/API skeleton and framework config model. Produces Agent CRUD/archive/duplicate, normalized immutable `AgentVersion` with SHA-256 config hash, readiness, V1 config import, and session-bound snapshot resolution.

- [ ] Add CRUD, hash stability, version immutability, import, invalid readiness, and existing-session-version tests; run focused server tests and confirm failure.
- [ ] Implement services/repositories/DTOs and docs; run `cd server && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(server): persist immutable agent versions`.

### Task 16: Provider credentials, capability registry, and discovery (SV-B/SV-C)

**Files:** `server/provider/`, `server/credential/`, `server/api/providers*`, server tests; `docs/security/credentials.md`, `docs/api/v1.md`.

**Interfaces:** Consumes auth/storage and provider contracts. Produces encrypted secret lifecycle, masked DTOs, validation state, explicit provider capability registry, model/voice discovery, and readiness calculation.

- [ ] Add encryption/tamper/key-rotation, masking/log redaction, invalid credentials, and capability tests; run focused server tests and confirm failure.
- [ ] Implement and document; run full server race/vet.
- [ ] Obtain independent security review; commit `feat(server): manage encrypted provider credentials`.

### Task 17: MCP server client, immutable discovery, and validation (SV-B)

**Files:** `server/mcp/`, `server/migrations/`, `server/go.mod`, server tests; `docs/v2-contracts.md`, `docs/security/mcp.md`.

**Interfaces:** Consumes frozen `(MCPServerID, remote tool name)` identity, schema-hash rules, Task 11 core tools, and official SDK decision. Produces MCP client adapter, configured transport/auth, validate/initialize/discover/refresh, immutable sorted discovery snapshots, bounded tool validation console, and sanitized traceable results.

- [ ] Add simulated remote-server transport/schema/timeout/error/security and hash-stability tests; run focused server tests and confirm failure.
- [ ] Implement adapter/domain services and document supported transports; run full server race/vet.
- [ ] Obtain independent MCP/security review; commit `feat(server): add mcp discovery and validation`.

### Task 18: Versioned AgentMCPTool association and safe lifecycle (SV-A/SV-B)

**Files:** `server/agent/`, `server/mcp/`, `server/runtime/`, migrations and tests; `docs/v2-contracts.md`.

**Interfaces:** Consumes Tasks 15 and 17. Produces version-scoped selected schema hash, required/optional readiness, schema-drift review, runtime MCP wrappers, disable, blocked delete, detach/delete, active-session drain, and preserved history.

- [ ] Add schema-change, tool disappearance, active-session, delete-block, disable, detach/drain, and historical-data tests; run focused server tests and confirm failure.
- [ ] Implement and document; run full server race/vet.
- [ ] Obtain independent data-lifecycle review; commit `feat(server): bind mcp tools to immutable agent versions`.

**Phase 4 independent tester gate:** A server reviewer runs the storage contract tests on SQLite and PostgreSQL, migration upgrades, and the full server race/vet suite; inspect D’s immutable snapshot and MCP schema/deletion evidence, then record Gate D and Phase 4 status in `progress.md`.

## Phase 5 — Trace Persistence and Product Observability

### Task 19: Framework-to-server trace adapter and query API (SV-A/SV-C)

**Files:** `server/trace/`, `server/session/`, migrations, `server/api/traces*`, server tests; `docs/v2-contracts.md`, `docs/api/v1.md`.

**Interfaces:** Consumes framework `Tracer`/`Span` and Task 3 storage. Produces non-blocking persistence of sessions/turns/spans/events/tool calls/usage plus paginated, correlated `/api/v1` history queries.

- [ ] Add parent ordering, concurrent writes, outage isolation, redaction, pagination, and metadata-only tests; run focused tests and confirm failure.
- [ ] Implement adapter and API; run full server race/vet.
- [ ] Obtain independent persistence/API review; commit `feat(server): persist framework traces and sessions`.

### Task 20: Retention, privacy controls, and optional OTel export (SV-E)

**Files:** `server/retention/`, `server/trace/`, `server/otel/`, server config/tests; `docs/privacy.md`, `docs/operations/retention.md`.

**Interfaces:** Consumes trace persistence and content policy. Produces configurable metadata/content retention, safe cleanup, policy-filtered transcript/tool content, and optional OpenTelemetry export correlated to internal trace IDs.

- [ ] Add retention boundary, opt-in content, deletion/history invariants, export correlation, and store-outage tests; confirm focused failures.
- [ ] Implement and document; run full server race/vet.
- [ ] Obtain independent privacy/operations review; commit `feat(server): add trace retention and optional export`.

**Phase 5 independent tester gate:** Send a real framework session through persistence and the query API; check metadata-only sentinels and inject trace-store write failure while speech remains active. Record the persistence/API evidence, but leave Gate E open until Task 23's UI timeline retrieves and presents the same correlated real-session trace.

## Phase 6 — Admin UI Product Features

### Task 21: Onboarding, providers, and agent configuration UI (SV-D)

**Files:** `ui/src/routes/onboarding/`, `ui/src/routes/providers/`, `ui/src/routes/agents/`, typed API client/components and tests; `docs/ui/`.

**Interfaces:** Consumes agent/provider APIs and readiness DTOs. Produces first-run setup, credential create/masked display, capability guidance, agent create/edit/duplicate/archive, readiness, voice settings, and agent detail.

- [ ] Add API-client/UI tests for empty install, missing providers, validation failure, secret masking, and immutable save/version behavior; run `cd ui && npm test -- --run` and confirm failure.
- [ ] Implement and document; run `cd ui && npm test -- --run && npm run build`.
- [ ] Obtain independent product/accessibility review; commit `feat(ui): add onboarding providers and agents`.

### Task 22: Agent voice test and Pipecat debug panel (SV-D)

**Files:** `ui/src/routes/agents/`, `ui/src/voice/`, `ui/src/components/debug/`, tests; `docs/ui/voice-test.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Gate D runtime, production RTVI flow, and framework traces. Produces the official Pipecat client panel for mic/connect/transcript/text/speaking/metrics/RTVI/barge-in, session summary, and trace link.

- [ ] Add browser-level tests for denied mic, connect failure, client-ready, audio, speaking, barge-in, disconnect, and trace link; run focused UI tests and observe failure.
- [ ] Implement using the production session path; run UI tests/build and focused RTVI transport/client suites.
- [ ] Obtain independent UX/compatibility review; commit `feat(ui): add production agent voice test panel`.

### Task 23: MCP and Sessions/Trace UI (SV-D)

**Files:** `ui/src/routes/mcp/`, `ui/src/routes/sessions/`, `ui/src/components/timeline/`, tests; `docs/ui/`.

**Interfaces:** Consumes MCP CRUD/discovery/validation APIs and trace/session query API. Produces MCP list/detail/test, tool selection/status, session filters, turn timeline with tool/error/interruption/latency, and historical trace detail.

- [ ] Add UI tests for schema drift, tool test error, delete protection, empty history, ordering, privacy omissions, and pagination; confirm focused test failure.
- [ ] Implement and document; run UI tests/build plus the affected server API tests.
- [ ] Obtain independent accessibility/data-display review; commit `feat(ui): add mcp and trace inspection screens`.

### Task 24: Dashboard and read-only server operations view (SV-D/SV-E)

**Files:** `ui/src/routes/dashboard/`, `ui/src/routes/server/`, `server/api/metrics*`, tests; `docs/ui/`, `docs/operations/`.

**Interfaces:** Consumes bounded-cardinality operational APIs. Produces active session/rate/latency/error/readiness dashboard and read-only runtime/database/VAD/capacity/uptime/health view.

- [ ] Add bounded-label/API failure and read-only authorization tests; confirm focused failures.
- [ ] Implement and document; run UI tests/build and full server race/vet.
- [ ] Obtain independent operations/UI review; commit `feat(ui): add operations dashboard and server view`.

**Phase 6 independent tester gate:** A tester not on UI implementation runs `cd ui && npm test -- --run && npm run build`, checks keyboard/accessibility and responsive flows, and verifies every view uses `/api/v1` with no secret-bearing browser DTOs. After Task 23, send one real session through Framework → Server persistence → API → UI timeline, verify session/turn/span correlation in the rendered trace under metadata-only policy, and record Gate E only when this evidence passes. Task 24's dashboard is not a Gate E dependency.

## Phase 7 — Full Vertical Integration

### Task 25: Seven product scenarios (FW/SV/UI)

**Files:** `server/e2e/`, `ui/e2e/`, `tools/eval/scenarios/`, `docs/v2-plan.md`, `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes completed framework, persistence, APIs, UI, and eval harness. Produces executable evidence for fresh install; mixed providers; MCP tool lifecycle; immutable agent versioning; MCP deletion safety; repeated barge-in; provider/MCP/database trace-failure isolation.

- [ ] Add executable scenario cases and prove each fails against a deliberately absent capability using test doubles/fixtures, never an external live account requirement.
- [ ] Run all scenario suites, document setup/results and known limitations; run full affected module/UI suites.
- [ ] Obtain independent end-to-end review; commit `test(v2): verify seven vertical product scenarios`.

**Phase 7 independent tester gate:** A clean-environment tester runs all seven scenarios without using implementation notes, checks session snapshot/version and trace correlation end to end, and records scenario evidence and any blockers in `progress.md`. This is the vertical-scenario input to Gate G; it does not pass the product-release gate before production hardening.

## Phase 8 — Production Hardening

### Task 26: Security, persistence reliability, operations, packaging, Linux performance (SV-E)

**Files:** `server/auth/`, `server/storage/`, `server/retention/`, `deploy/`, `docs/operations/`, benchmark packages; relevant tests.

**Interfaces:** Consumes all release-candidate APIs and domain rules. Produces reviewed secret/auth/MCP destination controls, SQLite/PostgreSQL upgrade/recovery guidance, graceful drain/readiness, MCP/VAD shutdown, pinned production packaging, and Linux benchmark evidence.

- [ ] Add security boundary, concurrent/failed migration, backup/restore, shutdown timeout, readiness, and deployment smoke tests; confirm failures with targeted server/deploy commands.
- [ ] Implement hardening, docs, image checks, and target Linux benchmark run; run `make verify`, release security checks where available, and database integration suite.
- [ ] Obtain independent security/operations review; commit `chore(server): harden v2 production release`.

**Phase 8 independent tester gate:** Run clean install/upgrade/backup-restore and graceful-shutdown drills; verify security review findings, immutable image dependencies, and target Linux benchmark comparisons. Combine this evidence with Phase 7's seven scenarios and Phase 6's onboarding/persistent-agent/Pipecat-test/MCP/trace results. Record Gate G as passed only when every listed product flow, security review, and production packaging check succeeds; otherwise record the exact failed condition and keep it open.

## Phase 9 — P1 Parity Completion (after P0 gates)

Each task below starts only after Gate B and the relevant Gate A/C/F contract is stable. Contract envelopes published in Phase 1 do not authorize implementing these behaviors early. Each subsystem lands in its own commit with focused RED/GREEN evidence, docs, full module race/vet, and independent review under the shared task protocol.

### Task 27: Atomic runtime settings updates

**Files:** `core/control.go`, `core/session.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes the Phase 0 `RuntimeSettingsUpdate` contract and Task 2 lifecycle. Produces revisioned, all-or-nothing updates for supported mute/interruption/language/timeout thresholds at defined control boundaries.

- [ ] Add invalid partial-update, stale revision, supported update, and no-session-rebuild tests; run `cd core && go test -run 'Test(RuntimeSettings|SettingsRevision)' ./...` and confirm failure.
- [ ] Implement the validated atomic update path and docs; rerun focused tests, then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent API/concurrency review; commit `feat(core): apply runtime settings atomically`.

### Task 28: Context summarization and compaction hook

**Files:** `core/context.go`, `core/context_test.go`, `core/context/`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 10 context commit semantics. Produces provider-neutral compaction hook that preserves system/tool state and leaves context usable when the hook declines or fails safely.

- [ ] Add hook success/decline/error, system-message preservation, tool-state retention, and bounded-context tests; run `cd core && go test -run 'Test(ContextCompaction|ContextSummary)' ./...` and confirm failure.
- [ ] Implement the hook and document application ownership; rerun focused tests and full core race/vet.
- [ ] Obtain independent context/privacy review; commit `feat(core): add context compaction hook`.

### Task 29: Opt-in audio recording/capture sink

**Files:** `core/audio/capture.go`, `core/audio/*_test.go`; `docs/v2-contracts.md`, `docs/privacy.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 6 audio formats and Task 13 content policy. Produces application-supplied sinks for user, assistant, mixed, or separate audio with consent, format metadata, bounded buffering, cancellation, and no core storage.

- [ ] Add consent-off, each capture mode, buffer overflow, cancellation, and metadata tests; run `cd core && go test -run 'Test(AudioCapture|CaptureConsent)' ./...` and confirm failure.
- [ ] Implement bounded opt-in capture and docs; rerun focused tests and full core race/vet.
- [ ] Obtain independent privacy/audio review; commit `feat(core): add opt-in audio capture sink`.

### Task 30: Urgent control lane

**Files:** `core/pipeline.go`, `core/control.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 2 P0 cancel/flush ordering. Produces a P1 priority lane for cancel, interruption, flush, end, and critical error that can pass congested media while preserving control order.

- [ ] Add media-congestion, ordering, starvation, and shutdown tests; run `cd core && go test -run 'Test(UrgentControl|ControlPriority)' ./...` and confirm failure.
- [ ] Implement bounded priority scheduling and docs; rerun focused tests and full core race/vet.
- [ ] Obtain independent scheduler review; commit `feat(core): prioritize urgent runtime control frames`.

### Task 31: Processor failure and unusable policies

**Files:** `core/pipeline.go`, `core/errors.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 2 lifecycle/error boundary. Produces explicit fail-session, disable, degrade, and safe-retry policy decisions with side-effect-aware error propagation.

- [ ] Add one test per policy plus cancellation/retry-after-output and session-isolation tests; run `cd core && go test -run 'Test(ProcessorFailure|UnusablePolicy|SafeRetry)' ./...` and confirm failure.
- [ ] Implement policy dispatch and docs; rerun focused tests and full core race/vet.
- [ ] Obtain independent failure-semantics review; commit `feat(core): define processor failure policies`.

### Task 32: Provider usage metrics

**Files:** `core/usage.go`, `providers/google/`, `providers/xai/`, provider tests; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 1 event envelope and Task 13 trace instrumentation. Produces optional typed token/audio/character/request/tool usage events correlated by session/turn; unavailable values remain absent and no billing decision is made.

- [ ] Add provider contract tests for available and unavailable units, correlation, and redaction; run focused tests in core, Google, and xAI modules and confirm failure.
- [ ] Implement optional usage extraction/emission and docs; run full race/vet in all three modules.
- [ ] Obtain independent provider/privacy review; commit `feat(providers): emit correlated usage metrics`.

### Task 33: Bot output and playback-state observer

**Files:** `core/audio/output*`, `core/observer.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 8 output lifecycle and Task 1 observer value contract. Produces ordered generating, buffered, playing, drained, interrupted, and completed state events.

- [ ] Add transitions/races for empty output, first audio, drain, interruption, and completion; run `cd core && go test -run 'Test(PlaybackState|BotOutputState)' ./...` and confirm failure.
- [ ] Implement events and docs; rerun focused tests and full core race/vet.
- [ ] Obtain independent state-machine review; commit `feat(core): expose ordered playback state events`.

### Task 34: Serializer abstraction

**Files:** `core/serializer.go`, `core/debug.go`, `core/*_test.go`, transport serializers; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 1 serializer boundary. Produces protocol-selected encode/decode of semantic events and frames, preserving RTVI as a separate adapter and avoiding wire types in processors.

- [ ] Add round-trip, unknown-field, malformed-input, and cross-serializer boundary tests; run `cd core && go test -run 'Test(Serializer|DebugSerialization)' ./...` and owning transport tests; confirm failure.
- [ ] Implement serializer adapters and docs; run full core race/vet and `go test -race -count=1 ./transport/...`.
- [ ] Obtain independent protocol-boundary review; commit `feat(core): add semantic serializer adapters`.

### Task 35: Frame and pipeline observer API

**Files:** `core/observer.go`, `core/pipeline.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 1 immutable observer contract. Produces non-mutating ordered frame/pipeline delivery with direction, processor position, timing, correlation, and panic/error isolation.

- [ ] Add mutation-impossibility, callback ordering, timing/position, panic, error, and session-survival tests; run `cd core && go test -run 'Test(Observer|PipelineObservation)' ./...` and confirm failure.
- [ ] Implement callback delivery/fault isolation and docs; rerun focused tests and full core race/vet.
- [ ] Obtain independent concurrency/API review; commit `feat(core): deliver isolated pipeline observations`.

**Phase 9 independent tester gate:** Run all P1 suites from Tasks 27–35, plus `make verify`; verify every P1 matrix row is marked from test evidence and not intent. Record exact results and any deferrals in `progress.md`.

## Phase 10 — P2 Parity / Nice-to-Haves (after P1)

### Task 36: DTMF and transport-neutral tone frames

**Files:** `core/frame.go`, `core/audio/tone.go`, core/transport tests; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes frame contract. Produces typed tone/DTMF values without implying PSTN/SIP support.

- [ ] Add validation, serialization, and transport-neutrality tests; run `cd core && go test -run 'Test(Tone|DTMF)' ./...` and confirm failure.
- [ ] Implement types/adapters and docs; run full core race/vet and transport tests.
- [ ] Obtain independent API review; commit `feat(core): add transport-neutral tone frames`.

### Task 37: Bounded audio mixer

**Files:** `core/audio/mixer.go`, `core/audio/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Tasks 6 and 8 audio format/pacing contracts. Produces bounded multi-source mix with deterministic format, timing, and interruption policy.

- [ ] Add source-count bound, format mismatch, timing, cancellation, and interruption tests; run `cd core && go test -run 'Test(AudioMixer|Mixer)' ./...` and confirm failure.
- [ ] Implement mixer/docs; run full core race/vet.
- [ ] Obtain independent audio review; commit `feat(core): add bounded audio mixer`.

### Task 38: Background audio and earcon processor

**Files:** `core/audio/background.go`, `core/audio/earcon.go`, tests; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Tasks 8 and 37 output policies. Produces paced background/earcon injection, classification, start/stop, and interruption behavior.

- [ ] Add lifecycle, pacing, output classification, and interruption tests; run `cd core && go test -run 'Test(BackgroundAudio|Earcon)' ./...` and confirm failure.
- [ ] Implement processor/docs; run full core race/vet.
- [ ] Obtain independent output review; commit `feat(core): add background audio and earcons`.

### Task 39: Generic audio source abstraction

**Files:** `core/audio/source.go`, `core/audio/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 6 formats and Task 8 output pacing. Produces generated/file/application source streams with shared format, bounded buffering, cancellation, and pacing.

- [ ] Add each source type, EOF, format, overflow, and cancellation tests; run `cd core && go test -run 'Test(AudioSource|GeneratedAudio|FileAudio)' ./...` and confirm failure.
- [ ] Implement source interface/adapters and docs; run full core race/vet.
- [ ] Obtain independent lifecycle review; commit `feat(core): add generic audio source interface`.

### Task 40: Composable advanced audio filter chain

**Files:** `core/audio/filter_chain.go`, `core/audio/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 6 `AudioFilter`. Produces ordered filters with declared format transitions, bounded copies/state, cancellation, and deterministic close/reset.

- [ ] Add format-chain mismatch, ordering, allocation/state bound, cancellation, and reset tests; run `cd core && go test -run 'Test(AudioFilterChain|FilterFormatTransition)' ./...` and confirm failure.
- [ ] Implement chain/docs; run full core race/vet.
- [ ] Obtain independent audio/API review; commit `feat(core): compose streaming audio filters`.

### Task 41: Read-only runtime pipeline settings introspection

**Files:** `core/runtime_snapshot.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Tasks 2 and 27 runtime state. Produces immutable read-only supported settings/state snapshots without mutable internals or content leakage.

- [ ] Add snapshot immutability, concurrent read, unsupported field, and privacy tests; run `cd core && go test -run 'Test(RuntimeSnapshot|PipelineSettings)' ./...` and confirm failure.
- [ ] Implement snapshot API/docs; run full core race/vet.
- [ ] Obtain independent API/privacy review; commit `feat(core): expose read-only runtime snapshots`.

### Task 42: Processor capability metadata

**Files:** `core/processor.go`, `core/capability.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes existing processor API. Produces optional audio, interruption, runtime-update, async, and stateful declarations with safe absence for older processors.

- [ ] Add old-processor compatibility, each capability, invalid declaration, and introspection tests; run `cd core && go test -run 'Test(ProcessorCapability|CapabilityMetadata)' ./...` and confirm failure.
- [ ] Implement optional metadata/docs; run full core race/vet.
- [ ] Obtain independent compatibility review; commit `feat(core): describe optional processor capabilities`.

### Task 43: Provider and processor readiness events

**Files:** `core/readiness.go`, `providers/google/`, `providers/xai/`, readiness tests; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 1 event envelope and provider lifecycle. Produces ready/degraded/unavailable/recovered transitions with safe reason categories; optional components do not block startup by default.

- [ ] Add state transition, safe reason, recovery, and optional-startup tests; run focused tests in core, Google, and xAI modules and confirm failure.
- [ ] Implement event emission/docs; run full race/vet in affected modules.
- [ ] Obtain independent reliability review; commit `feat(providers): emit component readiness events`.

### Task 44: Turn recording/replay hooks

**Files:** `core/turn/replay.go`, `core/*_test.go`, `tools/eval/`; `docs/v2-contracts.md`, `docs/privacy.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes stable turn/event order and Task 13 privacy policy. Produces application-owned hooks preserving order/consent without core storage.

- [ ] Add order, opt-in/consent, content redaction, cancellation, and application sink tests; run `cd core && go test -run 'Test(TurnReplay|RecordingHook)' ./...` and confirm failure.
- [ ] Implement hooks/eval integration/docs; run core race/vet and full eval suite.
- [ ] Obtain independent privacy/evaluation review; commit `feat(core): add consent-aware turn replay hooks`.

### Task 45: Extended behavioral parity scenarios

**Files:** `tools/eval/scenarios/`, `tools/eval/`, tests and `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Tasks 14 and 36–44. Produces deterministic edge scenarios for false speech start, repeated barge-in, late STT, reconnect, TTS/tool races, mute update, congestion, overflow, and degradation.

- [ ] Add each machine-readable scenario and assert expected results; run the focused eval runner and confirm new cases fail before behavior fixes.
- [ ] Implement missing evaluation adapters and document outcomes; run full eval suite, `make verify`, and all affected Go module race/vet suites.
- [ ] Obtain independent parity review; commit `test(eval): cover extended v2 behavior edges`.

**Phase 10 independent tester gate:** Confirm optional P2 work introduces no P0/P1 regression with `make verify` and the full behavioral eval suite. Update each P2 matrix row with evidence or a reasoned deferred status; P2 omissions do not block release absent a documented product dependency.

## Final release gate

- [ ] A fresh independent release reviewer audits this plan against `docs/v2-spec.md`, all seven vertical scenarios, the distinct compatibility suites, and Gates A–G.
- [ ] Run `make verify`; additionally run `cd core && go test -race -count=1 ./... && go vet ./...`, the equivalent commands in `providers/google`, `providers/xai`, and `server`, and `cd ui && npm test -- --run && npm run build`. Root test coverage does not substitute for nested modules.
- [ ] Record exact command output, live-provider availability, any unavailable Pipecat Python tooling, playback-clear decision, and final Rulings in `progress.md`; tag/commit only after all required release gates have evidence.
