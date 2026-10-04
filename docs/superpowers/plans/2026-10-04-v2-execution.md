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
- Land and integrate all P0 work before P1, and P1 before P2; optional P2 work does not block V2 release unless promoted by product evidence.
- Preserve independent RTVI conformance, Whisker semantic conformance, and Pipecat behavioral parity results; a recorded playback-clear decision is not a pass.
- Use `database/sql`, `modernc.org/sqlite`, `github.com/jackc/pgx/v5/stdlib`, embedded dialect-specific migrations, thin repositories, and shared repository-contract tests.
- Management APIs use versioned `/api/v1` DTOs; public voice admission is separate from administrator authentication; secrets never reach browser reads.
- Run verification in every affected nested module; root `go test ./...` alone is insufficient. `make verify` is the repository-wide acceptance command.
- Respect existing Go module and UI package boundaries; each task updates its owning docs, adds focused regression coverage, runs the complete affected module suite, receives independent review, and ends at a reviewable commit.

## Review Focus

1. Interrupt or flush racing late STT/LLM/TTS output must preserve turn identity and prevent stale frames, context commits, or misleading trace events; pin in Tasks 2 and 4.
2. Sentinel transcript, prompt, tool payload, credential, and provider-error content must not leak under metadata-only policy; pin in Tasks 1, 9, and 21.
3. MCP schema drift, tool deletion, and active sessions must preserve immutable snapshots and historical evidence; pin in Tasks 14–16 and 25.
4. SQLite/PostgreSQL behavior and upgrade paths must agree under concurrent writes and interrupted migrations; pin in Tasks 5 and 26.
5. A trace/history store outage must not terminate an active voice session, while invalid configuration must prevent a new session; pin in Tasks 9 and 26.

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

### Task 1: Turn, trace, observer, and serializer contracts (FW-A/FW-D)

**Files:** `core/identity.go`, `core/trace.go`, `core/observer.go`, `core/debug.go`, `core/*_test.go`; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Phase 0 contracts. Produces distinct `SessionID`, `TurnID`, `Generation`; no-op `Tracer`/`Span`; immutable `TraceEvent`, `ObserverEvent`, `DebugEvent`; serializer boundary. No exporter or persistence types in core.

