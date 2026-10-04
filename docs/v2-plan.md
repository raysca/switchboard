# Voice Platform V2 Implementation Plan

**Status:** Draft  
**Basis:** `Voice Platform V2 Specification.md`  
**Purpose:** Sequence the V2 work into implementation phases that can be delegated to multiple agents with clear dependencies, parallel workstreams, and integration gates.

This document intentionally contains **no implementation code**.

It defines:

- phases,
- workstreams,
- dependency order,
- parallelizable work,
- integration points,
- release gates.

---

# 1. Planning Principles

V2 has two major tracks:

```text
Framework V2
Server V2
```

They should be developed in parallel wherever the integration contracts are sufficiently stable.

The Server must remain a consumer of the Framework.

The Framework must not take dependencies on:

- Server persistence,
- UI,
- agent storage,
- MCP persistence,
- product authentication.

The implementation plan should optimize for:

1. stabilizing shared contracts early,
2. parallelizing independent subsystems,
3. integrating vertically at regular gates,
4. landing P0 audio/call parity before P1/P2 polish,
5. avoiding long-lived branches that only integrate at the end.

---

# 2. Workstream Model

Use these primary workstreams for agent delegation.

## FW-A — Core Runtime & Lifecycle

Owns:

- session lifecycle,
- supervision,
- turn lifecycle,
- interruption,
- control priority,
- failure policy,
- runtime settings updates.

## FW-B — Audio & Turn Processing

Owns:

- silence/no-speech,
- turn detection,
- resampling,
- audio filters,
- buffering,
- pacing,
- pre-roll,
- recording/capture,
- mixer/tone work.

## FW-C — Context, Tools & AI Semantics

Owns:

- context aggregation,
- interrupted assistant reconciliation,
- tool abstractions,
- tool loop,
- tool execution,
- provider-neutral tool semantics,
- context compaction.

## FW-D — Tracing, Observers, Compatibility & Eval

Owns:

- tracing interfaces,
- turn/session IDs,
- observer API,
- serializers,
- Whisker compatibility,
- RTVI compatibility,
- parity harness,
- behavioral evals,
- regression benchmarks.

## FW-E — Provider Enhancements

Owns:

- Google tool support,
- xAI tool support,
- provider usage metrics,
- provider readiness events,
- provider cancellation hardening,
- provider behavior required by P0/P1 framework features.

## SV-A — Persistence & Domain Model

Owns:

- SQLite/PostgreSQL,
- migrations,
- agents,
- agent versions/snapshots,
- sessions/turns,
- trace persistence schema,
- provider credentials,
- MCP domain objects.

## SV-B — Provider/MCP Integration Layer

Owns:

- provider capability registry,
- key validation,
- model/voice discovery,
- MCP client integration,
- MCP discovery,
- MCP tool wrappers,
- MCP validation/test execution,
- deletion/drain semantics.

## SV-C — Management API & Auth

Owns:

- management APIs,
- auth,
- client/session tokens,
- agent CRUD,
- provider credential APIs,
- MCP APIs,
- session/trace APIs,
- server config read-only API.

## SV-D — Admin UI

Owns:

- onboarding,
- agents UI,
- provider credentials UI,
- MCP UI,
- agent detail/test experience,
- sessions/traces UI,
- dashboard,
- server config view.

## SV-E — Production Runtime & Operations

Owns:

- deployment config boundary,
- graceful shutdown,
- readiness,
- Docker,
- migrations at startup,
- backup guidance,
- retention jobs,
- operational metrics,
- OpenTelemetry integration.

---

# 3. Dependency Overview

The high-level dependency graph is:

```text
Phase 0: Contracts / Baseline
         │
         ├──────────────┬──────────────┬──────────────┐
         ▼              ▼              ▼              ▼
Phase 1 FW core     Phase 1 tracing  Phase 1 storage  Phase 1 UI shell
         │              │              │
         ├──────┬───────┘              │
         ▼      ▼                      ▼
Phase 2 Audio  Tools/Context       Agent/Provider domains
         │      │                      │
         ├──────┴──────────────┬───────┘
         ▼                     ▼
Phase 3 Compatibility       MCP/Provider integration
         │                     │
         └──────────┬──────────┘
                    ▼
Phase 4 Vertical integration
                    │
                    ▼
Phase 5 Admin product completion
                    │
                    ▼
Phase 6 Production hardening
                    │
                    ▼
Phase 7 P1/P2 parity completion
```