- [ ] Add compile-time identity, no-op, parent/child span, observer isolation/order, semantic-event and privacy tests; run `cd core && go test -run 'Test(Identity|Trace|Observer|Debug)' ./...` and confirm failure.
- [ ] Implement public core contracts, docs and API examples; rerun focused tests, then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): add v2 identity and observability contracts`.

### Task 2: Runtime lifecycle, control priority, mute, flush, and settings (FW-A)

**Files:** `core/pipeline.go`, `core/session.go`, `core/turn.go`, `core/control.go`, `core/*_test.go`; `docs/v2-contracts.md`.

**Interfaces:** Consumes Task 1 identities/events. Produces turn manager, typed runtime update/revision, input mute, cancellation fence, urgent control ordering, interruptible-output classification, deterministic server-side flush semantics.

- [ ] Add race tests for duplicate completion, false start, mute/unmute, late frame after interruption, invalid/replayed settings revision, and flush versus queued media; verify failures with `cd core && go test -run 'Test(Turn|Mute|Flush|RuntimeSettings)' ./...`.
- [ ] Implement lifecycle/control behavior and document boundaries; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
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

**Phase 1 independent tester gate:** A reviewer outside Tasks 1–5 runs `cd core && go test -race -count=1 ./... && go vet ./...`, `cd server && go test -race -count=1 ./... && go vet ./...`, `cd ui && npm test -- --run && npm run build`; inspect that core imports no server/UI/storage/MCP packages and record Gate A evidence in `progress.md`.

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

**Phase 2 independent tester gate:** Run every affected module's full race suite and vet plus P0 audio eval scenarios for mute, silence/no-speech, turn completion, normalization, pacing, barge-in, server flush, and transcript finalization. Separately run the playback-clear probe and preserve its unresolved result; do not infer client buffer clearing. Update Gate B in `progress.md` before Phase 3 begins.

## Phase 3 — Context, Tools, Tracing, Compatibility

### Task 10: Context aggregation and interruption reconciliation (FW-C)

**Files:** `core/context.go`, `core/context_test.go`, `core/context/`; `docs/v2-contracts.md`.

**Interfaces:** Consumes stable turns, committed transcripts, and output completion barriers. Produces user/assistant/tool context aggregation, partial assistant reconciliation, deterministic commit, and a provider-neutral compaction hook (P1 implementation scheduled in Task 24).

- [ ] Add context ordering, partial write, cancelled output, tool-result, and bounded-window tests; run `cd core && go test -run 'Test(Context|AssistantContext|InterruptedAssistant)' ./...` and confirm failure.
- [ ] Implement semantics and docs; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): harden turn-scoped context commits`.

### Task 11: Provider-neutral ToolLoop and ToolExecutor (FW-C)

**Files:** `core/tools/`, `core/tools/*_test.go`, `core/go.mod`; `docs/v2-contracts.md`, `docs/decisions/tool-loop.md`.

**Interfaces:** Consumes Phase 0 tool contract and Task 10 context. Produces `ToolDefinition`, `ToolCall`, `ToolResult`, `ToolExecutor`, and bounded multi-round `ToolLoop`. Evaluate current Go libraries for schema/loop reuse only; reject ownership of voice lifecycle, pipeline, or providers.

- [ ] Add multiple-call, malformed/unknown tool, max-round, timeout, cancellation, interruption, and trace-correlation tests; run `cd core && go test -run 'Test(ToolLoop|ToolExecutor)' ./...` and confirm failure.
- [ ] Implement the minimal provider-neutral loop and publish the library evaluation; run focused tests then `cd core && go test -race -count=1 ./... && go vet ./...`.
- [ ] Obtain independent review; commit `feat(core): add bounded provider-neutral tool loop`.

### Task 12: Google/xAI tool adapters and usage (FW-E)

**Files:** `providers/google/llm.go`, `providers/google/tools.go`, `providers/xai/llm.go`, `providers/xai/tools.go`, provider tests; `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Task 11 tool types. Produces symmetric schema mapping, call/result parsing, continuation, cancellation/retry rules, optional usage units, and safe readiness events.

- [ ] Add identical shared contract cases to both provider modules; run focused provider tests and confirm failures.
- [ ] Implement adapters; run each focused suite then `cd providers/google && go test -race -count=1 ./... && go vet ./...` and the equivalent in `providers/xai`.
- [ ] Obtain independent cross-provider review; commit `feat(providers): support shared v2 tool calls`.

### Task 13: Framework trace instrumentation and content policy (FW-D)

**Files:** `core/trace.go`, `core/observer.go`, instrumentation in `core/`, `providers/google/`, `providers/xai/`; `docs/v2-contracts.md`, `docs/privacy.md`.

**Interfaces:** Consumes Task 1 trace API and completed turn/tool/audio interfaces. Produces correlated spans/events for session through transport, metadata default, selected text redaction, provider usage/readiness, and observer fault isolation.

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

**Phase 5 independent tester gate:** Send a real framework session through persistence and API; check metadata-only sentinels and inject trace-store write failure while speech remains active. Verify Gate E evidence and mark it only when the UI/API consumer can retrieve correctly correlated records.

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

**Phase 6 independent tester gate:** A tester not on UI implementation runs `cd ui && npm test -- --run && npm run build`, checks keyboard/accessibility and responsive flows, and verifies every view uses `/api/v1` with no secret-bearing browser DTOs. Record status in `progress.md`.

## Phase 7 — Full Vertical Integration

### Task 25: Seven product scenarios and Gate G integration (FW/SV/UI)

**Files:** `server/e2e/`, `ui/e2e/`, `tools/eval/scenarios/`, `docs/v2-plan.md`, `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes completed framework, persistence, APIs, UI, and eval harness. Produces executable evidence for fresh install; mixed providers; MCP tool lifecycle; immutable agent versioning; MCP deletion safety; repeated barge-in; provider/MCP/database trace-failure isolation.

- [ ] Add executable scenario cases and prove each fails against a deliberately absent capability using test doubles/fixtures, never an external live account requirement.
- [ ] Run all scenario suites, document setup/results and known limitations; run full affected module/UI suites.
- [ ] Obtain independent end-to-end review; commit `test(v2): verify seven vertical product scenarios`.

**Phase 7 independent tester gate:** A clean-environment tester runs all seven scenarios without using implementation notes, checks session snapshot/version and trace correlation end to end, and records Gate G blockers and evidence in `progress.md`.

## Phase 8 — Production Hardening

### Task 26: Security, persistence reliability, operations, packaging, Linux performance (SV-E)

**Files:** `server/auth/`, `server/storage/`, `server/retention/`, `deploy/`, `docs/operations/`, benchmark packages; relevant tests.

**Interfaces:** Consumes all release-candidate APIs and domain rules. Produces reviewed secret/auth/MCP destination controls, SQLite/PostgreSQL upgrade/recovery guidance, graceful drain/readiness, MCP/VAD shutdown, pinned production packaging, and Linux benchmark evidence.

- [ ] Add security boundary, concurrent/failed migration, backup/restore, shutdown timeout, readiness, and deployment smoke tests; confirm failures with targeted server/deploy commands.
- [ ] Implement hardening, docs, image checks, and target Linux benchmark run; run `make verify`, release security checks where available, and database integration suite.
- [ ] Obtain independent security/operations review; commit `chore(server): harden v2 production release`.

**Phase 8 independent tester gate:** Run clean install/upgrade/backup-restore and graceful-shutdown drills; verify security review findings, immutable image dependencies, and target Linux benchmark comparisons. Record production readiness and remaining environment-specific limits.

## Phase 9 — P1 Parity Completion (after P0 gates)

### Task 27: P1 runtime, context, capture, failure, usage, and observer features

**Files:** `core/`, `providers/google/`, `providers/xai/`, `server/`, affected tests; `docs/v2-contracts.md`, `docs/pipecat-compatibility.md`.

**Interfaces:** Consumes Gates A–F. Produces atomic runtime settings, context compaction hook, audio capture sink, urgent control priority refinement, failure/degradation policies, complete optional usage units, playback-state observer, serializer and observer hardening.

- [ ] Add P1 parity cases for partial invalid update, hook decline, consent/buffer bound, retry/degrade safety, absent metrics, playback state transitions, and serializer negatives; run the owning focused tests to confirm failure.
- [ ] Implement in separately reviewable subsystem commits; update status/evidence per P1 row; run full race/vet suites for every changed Go module and UI tests if APIs change.
- [ ] Obtain independent review per subsystem; final commit `feat(v2): complete p1 runtime parity`.

**Phase 9 independent tester gate:** Run the complete P1 capability suite plus `make verify`; verify P1 documentation reflects evidence rather than intended state. Record the release decision and any explicit deferrals.

## Phase 10 — P2 Parity / Nice-to-Haves (after P1)

### Task 28: P2 audio sources, mixing, metadata, readiness, replay, and extended evals

**Files:** `core/audio/`, `core/frame.go`, provider readiness packages, `tools/eval/`, associated tests and docs.

**Interfaces:** Consumes stable P0/P1 audio/control and privacy contracts. Produces typed DTMF/tone, bounded mixer, earcon/background audio, generic source, composable advanced filters, read-only runtime introspection, processor capability metadata, readiness events, consent-aware replay hooks, and extended edge-case scenarios.

- [ ] Add per-capability tests for mix bounds/timing, source cancellation, filter format transitions, tone typing, safe optional metadata, readiness transitions, replay privacy, reconnect/overflow and race cases; verify focused failures.
- [ ] Implement optional capabilities individually and update the compatibility matrix; run focused tests then complete race/vet suites in all affected modules and full eval suite.
- [ ] Obtain independent review for each optional subsystem; commit `feat(v2): add selected p2 audio and runtime capabilities`.

**Phase 10 independent tester gate:** Confirm P2 work introduces no P0/P1 regressions with `make verify` and the complete behavioral eval suite. Update every P2 row with evidence or a reasoned deferred status; P2 omissions do not block release absent a documented product dependency.

## Final release gate

- [ ] A fresh independent release reviewer audits this plan against `docs/v2-spec.md`, all seven vertical scenarios, the distinct compatibility suites, and Gates A–G.
- [ ] Run `make verify`; additionally run `cd core && go test -race -count=1 ./... && go vet ./...`, the equivalent commands in `providers/google`, `providers/xai`, and `server`, and `cd ui && npm test -- --run && npm run build`. Root test coverage does not substitute for nested modules.
- [ ] Record exact command output, live-provider availability, any unavailable Pipecat Python tooling, playback-clear decision, and final Rulings in `progress.md`; tag/commit only after all required release gates have evidence.