The implementation plan should favor many short integration cycles rather than waiting for the final phase to connect the two tracks.

---

# 4. Phase 0 — Baseline, Contracts, and Work Decomposition

## Goal

Freeze the V2 integration boundaries before large parallel implementation begins.

## Required outputs

### Framework contracts

Agree on stable-enough interfaces for:

- trace/span API,
- observer API,
- serializer/debug event API,
- turn identity/lifecycle,
- framework tool definition,
- ToolExecutor/ToolLoop boundary,
- runtime settings update mechanism,
- audio filter abstraction,
- turn detector abstraction,
- provider readiness/usage events.

### Server contracts

Agree on domain concepts for:

- Agent,
- AgentVersion / AgentSnapshot,
- ProviderCredential,
- ProviderCapability,
- MCPServer,
- MCPToolDiscovery,
- AgentMCPTool selection,
- SessionRecord,
- TurnRecord,
- Trace/Span persistence.

### Compatibility targets

Freeze:

- V2 Pipecat parity matrix,
- Whisker compatibility target,
- RTVI compatibility target,
- P0/P1/P2 capability list.

### Test baseline

Capture:

- V1 test status,
- benchmark baseline,
- V1 end-to-end baseline,
- existing provider behavior.

## Parallel work

Can be split across:

```text
Agent 1: Framework contract review
Agent 2: Pipecat parity/Whisker/RTVI matrix
Agent 3: Server domain model
Agent 4: Storage/API shape
Agent 5: UI information architecture
```

## Dependencies

None.

## Blocks

All later phases depend on Phase 0 contract decisions.

## Exit gate

No implementation-heavy work starts until the main cross-workstream contracts have owners and written acceptance criteria.

---

# 5. Phase 1 — Foundations in Parallel

Phase 1 should maximize parallelism.

---

## 5A. Framework — Turn, Trace, Observer Foundations

### Work

- formalize Session ID and Turn ID lifecycle,
- implement trace interface contract,
- implement observer interface contract,
- define standard trace event vocabulary,
- define serialization/debug-event mapping contract,
- define trace privacy/content policy,
- expose disabled/no-op tracing path.

### Can run in parallel with

- audio work,
- storage,
- MCP domain,
- UI shell.

### Depends on

Phase 0 only.

### Blocks

- trace persistence,
- session timeline UI,
- Whisker event stream,
- behavioral eval correlation.

---

## 5B. Framework — Runtime/Lifecycle Hardening

### Work

- control/urgent lane semantics,
- deterministic cancellation ordering,
- processor lifecycle rules,
- processor failure/unusable policy abstraction,
- runtime settings update mechanism,
- input mute/unmute control semantics,
- deterministic flush contract.

### Priority

P0 first:

```text
input mute/unmute
flush semantics
turn lifecycle
```

P1 later in the same stream:

```text
runtime settings update
failure/degradation policy
```

### Can run in parallel with

5A, 5C, 5D, 5E.

### Depends on

Phase 0 framework contracts.

### Blocks

- mature interruption,
- audio output state,
- eval harness scenarios,
- context interruption semantics.

---

## 5C. Server — Storage Foundation

### Work

- storage abstraction,
- SQLite support,
- PostgreSQL support,
- migrations,
- transaction conventions,
- migration testing,
- persistent IDs/version conventions.

### Initial tables/entities

Only establish structures required by subsequent work:

- agents,
- agent versions/snapshots,
- credentials,
- MCP servers,
- discovery snapshots,
- sessions,
- turns,
- spans/events.

Do not over-model future features.

### Can run in parallel with

all Framework Phase 1 work.

### Depends on

Phase 0 server domain decisions.

### Blocks

- persistent agents,
- trace persistence,
- MCP persistence,
- provider credentials,
- admin product.

---

## 5D. Server — API/Auth Skeleton

### Work

- management API versioning,
- auth boundary,
- admin session/auth mechanism,
- client connection token concept,
- consistent error envelope,
- API route grouping.

Do not build full CRUD yet.

### Can run in parallel with

storage and framework work.

### Depends on

Phase 0.

### Blocks

- UI integration,
- provider credentials APIs,
- agent APIs,
- trace APIs.

---

## 5E. UI — Product Shell

### Work

Create the admin product shell and information architecture:

```text
Dashboard
Agents
Providers
MCP Servers
Sessions
Server
```

Also establish:

- auth shell,
- navigation,
- typed API client strategy,
- shared forms/tables/status components,
- microphone/browser capability checks.

No deep feature implementation yet.

### Can run in parallel with

all other Phase 1 work.

### Depends on

Phase 0 UI/API conventions.

### Blocks

nothing critical, but prepares Phase 4/5 UI work.

---

# 6. Phase 2 — P0 Framework Audio/Call Parity

This is the highest-priority Framework phase.

Multiple agents can work concurrently, but integration tests should land continuously.

---

## 6A. Audio Conditioning

### Work

P0:

- audio filter abstraction,
- real noise suppression implementation,
- streaming resampler,
- audio format conversion,
- channel normalization,
- bounded audio buffering,
- pre-roll buffer,
- chunk/rechunk utility.

### Dependencies

- Phase 0 audio contracts.
- Does not require tracing, though trace hooks should be added once 5A is ready.

### Parallelizable internally

```text
Agent A: resampler + format/channel conversion
Agent B: filter abstraction + denoiser
Agent C: buffers + pre-roll + rechunker
```

### Blocks

- higher-quality turn detection,
- provider audio normalization,
- production audio path.

---

## 6B. Speech / Turn Processing

### Work

P0:

- silence detector,
- no-speech detector,
- turn detector abstraction,
- baseline silence/hybrid turn detector,
- speech debounce / false-start suppression,
- minimum speech duration,
- turn management controller.

### Dependencies

- VAD from V1,
- Phase 1 turn model,
- some audio normalization from 6A helpful but not mandatory for early implementation.

### Can run in parallel with

6A and 6C.

### Blocks

- production turn behavior,
- STT commit policy,
- parity evals.

---

## 6C. Output Audio / Barge-In

### Work

P0:

- bot speaking state,
- output audio pacing,
- output audio buffering,
- interruptible/uninterruptible output classification,
- interruption flush processor,
- deterministic flush of queued text/TTS/audio,
- playback-state observer events.

### Dependencies

- Phase 1 lifecycle/flush semantics.
- V1 TTS/transport path.

### Can run in parallel with

6A, 6B.

### Blocks

- hardened barge-in,
- RTVI speaking-state parity,
- Pipecat UI test behavior.

---

## 6D. STT Stabilization and Commit

### Work

P0:

- interim transcript filter,
- transcript stabilizer,
- deduplication,
- finalization timeout,
- end-of-turn grace period,
- late-final reconciliation,
- commit policy.

### Dependencies

- Phase 1 turn identity,
- Phase 2 turn detector contract.

### Can start

Once the Turn API is stable; full tuning can continue while 6B matures.

### Blocks

- context correctness,
- parity evals,
- reliable long conversations.

---

## 6E. Turn-Scoped Audio Capture

### Work

P0:

- exact per-turn audio buffer,
- retention/memory bounds,
- opt-in content access,
- integration with eval harness.

### Dependencies

- turn IDs,
- input audio path.

### Can run in parallel with

all Phase 2 work.

### Blocks

- richer eval/replay scenarios.

---

# 7. Phase 3 — Context, Tools, Tracing, Compatibility

This phase can largely run in parallel across separate agents.

---

## 7A. Context Hardening

### Work

- user context aggregation,
- assistant context aggregation,
- interrupted-assistant reconciliation,
- context commit semantics,
- tool-result context,
- system-message injection,
- context-limit hook.

### P1 extension

- context summarization/compaction hook.

### Dependencies

- turn model,
- STT commit policy,
- interruption semantics.

### Blocks

- reliable tool loop,
- production multi-turn behavior.

---

## 7B. Framework Tool Loop

### Work

- provider-neutral tool definition,
- ToolCall / ToolResult semantics,
- ToolExecutor boundary,
- multi-step tool loop,
- max iterations,
- multiple tool calls,
- timeout,
- cancellation,
- infinite-loop protection,
- tool tracing.

### Library evaluation

Before implementation, evaluate current Go libraries only for reusable tool-loop/schema functionality.

Reject any library that tries to own:

- voice session lifecycle,
- pipeline execution,
- provider architecture.

### Dependencies

- Phase 0 tool contracts,
- Phase 1 tracing helpful,
- Phase 3A context semantics.

### Can run in parallel with

compatibility and server MCP work after contracts are stable.

### Blocks

- Google/xAI tool provider support,
- MCP-backed tools.

---

## 7C. Provider Tool Support

### Work

- Google framework tool schema mapping,
- xAI framework tool schema mapping,
- provider tool-call parsing,
- tool result continuation,
- provider cancellation/retry behavior,
- provider usage metrics.

### Dependencies

- 7B ToolLoop.
- Existing V1 provider modules.

### Parallelizable

Google and xAI can be separate agents.

### Blocks

full MCP agent path.

---

## 7D. Tracing Integration

### Work

Instrument:

- sessions,
- turns,
- VAD,
- STT,
- context,
- LLM,
- ToolLoop,
- TTS,
- interruption,
- transport.

Add:

- trace policy,
- content redaction hooks,
- usage metrics,
- provider readiness events.

### Dependencies

- Phase 1 trace interface.
- Individual components can instrument as they land.

### Can run continuously

Do not wait until all features are finished.

### Blocks

- trace persistence quality,
- timeline UI.

---

## 7E. Behavioral Eval Harness

### Work

Build deterministic scenarios for all P0 behavior:

- barge-in,
- flush,
- turn completion,
- silence timeout,
- no-speech,
- transcript stabilization,
- output pacing,
- tool interruption,
- teardown.

### Dependencies

- Phase 2 feature contracts.
- Turn-scoped audio buffer useful.

### Can start early

Harness infrastructure can start in Phase 1; scenario coverage expands as features land.

### Blocks

Framework V2 release gate.

---

## 7F. RTVI / Whisker / Pipecat Compatibility

### Work

Three separate suites:

```text
RTVI conformance
Whisker conformance
Pipecat behavioral parity
```

Whisker scenarios:

- session start,
- audio frames,
- speech start/stop,
- STT,
- LLM,
- TTS,
- interruption,
- tool calls,
- errors,
- teardown.

### Dependencies

- serializer/observer/debug event contract,
- relevant Framework features.

### Parallelizable

```text
Agent A: RTVI suite
Agent B: Whisker suite
Agent C: Pipecat parity fixtures/reference runs
```

### Blocks

Framework V2 release.

---

# 8. Phase 4 — Server Persistent Product Core

By this point the Framework contracts required by the product should be stable enough for deeper Server implementation.

---

## 8A. Persistent Agents

### Work

- Agent CRUD,
- AgentVersion/Snapshot,
- stable runtime snapshot creation,
- migration/import from V1 config,
- readiness calculation,
- archive/duplicate.

### Dependencies

- Phase 1 storage,
- server API skeleton,
- framework configuration model stable.

### Blocks

agent UI and production sessions from persistent config.

---

## 8B. Provider Credential Management

### Work

- persisted credentials,
- encryption,
- masked display,
- validation,
- provider capability registry,
- model/voice discovery,
- capability readiness.

### Dependencies

- storage,
- auth/API.

### Can run in parallel with

8A and 8C.

### Blocks

onboarding and agent wizard.

---

## 8C. MCP Server Domain and Client

### Work

- MCPServer persistence,
- remote transport,
- optionally gated local/process transport,
- authentication model,
- connection validation,
- initialize/discovery,
- discovered tools,
- schema hash/version,
- refresh,
- availability state.

### Dependencies

- storage,
- management API,
- Framework Tool abstraction stable.

### Can run in parallel with

Framework ToolLoop after tool wrapper contract is stable.

### Blocks

MCP UI and agent MCP tools.

---

## 8D. MCP Tool Validation Console Backend

### Work

- execute discovered tool outside agent,
- same MCP execution path as agents,
- argument validation,
- timeout,
- sanitized result,
- trace generation,
- error reporting.

### Dependencies

- 8C,
- tracing contract.

### Blocks

MCP test UI.

---

## 8E. Agent ↔ MCP Associations

### Work

- select tools per agent version,
- required/optional policy if included,
- schema snapshot/hash,
- agent readiness for missing/schema-changed tools,
- runtime tool wrapper construction.

### Dependencies

- Agent persistence,
- MCP discovery,
- Framework ToolLoop.

### Blocks

end-to-end MCP agent behavior.

---

## 8F. MCP Lifecycle / Deletion

### Work

- disable,
- dependency checks,
- detach-and-delete,
- pending deletion,
- active-session draining,
- historical snapshot preservation,
- no cascading history deletion.

### Dependencies

- agent MCP associations,
- session runtime snapshot tracking.

### Can run after

8E begins; does not need UI first.

---

# 9. Phase 5 — Trace Persistence and Product Observability

---

## 9A. Server Trace Adapter

### Work

Implement Framework Tracer in Server.

Persist:

- sessions,
- turns,
- spans,
- events,
- provider usage,
- tool calls,
- errors,
- interruption events.

### Dependencies

- framework tracing stable,
- storage.

### Can begin

As soon as Phase 1 tracing + storage are ready, but full schema should be validated against Phase 3 traces.

---

## 9B. Retention and Privacy

### Work

- metadata-only mode,
- content persistence policy,
- transcript storage toggle,
- tool args/results policy,
- retention durations,
- cleanup jobs.

### Dependencies

- trace persistence.

### Blocks

production privacy posture.

---

## 9C. Optional OpenTelemetry Export

### Work

- adapter/export path,
- correlation with internal persisted trace IDs.

### Priority

P1/production hardening.

### Can run in parallel with

UI trace work.

---

# 10. Phase 6 — Admin UI Product Features

Most UI work can now run in parallel because APIs/domain objects exist.

---

## 10A. Onboarding

### Work

- first-run detection,
- provider key setup,
- capability readiness,
- missing STT/LLM/TTS guidance,
- first agent creation,
- handoff to Agent detail.

### Dependencies

- provider credentials,
- provider capability registry,
- Agent APIs.

---

## 10B. Agents UI

### Work

- list,
- create,
- edit,
- duplicate,
- archive,
- readiness,
- provider stack,
- MCP tools,
- voice behavior.

### Dependencies

- persistent agents,
- provider capabilities,
- MCP associations.

### Can run in parallel with

sessions UI, MCP UI.

---

## 10C. Agent Detail + Pipecat Debugging UI

### Work

Embed official Pipecat UI/client components for:

- microphone,
- connect/disconnect,
- transcript,
- assistant text,
- speaking state,
- session metrics,
- RTVI events,
- barge-in.

The test must use the normal production voice path.

Add:

```text
View Full Trace
```

link after session completion.

### Dependencies

- Agent runtime from persistent snapshot,
- RTVI endpoint,
- Server trace persistence.

### Can start earlier

Basic integration can begin once Agent API and V1 RTVI path exist.

Full trace linking requires Phase 9.

---

## 10D. MCP UI

### Work

MCP Servers area:

- list,
- add/edit,
- connection status,
- validate,
- refresh discovery,
- tools,
- attached agents,
- disable/delete.

Tool detail/test console:

- schema form,
- raw JSON mode,
- execute,
- duration,
- result,
- errors,
- trace link.

### Dependencies

- MCP APIs,
- MCP tool execution backend.

### Can run in parallel with

Agents UI.

---

## 10E. Sessions / Trace UI

### Work

- session list,
- filters,
- status,
- agent/provider stack,
- session detail,
- turn timeline,
- VAD/STT/LLM/Tool/TTS spans,
- errors,
- interruption,
- latency summary.

### Dependencies

- trace persistence API.

### Can run in parallel with

Agent/MCP UI.

---

## 10F. Dashboard / Server View

### Work

Dashboard:

- active sessions,
- session rate,
- latency,
- errors,
- VAD pressure,
- agent readiness.

Server view:

- read-only config,
- runtime version,
- DB mode,
- VAD executor,
- capacity,
- uptime,
- health.

### Dependencies

- operational metrics APIs.

### Priority

After core Agent/MCP/Session UX.

---

# 11. Phase 7 — Full Vertical Integration

This phase is about product behavior, not feature construction.

Run full scenarios repeatedly.

---

## Scenario 1 — Fresh Installation

```text
start server
→ migrations
→ onboarding
→ add xAI key
→ detect LLM/STT/TTS
→ create agent
→ test conversation
```

---

## Scenario 2 — Mixed Providers

```text
Google STT
→ xAI LLM
→ Google TTS
```

Verify:

- credential model,
- capability selection,
- runtime construction,
- traces.

---

## Scenario 3 — MCP

```text
add MCP server
→ validate
→ inspect tools
→ test tool manually
→ attach tools to agent
→ voice test
→ LLM calls MCP tool
→ inspect trace
```

---

## Scenario 4 — Agent Versioning

```text
start session on Agent v1
→ edit Agent
→ create v2
→ existing session remains v1
→ new session uses v2
```

---

## Scenario 5 — MCP Deletion Safety

```text
MCP attached to agents
→ delete attempt blocked
→ disable
→ agents degraded
→ detach-and-delete
→ running sessions drain
→ historical trace remains readable
```

---

## Scenario 6 — Barge-In

Exercise:

- LLM generation interruption,
- TTS generation interruption,
- buffered audio flush,
- repeated rapid interruption.

Verify trace ordering.

---

## Scenario 7 — Failure Isolation

Test:

- provider failure,
- MCP failure,
- DB trace write failure,
- trace backend failure,
- agent misconfiguration.

Running unrelated sessions must remain healthy.

---

# 12. Phase 8 — Production Hardening

---

## 12A. Security

- credential encryption,
- auth hardening,
- session/client token scopes,
- secret redaction,
- MCP remote destination controls,
- local MCP admin gating,
- CSRF/CORS as applicable,
- dependency review.

---

## 12B. Persistence Reliability

- migration upgrade tests,
- SQLite concurrency behavior,
- PostgreSQL integration tests,
- failure/reconnect behavior,
- retention jobs,
- backup guidance.

---

## 12C. Runtime Operations

- graceful drain,
- readiness transitions,
- session draining,
- VAD worker shutdown,
- MCP connection cleanup,
- rollout behavior,
- production timeouts.

---

## 12D. Packaging

- production Docker image,
- SQLite mode docs,
- PostgreSQL mode docs,
- reverse proxy/TLS docs,
- persistent volume guidance,
- environment config reference.

---

## 12E. Linux Benchmarks

Re-run the relevant runtime benchmarks on target Linux hardware.

Focus on regression detection rather than re-proving the architecture.

---

# 13. Phase 9 — P1 Parity Completion

P1 work can begin earlier if spare agents exist, but should not delay P0 integration.

Work includes:

- runtime settings updates,
- context compaction,
- audio recording/capture hooks,
- urgent control-lane refinement,
- processor degradation/failure policies,
- usage metrics,
- richer playback-state observers,
- serializer abstraction hardening,
- observer API hardening.

Parallelize by subsystem.

---

# 14. Phase 10 — P2 Parity / Nice-to-Haves

P2 work should be independently schedulable.

Examples:

- DTMF/tone frames,
- audio mixer,
- background audio/earcons,
- generic audio sources,
- advanced audio filter chains,
- runtime introspection,
- processor capability metadata,
- readiness events,
- replay hooks,
- extended parity scenarios.

Do not hold V2 release for all P2 items unless they become dependencies for real product requirements.

---

# 15. Recommended Parallel Agent Allocation

A productive multi-agent arrangement during the core implementation period could be:

```text
Agent 1 — FW runtime / lifecycle / interruption
Agent 2 — FW audio conditioning / filters / resampling
Agent 3 — FW turn detection / silence / STT commit
Agent 4 — FW context / tool loop
Agent 5 — FW tracing / Whisker / RTVI / eval harness

Agent 6 — Google provider enhancements
Agent 7 — xAI provider enhancements

Agent 8 — Server storage / migrations / domain
Agent 9 — Server provider credentials / capability registry
Agent 10 — Server MCP integration
Agent 11 — Server APIs / auth

Agent 12 — UI agents / onboarding
Agent 13 — UI MCP
Agent 14 — UI sessions / traces / dashboard
Agent 15 — Pipecat UI embedding / Agent test experience
```

This is not a requirement to run fifteen agents simultaneously.

It is a decomposition showing areas with relatively low code ownership overlap.

---

# 16. Areas That Should NOT Be Implemented Independently in Parallel

Some work requires a single owner or contract-first coordination.

## Tool contracts

Do not let:

```text
Framework ToolLoop
Google tool support
xAI tool support
MCP wrappers
```

invent separate tool representations.

Freeze the Framework tool model first.

## Trace schema

Do not let:

```text
Framework tracing
Server persistence
UI timeline
Whisker
```

invent incompatible event models.

Freeze trace/observer contracts first.

## Turn semantics

Do not independently define turn completion in:

```text
VAD
turn detector
STT commit
context
trace UI
```

The turn lifecycle has to be one framework-level contract.

## Agent snapshots

Agent persistence/runtime construction needs one explicit versioning/snapshot policy before UI/API work spreads.

## MCP tool identity

Discovery, agent selection, schema changes, deletion, and trace history all need the same identity/version model.

---

# 17. Integration Gates

Use explicit gates to prevent parallel work from drifting.

## Gate A — Core Contracts Stable

Required before deep provider/MCP/tool work:

- turn model,
- trace model,
- tool model,
- observer/debug model.

## Gate B — P0 Audio Path Stable

Required before declaring parity testing meaningful:

- turn management,
- STT commit,
- pacing,
- flush/barge-in,
- silence/no-speech,
- audio normalization.

## Gate C — Tool Loop Stable

Required before MCP end-to-end integration:

- provider-neutral tool loop,
- Google tool adapter,
- xAI tool adapter.

## Gate D — Persistent Agent Runtime

Required before UI test experience becomes authoritative:

- agent snapshot,
- credential resolution,
- provider selection,
- MCP tool wiring.

## Gate E — Trace End-to-End

Required before session debugging UX is considered complete:

```text
Framework span
→ Server persistence
→ API
→ UI timeline
```

## Gate F — Compatibility

Required for Framework release:

- RTVI suite passes,
- Whisker suite passes,
- P0 parity suite passes.

## Gate G — Product Release

Required for Server V2 release:

- onboarding,
- persistent agent,
- Pipecat UI test,
- MCP tool flow,
- session traces,
- security,
- production packaging.

---

# 18. Suggested Critical Path

The likely critical path is:

```text
Contracts
→ Turn/lifecycle semantics
→ P0 audio/barge-in/STT commit
→ Context semantics
→ ToolLoop
→ Provider tool support
→ MCP wrapper path
→ Persistent agent snapshot
→ Agent runtime construction
→ Embedded Agent test
→ End-to-end traces
→ Production hardening
```

Storage/UI work should be developed in parallel so it is ready when these runtime contracts stabilize.

---

# 19. Features That Can Safely Lag Behind the Critical Path

These can be assigned opportunistically without blocking the main V2 product:

- DTMF/tone frames,
- mixer/background audio,
- advanced filter chains,
- processor capability metadata,
- replay hooks,
- rich dashboard analytics,
- OpenTelemetry export,
- optional local MCP process mode,
- context summarizer implementation beyond the hook,
- extended P2 parity cases.

---

# 20. Release Strategy

Do not wait for every V2 feature before integrating.

Prefer internal milestones such as:

## V2-A — Framework Foundations

- trace/turn/tool contracts,
- P0 runtime semantics.

## V2-B — Audio Parity

- P0 audio/call features,
- eval harness,
- compatibility suites.

## V2-C — Persistent Server

- database,
- agents,
- credentials,
- MCP.

## V2-D — Admin Product

- onboarding,
- Agent detail/test,
- MCP UI,
- sessions/traces.

## V2-RC — Production Hardening

- security,
- deployment,
- retention,
- Linux testing,
- compatibility gates.

---

# 21. Definition of Done

V2 implementation planning is successful if multiple agents can independently pick up work from this plan and answer:

```text
What do I own?
What contract do I depend on?
What can I build in parallel?
What blocks my work?
What integration gate validates it?
What priority is it?
```

The final V2 release is successful only when the Framework and Server release criteria from the V2 specification both pass.
