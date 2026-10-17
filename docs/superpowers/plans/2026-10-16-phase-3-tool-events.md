# Phase 3: Tool-Call Events, Records and Provider Timeline (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. Each task is one independently reviewable deliverable with its own tests and its own commit. Tasks run in order: 3.1 → 3.2a → 3.2b → 3.3a → 3.3b → 3.4 → 3.5. Task 3.4 depends only on main and may run in parallel with 3.2a–3.3b on a separate worktree (rebase before 3.5).

**Goal:** The tool loop reports every tool call it runs (G1): the client gets Pipecat's `llm-function-call-started` / `-in-progress` / `-stopped` messages, every call is stored in `tool_calls` and shown on the console's Call page (A3), MCP trace spans say which tool ran (A3), and the per-call timeline records provider retries and circuit-state changes while the four never-recorded event kinds are deleted (D-0.5).

**Architecture:** The tool loop gains one derived-loop option, `WithFrames(emit)`. A call the loop claims while its generation is current is *announced* (`*core.ToolCallFrame`), and every announced call is *settled* exactly once (`*core.ToolResultFrame`: when its result is committed, or for an async tool when its background run ends). The three LLM providers re-enter those frames into the session at the call's generation. Both frames survive the generation fence once announced, so a call cut off by barge-in is still reported and settles (`cancelled: true`), as Pipecat's `FunctionCallCancelFrame` does. The transport turns the frames into RTVI messages carrying only the `tool_call_id` (Pipecat's default report level `none`). The server's turn recorder turns them into `tool_calls` rows (name, status, timing; no content), written after the turn row they reference. `TurnDTO.tools` is derived from those rows at read time. `resilience.Policy.OnRetry` and the existing breaker observers feed two timeline event kinds.

**Tech Stack:** Go 1.27 workspace (`core`, `providers/*`, `server`, root module for `transport`/`tools`), protobuf RTVI 2.1.0 frames, SQLite/Postgres (no migration), React/TS/zod/Vitest (`ui/`), Playwright (`e2e/`), Node 22 with the pinned `@pipecat-ai/client-js@1.13.1` (`tools/compat`), the pinned `pipecat-impl` venv (`make parity-regen`).

**Spec:** `docs/superpowers/specs/2026-10-15-deep-review.md` items **A3**, **G1** (rows "`ToolCallFrame` / `ToolResultFrame`" and "`tool_calls` table / `AddToolCall`"), the G1 tail "timeline events", and master decision **D-0.5**. Master plan: `docs/superpowers/plans/2026-10-15-review-remediation.md` Phase 3. Mapping: master 3.1 → Task 3.1; master 3.2 → Tasks 3.2a (wire) and 3.2b (real client, Pipecat goldens, parity row); master 3.3 → Tasks 3.3a (server) and 3.3b (UI); master 3.4 → Task 3.4; the phase gate is Task 3.5. The deep-review B section has no tool-call item; nothing is taken from it.

## Global Constraints

Copied from `AGENTS.md` and the master plan. Every task inherits them.

- `core/` never imports `server/`. Everything added to `core/` is consumed by `server/` on a call path: `ToolLoop.WithFrames` by `providers/google` (`google.llm`, `echo.llm`), `providers/openai` (`openai.llm`, `nvidia.llm`) and `providers/xai` (`xai.llm`, `offline.llm`), all built by `server/registry.go`; `ToolCallFrame`/`ToolResultFrame.SurvivesGenerationFence` by every call session; `resilience.Policy.OnRetry` by the LLM and TTS factories in `server/registry*.go` and `server/nvidia.go`.
- Every queue, channel, map and slice is bounded: the transport's announced-call set (64), the turn recorder's pending calls (64), settled calls and row ids (32 turns), the recorder's write channel (64, as today). No new goroutine: the frames ride the loop's existing goroutines and the session queue.
- No `context.Context` in new struct fields. `defer cancel()` immediately after a derived context (the recorder's per-write timeout keeps today's `cancel()` at the end of the loop body).
- Never hold a lock across I/O. The recorder's new mutex guards map updates and non-blocking channel sends only; database writes stay on its writer goroutine. `Server.recordCircuit` dedupes under its own `circuitMu` and takes `s.mu` (never both at once) only on a real state change, to read `activeCalls`; `Record` runs after unlock.
- Generation fencing: the loop never announces a call whose generation is stale at claim time (no `started` / `in-progress`); only announced calls are settled; once announced, both frames are delivered whatever the generation (the call may already be running on the MCP server, so the client and the record must hear of it).
- Monotonic time for durations (`time.Since`, timers). Tool timestamps are the loop's `cfg.Now()` values, as today.
- Errors wrapped with `%w`; callbacks into server code from core (`Policy.OnRetry`) are panic-contained.
- `go test -race` in every module (`make test`) and an empty `gofmt -l .`.
- Conventional commits, one logical change each. Commit trailer: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Provider secrets come from the environment only. Never write or store keys. Every test uses fakes and placeholder keys.
- RTVI 2.1.0 stays compatible with the unmodified pinned client; the new messages are ones the pinned client already handles (`dist/index.js:2448-2468`).
- **Gate rules (user requirement, binding for every task).** Run on an otherwise idle machine, one target at a time (a developer's own dev server, `make dev` / `bin/server`, may be running on this machine: leave it alone). Tee every run's full output to `.superpowers/sdd/2026-10-15-review-remediation/flake/<task>-<step>-<target>.log` (gitignored; `mkdir -p` it first) with `set -o pipefail`, and on any failure name the failing test (`grep -nE -- '--- FAIL|^FAIL|✘' <log>`) in the task report; a rerun never replaces the failing log. After **every** task run `make verify`. After every task that **changes behaviour** (3.1, 3.2a, 3.3a, 3.3b, 3.4, 3.5) also run `make e2e-echo` (Go e2e + Playwright flows + one-port) and `make offline-test` (the offline stack is up on 127.0.0.1:8099: never run `offline-up` or download models). **Never** run paid or live targets: `make e2e`, `make e2e-xai`, `live*`, `agent-matrix`, `provider-matrix`, `integration`-tagged tests.
- **Per-phase gate:** an independent adversarial reviewer agent audits the phase diff (Task 3.5) before the phase is done.

The gate block every task's "Gate" step runs (substitute the task id and step):

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
make verify 2>&1 | tee "$L/3.X-sN-verify.log"
# behaviour-changing tasks only:
make e2e-echo 2>&1 | tee "$L/3.X-sN-e2e-echo.log"
docker ps --format '{{.Names}}' | grep -qE 'offline-(adapter|speaches)-1' && make offline-test 2>&1 | tee "$L/3.X-sN-offline-test.log"
```

Expected: `=== All verify checks passed ===`; `make e2e-echo` exits 0 with every Playwright test passed; `make offline-test` prints `ok` for both packages. `make offline-test` is a regression check only in this phase (see Test strategy, L5).

## Review Focus

These are the failure modes this phase must not ship. Each is pinned by named tests.

1. **Stale generations (master Review Focus 3, refined).** A call whose generation is stale when the loop claims it is never announced, so the client sees no `started`/`in-progress` for it and its result sends nothing (the transport and the recorder only settle ids they saw announced). An announced call is always reported and settled exactly once, even when barge-in lands between the claim and delivery (both frames survive the fence) or the loop is closed under an async call; a call cut off by barge-in settles with `cancelled: true` (Pipecat broadcasts `FunctionCallCancelFrame`, a `SystemFrame`, `services/llm_service.py:2115-2135`, `frames/frames.py:1472`). A stale call frame never triggers turn-completion leniency. Pinned by `TestToolLoopReportsNothingForAStaleGeneration`, `TestToolLoopSettlesAnAnnouncedCallCancelledOnBargeIn`, `TestAsyncToolIsSettledCancelledWhenTheLoopCloses` (3.1), `TestToolFramesSurviveTheGenerationFence` (3.1), `TestToolResultOfAnInterruptedCandidateStillReachesTheSession`, `TestToolCallOfAnAbandonedCandidateStillReachesTheSession`, `TestCompletion_AToolCallOfAnotherTurnIsNotToolFirst` (3.1), `TestFunctionCallStoppedOnlyForAnnouncedCalls` (3.2a), `TestMCPCallBargeInSettlesTheAnnouncedCallAndNothingElse` (3.2a; every e2e sequence is re-checked after an extra turn, so a late or duplicate event fails it).
2. **Privacy.** No tool name, arguments or result ever reaches the client, no arguments or result are stored, and a tool name the model invented (unknown or invalid) is never stored or traced, with redaction on or off. MCP and built-in tool names (operator-defined) are stored. Pinned by `TestToolLoopAnnouncesAnUnknownToolByItsCategory` (3.1), `TestTurnRecorderNeverStoresAModelInventedToolName` (3.3a), `TestFunctionCallMessagesCarryTheIDOnly` (3.2a), `TestMCPCallToolEventsCarryNoToolContent` (3.2a, redaction on, a card number in the arguments), the Pipecat goldens `function_calls/*` (3.2b), `TestMCPCallToolCallsAreRecorded` (3.3a: empty summaries).
3. **Ordering for sequential, parallel and async calls.** Calls are announced in claim order before any of them runs; results are settled in call order (commits are gated in call order); an async tool is settled when its background run ends, never by its acknowledgement. Pinned by `TestToolLoopReportsEachCallThenItsResult`, `TestParallelRoundReportsCallsInClaimOrderAndResultsInCallOrder`, `TestAsyncToolIsSettledWhenItsBackgroundRunEnds` (3.1), `TestMCPCallParallelToolsEachGetTheirEvents`, `TestMCPCallAsyncToolStopsWhenItsBackgroundRunEnds` (3.2a), `TestMCPCallPipecatClientSeesFunctionCallEvents` (3.2b).
4. **Persistence.** A tool row is written after the turn row it references (FK `turn_id → turns(id) ON DELETE RESTRICT`), a call settled after its turn's row (async, barge-in) still joins that turn, a call whose turn never gets a row is kept unattached, a call still running at hang-up is stored `cancelled`, the recorder's maps are bounded, and `close` cannot race the run loop. Pinned by `TestTurnRecorderStoresToolCallsWithTheirTurn` (3.3a), `TestToolCallRowsFollowTheirTurn` (3.3a, SQLite and, with `TEST_POSTGRES_DSN`, Postgres), `TestMCPCallBargeInRecordsTheToolCancelled`, `TestMCPCallAsyncToolIsRecordedWhenItEnds` (3.3a), `TestTurnsCarryTheirToolCalls` (3.3a).
5. **Timeline hooks.** A retry is recorded once per retry with stage and provider; a circuit change is recorded once per change, only on live calls whose stages use that provider; the registry hook is race-free (`atomic.Pointer`) and a panicking hook cannot fail a request. Pinned by `TestExecuteReportsEachRetryBeforeItsBackoff`, `TestExecuteContainsAPanickingRetryHook`, `TestCircuitStateGoesOnTheTimelinesOfCallsUsingThatProviderOncePerChange` and `TestCallBehaviorTimelineShowsProviderRetriesAndTheCircuitOpening` (3.4).

---

## Architecture

### What the code does today (verified 2026-10-16 on `feat/phase-3-tool-events` @ 03eb695)

- `core/frame.go:375-393` defines `ToolCallFrame{Call, Timestamp}` and `ToolResultFrame{Turn, Gen, Result, Timestamp}`. Nothing in production posts them; `tools/compat/whisker/conformance.go` and `core/turn_completion_test.go` do. Their consumers already exist and are dead: `core/session.go:974-990` (`frameTrace`: `tool.requested` / `tool.complete` / `tool.failed`), `core/session.go:919-923` (trace `ToolCallID`), `core/assistant_context.go:206` (turn-completion tool-first leniency).
- `core/tools/loop.go`: `Run` (:200) → `runRound` (`parallel.go:43`). Sequential: `execute` (:309) = `begin` (:320, claims the call into history) + `finish` (:347, validates, runs the executor at :391-437, commits). Parallel: claims every call in order, then `finish` per call on its own goroutine with commits gated in call order (`parallel.go:63-110`). Async: `finish` → `startAsync` (`parallel.go:137`, commits an ack `{"status":"running","async":true}`) → `runAsync` (:169, the real result goes to `AsyncSink` or history). Replays (`begin` returns `done`) never re-run. Nobody is told about any of it except the `tool.execute` trace span.
- Providers derive a per-generation loop: `providers/google/llm.go:571` (`WithTraceContext(genCtx).WithCurrentFence(...)`; re-enters its own frames at `p.index+1`), `providers/openai/llm.go:402` and `providers/xai/llm.go:367` (re-enter at `p.index`; their `ProcessFrame` passes unknown frames through, `openai/llm.go:192`). The `echo` provider is the real Google processor against `providers/echo/llm.go`, which calls exactly one tool per message (`toolCall`, :165).
- `core/session.go:732` drops an envelope whose generation is stale unless the frame is a `FenceSurvivor` (:1258). `core/llm_failover.go:241` drops every frame from a finished or abandoned attempt; a non-error, non-usage frame commits the generation (:252-256), so a `ToolCallFrame` already counts as output (the `FirstTokenTimeout` doc at :44-47 says a tool call is output).
- `transport/rtvi/rtvi.go:23-40` has no function-call message types; `transport/ws/transport.go:936-1110` has no `FrameToolCall`/`FrameToolResult` case (they fall to `default` and pass).
- `server/call.go:59-66` builds a `turnRecorder` (:94-153) that writes turn rows only. `server/storage/history.go:568` `AddToolCall` has no production caller; `tool_calls` (`server/migrations/{sqlite,postgres}/001_initial.sql:12`) has `id, session_id, turn_id (FK turns, RESTRICT), mcp_server_id, remote_name, schema_hash, started_at, ended_at, status, arguments_summary, result_summary` and needs no migration. Retention already purges its summaries with transcripts (`server/storage/retention.go:70,131-142`) and deletes rows before turns (:284-298). `server/api/traces.go:360-380` reads `TurnRecord.ToolsJSON`, which is always `"[]"`.
- MCP spans are written by the trace adapter when no session tracer is set (`server/mcp/service.go:592-612` falls back to `s.tracer`, the adapter); its whitelist (`server/trace/adapter.go:218`) drops `mcp.server_id`, `mcp.tool_name` and `mcp.duration_ms`.
- UI: `ui/src/routes/sessions/callStats.ts:142-148` (`turnRows`) and `ui/src/components/timeline/TurnTimeline.tsx:74-185` already render `traces.tool_calls`, but expect statuses `completed`/`success`/`error` that nothing writes, and the zod `turnSchema.tools` is `z.unknown()` (`ui/src/api/client.ts:405`).
- `server/timeline.go:21-32` declares `EventSTTRotation`, `EventLLMStart`, `EventTTSAudio`, `EventContextEviction`, `EventProviderRetry`, `EventCircuitState`; none is recorded (`EventLLMStart`/`EventTTSAudio` appear only in `server/route_obs_test.go:17-20` and `server/timeline_test.go:95`). `TimelineEvent.Provider` is never set. Retries happen only in `core/resilience/resilience.go:209-288` (`Execute`); breakers report state through one observer each, set in `server/registry.go:859-889` (google, xai, echo, offline) and `server/nvidia.go:38`.
- Pipecat (`pipecat-impl` @ 2967e1c): `processors/frameworks/rtvi/observer.py:85-101,203-205` defaults `function_call_report_level` to `{"*": NONE}`: `started` with no data, `in-progress` with `tool_call_id`, `stopped` with `tool_call_id` and `cancelled` (:522-590, `models.py:266-334`, `exclude_none=True` at :412). `services/llm_service.py:1611` sends one `FunctionCallsStartedFrame` per batch before the per-call `FunctionCallInProgressFrame` (:1732). The pinned client (`examples/rtvi_client/node_modules/@pipecat-ai/client-js/dist/index.js:2448-2468`) fires `onLLMFunctionCallStarted`/`InProgress`/`Stopped`; it calls a registered client-side handler only when `function_name` is present (:2499-2505). The legacy `llm-function-call` is not sent by Pipecat's observer and is not sent here.
- `docs/privacy.md` promises: "Tool arguments/results are not stored by the server and are not exported to OTel".

### Decision 1: the loop announces and settles; providers deliver

```go
// core/tools (Task 3.1)
func (l *ToolLoop) WithFrames(emit func(core.Frame)) *ToolLoop
func (l *ToolLoop) announce(call ToolCall, started time.Time) bool       // ToolCallFrame iff emit != nil && isCurrent(call)
func (l *ToolLoop) report(call ToolCall, res ToolResult, err error)      // ToolResultFrame; err → failed "context_error"
```

- `execute` and the parallel claim loop call `announce` right after a successful claim, in call order, before any call runs. `finish` gets `announced bool` and settles through a deferred `report` unless the call became an async background run; `runAsync` settles it when the run ends. Replays, malformed calls (`ErrMalformedToolCall`) and stale-at-claim calls are never announced.
- Validation failures (unknown tool, bad arguments) are announced and settled `failed`, as Pipecat announces a call to a missing function and settles it with an error. A name the loop has no definition for (unknown or invalid) is announced as `unknown_tool`, never as the model's text: a model can put the caller's words into a tool name, and `tool_calls.remote_name` is not purged by retention. The recorder applies the same rule to any result with category `unknown_tool` / `invalid_tool_name` (defence in depth). The per-frame `tool.requested` span carries the name in `component`, but per-frame spans are never exported (`server/otelexport/otelexport.go:198-199`) and the trace adapter is not a session tracer, so the announce-time neutralisation covers it.
- Providers: `.WithFrames(func(f core.Frame) { _ = <session>.Reenter(f, core.Downstream, <their index>, gen) })`, the same `Reenter` they use for their text. A `Reenter` that fails (session closed, queue full after the session's 2 s producer budget) drops the frame; that is the same contract the providers' text frames have.
- `ToolCallFrame` and `ToolResultFrame` both implement `core.FenceSurvivor`. The fence that matters is `announce`'s (claim time): `Interruption` is a control frame and jumps the data queue, so a `ToolCallFrame` queued just before a barge-in would otherwise be dropped at `processEnvelope` while its executor already runs, losing the client's `started` and the `tool_calls` row. `core.LLMFailover.fromCandidate` forwards a `ToolResultFrame` from any attempt and a `ToolCallFrame` from a finished or abandoned one (a current attempt's call still commits the generation, as today).
- Surviving the fence changes one consumer: the only non-test handler of `FrameToolCall` besides the transport and the recorder is the turn-completion leniency (`core/assistant_context.go:206`), which gets a stale-turn guard (`isStaleTurn`, :418). The idle controller, async trigger and function-call mute never see these frames (they sit before the LLM, and mute uses the `ToolActivity` executor wrapper, `server/registry.go:595`); TTS and the unit-failure reporter pass them through.
- `runAsync` settles an announced call `cancelled` when the loop is closed under it (route flip, stage swap, teardown), and `startAsync`'s "closed meanwhile" branch settles it `cancelled` itself (the model still gets the ack it already has in history), so "exactly once" holds through `Close`.

### Decision 2: RTVI at Pipecat's default report level `none` (privacy default)

`transport/rtvi.Handler.FunctionCallStarted(id)` returns `llm-function-call-started` (`"data":{}`) and `llm-function-call-in-progress` (`{"tool_call_id":id}`) and remembers `id` (bounded to 64); `FunctionCallStopped(id, cancelled)` returns `llm-function-call-stopped` (`{"tool_call_id":id,"cancelled":bool}`) for a remembered id only. Evidence for the default: Pipecat's own default is `NONE` ("most secure"); tool arguments and results in this product carry caller data (the dental demo books patients by name); `docs/privacy.md` already promises tool content is not stored or exported; the pinned client needs `function_name` only for client-executed functions, which this server does not run (it ignores `llm-function-call-result`). No configuration knob is added (YAGNI: no client consumes names or arguments); ruling Q1.

### Decision 3: records are metadata only; the turn's `tools` is derived

- The turn recorder (already the pipeline stage just before the transport) pairs `ToolCallFrame` and `ToolResultFrame` by `CallID`, maps an MCP tool's model-facing name back to `(mcp_server_id, remote_name, schema_hash)` through `runtime.Session.MCPTool`, and writes `ToolCallRecord{ID: storage.NewID(), TurnID: <turn row id>, …, Status, StartedAt, EndedAt}` with empty summaries. A built-in tool (`transfer_to_<step>`, `end_call`, `send_dtmf`) is stored under its own name with no server.
- Write order: the turn row and its settled calls are one `turnWrite`; calls settled later join the remembered row id; calls whose turn never gets a row (an interrupted turn with no reply, a hang-up) are written unattached at `close`, still-running ones as `cancelled`.
- Accepted best-effort losses (as for turn rows today): a full write queue (64) drops the write (`sendLocked` never blocks the run loop); a late call whose turn row insert failed fails its own insert on the foreign key and is logged ("tool call not recorded"). No per-row "stored" flag: the writer already detaches the calls written together with a failed turn row, and the late case needs a database error first.
- `GET /sessions/{id}/turns` fills `TurnDTO.Tools` (`[{name, status, duration_ms}]`) from `tool_calls` at read time, so a late call is never missing from its turn and there is one source of truth. `TurnRecord.ToolsJSON` / `turns.tools_json` stay as written today (`"[]"`) and are removed by master Task 5.4 (deferred, listed in 3.5).
- No migration: there is no `tool_call_id` column, and none is added (nothing needs to correlate a stored row with a wire id).

### Decision 4: retries through the policy, circuit states through the existing observers

- `resilience.Policy.OnRetry func(retry int)` runs just before `Execute` backs off for retry `retry` (1 = the second attempt), panic-contained. `StageSpec.OnRetry` carries it; `BuildCallPipeline`'s `build` closure sets it for every candidate of every step from `CallSpec.OnProviderRetry`; the LLM and TTS factories copy it into their provider's `RetryPolicy`. The server records `provider_retry` with `Provider` and `Details: "<stage> retry <n>"`. STT stream dials are not wired (deferred, 3.5).
- `Registry.onCircuit atomic.Pointer[func(stage, provider string, st resilience.State)]` is called by every observer `BindMetrics` and `nvidiaBreaker` already install. `Server.recordCircuit` keeps the last state per `stage/provider` (unknown counts as `CLOSED`), so only changes are recorded, and writes `circuit_state` (`Provider`, `Details: "<stage> <closed|open|half_open>"`) on the timelines of live calls whose start step lists that provider for that stage. Breakers without an observer (OpenAI per base URL, Deepgram, Cartesia, ElevenLabs) are not covered: their calls get retries but no circuit events; they also have no circuit gauge (ruling Q5).
- `EventSTTRotation`, `EventLLMStart`, `EventTTSAudio`, `EventContextEviction` are deleted.

### Test strategy (layers)

- **L1 unit (TDD, every task):** `core/tools/frames_test.go`, `core/tool_frames_test.go`, `core/llm_failover_test.go`, provider tests (`providers/{echo,openai,xai}`), `transport/rtvi/rtvi_test.go`, `server/turn_recorder_tools_test.go`, `server/runtime/mcp_tool_test.go`, `server/storage/tool_calls_test.go`, `server/api/turn_tools_test.go`, `server/trace/adapter_test.go`, `core/resilience/retry_hook_test.go`, `server/timeline_test.go`, Vitest.
- **L2 Go e2e** (real server, real WebSocket, protobuf frames, echo LLM, `mcptest`): reuses `newMCPCallRig` (`server/mcp_call_e2e_test.go`) and `newFlowRigE2E` (`server/flow_e2e_test.go`), whose `e2eCall` reader gains a capture of `llm-function-call-*` messages. Every new test is named `TestMCPCall…` or `TestCallBehavior…`, so `make test-e2e-go` (regex `…|MCPCall|…|CallBehavior|…`) runs it with no Makefile change. Waits are `eventually` (8 s deadline, 5 ms poll) on observable conditions; no sleep synchronizes anything. Sequences and payloads are asserted exactly, with provider ids normalized to `#1`, `#2` (`fnShape`).
- **L3 protocol:** (a) Pipecat goldens: `tools/compat/parity/gen_goldens.py` drives Pipecat's own `RTVIObserver` (default params) with Pipecat's frames, and `TestParityFunctionCallMessages` compares the Go handler (area `function_calls`, 3 scenarios, 1 declared difference); (b) the real pinned `@pipecat-ai/client-js` against the real server (`TestMCPCallPipecatClientSeesFunctionCallEvents` runs `tools/compat/test_rtvi_function_calls.js`). `generate_fixtures.js` only frames protobuf audio/messages, which do not change, so it is not extended.
- **L4 browser (Playwright, echo stack):** `e2e/tests-flows/42-tool-calls.spec.ts`: an `mcptest` tool attached through the API (the `mcpFlow` idiom of 30-mcp), a console test call, the test call's existing RTVI event log (no new UI), then the Call page's turn note and technical trace.
- **Turn-completion tool-first leniency (now live) is pinned by unit tests, not e2e:** `TestCompletion_ToolFirstAndSyntheticTurnsNeverHold` (existing: a tool call before a `◐` reply speaks at once), `TestCompletion_AToolCallOfAnotherTurnIsNotToolFirst` (3.1), `TestRealLLMProcessorReportsToolFramesBeforeItsAnswer` (3.1: the real Google processor emits the call frame before its text) and `TestMCPCallToolFramesFlowThroughTheCallPipeline` (3.1: the frame crosses the assistant-context stage of a real call). An e2e case cannot tell leniency apart: the echo model never writes a completion marker, and a reply without a marker is spoken at once anyway (`TestCompletion_NoMarkerSpeaksImmediately`), so a `TestCallBehavior…` flow with `turn.stop: [llm-completion, speech-timeout]` passes with or without it. Making echo emit markers is new test-only provider behaviour with no other consumer; not added.
- **L5 offline stack:** `make offline-test` runs for regressions only. Its model (`offline/Modelfile`: `llama3.2:3b`, temperature 0) is not instructed to call tools and no offline test attaches one; making it call a tool deterministically would test the model, not this code. No offline tool case is added.
- **Flake sweep (3.5):** core and transport units `-count=20`, server e2e `-count=3`, Playwright `--repeat-each=3` for the new spec, all `-race`.

---

### Task 3.1: the tool loop announces and settles calls; providers deliver the frames

**Files:**
- Modify: `core/tools/loop.go`, `core/tools/parallel.go`, `core/frame.go`, `core/llm_failover.go`, `core/assistant_context.go`, `providers/google/llm.go`, `providers/openai/llm.go`, `providers/xai/llm.go`, `providers/echo/llm.go`, `server/flow_e2e_test.go` (probe), `docs/pipecat-parity-matrix.md`
- Create: `core/tools/frames_test.go`, `core/tool_frames_test.go`, `server/tool_frames_e2e_test.go`
- Test (append): `core/llm_failover_test.go`, `core/turn_completion_test.go`, `providers/echo/llm_test.go`, `providers/openai/llm_test.go`, `providers/xai/tools_test.go`

**Interfaces:**
- Consumes: `core.ToolCallFrame`, `core.ToolResultFrame`, `core.FenceSurvivor`, `core.SessionController.Reenter`, `ToolLoopConfig.IsCurrent`, the existing `begin`/`finish`/`startAsync`/`runAsync`.
- Produces: `(*ToolLoop).WithFrames(func(core.Frame)) *ToolLoop`; unexported `announce`, `report`; `finish(roundCtx, call, started, announced bool, gate)`; `startAsync(…, announced bool, commit) (ToolResult, settledElsewhere bool, error)`; `runAsync(call, def, started, announced bool)`; `(*core.ToolCallFrame).SurvivesGenerationFence()`, `(*core.ToolResultFrame).SurvivesGenerationFence()`; echo `(*LLMClient).toolCalls`; test helpers `stageProbe.toolFrames(key) []string`.

- [ ] **Step 1: Write the failing loop tests.** Create `core/tools/frames_test.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "govoice/core"
)

// frameLog records what a loop reports: "call:<id>" and "result:<id>:<status>:<category>".
type frameLog struct {
	mu     sync.Mutex
	got    []string
	frames []core.Frame
}

func (f *frameLog) emit(fr core.Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, fr)
	switch v := fr.(type) {
	case *core.ToolCallFrame:
		f.got = append(f.got, "call:"+v.Call.ID)
	case *core.ToolResultFrame:
		f.got = append(f.got, "result:"+v.Result.CallID+":"+string(v.Result.Status)+":"+v.Result.ErrorCategory)
	}
}

func (f *frameLog) seq() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.got, " ")
}

func TestToolLoopReportsEachCallThenItsResult(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		if c.ID == "b" {
			return ToolResult{}, errors.New("backend down")
		}
		return ToolResult{Result: json.RawMessage(`{"ok":true}`)}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}"), testCall("b", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a result:a:succeeded: call:b result:b:failed:executor_error"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	call, res := log.frames[0].(*core.ToolCallFrame), log.frames[1].(*core.ToolResultFrame)
	if call.Call.Generation != 1 || call.Call.TurnID != "t" || call.Timestamp.IsZero() {
		t.Fatalf("call frame %+v", call)
	}
	if res.Turn != "t" || res.Gen != 1 || string(res.Result.Result) != `{"ok":true}` || res.Result.EndedAt.Before(res.Result.StartedAt) {
		t.Fatalf("result frame %+v", res)
	}
}

func TestParallelRoundReportsCallsInClaimOrderAndResultsInCallOrder(t *testing.T) {
	bDone := make(chan struct{})
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
		if c.ID == "a" {
			<-bDone // a finishes after b; its result is still settled first
		} else {
			defer close(bDone)
		}
		return okResult(c.ID), nil
	}), nil, ToolDefinition{Name: "x"})
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}"), testCall("b", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a call:b result:a:succeeded: result:b:succeeded:"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestToolLoopReportsNothingForAStaleGeneration(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		t.Error("stale call executed")
		return ToolResult{}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	_, err := loop.WithCurrentFence(func(ToolCall) bool { return false }).WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "x", "{}")}, nil)
	if !errors.Is(err, ErrStaleToolCall) {
		t.Fatalf("err = %v", err)
	}
	if got := log.seq(); got != "" {
		t.Fatalf("a stale call was reported: %q", got)
	}
}

func TestToolLoopSettlesAnAnnouncedCallCancelledOnBargeIn(t *testing.T) {
	var current atomic.Bool
	current.Store(true)
	started := make(chan struct{})
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
		close(started)
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	}), func(c *ToolLoopConfig) {
		c.IsCurrent = func(ToolCall) bool { return current.Load() }
		c.DefaultCallTimeout, c.RoundTimeout = 5*time.Second, 5*time.Second
	}, ToolDefinition{Name: "slow"})
	go func() { <-started; current.Store(false) }() // barge-in while the call runs
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "slow", "{}")}, nil); !errors.Is(err, ErrStaleToolCall) {
		t.Fatalf("err = %v", err)
	}
	if got, want := log.seq(), "call:a result:a:cancelled:stale_generation"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestToolLoopSettlesAnUnknownToolAsFailed(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "nope", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a result:a:failed:unknown_tool"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestToolLoopReplayReportsNothing(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Result: json.RawMessage(`{}`)}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	call := testCall("a", "x", "{}")
	if _, err := loop.Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.seq(); got != "" {
		t.Fatalf("a replay was reported: %q", got)
	}
}

func TestAsyncToolIsSettledWhenItsBackgroundRunEnds(t *testing.T) {
	release, settled := make(chan struct{}), make(chan struct{})
	var log frameLog
	emit := func(f core.Frame) {
		log.emit(f)
		if _, ok := f.(*core.ToolResultFrame); ok {
			close(settled)
		}
	}
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		<-release
		return ToolResult{Status: ToolSucceeded, Result: json.RawMessage(`{"answer":42}`)}, nil
	}), func(c *ToolLoopConfig) { c.IsCurrent = func(ToolCall) bool { return true } }, ToolDefinition{Name: "report", Policy: ToolPolicy{Async: true}})
	if _, err := loop.WithFrames(emit).Run(context.Background(), []ToolCall{testCall("a", "report", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.seq(); got != "call:a" {
		t.Fatalf("before the background run ends: %q (the acknowledgement must not settle the call)", got)
	}
	close(release)
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("the async call was never settled")
	}
	if got, want := log.seq(), "call:a result:a:succeeded:"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if res := log.frames[1].(*core.ToolResultFrame); string(res.Result.Result) != `{"answer":42}` {
		t.Fatalf("settled with %s", res.Result.Result)
	}
}

func TestToolLoopDerivationsKeepTheFrameSink(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{Result: json.RawMessage(`{}`)}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	derived := loop.WithFrames(log.emit).WithTraceContext(context.Background()).WithCurrentFence(func(ToolCall) bool { return true })
	if _, err := derived.Run(context.Background(), []ToolCall{testCall("a", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := log.seq(), "call:a result:a:succeeded:"; got != want {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

// A model can put the caller's words into a tool name; a name the loop has no definition for is
// announced by its category, never as written.
func TestToolLoopAnnouncesAnUnknownToolByItsCategory(t *testing.T) {
	loop, _ := newTestLoop(t, ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) {
		return ToolResult{}, nil
	}), ToolDefinition{Name: "x"})
	defer loop.Close()
	var log frameLog
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "card_4111111111111111", "{}"), testCall("b", "x", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	var names []string
	for _, f := range log.frames {
		if c, ok := f.(*core.ToolCallFrame); ok {
			names = append(names, c.Call.Name)
		}
	}
	if strings.Join(names, ",") != "unknown_tool,x" {
		t.Fatalf("announced names %q", names)
	}
}

// Closing the loop (route flip, stage swap, teardown) under a running async call settles it once, cancelled.
func TestAsyncToolIsSettledCancelledWhenTheLoopCloses(t *testing.T) {
	running := make(chan struct{})
	var log frameLog
	loop, _ := newParallelLoop(t, ToolExecutorFunc(func(ctx context.Context, _ ToolCall) (ToolResult, error) {
		close(running)
		<-ctx.Done() // Close cancels the async context
		return ToolResult{}, ctx.Err()
	}), func(c *ToolLoopConfig) { c.IsCurrent = func(ToolCall) bool { return true } }, ToolDefinition{Name: "report", Policy: ToolPolicy{Async: true}})
	if _, err := loop.WithFrames(log.emit).Run(context.Background(), []ToolCall{testCall("a", "report", "{}")}, nil); err != nil {
		t.Fatal(err)
	}
	<-running
	loop.Close() // waits for the background run
	if got, want := log.seq(), "call:a result:a:cancelled:cancelled"; got != want {
		t.Fatalf("frames %q, want %q (exactly one settlement, never succeeded)", got, want)
	}
}
```

Create `core/tool_frames_test.go`:

```go
package core_test

import (
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// Once the loop announced a call (its generation was current at claim time), barge-in between the claim
// and delivery must not drop either frame: the executor may already be running. The loop itself refuses to
// announce a call that is stale at claim time (TestToolLoopReportsNothingForAStaleGeneration).
func TestToolFramesSurviveTheGenerationFence(t *testing.T) {
	tail := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(tail)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	stale := sess.CurrentGeneration()
	now := sess.IncrementGeneration()
	call := core.ToolCall{ID: "c1", Name: "x", SessionID: "1", TurnID: "t1", Generation: core.Generation(stale)}
	if err := sess.Reenter(&core.ToolCallFrame{Call: call}, core.Downstream, 0, stale); err != nil {
		t.Fatal(err)
	}
	if err := sess.Reenter(&core.ToolResultFrame{Turn: "t1", Gen: core.Generation(stale), Result: core.ToolResult{CallID: "c1", Status: core.ToolCancelled}}, core.Downstream, 0, stale); err != nil {
		t.Fatal(err)
	}
	if err := sess.Reenter(&core.LLMTextFrame{Text: "marker", Turn: "t2", Gen: now}, core.Downstream, 0, now); err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		calls, results = 0, 0
		marker := false
		for _, f := range tail.GetFrames() {
			switch f.(type) {
			case *core.ToolCallFrame:
				calls++
			case *core.ToolResultFrame:
				results++
			case *core.LLMTextFrame:
				marker = true
			}
		}
		if marker {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the marker never arrived")
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("call frames delivered %d, results %d (want 1 and 1)", calls, results)
	}
}
```

Append to `core/llm_failover_test.go`:

```go
// toolCaller announces one tool call and, once released, settles it at the
// generation it ran in, like a provider's tool loop.
type toolCaller struct {
	s       core.SessionController
	idx     int
	release chan struct{}
	started atomic.Bool
	late    bool // announce only after release too (an attempt abandoned before its call frame)
}

func (p *toolCaller) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *toolCaller) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, d)
	}
	if err := e.Push(f); err != nil {
		return err
	}
	gen := um.Gen
	call := core.ToolCall{ID: "call-1", Name: "lookup", SessionID: "s", TurnID: core.TurnID(um.Turn), Generation: core.Generation(gen)}
	return p.s.Go(func(ctx context.Context) {
		if !p.late {
			_ = p.s.Reenter(&core.ToolCallFrame{Call: call}, core.Downstream, p.idx+1, gen)
		}
		p.started.Store(true)
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		if p.late {
			_ = p.s.Reenter(&core.ToolCallFrame{Call: call}, core.Downstream, p.idx+1, gen)
		}
		_ = p.s.Reenter(&core.ToolResultFrame{Turn: call.TurnID, Gen: call.Generation, Result: core.ToolResult{CallID: call.ID, Status: core.ToolCancelled}}, core.Downstream, p.idx+1, gen)
	})
}

func countOf[T core.Frame](frames []core.Frame) int {
	n := 0
	for _, f := range frames {
		if _, ok := f.(T); ok {
			n++
		}
	}
	return n
}

func TestToolResultOfAnInterruptedCandidateStillReachesTheSession(t *testing.T) {
	primary := &toolCaller{release: make(chan struct{})}
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary}, {Provider: "b", Proc: fake([]string{"x"}, 0, nil)}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() && countOf[*core.ToolCallFrame](r.rec.GetFrames()) == 1 })
	if err := r.sess.Post(&core.InterruptionFrame{}); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, func() bool { return countOf[*core.InterruptionFrame](r.rec.GetFrames()) >= 1 })
	close(primary.release)
	r.waitFor(t, func() bool { return countOf[*core.ToolResultFrame](r.rec.GetFrames()) == 1 })
}

func TestToolCallOfAnAbandonedCandidateStillReachesTheSession(t *testing.T) {
	primary := &toolCaller{release: make(chan struct{}), late: true}
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary}, {Provider: "b", Proc: fake([]string{"x"}, 0, nil)}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() })
	if err := r.sess.Post(&core.InterruptionFrame{}); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, func() bool { return countOf[*core.InterruptionFrame](r.rec.GetFrames()) >= 1 })
	close(primary.release) // the attempt is done: its loop announced the call before it saw the interruption
	r.waitFor(t, func() bool {
		f := r.rec.GetFrames()
		return countOf[*core.ToolCallFrame](f) == 1 && countOf[*core.ToolResultFrame](f) == 1
	})
}

// Pin (passes before this task too): a tool call is the candidate's first
// output, so the first-token watchdog never replays a turn whose tool runs long.
func TestToolCallIsTheFirstOutput(t *testing.T) {
	primary, backup := &toolCaller{release: make(chan struct{})}, fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		FirstTokenTimeout: 40 * time.Millisecond,
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() })
	time.Sleep(120 * time.Millisecond) // past the deadline: asserts an absence
	if backup.Started.Load() != 0 {
		t.Fatal("a turn whose model called a tool was replayed on the fallback")
	}
}
```

In `core/turn_completion_test.go`, in `scriptLLM.ProcessFrame`'s chunk loop, directly before `		if c == "<tool>" {` add:

```go
		if c == "<stale-tool>" { // a call frame of another (older) turn, delivered late
			_ = l.sess.Reenter(&core.ToolCallFrame{Call: core.ToolCall{ID: "c0", Name: "t", TurnID: "old-turn"}}, core.Downstream, l.index+1, gen)
			continue
		}
```

and append:

```go
// Call frames now survive the generation fence, so one of an older turn can arrive while a new reply is being
// judged: it must not make that reply tool-first.
func TestCompletion_AToolCallOfAnotherTurnIsNotToolFirst(t *testing.T) {
	r := newComplRig(t, func(turn, _ string) []string {
		if strings.HasPrefix(turn, "completion-") {
			return nil // the nudge never answers in this test
		}
		return []string{"<stale-tool>", "◐ Sunny today."}
	})
	r.say("turn-1", "weather?")
	waitFor(t, "hold", r.asst.Held)
	if u := units(r.shape()); len(u) != 0 {
		t.Fatalf("an older turn's tool call made the reply tool-first: spoke %q", u)
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
cd core && go test -race -count=1 -run 'ToolLoopReports|ParallelRoundReports|ToolLoopSettles|ToolLoopReplayReports|AsyncToolIsSettled|ToolLoopDerivations|ToolLoopAnnounces' ./tools/ ; go test -race -count=1 -run 'ToolFramesSurvive|ToolResultOfAnInterrupted|ToolCallOfAnAbandoned|ToolCallIsTheFirstOutput|AToolCallOfAnotherTurn' . ; cd ..
```

Expected: `./tools` does not compile (`loop.WithFrames undefined`); in `core`, `TestToolFramesSurviveTheGenerationFence` fails (`call frames delivered 0, results 0`), `TestToolResultOfAnInterruptedCandidateStillReachesTheSession` and `TestToolCallOfAnAbandonedCandidateStillReachesTheSession` fail (`condition not met`), `TestCompletion_AToolCallOfAnotherTurnIsNotToolFirst` fails (`timed out waiting for hold`: the stale frame made the reply lenient), `TestToolCallIsTheFirstOutput` passes (pin).

- [ ] **Step 3: Implement the core changes.**

(a) `core/frame.go`: directly after `func (f *ToolCallFrame) Generation() uint64 { return uint64(f.Call.Generation) }` add:

```go

// SurvivesGenerationFence: the tool loop announces a call only while its
// generation is current (ToolLoop.announce), and the executor may already run
// when an interruption (a control frame, ahead of the data queue) moves the
// generation on, so the announcement is delivered and recorded regardless.
func (*ToolCallFrame) SurvivesGenerationFence() {}
```

and directly after `func (f *ToolResultFrame) Generation() uint64 { return uint64(f.Gen) }` add:

```go

// SurvivesGenerationFence: a result settles a call the client already saw, so
// it is delivered even when barge-in moved the generation on (Pipecat settles
// such a call with FunctionCallCancelFrame, a SystemFrame no interruption drops).
func (*ToolResultFrame) SurvivesGenerationFence() {}
```

(b) `core/llm_failover.go`: replace the first line of `fromCandidate`'s body, `	w.mu.Lock()` (the line right after `func (w *LLMFailover) fromCandidate(idx int, f Frame, dir Direction, resume int, gen uint64) error {`), with:

```go
	if _, settles := f.(*ToolResultFrame); settles {
		// An announced call is settled whatever became of its attempt
		// (interrupted, abandoned).
		return w.session.Reenter(f, dir, resume, gen)
	}
	w.mu.Lock()
```

and in the same function replace

```go
	case g.done || g.order[g.pos] != idx:
		// A finished, interrupted or abandoned attempt: drop, whatever its
		// generation (a replay can start under a newer one than g.gen).
		w.mu.Unlock()
		return nil
```

with

```go
	case g.done || g.order[g.pos] != idx:
		// A finished, interrupted or abandoned attempt: drop, whatever its
		// generation (a replay can start under a newer one than g.gen), except
		// a tool call its loop already announced (it may be running).
		w.mu.Unlock()
		if _, announced := f.(*ToolCallFrame); announced {
			return w.session.Reenter(f, dir, resume, gen)
		}
		return nil
```

A current attempt's `ToolCallFrame` still takes the existing path and commits the generation.

(b2) `core/assistant_context.go`: replace

```go
	case FrameToolCall:
		if p.gate != nil && !p.gate.decided {
```

with

```go
	case FrameToolCall:
		// Call frames survive the generation fence: only this reply's own call is tool-first.
		if tc := f.(*ToolCallFrame); p.gate != nil && !p.gate.decided && !p.isStaleTurn(string(tc.Call.TurnID), uint64(tc.Call.Generation)) {
```

(c) `core/tools/loop.go`. In `type ToolLoop struct`, after `	async       *asyncState` add:

```go
	emit        func(core.Frame) // WithFrames; nil reports nothing
```

In `WithCurrentFence` and in `WithTraceContext`, change each `derived := &ToolLoop{definitions: l.definitions, executor: l.executor, history: l.history, cfg: l.cfg, async: l.async}` to:

```go
	derived := &ToolLoop{definitions: l.definitions, executor: l.executor, history: l.history, cfg: l.cfg, async: l.async, emit: l.emit}
```

After the closing brace of `WithTraceContext` add:

```go

// WithFrames derives a loop that reports the calls it runs through emit: a
// *core.ToolCallFrame when it claims a call whose generation is current (an
// announced call), and exactly one *core.ToolResultFrame per announced call
// once its result is final (for an async tool, when the background run ends).
// Replays and stale calls are never announced. emit runs on the loop's
// goroutines; providers re-enter the frames into the session.
func (l *ToolLoop) WithFrames(emit func(core.Frame)) *ToolLoop {
	if l == nil {
		return nil
	}
	return &ToolLoop{definitions: l.definitions, executor: l.executor, history: l.history, cfg: l.cfg, async: l.async, emit: emit}
}

// announce reports a claimed call when frames are wanted and its generation is
// current, and says whether it did; only an announced call is later settled. A
// name the loop has no definition for is announced as "unknown_tool": the model
// wrote it, and it can hold the caller's words.
func (l *ToolLoop) announce(call ToolCall, started time.Time) bool {
	if l.emit == nil || !l.isCurrent(call) {
		return false
	}
	c := cloneCall(call)
	if _, ok := l.definitions[c.Name]; !ok {
		c.Name = "unknown_tool"
	}
	l.emit(&core.ToolCallFrame{Call: c, Timestamp: started})
	return true
}

// report settles an announced call. A loop error (history refused the result)
// settles it as failed with the category context_error.
func (l *ToolLoop) report(call ToolCall, res ToolResult, err error) {
	if err != nil {
		res = ToolResult{CallID: call.ID, Status: ToolFailed, ErrorCategory: "context_error", EndedAt: l.cfg.Now()}
	}
	l.emit(&core.ToolResultFrame{Turn: call.TurnID, Gen: call.Generation, Result: cloneToolResult(res), Timestamp: l.cfg.Now()})
}
```

Replace `execute` and its first comment line:

```go
// execute is begin, announce and finish with no commit gate (sequential path).
// abandonGrace is how long a cancelled executor may take to return before
// its round stops waiting for it.
const abandonGrace = time.Second

func (l *ToolLoop) execute(roundCtx context.Context, call ToolCall) (ToolResult, error) {
	res, done, started, err := l.begin(call)
	if done {
		return res, err
	}
	return l.finish(roundCtx, call, started, l.announce(call, started), nil)
}
```

Replace the `finish` signature and its first three lines (from `func (l *ToolLoop) finish(roundCtx context.Context, call ToolCall, started time.Time, gate func()) (ToolResult, error) {` through `	if gate == nil {`) with:

```go
func (l *ToolLoop) finish(roundCtx context.Context, call ToolCall, started time.Time, announced bool, gate func()) (res ToolResult, err error) {
	async := false // startAsync settled the call elsewhere (background run, or cancelled on close)
	if announced {
		defer func() {
			if !async {
				l.report(call, res, err)
			}
		}()
	}
	if gate == nil {
```

and update the comment above `finish` to: `// finish validates a claimed call, runs the executor (or acknowledges an async tool) and commits the result; an announced call is settled when finish returns. gate, when non-nil, is called before every history write so parallel rounds commit in call order.` Replace the async branch:

```go
	if def := l.definitions[call.Name]; def.Policy.Async {
		gate() // in call order, so MaxAsyncInFlight rejects deterministically
		res, async, err = l.startAsync(call, def, started, announced, commit)
		return res, err
	}
```

(d) `core/tools/parallel.go`. In `runRound` replace the claim loop (from `	started := make([]time.Time, len(calls))` through the closing brace of the `for ; claimed < len(calls); claimed++ {` loop) with:

```go
	started := make([]time.Time, len(calls))
	announced := make([]bool, len(calls))
	claimed := 0
	for ; claimed < len(calls); claimed++ {
		res, fin, at, err := l.begin(calls[claimed])
		started[claimed] = at
		if !fin {
			announced[claimed] = l.announce(calls[claimed], at) // in call order, before any call runs
			continue
		}
		slots[claimed] = slot{res, err, true}
		if failed(slots[claimed]) {
			cancel() // claimed calls still run and commit "cancelled"
			claimed++
			break
		}
	}
```

and change `			res, err := l.finish(rctx, calls[i], started[i], func() {` to `			res, err := l.finish(rctx, calls[i], started[i], announced[i], func() {`. Replace `startAsync` entirely:

```go
// startAsync acknowledges an async call at once and runs the executor in the
// background. It reports true when the announced call is settled elsewhere: by
// the background run, or here as cancelled when the loop closed meanwhile. The real result is injected into history as a user-role context
// message when it arrives. Async calls deliberately ignore the generation
// fence: like Pipecat's cancel_on_interruption=False, they outlive barge-in.
func (l *ToolLoop) startAsync(call ToolCall, def ToolDefinition, started time.Time, announced bool, commit func(ToolStatus, json.RawMessage, string) (ToolResult, error)) (ToolResult, bool, error) {
	a := l.async
	if int(a.inflight.Add(1)) > l.cfg.MaxAsyncInFlight {
		a.inflight.Add(-1)
		res, err := commit(ToolFailed, errorPayload("too_many_async"), "too_many_async")
		return res, false, err
	}
	if l.cfg.OnAsyncStart != nil {
		l.cfg.OnAsyncStart(cloneCall(call))
	}
	ack, err := commit(ToolSucceeded, json.RawMessage(`{"status":"running","async":true}`), "")
	if err != nil {
		a.inflight.Add(-1)
		return ack, false, err
	}
	// Launch only after the ack is in history so the injected message can never
	// precede the tool result it follows.
	a.mu.Lock()
	if a.ctx.Err() != nil { // loop closed meanwhile: never start; the model keeps the ack, the call is settled cancelled
		a.mu.Unlock()
		a.inflight.Add(-1)
		if announced {
			l.report(call, ToolResult{CallID: call.ID, Status: ToolCancelled, Result: errorPayload("cancelled"), ErrorCategory: "cancelled", StartedAt: started, EndedAt: l.cfg.Now()}, nil)
		}
		return ack, true, nil
	}
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer a.inflight.Add(-1)
		l.runAsync(call, def, started, announced)
	}()
	return ack, true, nil
}
```

Change `func (l *ToolLoop) runAsync(call ToolCall, def ToolDefinition, started time.Time) {` to `func (l *ToolLoop) runAsync(call ToolCall, def ToolDefinition, started time.Time, announced bool) {`; replace

```go
	if l.async.ctx.Err() != nil {
		return // loop closed: drop silently
	}
```

with

```go
	if l.async.ctx.Err() != nil { // loop closed: no result reaches history, but an announced call is settled
		if announced {
			l.report(call, ToolResult{CallID: call.ID, Status: ToolCancelled, Result: errorPayload("cancelled"), ErrorCategory: "cancelled", StartedAt: started, EndedAt: l.cfg.Now()}, nil)
		}
		return
	}
```

and directly before `	// The result is model-visible data in a user-role message: keep it inside a` insert:

```go
	if announced {
		l.report(call, res, nil)
	}
```

- [ ] **Step 4: Run the core tests to see them pass.**

```bash
cd core && gofmt -l ./tools . && go vet ./... && go test -race -count=1 ./tools/ && go test -race -count=1 -run 'ToolFramesSurvive|ToolResultOfAnInterrupted|ToolCallOfAnAbandoned|ToolCallIsTheFirstOutput|Failover|Completion' . && cd ..
```

Expected: no gofmt output; `ok govoice/core/tools`; `ok govoice/core`.

- [ ] **Step 5: Write the failing provider and wiring tests.** Append to `providers/echo/llm_test.go`:

```go
func TestEchoCallsEveryMCPToolItsMessageNames(t *testing.T) {
	c := NewLLMClient(LLMOptions{
		Phrases: map[string]string{"call tool lookup_order": "mcp__x_lookup_order", "call tool echo": "mcp__x_echo", "transfer to sales": "transfer_to_sales"},
		Spoken:  map[string]string{"mcp__x_lookup_order": "lookup_order", "mcp__x_echo": "echo"},
	})
	defs := []core.ToolDefinition{{Name: "mcp__x_lookup_order", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", user(`call tool lookup_order {"order_id":"42"} then call tool echo {"text":"hi"} and transfer to sales`), nil, defs, core.ToolCall{})
	var calls []core.ToolCall
	for _, ck := range collect(t, ch, errs) {
		calls = append(calls, ck.ToolCalls...)
	}
	if len(calls) != 2 || calls[0].Name != "mcp__x_lookup_order" || string(calls[0].Arguments) != `{"order_id":"42"}` ||
		calls[1].Name != "mcp__x_echo" || string(calls[1].Arguments) != `{"text":"hi"}` || calls[0].ID == calls[1].ID {
		t.Fatalf("calls %+v", calls)
	}
}

func TestRealLLMProcessorReportsToolFramesBeforeItsAnswer(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	cfg := google.DefaultLLMConfig()
	cfg.Provider = "echo"
	cfg.Client = NewLLMClient(LLMOptions{Phrases: map[string]string{"transfer to sales": "transfer_to_sales"}})
	cfg.ToolDefinitions = tools
	cfg.ToolExecutor = core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	})
	cfg.ToolHistory = history
	cfg.ContextProvider = history.Messages
	proc, err := google.NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	sess, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(sess, 0)
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := history.AddMessage(core.Message{Role: core.RoleUser, Content: "please transfer to sales", TurnID: "turn-1"}); err != nil {
		t.Fatal(err)
	}
	_ = sess.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1, Content: "please transfer to sales"})
	var got []string
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		got = got[:0]
		for _, f := range sink.GetFrames() {
			switch v := f.(type) {
			case *core.ToolCallFrame:
				got = append(got, "call:"+v.Call.Name)
			case *core.ToolResultFrame:
				got = append(got, "result:"+string(v.Result.Status))
			case *core.LLMTextFrame:
				got = append(got, "text")
			}
		}
		if len(got) >= 3 {
			break
		}
	}
	if strings.Join(got, ",") != "call:transfer_to_sales,result:succeeded,text" {
		t.Fatalf("frames %v", got)
	}
}
```

Append to `providers/openai/llm_test.go`:

```go
func TestToolLoopFramesReachTheSession(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if n.Add(1) == 1 {
			sse(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"weather","arguments":"{}"}}]}}]}`, "[DONE]")
			return
		}
		sse(w, `{"choices":[{"delta":{"content":"done"}}]}`, "[DONE]")
	}))
	defer srv.Close()
	defs := []core.ToolDefinition{{Name: "weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := run(t, Config{BaseURL: srv.URL, ToolDefinitions: defs, ToolLoop: loop, HTTPClient: srv.Client()})
	var got []string
	for _, f := range s.got {
		switch f := f.(type) {
		case *core.ToolCallFrame:
			got = append(got, "call:"+f.Call.ID)
		case *core.ToolResultFrame:
			got = append(got, "result:"+f.Result.CallID+":"+string(f.Result.Status))
		case *core.LLMTextFrame:
			got = append(got, "text:"+f.Text)
		}
	}
	if strings.Join(got, ",") != "call:call_a,result:call_a:succeeded,text:done" {
		t.Fatalf("frames %v", got)
	}
}
```

Append to `providers/xai/tools_test.go`:

```go
func TestXAIToolLoopFramesReachTheSession(t *testing.T) {
	var n atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{}\"}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{}\"}]}}\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Sunny.\"}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output_text\":\"Sunny.\"}}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defs := []core.ToolDefinition{{Name: "weather", Description: "Get weather", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"weather":"sunny"}`)}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: server.URL, Model: "grok-test", HTTPClient: server.Client(), ToolDefinitions: defs, ToolLoop: loop})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(3, 4)
	if err := proc.Attach(session, 0); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn-1", Gen: 4, Content: "weather"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatal(err)
	}
	session.taskWg.Wait()
	var got []string
	session.reenteredMu.Lock()
	for _, f := range session.reentered {
		switch f := f.(type) {
		case *core.ToolCallFrame:
			got = append(got, "call:"+f.Call.ID)
		case *core.ToolResultFrame:
			got = append(got, "result:"+f.Result.CallID+":"+string(f.Result.Status))
		}
	}
	session.reenteredMu.Unlock()
	if strings.Join(got, ",") != "call:call_1,result:call_1:succeeded" {
		t.Fatalf("frames %v", got)
	}
}
```

In `server/flow_e2e_test.go`: in `type stageProbe struct`, after the `said` field add `	tools         map[string][]string // tool calls and results each stage saw ("call:<name>", "result:<status>")`; in `newProbe` add `tools: map[string][]string{}, ` to the literal (next to `said: map[string][]string{},`); in `probedStage.ProcessFrame`, before `	return s.Processor.ProcessFrame(ctx, f, dir, e)` add:

```go
	switch v := f.(type) {
	case *core.ToolCallFrame:
		s.probe.mu.Lock()
		s.probe.tools[s.key] = append(s.probe.tools[s.key], "call:"+v.Call.Name)
		s.probe.mu.Unlock()
	case *core.ToolResultFrame:
		s.probe.mu.Lock()
		s.probe.tools[s.key] = append(s.probe.tools[s.key], "result:"+string(v.Result.Status))
		s.probe.mu.Unlock()
	}
```

and after `sayings` add:

```go
// toolFrames is the tool calls and results a stage saw, in order.
func (p *stageProbe) toolFrames(k string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tools[k]...)
}
```

Create `server/tool_frames_e2e_test.go` (L2 skeleton: the frames cross a real call pipeline):

```go
package server

import (
	"strings"
	"testing"
)

// The MCP tool's call and result pass the voice stage of a real call, between the model and the transport.
func TestMCPCallToolFramesFlowThroughTheCallPipeline(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	eventually(t, "the call and its result at the voice stage", func() bool { return len(r.probe.toolFrames("v")) >= 2 })
	got := r.probe.toolFrames("v")
	if len(got) != 2 || !strings.HasPrefix(got[0], "call:mcp__") || !strings.Contains(got[0], "_lookup_order") || got[1] != "result:succeeded" {
		t.Fatalf("tool frames at the voice stage = %q", got)
	}
}
```

- [ ] **Step 6: Run them to see them fail.**

```bash
(cd providers/echo && go test -race -count=1 -run 'EchoCallsEveryMCPTool|ReportsToolFrames' .)
(cd providers/openai && go test -race -count=1 -run ToolLoopFramesReachTheSession .)
(cd providers/xai && go test -race -count=1 -run XAIToolLoopFramesReachTheSession .)
(cd server && go test -race -count=1 -run TestMCPCallToolFramesFlowThroughTheCallPipeline .)
```

Expected: `TestEchoCallsEveryMCPToolItsMessageNames` fails (`calls [{… lookup_order …}]`, one call); `TestRealLLMProcessorReportsToolFramesBeforeItsAnswer`, `TestToolLoopFramesReachTheSession`, `TestXAIToolLoopFramesReachTheSession` fail (no `call:`/`result:` entries); the server test times out waiting for the tool frames.

- [ ] **Step 7: Implement the provider wiring and the echo multi-call.**

`providers/google/llm.go`: replace

```go
			loop = loop.WithTraceContext(genCtx).WithCurrentFence(func(call core.ToolCall) bool { return p.isCurrentGeneration(uint64(call.Generation)) })
```

with

```go
			loop = loop.WithTraceContext(genCtx).WithCurrentFence(func(call core.ToolCall) bool { return p.isCurrentGeneration(uint64(call.Generation)) }).
				WithFrames(func(f core.Frame) { _ = p.session.Reenter(f, core.Downstream, p.index+1, gen) })
```

`providers/openai/llm.go`: replace

```go
				loop := p.cfg.ToolLoop.WithTraceContext(streamCtx).WithCurrentFence(func(c core.ToolCall) bool {
					return c.SessionID == core.SessionID(sid) && c.TurnID == core.TurnID(turn) && c.Generation == core.Generation(gen) && p.isCurrent(gen)
				})
```

with

```go
				loop := p.cfg.ToolLoop.WithTraceContext(streamCtx).WithCurrentFence(func(c core.ToolCall) bool {
					return c.SessionID == core.SessionID(sid) && c.TurnID == core.TurnID(turn) && c.Generation == core.Generation(gen) && p.isCurrent(gen)
				}).WithFrames(func(f core.Frame) { _ = s.Reenter(f, core.Downstream, p.index, gen) })
```

`providers/xai/llm.go`: replace

```go
				currentLoop := p.cfg.ToolLoop.WithTraceContext(streamCtx).WithCurrentFence(func(call core.ToolCall) bool {
					return call.SessionID == core.SessionID(fmt.Sprint(s.ID())) && call.TurnID == core.TurnID(turn) && call.Generation == core.Generation(gen) && p.isCurrent(gen)
				})
```

with

```go
				currentLoop := p.cfg.ToolLoop.WithTraceContext(streamCtx).WithCurrentFence(func(call core.ToolCall) bool {
					return call.SessionID == core.SessionID(fmt.Sprint(s.ID())) && call.TurnID == core.TurnID(turn) && call.Generation == core.Generation(gen) && p.isCurrent(gen)
				}).WithFrames(func(f core.Frame) { _ = s.Reenter(f, core.Downstream, p.index, gen) })
```

`providers/echo/llm.go`: in `stream`, replace

```go
		if call, ok := c.toolCall(history, hasTools); ok {
```

with `		if calls := c.toolCalls(history, hasTools); len(calls) > 0 {`, and in that block replace `ToolCalls: []core.ToolCall{call}` with `ToolCalls: calls`. Replace the whole `toolCall` function (its comment and body, through its closing brace) with:

```go
// toolCalls is what the user's own last message asks for, unless the model is already answering a
// tool result: every attached MCP tool whose phrase it contains, in the order the phrases appear, each
// with the {json} that follows its phrase (several make one parallel round); otherwise the first other
// phrase (a transfer, end_call) in sorted order, so the choice is deterministic.
// ponytail: offsets assume ToLower keeps byte lengths (ASCII messages); otherwise every call gets the
// message's first {json}.
func (c *LLMClient) toolCalls(history []core.Message, hasTools bool) []core.ToolCall {
	// Only the user's own last message asks for a tool: an older one must not call it again.
	if !hasTools || len(history) == 0 {
		return nil
	}
	if last := history[len(history)-1]; last.Role != core.RoleUser || last.Kind != "" && last.Kind != core.MessageText {
		return nil
	}
	msg := lastUser(history)
	lower := strings.ToLower(msg)
	type hit struct {
		at   int
		name string
	}
	var mcp []hit
	for _, p := range c.phrases {
		name := c.o.Phrases[p]
		if _, spoken := c.o.Spoken[name]; !spoken {
			continue
		}
		if at := strings.Index(lower, p); at >= 0 {
			mcp = append(mcp, hit{at + len(p), name})
		}
	}
	if len(mcp) > 0 {
		sort.Slice(mcp, func(i, j int) bool { return mcp[i].at < mcp[j].at })
		calls := make([]core.ToolCall, 0, len(mcp))
		for _, h := range mcp {
			args := toolArguments(msg)
			if len(lower) == len(msg) {
				args = toolArguments(msg[h.at:])
			}
			calls = append(calls, c.call(h.name, args))
		}
		return calls
	}
	for _, p := range c.phrases {
		if strings.Contains(lower, p) {
			return []core.ToolCall{c.call(c.o.Phrases[p], json.RawMessage(`{}`))}
		}
	}
	return nil
}

func (c *LLMClient) call(name string, args json.RawMessage) core.ToolCall {
	return core.ToolCall{ID: "echo-call-" + strconv.FormatUint(callSeq.Add(1), 10), Name: name, Arguments: args}
}
```

(`sort` is already imported for `NewLLMClient`.)

`docs/pipecat-parity-matrix.md`: in the "LLM turn completion" row replace `Tool-first leniency is reachable only in tests: no provider emits `ToolCallFrame`.` with `A tool call before any text completes the turn (tool-first leniency): the providers' tool loops emit `ToolCallFrame`.`; in "Known gaps and risks" delete the bullet that starts `- **No provider emits `ToolCallFrame`**`.

- [ ] **Step 8: Run them to see them pass.**

```bash
for m in providers/echo providers/openai providers/xai providers/google; do (cd $m && gofmt -l . && go vet ./... && go test -race -count=1 ./...) || exit 1; done
cd server && go test -race -count=1 -run 'TestMCPCall|TestFlowE2E|TestCallPipelineAsync' . && cd ..
go test -race -count=1 ./tools/compat/...
```

Expected: every package `ok`, no gofmt output (`TestParityMatrix` passes with the edited rows).

- [ ] **Step 9: Gate.** Run the gate block (verify, e2e-echo, offline-test) with `3.1-s9`. Behaviour changes: turn-completion leniency is now live on tool calls.

- [ ] **Step 10: Commit.**

```bash
git add core/tools/loop.go core/tools/parallel.go core/tools/frames_test.go core/frame.go core/tool_frames_test.go core/llm_failover.go core/llm_failover_test.go core/assistant_context.go core/turn_completion_test.go \
  providers/google/llm.go providers/openai/llm.go providers/openai/llm_test.go providers/xai/llm.go providers/xai/tools_test.go providers/echo/llm.go providers/echo/llm_test.go \
  server/flow_e2e_test.go server/tool_frames_e2e_test.go docs/pipecat-parity-matrix.md
git commit -m "feat(core): tool loop reports ToolCallFrame and ToolResultFrame

WithFrames derives a loop that announces each call it claims while the
call's generation is current and settles every announced call exactly
once (an async tool when its background run ends). Replays and stale
calls are never announced; an undefined tool name is announced as
unknown_tool. Both frames survive the generation fence once announced and
the LLM failover forwards them from finished attempts, so a call cut off by
barge-in or a loop close is still reported and settled; turn-completion
leniency ignores another turn's call. The google, openai and xai providers re-enter
the frames; the echo model calls every MCP tool a message names.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3.2a: RTVI function-call messages on the wire

**Files:**
- Modify: `transport/rtvi/rtvi.go`, `transport/ws/transport.go`, `server/flow_e2e_test.go` (wire capture, `voiceToken`), `docs/privacy.md`, `docs/pipecat-compatibility.md`
- Create: `server/tool_events_e2e_test.go`
- Test (append): `transport/rtvi/rtvi_test.go`

**Interfaces:**
- Consumes: `core.FrameToolCall`, `core.FrameToolResult` (3.1), `(*Handler).formatMessage`.
- Produces: `rtvi.TypeLLMFunctionCallStarted`, `TypeLLMFunctionCallInProgress`, `TypeLLMFunctionCallStopped`, `rtvi.FunctionCallInProgressData`, `rtvi.FunctionCallStoppedData`, `(*Handler).FunctionCallStarted(id) []string`, `(*Handler).FunctionCallStopped(id, cancelled) []string`; test helpers `fnEvent`, `(*e2eCall).fnEvents()`, `(*flowRigE2E).voiceToken(flowID)`, `fnShape`, `(*mcpCallRig).liveSession()`, `bargeIn`.

- [ ] **Step 1: Write the failing unit tests.** Append to `transport/rtvi/rtvi_test.go`:

```go
// Pipecat's default function_call_report_level is "none": the call's id only.
func TestFunctionCallMessagesCarryTheIDOnly(t *testing.T) {
	h := rtvi.NewHandler()
	var got []string
	add := func(msgs []string) {
		for _, m := range msgs {
			var msg rtvi.Message
			if err := json.Unmarshal([]byte(m), &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Label != rtvi.LabelRTVI || msg.ID == "" {
				t.Fatalf("envelope %s", m)
			}
			got = append(got, msg.Type+" "+string(msg.Data))
		}
	}
	add(h.FunctionCallStarted("call-1"))
	add(h.FunctionCallStarted("call-2"))
	add(h.FunctionCallStopped("call-1", false))
	add(h.FunctionCallStopped("call-2", true))
	want := []string{
		`llm-function-call-started {}`,
		`llm-function-call-in-progress {"tool_call_id":"call-1"}`,
		`llm-function-call-started {}`,
		`llm-function-call-in-progress {"tool_call_id":"call-2"}`,
		`llm-function-call-stopped {"tool_call_id":"call-1","cancelled":false}`,
		`llm-function-call-stopped {"tool_call_id":"call-2","cancelled":true}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFunctionCallStoppedOnlyForAnnouncedCalls(t *testing.T) {
	h := rtvi.NewHandler()
	if msgs := h.FunctionCallStopped("never-announced", true); len(msgs) != 0 {
		t.Fatalf("stopped a call that was never announced: %v", msgs)
	}
	h.FunctionCallStarted("call-1")
	if msgs := h.FunctionCallStopped("call-1", false); len(msgs) != 1 {
		t.Fatalf("announced call not stopped: %v", msgs)
	}
	if msgs := h.FunctionCallStopped("call-1", false); len(msgs) != 0 {
		t.Fatal("a call was stopped twice")
	}
	for i := 0; i < 64; i++ { // results that never came
		h.FunctionCallStarted(fmt.Sprint("lost-", i))
	}
	h.FunctionCallStarted("late") // the 65th forgets the lost ones
	if len(h.FunctionCallStopped("lost-0", false)) != 0 || len(h.FunctionCallStopped("late", false)) != 1 {
		t.Fatal("the announced-call set is not bounded to 64")
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
go test -race -count=1 -run 'FunctionCall' ./transport/rtvi/
```

Expected: build failure, `h.FunctionCallStarted undefined`.

- [ ] **Step 3: Implement.** In `transport/rtvi/rtvi.go`, in the type constant block after `	TypePushToTalk          = "push-to-talk" // …` add:

```go
	// Function calls, at Pipecat's default function_call_report_level "none".
	TypeLLMFunctionCallStarted    = "llm-function-call-started"
	TypeLLMFunctionCallInProgress = "llm-function-call-in-progress"
	TypeLLMFunctionCallStopped    = "llm-function-call-stopped"
```

After `type ErrorData struct {…}` add:

```go
// FunctionCallInProgressData is llm-function-call-in-progress at report level
// "none": the call's id, never its name or arguments.
type FunctionCallInProgressData struct {
	ToolCallID string `json:"tool_call_id"`
}

// FunctionCallStoppedData is llm-function-call-stopped at report level "none":
// the call's id and whether it was cancelled (barge-in, a loop close or the
// loop's own call timeout; an MCP server timeout is a failed call, cancelled
// false), never its result.
type FunctionCallStoppedData struct {
	ToolCallID string `json:"tool_call_id"`
	Cancelled  bool   `json:"cancelled"`
}
```

In `type Handler struct`, after `	llmReply string // key of the open reply` add:

```go

	fnMu    sync.Mutex
	fnCalls map[string]struct{} // tool_call_ids sent as in-progress and not yet stopped
```

After `BotLLMStopped` add:

```go
// maxOpenFunctionCalls bounds the announced calls a connection remembers; a
// tool round runs at most 16 calls, so only lost results ever reach it.
const maxOpenFunctionCalls = 64

// FunctionCallStarted returns llm-function-call-started and
// llm-function-call-in-progress for one tool call. They carry only the
// call's id (Pipecat's default report level "none"): tool names, arguments
// and results can hold the caller's personal details.
func (h *Handler) FunctionCallStarted(toolCallID string) []string {
	h.fnMu.Lock()
	if h.fnCalls == nil || len(h.fnCalls) >= maxOpenFunctionCalls {
		h.fnCalls = make(map[string]struct{}) // ponytail: forgets calls whose result never came
	}
	h.fnCalls[toolCallID] = struct{}{}
	h.fnMu.Unlock()
	started, _ := h.formatMessage(TypeLLMFunctionCallStarted, json.RawMessage(`{}`))
	data, _ := json.Marshal(FunctionCallInProgressData{ToolCallID: toolCallID})
	inProgress, _ := h.formatMessage(TypeLLMFunctionCallInProgress, data)
	return []string{started, inProgress}
}

// FunctionCallStopped returns llm-function-call-stopped for a call announced
// by FunctionCallStarted, once, and nothing for any other call.
func (h *Handler) FunctionCallStopped(toolCallID string, cancelled bool) []string {
	h.fnMu.Lock()
	_, open := h.fnCalls[toolCallID]
	delete(h.fnCalls, toolCallID)
	h.fnMu.Unlock()
	if !open {
		return nil
	}
	data, _ := json.Marshal(FunctionCallStoppedData{ToolCallID: toolCallID, Cancelled: cancelled})
	msg, _ := h.formatMessage(TypeLLMFunctionCallStopped, data)
	return []string{msg}
}
```

In `transport/ws/transport.go` `(*Connection).ProcessFrame`, directly before `	case core.FrameTTSStarted:` add:

```go
	case core.FrameToolCall:
		tc := f.(*core.ToolCallFrame)
		for _, msg := range c.rtvi.FunctionCallStarted(tc.Call.ID) {
			_ = c.WriteWireMessage(ctx, msg)
		}
		return e.PushDirection(f, dir)

	case core.FrameToolResult:
		tr := f.(*core.ToolResultFrame)
		for _, msg := range c.rtvi.FunctionCallStopped(tr.Result.CallID, tr.Result.Status == core.ToolCancelled) {
			_ = c.WriteWireMessage(ctx, msg)
		}
		return e.PushDirection(f, dir)

```

- [ ] **Step 4: Run the unit tests to see them pass.**

```bash
gofmt -l transport && go vet ./transport/... && go test -race -count=1 ./transport/...
```

Expected: no gofmt output, every `transport` package `ok`.

- [ ] **Step 5: Write the failing wire e2e tests (L2).** In `server/flow_e2e_test.go`:

Replace `dial`:

```go
func (r *flowRigE2E) dial(flowID string) *e2eCall {
	r.t.Helper()
	return r.dialToken(r.voiceToken(flowID))
}

// voiceToken issues a voice token for flowID, as an integration's backend does.
func (r *flowRigE2E) voiceToken(flowID string) string {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodPost, "/api/v1/voice-tokens", e2eMgmtKey, `{"principal":"alice","flow_id":"`+flowID+`"}`)
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Data.Token
}
```

In `type e2eCall struct`, after `	texts     []string` add `	fn        []fnEvent // llm-function-call-* messages, in arrival order`. After the struct add:

```go
// fnEvent is one llm-function-call-* message the client received.
type fnEvent struct {
	Type        string         `json:"type"`
	Data        map[string]any `json:"data"`
	Raw         string         `json:"-"` // the whole message, as sent
	TextsBefore int            `json:"-"` // bot-llm-text messages received before it
}

func (c *e2eCall) fnEvents() []fnEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.fn)
}
```

In `read`, after the closing brace of `if strings.Contains(m.Data, "bot-llm-text") {…}` add:

```go
				if strings.Contains(m.Data, `"type":"llm-function-call-`) {
					ev := fnEvent{Raw: m.Data}
					if json.Unmarshal([]byte(m.Data), &ev) == nil {
						c.mu.Lock()
						ev.TextsBefore = len(c.texts)
						c.fn = append(c.fn, ev)
						c.mu.Unlock()
					}
				}
```

Create `server/tool_events_e2e_test.go`:

```go
package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/turn"
	"govoice/transport/ws"
)

// fnShape renders function-call messages as "<started|in-progress|stopped><data json>", each provider
// tool_call_id replaced by #1, #2… in order of first appearance, so a test asserts the exact sequence
// and payloads while the model picks the ids.
func fnShape(t *testing.T, evs []fnEvent) []string {
	t.Helper()
	ids := map[string]string{}
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		data := make(map[string]any, len(ev.Data))
		for k, v := range ev.Data {
			data[k] = v
		}
		if id, ok := data["tool_call_id"].(string); ok {
			if !strings.HasPrefix(id, "echo-call-") {
				t.Fatalf("tool_call_id %q is not the model's call id", id)
			}
			if ids[id] == "" {
				ids[id] = fmt.Sprintf("#%d", len(ids)+1)
			}
			data["tool_call_id"] = ids[id]
		}
		b, _ := json.Marshal(data)
		out = append(out, strings.TrimPrefix(ev.Type, "llm-function-call-")+string(b))
	}
	return out
}

func waitFnEvents(t *testing.T, c *e2eCall, n int) []fnEvent {
	t.Helper()
	eventually(t, fmt.Sprintf("%d function-call messages (have %d)", n, len(c.fnEvents())), func() bool { return len(c.fnEvents()) >= n })
	return c.fnEvents()
}

// assertFnEvents waits for len(want) function-call messages and checks them, then sends one more turn as an
// ordering barrier: once its answer arrived, everything the call emitted before it has too, so a late or
// duplicate event (a second stopped, a started after barge-in) shows. The list must still be exactly want.
func assertFnEvents(t *testing.T, c *e2eCall, want []string) []fnEvent {
	t.Helper()
	if got := fnShape(t, waitFnEvents(t, c, len(want))); !slices.Equal(got, want) {
		t.Fatalf("function-call messages %q, want %q", got, want)
	}
	c.barrier(t)
	evs := c.fnEvents()
	if got := fnShape(t, evs); !slices.Equal(got, want) {
		t.Fatalf("after the barrier turn: function-call messages %q, want exactly %q", got, want)
	}
	return evs
}

// barrier sends one more text turn and waits for the echo model's answer to it.
// ponytail: a text turn sent while the previous reply is still being committed can be lost (a known,
// tool-independent issue tracked separately), so it is resent, at most three times, 3 s apart.
func (c *e2eCall) barrier(t *testing.T) {
	t.Helper()
	for i := 0; i < 3; i++ {
		c.say("barrier")
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if c.replies("You said: barrier") > 0 {
				return
			}
		}
	}
	t.Fatal("the barrier turn was never answered")
}

// liveSession hooks the rig's WebSocket server so a test reaches the call's core session. Call it before dial.
func (r *mcpCallRig) liveSession() func() *core.Session {
	var live atomic.Pointer[ws.Connection]
	onText := r.s.wsServer.OnText
	r.s.wsServer.OnText = func(conn *ws.Connection, text string) error {
		live.Store(conn)
		return onText(conn, text)
	}
	return func() *core.Session { return live.Load().Session() }
}

// bargeIn is what the VAD and the STT send when the caller says "wait never mind" over the assistant.
func bargeIn(t *testing.T, sess *core.Session) {
	t.Helper()
	at := -1
	for i, p := range sess.Processors() {
		if _, ok := p.(*turn.Processor); ok {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("no turn processor in the call pipeline")
	}
	gen := sess.CurrentGeneration()
	if err := sess.Reenter(&core.UserStartedSpeakingFrame{Seq: 99, Gen: gen, Timestamp: time.Now()}, core.Downstream, at, gen); err != nil {
		t.Fatal(err)
	}
	if err := sess.Reenter(&core.TranscriptCandidateFrame{Text: "wait never mind", Turn: core.TurnID(fmt.Sprintf("turn-%d-99", gen)), Gen: core.Generation(gen), Final: true, EndOfUtterance: true, Timestamp: time.Now()}, core.Downstream, at, gen); err != nil {
		t.Fatal(err)
	}
}

func TestMCPCallToolEventsOnTheWire(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	assertFnEvents(t, c, want)
}

func TestMCPCallFailedToolStopsUncancelled(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("fail", r.ts.URL+"/fail/tools", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "tool_error").ID)
	c.say("call tool tool_error")
	c.waitReplies("Tool tool_error failed", 1)
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	assertFnEvents(t, c, want)
}

func TestMCPCallParallelToolsEachGetTheirEvents(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.Tools.MaxParallel = 4 })
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order", "echo").ID)
	c.say(`call tool lookup_order {"order_id":"42"} then call tool echo {"text":"hi"}`)
	c.waitReplies("Tool echo returned", 1)
	want := []string{
		`started{}`, `in-progress{"tool_call_id":"#1"}`,
		`started{}`, `in-progress{"tool_call_id":"#2"}`,
		`stopped{"cancelled":false,"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#2"}`,
	}
	assertFnEvents(t, c, want)
	if r.lastCall("lookup_order").Tool == "" || r.lastCall("echo").Tool == "" {
		t.Fatal("the MCP server did not see both calls")
	}
}

func TestMCPCallBargeInSettlesTheAnnouncedCallAndNothingElse(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.MCP.Timeout = 10 * time.Second })
	session := r.liveSession()
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":4000}`)
	waitFnEvents(t, c, 2) // announced: started, in-progress
	bargeIn(t, session())
	c.waitReplies("wait never mind", 1)
	eventually(t, "the server to see the call abandoned", func() bool { return r.lastCall("slow_lookup").Cancelled })
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":true,"tool_call_id":"#1"}`}
	assertFnEvents(t, c, want)
}

func TestMCPCallAsyncToolStopsWhenItsBackgroundRunEnds(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, []string{id + "/slow_lookup"}, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":1000}`)
	c.waitReplies("running", 1) // the acknowledgement is answered at once
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	evs := assertFnEvents(t, c, want)
	if evs[2].TextsBefore <= evs[1].TextsBefore {
		t.Fatal("the call was settled before the acknowledgement was answered (the ack must not stop it)")
	}
}

func TestMCPCallToolEventsCarryNoToolContent(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.Privacy.Redaction.Enabled = true })
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"4111 1111 1111 1111"}`)
	c.waitReplies("shipped", 1)
	waitFnEvents(t, c, 3)
	c.barrier(t)
	evs := c.fnEvents()
	if len(evs) != 3 {
		t.Fatalf("%d function-call messages, want 3", len(evs))
	}
	for _, ev := range evs {
		for _, leak := range []string{"4111", "lookup_order", "function_name", "arguments", "result", "shipped"} {
			if strings.Contains(ev.Raw, leak) {
				t.Fatalf("%q reached the client: %s", leak, ev.Raw)
			}
		}
	}
}
```

- [ ] **Step 6: Run them to see them fail first, then pass.** Stash the Step 3 transport edit (`git stash push transport/ws/transport.go`), run, then restore (`git stash pop`) and run again:

```bash
cd server && go test -race -count=1 -run 'TestMCPCallToolEventsOnTheWire|TestMCPCallFailedToolStopsUncancelled|TestMCPCallParallelTools|TestMCPCallBargeInSettles|TestMCPCallAsyncToolStops|TestMCPCallToolEventsCarryNoToolContent' . ; cd ..
```

Expected without the transport case: each fails `timed out waiting for 3 function-call messages (have 0)`. With it: `ok govoice/server`. Then the flake check for these six: same command with `-count=3`, expected `ok`.

- [ ] **Step 7: Docs.** `docs/privacy.md`: directly before the bullet that starts `- **NOT covered:**` add:

```markdown
- **Tool calls on the client wire:** `llm-function-call-started`, `-in-progress` and `-stopped` carry only the call's `tool_call_id` (and `cancelled`), Pipecat's default `function_call_report_level` `none`: no tool name, arguments or result reaches the client, whatever the redaction settings.
```

`docs/pipecat-compatibility.md` line 38: replace `` `bot-tts-text`, `bot-interrupted`, function-call and `vad-*` messages are not emitted.`` with `` `bot-tts-text`, `bot-interrupted` and `vad-*` messages are not emitted. A tool call sends `llm-function-call-started`, `llm-function-call-in-progress` and `llm-function-call-stopped` at Pipecat's default report level `none` (`tool_call_id` and `cancelled` only); a call cut off by barge-in (or a loop close) stops with `cancelled: true`; an MCP timeout or tool error stops with `cancelled: false`.``

- [ ] **Step 8: Gate.** Run the gate block with `3.2a-s8` (verify, e2e-echo, offline-test).

- [ ] **Step 9: Commit.**

```bash
git add transport/rtvi/rtvi.go transport/rtvi/rtvi_test.go transport/ws/transport.go server/flow_e2e_test.go server/tool_events_e2e_test.go docs/privacy.md docs/pipecat-compatibility.md
git commit -m "feat(transport): RTVI llm-function-call messages

A tool call the loop announces sends llm-function-call-started and
-in-progress; its result sends -stopped, cancelled when barge-in (or a
loop close) ended it; an MCP timeout stops with cancelled false. Only the tool_call_id goes on the wire (Pipecat's
default function_call_report_level none), and a result is sent only for
a call that was announced on this connection.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3.2b: the real pinned client and Pipecat's own goldens (L3)

**Files:**
- Create: `tools/compat/test_rtvi_function_calls.js`, `server/tool_events_client_e2e_test.go`, `tools/compat/testdata/parity/function_calls.json` (generated)
- Modify: `tools/compat/parity/gen_goldens.py`, `tools/compat/parity_fixtures_test.go`, `docs/pipecat-parity-matrix.md`, `docs/pipecat-compatibility.md`

**Interfaces:**
- Consumes: `rtvi.(*Handler).FunctionCallStarted/FunctionCallStopped` (3.2a), `runParity`, `parityScenario.Steps` (`ev`, `id`, `text`), `fnShape`, `(*flowRigE2E).voiceToken`, `newMCPCallRig`.
- Produces: golden area `function_calls` (3 scenarios, 1 declared difference), `TestParityFunctionCallMessages`, `TestMCPCallPipecatClientSeesFunctionCallEvents`.

- [ ] **Step 1: Write the real-client script and its Go driver (failing until the server sends the messages; it passes after 3.2a, so verify it fails by stashing the 3.2a transport case as in 3.2a Step 6).** Create `tools/compat/test_rtvi_function_calls.js`:

```js
/**
 * test_rtvi_function_calls.js
 *
 * The released @pipecat-ai/client-js@1.13.1 with @pipecat-ai/websocket-transport@1.7.2, against a running
 * Switchboard server: one text turn makes the flow call a tool; prints, as one JSON line after "EVENTS ",
 * the function-call callbacks the client fired (in order) and the ids its event emitter saw.
 * Driven by server/tool_events_client_e2e_test.go, which asserts them. A
 * second "barrier" turn is answered before the events are printed, so the list is complete.
 *
 * Usage: node test_rtvi_function_calls.js --url <wsUrl> --token <token> --text <message>
 */
if (typeof globalThis.window === 'undefined') {
  globalThis.window = { addEventListener: () => {}, removeEventListener: () => {} };
}
if (typeof globalThis.navigator === 'undefined') {
  globalThis.navigator = { mediaDevices: { addEventListener: () => {}, removeEventListener: () => {} } };
}

const { WebSocketTransport } = require('@pipecat-ai/websocket-transport');
const { PipecatClient } = require('@pipecat-ai/client-js');

function arg(name) {
  const i = process.argv.indexOf(name);
  if (i === -1 || i + 1 >= process.argv.length) throw new Error(`missing ${name}`);
  return process.argv[i + 1];
}

function withTimeout(promise, ms, what) {
  return Promise.race([promise, new Promise((_, reject) => {
    const t = setTimeout(() => reject(new Error(`${what} timed out after ${ms}ms`)), ms);
    if (t.unref) t.unref();
  })]);
}

async function main() {
  const events = [];
  const emitted = [];
  const errors = [];
  let replied = false;
  let barrierAnswered = false;
  const mediaManager = {
    initialize: async () => {}, connect: async () => {}, disconnect: async () => {},
    setUserAudioCallback: () => {}, setClientOptions: () => {}, tracks: () => ({}),
    userStartedSpeaking: async () => {}, userStoppedSpeaking: async () => {}, bufferBotAudio: () => {},
  };
  const client = new PipecatClient({
    transport: new WebSocketTransport({ mediaManager }),
    enableMic: false,
    enableCam: false,
    callbacks: {
      onLLMFunctionCallStarted: (data) => events.push({ type: 'started', data }),
      onLLMFunctionCallInProgress: (data) => events.push({ type: 'in-progress', data }),
      onLLMFunctionCallStopped: (data) => events.push({ type: 'stopped', data }),
      onBotLlmText: (data) => {
        replied = true;
        if (data?.text?.includes('barrier')) barrierAnswered = true;
      },
      onError: (err) => errors.push(err),
    },
  });
  // RTVIEvent.LLMFunctionCallInProgress is "llmFunctionCallInProgress" in the pinned dist (index.js:957).
  client.on('llmFunctionCallInProgress', (data) => emitted.push(data.tool_call_id));
  await withTimeout(client.connect({ wsUrl: arg('--url'), token: arg('--token') }), 5000, 'connect');
  await client.sendText(arg('--text'));
  const until = Date.now() + 8000;
  while (Date.now() < until && !(replied && events.some((e) => e.type === 'stopped'))) {
    await new Promise((r) => setTimeout(r, 20));
  }
  // Ordering barrier: once a later turn is answered, a late or duplicate event would have arrived too.
  // (A text turn sent while the previous reply is still committing can be lost: resend, at most 3 times.)
  for (let i = 0; i < 3 && !barrierAnswered; i++) {
    await client.sendText('barrier');
    const by = Date.now() + 3000;
    while (Date.now() < by && !barrierAnswered) await new Promise((r) => setTimeout(r, 20));
  }
  if (!barrierAnswered) throw new Error('the barrier turn was never answered');
  await client.disconnect();
  if (errors.length) throw new Error(`RTVI errors: ${JSON.stringify(errors)}`);
  console.log('EVENTS ' + JSON.stringify({ events, emitted }));
}

main().then(() => process.exit(0)).catch((err) => {
  console.error('FAILED:', err);
  process.exit(1);
});
```

Create `server/tool_events_client_e2e_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The unmodified pinned Pipecat client, connected to the real server, fires its function-call callbacks in order
// with the ids only.
func TestMCPCallPipecatClientSeesFunctionCallEvents(t *testing.T) {
	compat := filepath.Join("..", "tools", "compat")
	if _, err := os.Stat(filepath.Join(compat, "node_modules", "@pipecat-ai", "client-js")); err != nil {
		t.Fatalf("the pinned Pipecat client is not installed (run: npm ci --prefix tools/compat): %v", err)
	}
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	a := r.flowWith(id, hashes, true, nil, "lookup_order")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "node", filepath.Join(compat, "test_rtvi_function_calls.js"),
		// r.ts is the mcptest server (mcpCallRig shadows the embedded flowRigE2E.ts): dial the voice server.
		"--url", "ws"+strings.TrimPrefix(r.flowRigE2E.ts.URL, "http"), "--token", r.voiceToken(a.ID),
		"--text", `call tool lookup_order {"order_id":"42"}`).CombinedOutput()
	if err != nil {
		t.Fatalf("client-js run failed: %v\n%s", err, out)
	}
	_, line, ok := strings.Cut(string(out), "EVENTS ")
	if !ok {
		t.Fatalf("no EVENTS line:\n%s", out)
	}
	line, _, _ = strings.Cut(line, "\n")
	var got struct {
		Events  []fnEvent `json:"events"`
		Emitted []string  `json:"emitted"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("EVENTS %s: %v", line, err)
	}
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	if shape := fnShape(t, got.Events); !slices.Equal(shape, want) {
		t.Fatalf("client callbacks %q, want %q", shape, want)
	}
	if len(got.Emitted) != 1 || got.Emitted[0] != got.Events[1].Data["tool_call_id"] {
		t.Fatalf("event emitter saw %q", got.Emitted)
	}
}
```

If the handshake is refused with an origin error (Node's WebSocket sending an `Origin` the development allowlist rejects), add `func(c *Config) { c.OriginAllowlist = []string{"*"} }` to `newMCPCallRig` in this test only, and note it in the report.

- [ ] **Step 2: Add the Pipecat golden generator.** In `tools/compat/parity/gen_goldens.py`, directly before the line `# --------------------------------------------------------------------------- declared intentional differences` add:

```python
# --------------------------------------------------------------------------- function-call RTVI messages

def _function_calls(steps):
    from pipecat.frames.frames import (FunctionCallCancelFrame, FunctionCallFromLLM, FunctionCallInProgressFrame,
                                       FunctionCallResultFrame, FunctionCallsStartedFrame)
    from pipecat.observers.base_observer import FramePushed
    from pipecat.processors.frame_processor import FrameDirection
    from pipecat.processors.frameworks.rtvi.observer import RTVIObserver

    out = []

    class Capture(RTVIObserver):
        async def send_rtvi_message(self, model, exclude_none=True):
            d = model.model_dump(exclude_none=exclude_none)
            out.append(json.dumps({"type": d["type"], "data": d.get("data", {})}, sort_keys=True, separators=(",", ":")))

    frames, i = [], 0
    while i < len(steps):
        if steps[i]["ev"] == "call":  # consecutive calls are one model response: one batch, as LLMService runs them
            batch = []
            while i < len(steps) and steps[i]["ev"] == "call":
                batch.append(steps[i])
                i += 1
            frames.append(FunctionCallsStartedFrame(function_calls=[
                FunctionCallFromLLM(function_name=s["text"], tool_call_id=s["id"], arguments={}, context=None) for s in batch]))
            frames += [FunctionCallInProgressFrame(function_name=s["text"], tool_call_id=s["id"], arguments={}) for s in batch]
            continue
        s = steps[i]
        i += 1
        if s["ev"] == "result":
            frames.append(FunctionCallResultFrame(function_name=s["text"], tool_call_id=s["id"], arguments={}, result={"ok": True}))
        else:
            frames.append(FunctionCallCancelFrame(function_name=s["text"], tool_call_id=s["id"]))

    async def main():
        obs = Capture()  # default params: function_call_report_level {"*": NONE}
        for f in frames:
            await obs.on_push_frame(FramePushed(source=None, destination=None, frame=f, direction=FrameDirection.DOWNSTREAM, timestamp=0))
        return out

    return run_virtual(main)


def function_call_scenarios():
    def F(ev, call_id, name="lookup_order"):
        return {"t": 0, "ev": ev, "id": call_id, "text": name}
    mod = "processors/frameworks/rtvi/observer.py"
    return [
        dict(id="one_call_result", pipecat_module=mod, params={}, steps=[F("call", "call-a"), F("result", "call-a")]),
        dict(id="one_call_cancelled", pipecat_module=mod, params={}, steps=[F("call", "call-a"), F("cancel", "call-a")]),
        dict(id="two_calls_parallel", pipecat_module=mod, params={},
             steps=[F("call", "call-a"), F("call", "call-b", "echo"), F("result", "call-a"), F("result", "call-b", "echo")]),
    ]
```

In `DIFFS`, before its closing `}` add:

```python
    "function_calls/two_calls_parallel": (
        "go-voice reports each call as one ToolCallFrame when its tool loop claims it, so a call's llm-function-call-started goes out with its own in-progress; Pipecat sends one started per call of the batch (FunctionCallsStartedFrame) before any in-progress.",
        ['{"data":{},"type":"llm-function-call-started"}',
         '{"data":{"tool_call_id":"call-a"},"type":"llm-function-call-in-progress"}',
         '{"data":{},"type":"llm-function-call-started"}',
         '{"data":{"tool_call_id":"call-b"},"type":"llm-function-call-in-progress"}',
         '{"data":{"cancelled":false,"tool_call_id":"call-a"},"type":"llm-function-call-stopped"}',
         '{"data":{"cancelled":false,"tool_call_id":"call-b"},"type":"llm-function-call-stopped"}']),
```

In `AREAS`, after the `("constants", …)` entry add:

```python
    ("function_calls", function_call_scenarios, lambda sc: _function_calls(sc["steps"])),
```

Generate:

```bash
make parity-regen
git status --short tools/compat/testdata/parity
```

Expected: `function_calls: 3 scenarios` among the printed areas, and `git status` shows only `?? tools/compat/testdata/parity/function_calls.json` (every existing golden unchanged). Open the file and check `one_call_result` is `{"data":{},…started}`, `{"data":{"tool_call_id":"call-a"},…in-progress}`, `{"data":{"cancelled":false,"tool_call_id":"call-a"},…stopped}`; `one_call_cancelled` ends with `"cancelled":true`; `two_calls_parallel`'s `pipecat` has both `started` first. If Pipecat's output differs from that, stop and report it (the Go side must not be edited to chase an unexpected golden).

- [ ] **Step 3: Write the Go side of the pair.** In `tools/compat/parity_fixtures_test.go`, change the import block's last line `	"govoice/core/turn"` to

```go
	"govoice/core/turn"
	"govoice/transport/rtvi"
```

and append:

```go
// ---------------------------------------------------------------- function-call RTVI messages

func TestParityFunctionCallMessages(t *testing.T) {
	runParity(t, "function_calls", func(t *testing.T, sc parityScenario) []string {
		h := rtvi.NewHandler()
		var out []string
		for _, st := range sc.Steps {
			var msgs []string
			switch st.Ev {
			case "call":
				msgs = h.FunctionCallStarted(st.ID)
			case "result":
				msgs = h.FunctionCallStopped(st.ID, false)
			case "cancel":
				msgs = h.FunctionCallStopped(st.ID, true)
			default:
				t.Fatalf("unknown step %q", st.Ev)
			}
			for _, m := range msgs {
				out = append(out, rtviTypeAndData(t, m))
			}
		}
		return out
	})
}

// rtviTypeAndData is a wire message as the goldens hold it: type and data, keys sorted, compact.
func rtviTypeAndData(t *testing.T, msg string) string {
	t.Helper()
	var m struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		t.Fatal(err)
	}
	var data any = map[string]any{}
	if len(m.Data) > 0 {
		if err := json.Unmarshal(m.Data, &data); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(map[string]any{"type": m.Type, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
```

- [ ] **Step 4: Matrix and compatibility docs.** `docs/pipecat-parity-matrix.md` line 9: replace `56 scenarios in 8 areas, 14 with a declared` with `59 scenarios in 9 areas, 15 with a declared`. In the "Context and tools" table, directly before the row that starts `| Tool-call/result ordering |`, insert this row (one line):

```markdown
| LLM function-call events | `processors/frameworks/rtvi/observer.py` (`function_call_report_level`), `services/llm_service.py` (FunctionCallsStartedFrame, FunctionCallInProgressFrame, FunctionCallResultFrame, FunctionCallCancelFrame) | `core/tools/loop.go` (`WithFrames`), `transport/rtvi/rtvi.go`, `transport/ws/transport.go`, `server/call.go` (tool call rows) | parity | on | Paired (`function_calls`, 3 scenarios) at Pipecat's default report level `none`: `started` carries no data, `in-progress` the `tool_call_id`, `stopped` the id and `cancelled`; no tool name, arguments or result reach the client, and there are no per-function levels. Differs: a call's `started` goes out with its own `in-progress` instead of once per batch first (`function_calls/two_calls_parallel`). A call cut off by barge-in stops with `cancelled: true`, a call of a stale generation sends nothing, an async tool stops when its background run ends. The unmodified pinned client fires `onLLMFunctionCallStarted`, `onLLMFunctionCallInProgress` and `onLLMFunctionCallStopped` against the real server. | `tools/compat/parity_fixtures_test.go`, `tools/compat/test_rtvi_function_calls.js`, `server/tool_events_e2e_test.go`, `core/tools/frames_test.go` |
```

`docs/pipecat-compatibility.md` line 103: replace `(56 paired scenarios, below)` with `(59 paired scenarios, below)`; line 101 (row "Pinned RTVI client"): append to its third cell, before ` |` at the end, the sentence ` A tool-calling echo call through the real server makes the client fire its three function-call callbacks with ids only (TestMCPCallPipecatClientSeesFunctionCallEvents).`

- [ ] **Step 5: Run the pair, the matrix check and the client test.**

```bash
gofmt -l tools server && go test -race -count=1 -run 'TestParity|TestPinned' ./tools/compat/...
cd server && go test -race -count=1 -run TestMCPCallPipecatClientSeesFunctionCallEvents . && cd ..
```

Expected: `ok govoice/tools/compat` (including `TestParityFunctionCallMessages/{one_call_result,one_call_cancelled,two_calls_parallel}` and `TestParityMatrix`), `ok govoice/server`.

- [ ] **Step 6: Gate.** `make verify` only, with `3.2b-s6` (no runtime behaviour changes in this task).

- [ ] **Step 7: Commit.**

```bash
git add tools/compat/test_rtvi_function_calls.js tools/compat/parity/gen_goldens.py tools/compat/testdata/parity/function_calls.json tools/compat/parity_fixtures_test.go \
  server/tool_events_client_e2e_test.go docs/pipecat-parity-matrix.md docs/pipecat-compatibility.md
git commit -m "test(compat): pair function-call RTVI with Pipecat and the pinned client

Pipecat's own RTVIObserver (default report level) writes the function_calls
goldens; the Go handler matches them except the declared per-call started.
The unmodified @pipecat-ai/client-js 1.13.1 against the real server fires
its started, in-progress and stopped callbacks with ids only.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3.3a: tool calls in the session history, the traces API and the trace spans

**Files:**
- Modify: `server/call.go`, `server/runtime/mcp.go`, `server/api/traces.go`, `server/trace/adapter.go`, `server/redaction_test.go`, `server/stored_flow_call_test.go`, `docs/api/v1.md`, `docs/privacy.md`
- Create: `server/turn_recorder_tools_test.go`, `server/runtime/mcp_tool_test.go`, `server/storage/tool_calls_test.go`, `server/api/turn_tools_test.go`, `server/tool_records_e2e_test.go`
- Test (append): `server/trace/adapter_test.go`

**Interfaces:**
- Consumes: `ToolCallFrame`/`ToolResultFrame` (3.1), `storage.TraceRepository.AddToolCall`/`ToolCalls`, `storage.SessionRepository.Turns`, `flow.MCPToolSnapshot`, `runtimeToolName`, test helpers `fnEvents`, `waitFnEvents`, `liveSession`, `bargeIn` (3.2a).
- Produces: `newTurnRecorder(addTurn, addToolCall, mcpTool, sessionID, logger, lat)`, `turnWrite`, `pendingTool`, `(*runtime.Session).MCPTool(name) (flow.MCPToolSnapshot, bool)`, `api.TurnToolDTO`, `TurnDTO.Tools []TurnToolDTO`, `toTurnDTO(r, tools)`, test helpers `(*flowRigE2E).sessionTraces`, `sessionTurns`, `waitTurns`.

- [ ] **Step 1: Write the failing unit tests.** Create `server/turn_recorder_tools_test.go`:

```go
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/server/flow"
	"govoice/server/storage"
)

func TestTurnRecorderStoresToolCallsWithTheirTurn(t *testing.T) {
	var mu sync.Mutex
	var turns []storage.TurnRecord
	var calls []storage.ToolCallRecord
	snap := flow.MCPToolSnapshot{ServerID: "srv-1", RemoteName: "lookup_order", SchemaHash: "hash-1"}
	r := newTurnRecorder(func(_ context.Context, row storage.TurnRecord) error {
		mu.Lock()
		defer mu.Unlock()
		turns = append(turns, row)
		return nil
	}, func(_ context.Context, tc storage.ToolCallRecord) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, tc)
		return nil
	}, func(name string) (flow.MCPToolSnapshot, bool) { return snap, name == "mcp__lookup" }, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())
	tail := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(r, tail)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(100, 0)
	call := func(id, name, turn string) *core.ToolCallFrame {
		return &core.ToolCallFrame{Call: core.ToolCall{ID: id, Name: name, SessionID: "1", TurnID: core.TurnID(turn), Generation: 1}, Timestamp: at}
	}
	result := func(id, turn string, st core.ToolStatus) *core.ToolResultFrame {
		return &core.ToolResultFrame{Turn: core.TurnID(turn), Gen: 1, Result: core.ToolResult{CallID: id, Status: st, StartedAt: at, EndedAt: at.Add(120 * time.Millisecond)}}
	}
	for _, f := range []core.Frame{
		&core.UserMessageFrame{Turn: "t1", Content: "Where is my order?", Timestamp: at},
		call("c1", "mcp__lookup", "t1"),
		result("c1", "t1", core.ToolSucceeded),
		call("c2", "transfer_to_sales", "t1"),
		&core.AssistantMessageFrame{Turn: "t1", Content: "Shipped.", Timestamp: at.Add(time.Second)},
		result("c2", "t1", core.ToolCancelled), // settled after its turn's row (barge-in, an async tool)
		result("c3", "t1", core.ToolFailed),    // never announced: its generation was stale
		call("c4", "mcp__lookup", "t2"),        // still running when the call ends
		&core.LLMTextFrame{Text: "marker", Turn: "t9", Gen: 1},
	} {
		if err := session.Post(f); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every frame through the recorder", func() bool {
		for _, f := range tail.GetFrames() {
			if _, ok := f.(*core.LLMTextFrame); ok {
				return true
			}
		}
		return false
	})
	_ = session.Close()
	r.close(t.Context())

	mu.Lock()
	defer mu.Unlock()
	if len(turns) != 1 || turns[0].AssistantText != "Shipped." {
		t.Fatalf("turns %+v", turns)
	}
	row := turns[0].ID
	if len(calls) != 3 {
		t.Fatalf("tool rows %+v, want 3 (c1, c2, c4; never c3)", calls)
	}
	c1, c2, c4 := calls[0], calls[1], calls[2]
	if c1.SessionID != "sess" || c1.TurnID != row || c1.MCPServerID != "srv-1" || c1.RemoteName != "lookup_order" || c1.SchemaHash != "hash-1" ||
		c1.Status != "succeeded" || c1.EndedAt.Sub(c1.StartedAt) != 120*time.Millisecond || c1.ArgumentsSummary != "" || c1.ResultSummary != "" {
		t.Fatalf("c1 %+v", c1)
	}
	if c2.TurnID != row || c2.RemoteName != "transfer_to_sales" || c2.MCPServerID != "" || c2.Status != "cancelled" {
		t.Fatalf("c2 %+v", c2)
	}
	if c4.TurnID != "" || c4.RemoteName != "lookup_order" || c4.Status != "cancelled" || !c4.EndedAt.IsZero() {
		t.Fatalf("c4 %+v", c4)
	}
}
```

Also in `server/turn_recorder_tools_test.go`:

```go
// A tool name the model invented can carry the caller's words; it is stored by its category only.
func TestTurnRecorderNeverStoresAModelInventedToolName(t *testing.T) {
	var mu sync.Mutex
	var calls []storage.ToolCallRecord
	r := newTurnRecorder(func(context.Context, storage.TurnRecord) error { return nil }, func(_ context.Context, tc storage.ToolCallRecord) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, tc)
		return nil
	}, func(string) (flow.MCPToolSnapshot, bool) { return flow.MCPToolSnapshot{}, false }, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())
	tail := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(r, tail)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	name := "my_card_is_4111111111111111_" + strings.Repeat("x", 40)
	for _, f := range []core.Frame{
		&core.ToolCallFrame{Call: core.ToolCall{ID: "c1", Name: name, SessionID: "1", TurnID: "t1", Generation: 1}, Timestamp: time.Unix(100, 0)},
		&core.ToolResultFrame{Turn: "t1", Gen: 1, Result: core.ToolResult{CallID: "c1", Status: core.ToolFailed, ErrorCategory: "unknown_tool"}},
		&core.LLMTextFrame{Text: "marker", Turn: "t9", Gen: 1},
	} {
		if err := session.Post(f); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every frame through the recorder", func() bool { return len(tail.GetFrames()) == 3 })
	_ = session.Close()
	r.close(t.Context())
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0].RemoteName != "unknown_tool" || strings.Contains(fmt.Sprintf("%+v", calls[0]), "4111") {
		t.Fatalf("stored %+v", calls)
	}
}
```

Create `server/runtime/mcp_tool_test.go`:

```go
package runtime

import (
	"testing"

	"govoice/server/flow"
)

func TestMCPToolNamesTheCapturedToolBehindAModelFacingName(t *testing.T) {
	snap := flow.MCPToolSnapshot{ServerID: "0192aaaa-0000-7000-8000-000000000001", RemoteName: "lookup_order", SchemaHash: "h1"}
	s := &Session{Snapshot: flow.Snapshot{MCPTools: []flow.MCPToolSnapshot{snap}}}
	got, ok := s.MCPTool(runtimeToolName(snap.ServerID, snap.RemoteName))
	if !ok || got.ServerID != snap.ServerID || got.RemoteName != "lookup_order" || got.SchemaHash != "h1" {
		t.Fatalf("MCPTool = %+v, %v", got, ok)
	}
	if _, ok := s.MCPTool("transfer_to_sales"); ok {
		t.Fatal("a built-in tool resolved to an MCP tool")
	}
}
```

Create `server/storage/tool_calls_test.go`:

```go
package storage

import (
	"context"
	"testing"
	"time"
)

// The turn recorder's write pattern, on SQLite and (TEST_POSTGRES_DSN) Postgres: a call joins a stored turn, an
// unattached call has no turn, and a call cannot join a turn row that does not exist.
func TestToolCallRowsFollowTheirTurn(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		sess := seedSession(t, s, time.Now().Add(-time.Minute), "active")
		turn := TurnRecord{ID: NewID(), SessionID: sess, Ordinal: 1, StartedAt: time.Now()}
		if err := s.Sessions().AddTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Millisecond)
		for _, tc := range []ToolCallRecord{
			{ID: NewID(), SessionID: sess, TurnID: turn.ID, RemoteName: "lookup_order", SchemaHash: "h", StartedAt: at, EndedAt: at.Add(120 * time.Millisecond), Status: "succeeded"},
			{ID: NewID(), SessionID: sess, RemoteName: "transfer_to_sales", StartedAt: at.Add(time.Second), Status: "cancelled"},
		} {
			if err := s.Traces().AddToolCall(ctx, tc); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Traces().AddToolCall(ctx, ToolCallRecord{ID: NewID(), SessionID: sess, TurnID: NewID(), RemoteName: "x", StartedAt: at, Status: "failed"}); err == nil {
			t.Fatal("a tool call joined a turn row that does not exist")
		}
		got, err := s.Traces().ToolCalls(ctx, sess, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].TurnID != turn.ID || got[0].EndedAt.Sub(got[0].StartedAt) != 120*time.Millisecond ||
			got[1].TurnID != "" || !got[1].EndedAt.IsZero() || got[1].Status != "cancelled" {
			t.Fatalf("rows %+v", got)
		}
	})
}
```

Create `server/api/turn_tools_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"govoice/server/api"
	"govoice/server/storage"
)

func TestTurnsCarryTheirToolCalls(t *testing.T) {
	s, handler, sessID, turnID := setupTracesHandler(t)
	at := time.Now().UTC().Truncate(time.Millisecond).Add(2 * time.Second)
	for _, tc := range []storage.ToolCallRecord{
		{ID: storage.NewID(), SessionID: sessID, TurnID: turnID, RemoteName: "lookup_order", SchemaHash: "h", StartedAt: at, EndedAt: at.Add(150 * time.Millisecond), Status: "succeeded"},
		{ID: storage.NewID(), SessionID: sessID, RemoteName: "end_call", StartedAt: at, Status: "cancelled"}, // on no turn
	} {
		if err := s.Traces().AddToolCall(context.Background(), tc); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessID+"/turns", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []api.TurnDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Data) != 1 {
		t.Fatalf("turns %s: %v", w.Body.String(), err)
	}
	// setupTracesHandler stores get_weather (status completed, no end) on the same turn, earlier.
	want := []api.TurnToolDTO{{Name: "get_weather", Status: "completed", DurationMS: 0}, {Name: "lookup_order", Status: "succeeded", DurationMS: 150}}
	if !reflect.DeepEqual(resp.Data[0].Tools, want) {
		t.Fatalf("tools %+v, want %+v", resp.Data[0].Tools, want)
	}
}
```

Append to `server/trace/adapter_test.go`:

```go
func TestAdapterKeepsWhichMCPToolRan(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)
	adapter := trace.NewAdapter(s.Traces())
	ctx := context.Background()
	span := adapter.StartSpan(ctx, core.NewTraceEvent("mcp.tool.execute", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{Content: core.TraceMetadata}, nil))
	span.SetAttribute("mcp.server_id", "0192aaaa-0000-7000-8000-000000000001")
	span.SetAttribute("mcp.tool_name", "lookup_order")
	span.SetAttribute("mcp.duration_ms", int64(42))
	span.End(nil)
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil || len(spans) != 1 {
		t.Fatalf("spans %+v err %v", spans, err)
	}
	for _, want := range []string{`"mcp.server_id":"0192aaaa-0000-7000-8000-000000000001"`, `"mcp.tool_name":"lookup_order"`, `"mcp.duration_ms":42`} {
		if !strings.Contains(spans[0].AttributesJSON, want) {
			t.Fatalf("attributes %s lack %s", spans[0].AttributesJSON, want)
		}
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
cd server && go test -race -count=1 -run 'TestTurnRecorderStoresToolCalls|TestTurnRecorderNeverStores|TestToolCallRowsFollowTheirTurn|TestTurnsCarryTheirToolCalls|TestAdapterKeepsWhichMCPToolRan|TestMCPToolNames' ./ ./runtime/ ./storage/ ./api/ ./trace/ ; cd ..
```

Expected: `server` does not compile (`too many arguments in call to newTurnRecorder`), `runtime` (`s.MCPTool undefined`), `api` (`api.TurnToolDTO undefined`); `TestAdapterKeepsWhichMCPToolRan` fails (`lack "mcp.server_id"…`); `TestToolCallRowsFollowTheirTurn` passes (it pins the existing storage the recorder relies on; with `TEST_POSTGRES_DSN` set it also runs on Postgres).

- [ ] **Step 3: Implement.**

(a) `server/runtime/mcp.go`, after `StartSnapshotSession`:

```go
// MCPTool is the captured MCP tool a model-facing tool name stands for; false
// for a built-in tool (a route's transfer, end_call, send_dtmf).
func (s *Session) MCPTool(name string) (flow.MCPToolSnapshot, bool) {
	for _, t := range s.Snapshot.MCPTools {
		if runtimeToolName(t.ServerID, t.RemoteName) == name {
			return t, true
		}
	}
	return flow.MCPToolSnapshot{}, false
}
```

(b) `server/call.go`. Add `"sync"` to the imports. In `resolveCall` replace

```go
	}, rs.Pin.ID, s.logger, lat)
```

with

```go
	}, s.store.Traces().AddToolCall, rs.MCPTool, rs.Pin.ID, s.logger, lat)
```

Replace everything from the comment `// turnRecorder stores one turn row per assistant reply, complete or` through the end of `func (r *turnRecorder) close(ctx context.Context) {…}` with:

```go
// turnRecorder stores one turn row per assistant reply, complete or
// interrupted, pairing it with the user message that started it, and one
// tool_calls row per tool call the loop announced: its name, status and
// timing, never its arguments or result. Rows are written by one goroutine per
// call so the session run loop never waits on the database; a turn row is
// written before the tool calls that reference it.
type turnRecorder struct {
	mu       sync.Mutex // the run loop and close; never held across a write
	users    map[string]*core.UserMessageFrame
	calls    map[string]pendingTool              // announced calls by tool call id, until their result
	finished map[string][]storage.ToolCallRecord // settled calls by turn, until the turn's row is queued
	rowIDs   map[string]string                   // turn -> its queued row id, for calls settled after it
	ordinal  int
	closed   bool
	rows     chan turnWrite
	done     chan struct{}
	lat      *turnLatencies
	mcpTool  func(name string) (flow.MCPToolSnapshot, bool)
}

// pendingTool is an announced call waiting for its result.
type pendingTool struct {
	turn string
	rec  storage.ToolCallRecord
}

// turnWrite is one ordered write: a turn row (nil for none), then the tool
// calls that belong with it.
type turnWrite struct {
	turn  *storage.TurnRecord
	tools []storage.ToolCallRecord
}

// The recorder forgets what it can no longer pair. A tool round runs at most 16
// calls and a reply settles its turn, so these bounds are reached only when
// results or replies never come.
const (
	maxRecordedTurns = 32
	maxPendingTools  = 64
)

func newTurnRecorder(addTurn func(context.Context, storage.TurnRecord) error, addToolCall func(context.Context, storage.ToolCallRecord) error,
	mcpTool func(name string) (flow.MCPToolSnapshot, bool), sessionID string, logger *slog.Logger, lat *turnLatencies) *turnRecorder {
	r := &turnRecorder{lat: lat, mcpTool: mcpTool, users: map[string]*core.UserMessageFrame{}, calls: map[string]pendingTool{},
		finished: map[string][]storage.ToolCallRecord{}, rowIDs: map[string]string{}, rows: make(chan turnWrite, 64), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for w := range r.rows {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			stored := true
			if w.turn != nil {
				w.turn.SessionID = sessionID
				if err := addTurn(ctx, *w.turn); err != nil {
					stored = false
					logger.Warn("turn not recorded", "session", sessionID, "err", err.Error())
				}
			}
			for _, tc := range w.tools {
				tc.SessionID = sessionID
				if !stored {
					tc.TurnID = "" // its turn row is missing: keep the call, unattached
				}
				if addToolCall == nil {
					continue
				}
				if err := addToolCall(ctx, tc); err != nil {
					logger.Warn("tool call not recorded", "session", sessionID, "err", err.Error())
				}
			}
			cancel()
		}
	}()
	return r
}

func (r *turnRecorder) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	r.mu.Lock()
	switch m := f.(type) {
	case *core.UserMessageFrame:
		if len(r.users) >= maxRecordedTurns { // ponytail: replies never came for these; forget them
			clear(r.users)
		}
		r.users[m.Turn] = m
	case *core.ToolCallFrame:
		if len(r.calls) >= maxPendingTools { // ponytail: results never came for these; forget them
			clear(r.calls)
		}
		r.calls[m.Call.ID] = pendingTool{turn: string(m.Call.TurnID), rec: r.toolRecord(m.Call.Name, m.Timestamp)}
	case *core.ToolResultFrame:
		r.settleLocked(m.Result)
	case *core.AssistantMessageFrame:
		user := r.users[m.Turn]
		delete(r.users, m.Turn)
		row := storage.TurnRecord{ID: storage.NewID(), Ordinal: r.ordinal + 1, StartedAt: m.Timestamp, EndedAt: m.Timestamp, AssistantText: m.Content, Interrupted: m.Partial, LatenciesJSON: r.lat.take(m.Turn)}
		if user != nil {
			row.Transcript, row.StartedAt = user.Content, user.Timestamp
		}
		tools := r.finished[m.Turn]
		delete(r.finished, m.Turn)
		for i := range tools {
			tools[i].TurnID = row.ID
		}
		if r.sendLocked(turnWrite{turn: &row, tools: tools}) {
			r.ordinal++
			if len(r.rowIDs) >= maxRecordedTurns {
				clear(r.rowIDs)
			}
			r.rowIDs[m.Turn] = row.ID
		}
	}
	r.mu.Unlock()
	return e.PushDirection(f, dir)
}

// toolRecord starts a call's row: an MCP tool by its server, remote name and
// schema hash, a built-in tool (a route's transfer, end_call) by its own name.
// It reads cancelled until a result settles it.
func (r *turnRecorder) toolRecord(name string, at time.Time) storage.ToolCallRecord {
	rec := storage.ToolCallRecord{ID: storage.NewID(), RemoteName: name, StartedAt: at, Status: string(core.ToolCancelled)}
	if r.mcpTool != nil {
		if t, ok := r.mcpTool(name); ok {
			rec.MCPServerID, rec.RemoteName, rec.SchemaHash = t.ServerID, t.RemoteName, t.SchemaHash
		}
	}
	return rec
}

// settleLocked completes an announced call's row with its result; a result for
// a call that was never announced (a stale generation) is ignored.
func (r *turnRecorder) settleLocked(res core.ToolResult) {
	p, ok := r.calls[res.CallID]
	if !ok {
		return
	}
	delete(r.calls, res.CallID)
	p.rec.Status, p.rec.EndedAt = string(res.Status), res.EndedAt
	if c := res.ErrorCategory; c == "unknown_tool" || c == "invalid_tool_name" {
		// The model wrote that name (it can hold the caller's words) and remote_name is never purged.
		p.rec.RemoteName, p.rec.MCPServerID, p.rec.SchemaHash = c, "", ""
	}
	if !res.StartedAt.IsZero() {
		p.rec.StartedAt = res.StartedAt
	}
	if id, ok := r.rowIDs[p.turn]; ok { // settled after its turn's row: an async tool, a call cut off by barge-in
		p.rec.TurnID = id
		r.sendLocked(turnWrite{tools: []storage.ToolCallRecord{p.rec}})
		return
	}
	if _, ok := r.finished[p.turn]; !ok && len(r.finished) >= maxRecordedTurns {
		r.flushWaitingLocked() // ponytail: replies never came for these turns
	}
	r.finished[p.turn] = append(r.finished[p.turn], p.rec)
}

// flushWaitingLocked writes the settled calls still waiting for a turn row, as
// they are (unattached unless their turn's row id is known).
func (r *turnRecorder) flushWaitingLocked() {
	var rest []storage.ToolCallRecord
	for _, tools := range r.finished {
		rest = append(rest, tools...)
	}
	clear(r.finished)
	if len(rest) > 0 {
		r.sendLocked(turnWrite{tools: rest})
	}
}

// sendLocked queues w without waiting: history is best effort and never stalls the call on the database.
func (r *turnRecorder) sendLocked(w turnWrite) bool {
	if r.closed {
		return false
	}
	select {
	case r.rows <- w:
		return true
	default:
		return false
	}
}

// close records the calls still running (cancelled), stops accepting rows and
// waits (bounded by ctx) for queued ones.
func (r *turnRecorder) close(ctx context.Context) {
	r.mu.Lock()
	if !r.closed {
		for _, p := range r.calls {
			if id, ok := r.rowIDs[p.turn]; ok {
				p.rec.TurnID = id
			}
			r.finished[p.turn] = append(r.finished[p.turn], p.rec)
		}
		clear(r.calls)
		r.flushWaitingLocked()
		r.closed = true
		close(r.rows)
	}
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}
```

(c) Existing callers. In `server/redaction_test.go` and `server/stored_flow_call_test.go`, replace every `}, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())` with `}, nil, nil, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())`; in `server/redaction_test.go` also replace `}, "sess", s.logger, newTurnLatencies())` with `}, nil, nil, "sess", s.logger, newTurnLatencies())`.

(d) `server/api/traces.go`. Replace the `Tools` field of `TurnDTO` (`	Tools         json.RawMessage `json:"tools,omitempty"``) with:

```go
	// Tools are the turn's tool calls, from tool_calls: name, outcome and duration only.
	Tools []TurnToolDTO `json:"tools"`
}

// TurnToolDTO is one tool call of a turn; never its arguments or result.
type TurnToolDTO struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
```

(the `}` that closed `TurnDTO` now closes `TurnToolDTO`). In `turns`, replace

```go
	dtos := make([]TurnDTO, 0, len(turns))
	for _, turn := range turns {
		dtos = append(dtos, toTurnDTO(turn))
	}
```

with

```go
	calls, err := h.traces.ToolCalls(r.Context(), id, "")
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get tool calls", requestID)
		return
	}
	byTurn := make(map[string][]TurnToolDTO, len(turns))
	for _, c := range calls {
		if c.TurnID == "" {
			continue
		}
		var ms int64
		if !c.EndedAt.IsZero() {
			ms = max(c.EndedAt.Sub(c.StartedAt).Milliseconds(), 0)
		}
		byTurn[c.TurnID] = append(byTurn[c.TurnID], TurnToolDTO{Name: c.RemoteName, Status: c.Status, DurationMS: ms})
	}
	dtos := make([]TurnDTO, 0, len(turns))
	for _, turn := range turns {
		dtos = append(dtos, toTurnDTO(turn, byTurn[turn.ID]))
	}
```

Change `func toTurnDTO(r storage.TurnRecord) TurnDTO {` to `func toTurnDTO(r storage.TurnRecord, tools []TurnToolDTO) TurnDTO {`, delete its three lines `	if r.ToolsJSON != "" && json.Valid([]byte(r.ToolsJSON)) {` / `		dto.Tools = json.RawMessage(r.ToolsJSON)` / `	}`, and before its `return dto` add:

```go
	dto.Tools = tools
	if dto.Tools == nil {
		dto.Tools = []TurnToolDTO{}
	}
```

(e) `server/trace/adapter.go` `filterAttributes`: in the whitelist `case "count", … "to_step":` append `, "mcp.server_id", "mcp.tool_name", "mcp.duration_ms"` before the colon.

- [ ] **Step 4: Run the unit tests to see them pass.**

```bash
cd server && gofmt -l . && go vet ./... && go test -race -count=1 ./runtime/ ./storage/ ./api/ ./trace/ && go test -race -count=1 -run 'TurnRecorder|Redaction|DTMFTurnStored|StoredFlow' . && cd ..
```

Expected: no gofmt output; every package `ok`.

- [ ] **Step 5: Write the records e2e tests (L2).** Create `server/tool_records_e2e_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/server/api"
	"govoice/server/storage"
)

// sessionTraces and sessionTurns read a stored call through the management API, as the console does.
func (r *flowRigE2E) sessionTraces(id string) api.SessionTracesDTO {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodGet, "/api/v1/sessions/"+id+"/traces", e2eMgmtKey, "")
	var resp struct {
		Data api.SessionTracesDTO `json:"data"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		r.t.Fatalf("traces: %d %s", w.Code, w.Body.String())
	}
	return resp.Data
}

func (r *flowRigE2E) sessionTurns(id string) []api.TurnDTO {
	r.t.Helper()
	w := apiRequest(r.s, http.MethodGet, "/api/v1/sessions/"+id+"/turns", e2eMgmtKey, "")
	var resp struct {
		Data []api.TurnDTO `json:"data"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		r.t.Fatalf("turns: %d %s", w.Code, w.Body.String())
	}
	return resp.Data
}

func (r *flowRigE2E) waitTurns(id string, n int) {
	r.t.Helper()
	eventually(r.t, fmt.Sprintf("%d stored turns", n), func() bool {
		turns, _ := r.s.store.Sessions().Turns(context.Background(), id)
		return len(turns) >= n
	})
}

func (c *e2eCall) hangUp() { _ = c.conn.Close(websocket.StatusNormalClosure, "bye") }

func TestMCPCallToolCallsAreRecorded(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.flowWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	r.waitTurns(c.sessionID, 1)
	c.barrier(t) // a late or duplicate row would be written by now
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)

	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool calls %+v", calls)
	}
	tc := calls[0]
	if tc.RemoteName != "lookup_order" || tc.MCPServerID != id || tc.SchemaHash != hashes["lookup_order"] || tc.Status != "succeeded" ||
		tc.EndedAt == nil || tc.EndedAt.Before(tc.StartedAt) || tc.TurnID == "" || tc.ArgumentsSummary != "" || tc.ResultSummary != "" {
		t.Fatalf("tool call %+v", tc)
	}
	var tools []api.TurnToolDTO
	for _, turn := range r.sessionTurns(c.sessionID) {
		if turn.ID == tc.TurnID {
			tools = turn.Tools
		}
	}
	if len(tools) != 1 || tools[0].Name != "lookup_order" || tools[0].Status != "succeeded" || tools[0].DurationMS < 0 {
		t.Fatalf("the call's turn lists %+v", tools)
	}
	attrs := r.spanAttributes(c.sessionID, "mcp.tool.execute")
	for _, want := range []string{`"mcp.server_id":"` + id + `"`, `"mcp.tool_name":"lookup_order"`, `"mcp.duration_ms":`} {
		if !strings.Contains(attrs, want) {
			t.Fatalf("mcp.tool.execute attributes %s lack %s", attrs, want)
		}
	}
}

func TestMCPCallFailedToolIsRecordedFailed(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("fail", r.ts.URL+"/fail/tools", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "tool_error").ID)
	c.say("call tool tool_error")
	c.waitReplies("Tool tool_error failed", 1)
	r.waitTurns(c.sessionID, 1)
	c.barrier(t) // a late or duplicate row would be written by now
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].RemoteName != "tool_error" || calls[0].MCPServerID != id || calls[0].Status != "failed" {
		t.Fatalf("tool calls %+v", calls)
	}
}

func TestMCPCallBargeInRecordsTheToolCancelled(t *testing.T) {
	r := newMCPCallRig(t, func(c *Config) { c.MCP.Timeout = 10 * time.Second })
	session := r.liveSession()
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, nil, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":4000}`)
	waitFnEvents(t, c, 2)
	bargeIn(t, session())
	c.waitReplies("wait never mind", 1)
	assertFnEvents(t, c, []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":true,"tool_call_id":"#1"}`})
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].RemoteName != "slow_lookup" || calls[0].Status != "cancelled" || calls[0].EndedAt == nil {
		t.Fatalf("tool calls %+v", calls)
	}
}

func TestMCPCallAsyncToolIsRecordedWhenItEnds(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("slow", r.ts.URL+"/slow", "")
	c := r.dial(r.flowWith(id, hashes, false, []string{id + "/slow_lookup"}, "slow_lookup").ID)
	c.say(`call tool slow_lookup {"delay_ms":300}`)
	assertFnEvents(t, c, []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}) // the background run ended
	c.hangUp()
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	calls := r.sessionTraces(c.sessionID).ToolCalls
	if len(calls) != 1 || calls[0].Status != "succeeded" || calls[0].EndedAt == nil || calls[0].TurnID == "" ||
		calls[0].EndedAt.Sub(calls[0].StartedAt) < 250*time.Millisecond {
		t.Fatalf("tool calls %+v (want the background run's outcome and duration, on its turn)", calls)
	}
}
```

- [ ] **Step 6: Run the records e2e.**

```bash
cd server && go test -race -count=3 -run 'TestMCPCallToolCallsAreRecorded|TestMCPCallFailedToolIsRecordedFailed|TestMCPCallBargeInRecordsTheToolCancelled|TestMCPCallAsyncToolIsRecordedWhenItEnds' . && cd ..
```

Expected: `ok govoice/server`. RED for this step was Step 2's compile failure (`newTurnRecorder` takes six arguments only after Step 3); to see a behavioural failure, stash only the wiring line in `resolveCall` (back to `nil, nil` for `addToolCall, mcpTool`) and expect `tool calls []`.

- [ ] **Step 7: Docs.** `docs/api/v1.md`, directly after the paragraph that starts `**Turn latencies.**` add:

```markdown
**Tool calls.** Every tool call the model makes is recorded when its result is known, or as `cancelled` when the call ended first. `GET /api/v1/sessions/{id}/traces` lists them under `tool_calls`: `remote_name` (the MCP tool's own name, or a built-in tool's such as `transfer_to_<step>` or `end_call`), `mcp_server_id` and `schema_hash` (MCP tools only), `status` (`succeeded`, `failed` or `cancelled`), `started_at`, `ended_at`, and `turn_id` (the turn the call belongs to; absent when that turn stored no row). Each turn of `GET /api/v1/sessions/{id}/turns` carries `tools`, `[{"name","status","duration_ms"}]`, built from the same rows (an empty list when it made none). Arguments and results are never stored: `arguments_summary` and `result_summary` are always empty.
```

`docs/privacy.md` (lines 163-165, wrapped): replace the three wrapped lines

```text
  are not exported). Debug events and stored traces are metadata only. Tool
  arguments/results are not stored by the server and are not exported to
  OTel (no tool-content redactor is configured).
```

with `  are not exported). Debug events and stored traces are metadata only.` followed by (wrapped the same way) `Tool calls are stored by name, status and timing only (`tool_calls`; `arguments_summary` and `result_summary` stay empty); their arguments and results are never stored and are not exported to OTel (no tool-content redactor is configured).`

- [ ] **Step 8: Gate.** Run the gate block with `3.3a-s8` (verify, e2e-echo, offline-test).

- [ ] **Step 9: Commit.**

```bash
git add server/call.go server/runtime/mcp.go server/runtime/mcp_tool_test.go server/api/traces.go server/api/turn_tools_test.go server/trace/adapter.go server/trace/adapter_test.go \
  server/storage/tool_calls_test.go server/turn_recorder_tools_test.go server/tool_records_e2e_test.go server/redaction_test.go server/stored_flow_call_test.go docs/api/v1.md docs/privacy.md
git commit -m "feat(server): record tool calls in session history and traces

The turn recorder stores one tool_calls row per announced call (MCP server,
remote name, schema hash, status, timing; no content), after the turn row
it references; a call settled later still joins its turn, and one whose
turn never got a row is kept unattached. A turn's tools are derived from
those rows, and MCP spans keep mcp.server_id, mcp.tool_name and
mcp.duration_ms. Fixes A3.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3.3b: the Call page shows the tool calls

**Files:**
- Modify: `ui/src/api/client.ts`, `ui/src/routes/sessions/callStats.ts`, `ui/src/routes/sessions/CallView.tsx`, `ui/src/components/timeline/TurnTimeline.tsx`, `ui/src/routes/sessions/callStats.test.ts`, `ui/src/routes/sessions/CallView.test.tsx`, `ui/src/components/timeline/TurnTimeline.test.tsx`, `ui/src/api/client.test.ts`
- Create: `e2e/tests-flows/42-tool-calls.spec.ts`

**Interfaces:**
- Consumes: `TurnDTO.tools` and `tool_calls` statuses from 3.3a; `mcpFlow`, `connectAdmin`, `openFlowTest`, `startCall`, `sendText`, `botReply`, `step` (e2e helpers); the existing `PipecatDebugPanel` (`toggle-debug-btn`, "Filter events", "RTVI event log").
- Produces: `turnToolSchema`, `TurnToolDTO`; `turnRows(session, turns)` (the `traces` parameter is removed).

- [ ] **Step 1: Write the failing Vitest tests.** In `ui/src/routes/sessions/callStats.test.ts`, replace the body of `describe("turnRows", () => {` up to (not including) `  it("falls back to the caller's words, then to a plain note", () => {` with:

```ts
  const turns = [
    {
      ...turn(2, "2026-10-12T14:00:06Z", "2026-10-12T14:00:09Z", "My bill went up.", "I can look into that."),
      tools: [
        { name: "crm.verify_identity", status: "succeeded", duration_ms: 160 },
        { name: "crm.lookup", status: "failed", duration_ms: 0 },
        { name: "crm.slow", status: "cancelled", duration_ms: 900 },
      ],
    },
    turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:01Z", "", "Hi there."),
    turn(3, "2026-10-12T14:00:43Z", "2026-10-12T14:00:45Z", "[async tool result: transfer_to_billing]", "Let me check", true),
  ];
  it("lists each turn in order with the reply, and notes what happened in it", () => {
    expect(turnRows(session(), turns)).toEqual([
      { id: "t1", at: "00:01", text: "Hi there.", notes: [], latencies: undefined },
      { id: "t2", at: "00:06", text: "I can look into that.", notes: ["Tool crm.verify_identity · 160 ms", "Tool crm.lookup failed", "Tool crm.slow cancelled"], latencies: undefined },
      { id: "t3", at: "00:43", text: "Let me check", notes: ["Interrupted by the caller"], latencies: undefined },
    ]);
  });
```

and in the next test replace `turnRows(session(), [turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:01Z", "Hello?", ""), turn(2, "2026-10-12T14:00:05Z", "2026-10-12T14:00:05Z", " ", "")], null)` with `turnRows(session(), [turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:01Z", "Hello?", ""), turn(2, "2026-10-12T14:00:05Z", "2026-10-12T14:00:05Z", " ", "")])`, and in `it("notes the reply time on the turn row"` replace `turnRows(session(), [withLat(1, { voice_to_voice_ms: 900 }), withLat(2, {})], null)` with `turnRows(session(), [withLat(1, { voice_to_voice_ms: 900 }), withLat(2, {})])`. These two, the `turnRows` describe and `CallView.tsx:156` are every caller of `turnRows` in `ui/src` (`grep -rn "turnRows(" ui/src`); re-run that grep after the edit and expect only two-argument calls. (If the existing expectation objects omit `latencies`, keep them as they are and only change `notes`; `toEqual` ignores an `undefined` property.)

In `ui/src/routes/sessions/CallView.test.tsx`, replace the test `it("lists the turns with what happened in them", async () => {` (through its closing `});`) with:

```tsx
  it("lists the turns with what happened in them", async () => {
    const withTool = { ...turns[1], tools: [{ name: "crm.verify_identity", status: "succeeded", duration_ms: 160 }] };
    setup({ getSessionTurns: vi.fn().mockResolvedValue([turns[0], withTool, turns[2]]) });
    const region = await screen.findByRole("region", { name: "Turns" });
    const items = within(region).getAllByRole("listitem").map((li) => li.textContent);
    expect(items).toEqual([
      "00:01Hi, you're through. How can I help?",
      "00:06I can look into that.Tool crm.verify_identity · 160 ms",
      "00:43Let me checkInterrupted by the caller",
    ]);
    expect(within(region).queryByText(/not recorded yet/)).toBeNull();
  });
```

Append to `ui/src/components/timeline/TurnTimeline.test.tsx`, inside the top-level `describe`, before its last `});`:

```tsx
  it("shows each tool call's outcome in words and tone", async () => {
    const user = userEvent.setup();
    const turns: TurnDTO[] = [
      { id: "t1", session_id: "s", ordinal: 1, started_at: "2026-10-05T12:00:00Z", transcript: "Book me in", assistant_text: "Done.", interrupted: false },
    ];
    const call = (id: string, status: string) => ({ id, session_id: "s", turn_id: "t1", remote_name: `tool_${id}`, schema_hash: "h", started_at: "2026-10-05T12:00:01Z", ended_at: "2026-10-05T12:00:01.200Z", status, arguments_summary: "", result_summary: "" });
    const traces: SessionTracesDTO = { session_id: "s", spans: [], events: [], tool_calls: [call("a", "succeeded"), call("b", "failed"), call("c", "cancelled")] };
    render(<TurnTimeline turns={turns} traces={traces} />);
    await user.click(screen.getByRole("button", { name: /Show traces/i }));
    expect(screen.getByText("succeeded")).toHaveClass("status--good");
    expect(screen.getByText("failed")).toHaveClass("status--bad");
    expect(screen.getByText("cancelled")).toHaveClass("status--neutral");
  });
```

Append to `ui/src/api/client.test.ts`, inside the `describe` that holds `it("handles sessions and traces query operations"`, after that test:

```ts
    it("parses the tool calls a turn made", async () => {
      const turn = { id: "t1", session_id: "s", ordinal: 1, started_at: "2026-10-05T12:00:01Z", interrupted: false, tools: [{ name: "lookup_order", status: "succeeded", duration_ms: 42 }] };
      const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(new Response(JSON.stringify({ data: [turn] }), { status: 200 }));
      const turns = await createApiClient(() => ({ key: "k", generation: 1 }), fetcher).getSessionTurns("s");
      expect(turns[0].tools).toEqual([{ name: "lookup_order", status: "succeeded", duration_ms: 42 }]);
    });
```

- [ ] **Step 2: Run them to see them fail.**

```bash
cd ui && npx vitest run src/routes/sessions src/components/timeline src/api/client.test.ts ; cd ..
```

Expected: the `turnRows` and `CallView` tests fail (no tool notes), the tone test fails (`succeeded` has `status--bad`), and `npx tsc --noEmit` would reject `tools` on the object literals (`turnRows` still takes three arguments).

- [ ] **Step 3: Implement.** `ui/src/api/client.ts`: directly before `export const turnSchema = z.object({` add:

```ts
/** One tool call a turn made: never its arguments or result. */
export const turnToolSchema = z.object({
  name: z.string(),
  status: z.string(),
  duration_ms: z.number(),
});
export type TurnToolDTO = z.infer<typeof turnToolSchema>;

```

and in `turnSchema` replace `  tools: z.unknown().optional(),` with `  tools: z.array(turnToolSchema).optional(),`.

`ui/src/routes/sessions/callStats.ts`: replace the signature line `export function turnRows(session: SessionDTO, turns: TurnDTO[], traces: SessionTracesDTO | null | undefined): TurnRow[] {` with `export function turnRows(session: SessionDTO, turns: TurnDTO[]): TurnRow[] {`, and replace the loop

```ts
    for (const c of traces?.tool_calls ?? []) {
      if (c.turn_id !== t.id) continue;
      if (c.status === "failed" || c.status === "error") notes.push(`Tool ${c.remote_name} failed`);
      else if (c.ended_at) notes.push(`Tool ${c.remote_name} · ${Math.max(0, Date.parse(c.ended_at) - Date.parse(c.started_at))} ms`);
      else notes.push(`Tool ${c.remote_name} running`);
    }
```

with

```ts
    for (const c of t.tools ?? []) {
      notes.push(c.status === "succeeded" ? `Tool ${c.name} · ${c.duration_ms} ms` : `Tool ${c.name} ${c.status}`);
    }
```

(If `SessionTracesDTO` is no longer used in `callStats.ts` after this, remove it from the import; it is still used by `path`/`transcript`/`endedByFlow` today, so it most likely stays.)

`ui/src/routes/sessions/CallView.tsx`: replace `{turnRows(s, turns, traces).map((r) => (` with `{turnRows(s, turns).map((r) => (`.

`ui/src/components/timeline/TurnTimeline.tsx`: replace

```tsx
                                <Status tone={tc.status === "completed" || tc.status === "success" ? "good" : "bad"}>
```

with

```tsx
                                <Status tone={tc.status === "succeeded" ? "good" : tc.status === "cancelled" ? "neutral" : "bad"}>
```

- [ ] **Step 4: Run them to see them pass.**

```bash
cd ui && npx tsc --noEmit && npx vitest run && cd ..
```

Expected: no type errors; every Vitest file passes.

- [ ] **Step 5: Write the browser test (L4).** Create `e2e/tests-flows/42-tool-calls.spec.ts`:

```ts
import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { botReply, connectAdmin, openFlowTest, sendText, startCall } from "../lib/flows-helpers";
import { mcpFlow, pace } from "../lib/mcp-helpers";

// A tool call seen live in the test call's RTVI event log, then on the call's page: its turn and its technical trace.
// The local test MCP server (cmd/mcptest) and the echo providers: free and deterministic.
test.skip(LIVE, "the MCP cases run against the echo stack only");

const NAME = "E2E Tool Records";

test("a tool call shows in the test call's events and on the call's page", async ({ page, stack }) => {
  await mcpFlow(stack, NAME, "/ok", ["lookup_order"], "MCP_TOKEN_TEST");
  await connectAdmin(page, stack);
  await openFlowTest(page, NAME);
  await startCall(page);
  await sendText(page, 'call tool lookup_order {"order_id":"42"}');
  await expect(botReply(page, /shipped/)).toBeVisible();

  // The client received the call's three events (newest first in the log).
  await page.getByTestId("toggle-debug-btn").click();
  await page.getByLabel("Filter events").fill("llm-function-call");
  await expect(page.getByLabel("RTVI event log").locator("code")).toHaveText(["llm-function-call-stopped", "llm-function-call-in-progress", "llm-function-call-started"]);
  // Ordering barrier: after one more answered turn the log must still hold exactly those three.
  await pace(page); // the stack's documented guard against the known lost-text-turn issue (mcp-helpers.ts)
  await sendText(page, "barrier");
  await expect(botReply(page, /You said: barrier/)).toBeVisible();
  await expect(page.getByLabel("RTVI event log").locator("code")).toHaveText(["llm-function-call-stopped", "llm-function-call-in-progress", "llm-function-call-started"]);
  await step(page, "tool call events in the test call");

  await page.getByTestId("disconnect-btn").click();
  await page.getByTestId("view-trace-link").click();
  await expect(page).toHaveURL(/\/sessions\/[^/]+$/);
  await expect(page.locator(".call-turns").getByText(/^Tool lookup_order · \d+ ms$/)).toBeVisible({ timeout: 15_000 });
  await step(page, "the tool call on its turn");

  await page.getByText("Technical trace").click();
  await page.getByRole("button", { name: /Show traces \(.*1 tool calls/ }).click();
  const item = page.getByTestId("tool-call-item");
  await expect(item).toContainText("lookup_order");
  await expect(item.getByRole("status")).toHaveText("succeeded");
  await step(page, "the tool call in the technical trace");
});
```

- [ ] **Step 6: Run it.** Build and run the echo browser stack, then repeat the new spec:

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
make e2e-fake 2>&1 | tee "$L/3.3b-s6-e2e-fake.log"
(cd e2e && npx playwright test -c playwright.flows.config.ts tests-flows/42-tool-calls.spec.ts --repeat-each=3) 2>&1 | tee "$L/3.3b-s6-repeat.log"
```

Expected: every test passes, including `42-tool-calls.spec.ts` three times. (To see it fail first, run the spec against a UI built from Step 3's `callStats.ts` change stashed: the turn note never appears.)

- [ ] **Step 7: Gate.** Run the gate block with `3.3b-s7` (verify, e2e-echo, offline-test).

- [ ] **Step 8: Commit.**

```bash
git add ui/src/api/client.ts ui/src/api/client.test.ts ui/src/routes/sessions/callStats.ts ui/src/routes/sessions/callStats.test.ts ui/src/routes/sessions/CallView.tsx ui/src/routes/sessions/CallView.test.tsx \
  ui/src/components/timeline/TurnTimeline.tsx ui/src/components/timeline/TurnTimeline.test.tsx e2e/tests-flows/42-tool-calls.spec.ts
git commit -m "feat(ui): show a call's tool calls on its turns

A turn's notes list its tool calls from the turn's tools (name, outcome,
duration), so calls settled after the reply are not missed, and the
technical trace colours succeeded, failed and cancelled calls by what the
server stores.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3.4: provider retries and circuit states on the call timeline (D-0.5)

**Files:**
- Modify: `core/resilience/resilience.go`, `server/registry.go`, `server/nvidia.go`, `server/registry_xaispeech.go`, `server/registry_offline_speech.go`, `server/server.go`, `server/timeline.go`, `server/route_obs_test.go`, `server/timeline_test.go`, `docs/observability.md`, `docs/operations_runbook.md`
- Create: `core/resilience/retry_hook_test.go`, `server/provider_timeline_e2e_test.go`

**Interfaces:**
- Consumes: `resilience.Execute`, `Breaker.SetObserver` observers in `BindMetrics`/`nvidiaBreaker`, `Server.activeCalls`, `SessionTimeline.Record`, `googleLLMConfig`.
- Produces: `resilience.Policy.OnRetry func(retry int)`; `StageSpec.OnRetry func(retry int)`; `CallSpec.OnProviderRetry func(stage, provider string, retry int)`; `Registry.onCircuit atomic.Pointer[func(stage, provider string, st resilience.State)]`, `(*Registry).circuit`; `(*Server).recordCircuit`; `Server.circuitMu`, `Server.circuitStates`; `activeCall.providers`; `callProviders(CallSpec) map[string]bool`.

- [ ] **Step 1: Write the failing tests.** Create `core/resilience/retry_hook_test.go`:

```go
package resilience

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestExecuteReportsEachRetryBeforeItsBackoff(t *testing.T) {
	slept := noSleep(t)
	var retries []string
	p := Policy{MaxAttempts: 3, OnRetry: func(retry int) { retries = append(retries, fmt.Sprintf("retry %d after %d sleeps", retry, len(*slept))) }}
	if err := Execute(context.Background(), nil, p, classify, nil, fail(errOutage)); !errors.Is(err, errOutage) {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"retry 1 after 0 sleeps", "retry 2 after 1 sleeps"}; !slices.Equal(retries, want) {
		t.Fatalf("retries %q, want %q", retries, want)
	}
	retries = nil
	_ = Execute(context.Background(), nil, p, classify, nil, fail(errClient))
	if len(retries) != 0 {
		t.Fatalf("a client error was retried: %q", retries)
	}
}

func TestExecuteContainsAPanickingRetryHook(t *testing.T) {
	noSleep(t)
	calls := 0
	p := Policy{MaxAttempts: 2, OnRetry: func(int) { panic("observer bug") }}
	err := Execute(context.Background(), nil, p, classify, nil, func(context.Context, int) error { calls++; return errOutage })
	if !errors.Is(err, errOutage) || calls != 2 {
		t.Fatalf("err = %v after %d attempts", err, calls)
	}
}
```

Append to `server/timeline_test.go` (add `"govoice/core/resilience"` to its imports):

```go
func TestCircuitStateGoesOnTheTimelinesOfCallsUsingThatProviderOncePerChange(t *testing.T) {
	uses, other := NewSessionTimeline(1, 10, time.Minute), NewSessionTimeline(2, 10, time.Minute)
	s := &Server{activeCalls: map[uint64]*activeCall{
		1: {timeline: uses, providers: map[string]bool{"llm/echo": true}},
		2: {timeline: other, providers: map[string]bool{"llm/google": true}},
	}}
	for _, st := range []resilience.State{resilience.Closed, resilience.Open, resilience.Open, resilience.HalfOpen, resilience.Closed, resilience.Closed} {
		s.recordCircuit("llm", "echo", st)
	}
	var got []string
	for _, e := range uses.Events() {
		if e.Type != EventCircuitState {
			t.Fatalf("event %+v", e)
		}
		got = append(got, e.Provider+":"+e.Details)
	}
	if want := []string{"echo:llm open", "echo:llm half_open", "echo:llm closed"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("timeline %q, want %q", got, want)
	}
	if n := len(other.Events()); n != 0 {
		t.Fatalf("a call on another provider got %d circuit events", n)
	}
}
```

(`server/timeline_test.go` must import `strings`; add it if absent.)

Create `server/provider_timeline_e2e_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"govoice/core"
	"govoice/providers/google"
)

// flakyLLM is a model backend that is down: every request is a retryable 503.
type flakyLLM struct{}

func (flakyLLM) GenerateStream(context.Context, string, string, []core.Message) (<-chan google.LLMStreamChunk, <-chan error, error) {
	return nil, nil, &google.ProviderError{Capability: "llm", BaseErr: google.ErrProviderUnavailable, SafeMessage: "injected outage", StatusCode: http.StatusServiceUnavailable, Retryable: true}
}

// The real Google processor against a dead backend, with the echo breaker the server shares: one turn of five
// attempts retries four times and opens the circuit (threshold 5), and the call's timeline says both.
func TestCallBehaviorTimelineShowsProviderRetriesAndTheCircuitOpening(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	r.s.registry.Register("echo.llm", func(_ *Config, spec StageSpec) (core.Processor, error) {
		llmCfg := googleLLMConfig(spec, r.s.registry.echoLLMBreaker)
		llmCfg.Provider = "echo"
		llmCfg.Client = flakyLLM{}
		llmCfg.RetryPolicy.MaxAttempts = 5
		return google.NewLLMProcessor(llmCfg)
	})
	c := r.dial(behaviorFlow(t, r, "Plain", "plainvoice", ``))
	c.say("hello")
	eventually(t, "the circuit to open on the call's timeline", func() bool { return r.has(EventCircuitState, "llm open") })

	rec := httptest.NewRecorder()
	r.s.timelines.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/timeline?session=1", nil))
	var body struct {
		Events []TimelineEvent `json:"events"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("/debug/timeline: %d %s", rec.Code, rec.Body.String())
	}
	var retries, circuits []string
	for _, e := range body.Events {
		switch e.Type {
		case EventProviderRetry:
			retries = append(retries, e.Provider+":"+e.Details)
		case EventCircuitState:
			circuits = append(circuits, e.Provider+":"+e.Details)
		}
	}
	if want := "echo:llm retry 1,echo:llm retry 2,echo:llm retry 3,echo:llm retry 4"; strings.Join(retries, ",") != want {
		t.Fatalf("retries %q, want %s", retries, want)
	}
	if strings.Join(circuits, ",") != "echo:llm open" {
		t.Fatalf("circuit events %q", circuits)
	}
	if c.ended() {
		t.Fatal("an LLM outage ended the call")
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
(cd core && go test -race -count=1 -run 'TestExecuteReportsEachRetry|TestExecuteContainsAPanicking' ./resilience/)
(cd server && go test -race -count=1 -run 'TestCircuitStateGoes|TestCallBehaviorTimelineShowsProviderRetries' .)
```

Expected: build failures: `unknown field OnRetry in struct literal of type Policy`; `unknown field providers in struct literal of type activeCall`, `s.recordCircuit undefined`.

- [ ] **Step 3: Implement.**

(a) `core/resilience/resilience.go`: in `type Policy struct`, after the `AttemptTimeout` field add:

```go
	// OnRetry, if set, is told just before a failed attempt is retried, with
	// the retry's number (1 for the second attempt). It runs on the caller's
	// goroutine and must not block; a panic in it is contained.
	OnRetry func(retry int)
```

In `Execute`, directly before `		if serr := sleep(ctx, delay); serr != nil {` add:

```go
		if p.OnRetry != nil {
			func() {
				defer func() { _ = recover() }() // an observer bug must not fail the request
				p.OnRetry(attempt + 1)
			}()
		}
```

(b) `server/timeline.go`: delete the four lines declaring `EventSTTRotation`, `EventLLMStart`, `EventTTSAudio` and `EventContextEviction`. Add `"strings"` and `"govoice/core/resilience"` to its imports and append:

```go
// recordCircuit puts a provider circuit's state change on the timeline of every
// live call whose start step uses that provider for that stage. Breakers report
// every success and failure, so the dedupe has its own lock and the server's
// mu is taken only for a real change; a circuit never seen before counts as
// closed. Breakers notify after unlocking, so reports from concurrent requests
// can arrive out of order and a timeline can end on "open" while the breaker is
// closed again (the circuit gauge has the same race).
// ponytail: matches the start step's candidates; a later step's providers are not matched.
func (s *Server) recordCircuit(stage, provider string, st resilience.State) {
	key := stage + "/" + provider
	s.circuitMu.Lock()
	prev, seen := s.circuitStates[key]
	if (seen && prev == st) || (!seen && st == resilience.Closed) {
		s.circuitMu.Unlock()
		return
	}
	if s.circuitStates == nil {
		s.circuitStates = map[string]resilience.State{}
	}
	s.circuitStates[key] = st
	s.circuitMu.Unlock()
	var timelines []*SessionTimeline
	s.mu.Lock()
	for _, c := range s.activeCalls {
		if c.providers[key] {
			timelines = append(timelines, c.timeline)
		}
	}
	s.mu.Unlock()
	for _, tl := range timelines {
		tl.Record(TimelineEvent{Type: EventCircuitState, Provider: provider, Details: stage + " " + strings.ToLower(string(st))})
	}
}
```

(c) `server/server.go`. In `type Server struct`, after `	activeCalls map[uint64]*activeCall` add `	circuitMu     sync.Mutex                  // guards circuitStates only (never held with mu)` and `	circuitStates map[string]resilience.State // last state each "stage/provider" circuit reported (recordCircuit)` and add `"govoice/core/resilience"` to the imports if absent. In `type activeCall struct`, after `	ending    *endReason` add `	providers map[string]bool // "stage/provider" of every candidate of the start step`. After `func stageProviders(c CallSpec) map[string]string {…}` add:

```go
// callProviders is "stage/provider" for every candidate (primary and fallbacks) of the call's start step.
func callProviders(c CallSpec) map[string]bool {
	m := map[string]bool{}
	add := func(stage string, primary StageSpec, fallbacks []StageSpec) {
		m[stage+"/"+primary.Provider] = true
		for _, f := range fallbacks {
			m[stage+"/"+f.Provider] = true
		}
	}
	add("stt", c.STT, c.STTFallbacks)
	add("llm", c.LLM, c.LLMFallbacks)
	add("tts", c.TTS, c.TTSFallbacks)
	return m
}
```

In the `active := &activeCall{` literal add `		providers: callProviders(call),`. After `	call.OnEndCall = s.onEndCall(call.StoredSessionID)` add:

```go
	call.OnProviderRetry = func(stage, provider string, retry int) {
		timeline.Record(TimelineEvent{Type: EventProviderRetry, Provider: provider, Details: fmt.Sprintf("%s retry %d", stage, retry)})
	}
```

In `NewServer`, after the line `	s.availability = providerAvailability{…}` add:

```go
	circuit := s.recordCircuit
	s.registry.onCircuit.Store(&circuit)
```

(d) `server/registry.go`. In `type StageSpec struct`, after `	Failover bool` add:

```go

	// OnRetry, if set, is told before each retried request (LLM and TTS
	// factories copy it into their provider's retry policy).
	OnRetry func(retry int)
```

In `type CallSpec struct`, after `	OnEndCall func(stepID string)` add:

```go
	// OnProviderRetry, if set, is told of every retried LLM or TTS request (timeline).
	OnProviderRetry func(stage, provider string, retry int)
```

In `type Registry struct`, after `	onTTSFallbackSkipped func()                                           // set by BindMetrics` add:

```go
	// onCircuit receives every breaker state report (the server's call timelines);
	// set once by NewServer, read by breaker observers on any goroutine.
	onCircuit atomic.Pointer[func(stage, provider string, st resilience.State)]
```

After `func circuitCode(state string) int64 {…}` add:

```go
// circuit passes a breaker state report on to the server's call timelines.
func (r *Registry) circuit(stage, provider string, st resilience.State) {
	if h := r.onCircuit.Load(); h != nil {
		(*h)(stage, provider, st)
	}
}
```

In `BindMetrics`, add `r.circuit(<stage>, <provider>, st)` as the last statement of every observer: `r.circuit("stt", "google", st)`, `r.circuit("llm", "google", st)`, `r.circuit("tts", "google", st)`, `r.circuit("llm", "xai", st)`, `r.circuit("stt", "xai", st)`, `r.circuit("tts", "xai", st)`; and change the two loop observers to

```go
		br.SetObserver(func(st resilience.State) {
			m.circuits.set(stage, "echo", "simulated", circuitCode(string(st)))
			r.circuit(stage, "echo", st)
		})
```

```go
		br.SetObserver(func(st resilience.State) {
			m.circuits.set(stage, "offline", "local", circuitCode(string(st)))
			r.circuit(stage, "offline", st)
		})
```

In `BuildCallPipeline`, replace the `build` closure's first line `		proc, err := r.BuildStage(cfg, stage, spec)` with:

```go
		if report := call.OnProviderRetry; report != nil {
			provider := spec.Provider
			spec.OnRetry = func(retry int) { report(stage, provider, retry) }
		}
		proc, err := r.BuildStage(cfg, stage, spec)
```

Copy the hook into every LLM and TTS factory's policy:
- `googleLLMConfig`: after `	c.CircuitBreaker = breaker` add `	c.RetryPolicy.OnRetry = spec.OnRetry`.
- `googleTTSConfig`: directly before its `	return c` add `	c.RetryPolicy.OnRetry = spec.OnRetry`.
- `xaiLLMProcessor`: after `	xaiCfg := xai.Config{…}` add `	xaiCfg.RetryPolicy.OnRetry = spec.OnRetry`.
- `openai.llm` factory: after the `oaCfg := openai.Config{…}` statement add `		oaCfg.RetryPolicy.OnRetry = spec.OnRetry`.
- `cartesia.tts` and `elevenlabs.tts` factories: directly before `		return cartesia.NewTTSProcessor(ttsCfg)` / `		return elevenlabs.NewTTSProcessor(ttsCfg)` add `		ttsCfg.RetryPolicy.OnRetry = spec.OnRetry`.

`server/nvidia.go`: in `nvidiaBreaker`'s observer add `			r.circuit(stage, "nvidia", st)` after `r.metrics.circuits.set(…)`; in `nvidia.llm` after `oaCfg := openai.Config{…}` add `		oaCfg.RetryPolicy.OnRetry = spec.OnRetry`; in `nvidia.tts` directly before its `return` add `		ttsCfg.RetryPolicy.OnRetry = spec.OnRetry`.

`server/registry_xaispeech.go` (`xai.tts`) and `server/registry_offline_speech.go` (`offline.tts`): directly after each `if spec.Failover { ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy) }` block add `		ttsCfg.RetryPolicy.OnRetry = spec.OnRetry`.

(e) Tests that used the deleted constants: in `server/route_obs_test.go` replace `EventLLMStart` with `EventLLMFirstToken` (twice) and `EventTTSAudio` with `EventTTSUnit`; in `server/timeline_test.go` replace `EventTTSAudio` with `EventTTSUnit`.

- [ ] **Step 4: Run them to see them pass.**

```bash
(cd core && gofmt -l ./resilience && go test -race -count=1 ./resilience/)
(cd server && gofmt -l . && go vet ./... && go vet -tags xaispeech ./... && go test -race -count=1 -run 'TestCircuitState|TestCallBehaviorTimeline|TestTimeline|TestRouteMetrics|TestFailoverPolicy' . && go test -race -count=3 -run TestCallBehaviorTimelineShowsProviderRetries .)
grep -rn "EventSTTRotation\|EventLLMStart\|EventTTSAudio\|EventContextEviction" --include='*.go' server
```

Expected: `ok` for each; the grep prints nothing; `go vet -tags xaispeech` compiles the speech factories.

- [ ] **Step 5: Docs.** `docs/observability.md`: after the paragraph under `### Routes between steps` add:

```markdown
### Provider retries and circuits

`provider_retry` is recorded each time an LLM or TTS request of the call is retried (`provider`, `details` `<stage> retry <n>`; `n` = 1 for the second attempt). `circuit_state` is recorded when a provider circuit the call uses changes state (`provider`, `details` `<stage> <closed|open|half_open>`); circuits are shared by every call, so each live call that uses the provider for that stage in its start step gets the event, whichever call's requests moved the circuit. Only the circuits exported as `voice_{stt,llm,tts}_circuit_state` report (google, xai, echo, offline, nvidia): a call on an OpenAI-compatible, Deepgram, Cartesia or ElevenLabs stage gets `provider_retry` events but no `circuit_state` (those breakers have no observer). STT stream dials do not record retries. Breaker reports from concurrent requests can arrive out of order, so a timeline can end on `open` while the circuit has closed again (the gauge has the same race).
```

`docs/operations_runbook.md`: in the provider-circuit section (the bullet that starts `- **Symptoms**: alert `GovoiceProviderCircuitOpen``), append to that bullet: ` The timelines (`/debug/timeline?session=<ID>`) of every live call using that provider show the `circuit_state` change (the circuit is shared, so a call may show a change other calls caused) and their own `provider_retry` events. OpenAI-compatible, Deepgram, Cartesia and ElevenLabs circuits are not reported on the timeline (no observer); their calls show retries only.`

- [ ] **Step 6: Gate.** Run the gate block with `3.4-s6` (verify, e2e-echo, offline-test).

- [ ] **Step 7: Commit.**

```bash
git add core/resilience/resilience.go core/resilience/retry_hook_test.go server/registry.go server/nvidia.go server/registry_xaispeech.go server/registry_offline_speech.go \
  server/server.go server/timeline.go server/timeline_test.go server/route_obs_test.go server/provider_timeline_e2e_test.go docs/observability.md docs/operations_runbook.md
git commit -m "feat(server): timeline records provider retries and circuit state

resilience.Policy.OnRetry reports each retry; the LLM and TTS factories
take it from the call, which records provider_retry. Breaker observers
pass state changes to the server, which records circuit_state once per
change on the live calls using that provider. The four event kinds
nothing records are deleted (D-0.5).

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3.5: phase close-out (flake sweep, full gate, review)

**Files:** none planned. Reviewer-driven fixes only, each with its own failing test and commit.

**Interfaces:** Consumes everything above. Produces the phase verdict.

- [ ] **Step 1: Flake sweep** (all logs under the flake directory).

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
(cd core && go test -race -count=20 ./tools/ && go test -race -count=20 -run 'ToolResult|ToolCall|Failover|TurnCompletion' . && go test -race -count=20 ./resilience/) 2>&1 | tee "$L/3.5-s1-core.log"
go test -race -count=20 -run 'FunctionCall|TestParity' ./transport/rtvi/ ./tools/compat/ 2>&1 | tee "$L/3.5-s1-transport.log"
for m in providers/echo providers/openai providers/xai providers/google; do (cd $m && go test -race -count=5 ./...) || exit 1; done 2>&1 | tee "$L/3.5-s1-providers.log"
(cd server && go test -race -count=3 -run 'MCPCall|CallBehaviorTimeline|TurnRecorder|CircuitState|TurnsCarry|ToolCallRows' ./ ./storage/ ./api/) 2>&1 | tee "$L/3.5-s1-server.log"
(cd ui && npx vitest run) 2>&1 | tee "$L/3.5-s1-vitest.log"
(cd e2e && npx playwright test -c playwright.flows.config.ts tests-flows/42-tool-calls.spec.ts tests-flows/30-mcp.spec.ts --repeat-each=3) 2>&1 | tee "$L/3.5-s1-playwright.log"
```

Expected: everything `ok` / passed. Any failure: name the test, keep the log, fix with a failing test first.

- [ ] **Step 2: Full gate, sequentially, on an idle machine.** Run the gate block with `3.5-s2` (verify, e2e-echo, offline-test). If `TEST_POSTGRES_DSN` is available or Docker can run `make test-postgres`, run it too (`3.5-s2-postgres.log`); it covers `TestToolCallRowsFollowTheirTurn` on Postgres. Expected: all green.

- [ ] **Step 3: Dead-code and bloat check.**

```bash
grep -rn "ToolsJSON" --include='*.go' server | grep -v _test.go
grep -rn "EventSTTRotation\|EventLLMStart\|EventTTSAudio\|EventContextEviction" --include='*.go' .
git diff --stat main...HEAD
```

Expected: the first grep shows only `server/storage/history.go` (the field and the `"[]"` default: Task 5.4 removes them); the second prints nothing.

- [ ] **Step 4: Adversarial review.** Dispatch an independent reviewer agent (AGENTS.md "Mandatory Code Review Workflow") on `git diff main...HEAD` with this brief:

> Review Phase 3 (tool-call events) against docs/superpowers/plans/2026-10-16-phase-3-tool-events.md. Hunt for: (1) any path where an announced call is never settled or settled twice (sequential, parallel claim failure, async ack failure, loop Close mid-run and closed-before-start, history refusal), any announcement of a call stale at claim time, and any announced call whose ToolCallFrame or ToolResultFrame can still be dropped (session fence, LLM failover attempt drop); (2) frames emitted while holding a lock other than the round's read lock, and whether a 2 s Reenter block in emit can stall a round or an async goroutine harmfully; (3) the FenceSurvivor on ToolCallFrame and ToolResultFrame: any processor between the LLM and the transport (assistant context and its stale-turn leniency guard, TTS, unit failure reporter, recorder) that mishandles a stale-generation frame; (4) turnRecorder: FK order, late results, bounds, the mutex never held across a write, close racing the run loop, rows lost when the channel is full; (5) privacy: any path that puts a tool name, arguments or result on the wire, a model-invented tool name in tool_calls or traces (it must become unknown_tool), arguments or results in tool_calls, tools_json, the timeline or logs; (6) Server.recordCircuit and Registry.onCircuit: data races, circuitMu and s.mu never held together, dedupe correctness, events on calls that do not use the provider; Policy.OnRetry: double reporting, panics; (7) YAGNI: anything added that no server path consumes (echo multi-call is test support for parallel e2e; voiceToken, liveSession, bargeIn, fnShape are test helpers), dead fields (TurnRecord.ToolsJSON is deferred to Task 5.4), duplicated test rigs; (8) behaviour changes versus main that no test pins (turn-completion tool-first leniency is now live and pinned by unit tests only; LLM failover forwards tool frames from finished attempts); (9) e2e determinism: every event sequence is re-checked after an ordering-barrier turn; flag any assertion that could pass with a late or duplicate event. Report each finding with file:line and a failing-test idea.

Resolve every finding with a failing test, then the fix, then `make verify`, then a `fix(<area>): …` commit.

- [ ] **Step 5: Deferred-minor sweep.** Record these in the PR description as deferred (not done here), each with its owner phase: drop `TurnRecord.ToolsJSON` and `turns.tools_json` (master 5.4); STT stream dial retries on the timeline; circuit observers (and gauges) for OpenAI-compatible, Deepgram, Cartesia and ElevenLabs breakers; Pipecat's batch `started` ordering (`function_calls/two_calls_parallel`); tool bars in the reply waterfall (ruled out, Q4); the existing barge-in test `TestMCPCallBargeInCancelsSlowTool` could reuse `liveSession`/`bargeIn`.

- [ ] **Step 6: Report.** In the PR description: the gate logs' paths and verdicts, the reviewer's findings and their resolutions, the deferred list, and the rulings below. State explicitly, as requirement notes for the human (not decided silently): (a) the offline stack has no tool-call case, because `llama3.2:3b` via Ollama (`offline/Modelfile`) is not deterministic about calling tools, so a case would test the model, not this code; the xai processor that `offline.llm` runs is covered by `TestXAIToolLoopFramesReachTheSession` (L1), and `make offline-test` ran as a regression check only (say whether it ran and passed); (b) whether the Postgres storage test ran (`TEST_POSTGRES_DSN` set, or `make test-postgres`), and if not, that `TestToolCallRowsFollowTheirTurn` ran on SQLite only. Phase 3 is done when Steps 1–4 are green and the reviewer has no open findings.

---

## Rulings (with evidence; binding for this phase)

1. **Wire privacy default = Pipecat's default report level `none`.** Evidence: `observer.py:174,203-205`; the client only needs `function_name` for client-executed functions (`dist/index.js:2499-2505`), which this server does not run; `docs/privacy.md` promises no tool content leaves the server. No knob (Q1).
2. **Barge-in settles an announced call with `cancelled: true`; an unannounced call sends nothing.** Evidence: `llm_service.py:2115-2135` broadcasts `FunctionCallCancelFrame` (a `SystemFrame`, `frames.py:1472`) → `stopped{cancelled:true}` (`observer.py:555-568`). This refines master Review Focus 3 ("no event from a stale generation") to "no *announcement* from a stale generation, and every announcement is settled".
3. **Async tools: announced at the call, settled when the background run ends; the ack is not reported.** Evidence: Pipecat sends `FunctionCallResultFrame` when the function's final result arrives (`llm_service.py:1755-1791`).
4. **Validation failures are announced and settled `failed`.** Evidence: Pipecat runs a missing function through `_build_missing_function_call_registry_item` and still broadcasts in-progress and a result (`llm_service.py:1721-1735`).
5. **Records are metadata only; `TurnDTO.tools` is derived from `tool_calls` at read time; no migration.** Evidence: the existing columns cover name, server, schema, status and timing; `docs/privacy.md`; late results (async, barge-in) would be missing from a write-time `tools_json`.
6. **`ToolCallFrame` already commits an LLM failover generation (kept); `ToolResultFrame` bypasses the abandoned-attempt drop.** Evidence: `llm_failover.go:44-47,252-256`.
7. **Timeline: retries via `Policy.OnRetry` for LLM and TTS stages; circuit states via the existing observers, once per change, on live calls using the provider.** D-0.5 as adopted.
8. **The echo model's multi-call** is the only provider-behaviour change made for tests; it lives in `providers/echo` (reached through `echo.llm` in the server registry), keeps single-call behaviour for one phrase, and is pinned by `TestEchoCallsEveryMCPToolItsMessageNames`.

## Rulings on the open questions (controller, 2026-10-16; binding)

- **Q1. No report-level knob.** Pipecat's default level is `none` and this plan ships it fixed. MCP and built-in tool names are operator-defined and are stored; names the model invented are not stored or announced (they become `unknown_tool`, Decision 1).
- **Q2. No argument or result summaries stored.** `arguments_summary` and `result_summary` stay empty.
- **Q3. Pipecat's batch `started` ordering is not matched.** Declared difference `function_calls/two_calls_parallel`.
- **Q4. No tool bars in the reply waterfall.** The turn note and the technical trace show tool calls.
- **Q5. New breaker observers for OpenAI-compatible, Deepgram, Cartesia and ElevenLabs are deferred.** The gap (retries recorded, no `circuit_state`) is stated in `docs/observability.md` and the runbook (Task 3.4 Step 5).

## Self-review

- **Spec coverage.** A3: Tasks 3.3a (rows written, traces API, turn tools, whitelist, retention unchanged and correct since summaries stay empty) and 3.3b (UI). G1 `ToolCallFrame`/`ToolResultFrame` row: 3.1 (posted by the loop; session trace branches, turn-completion leniency now live), 3.2a (RTVI), 3.2b (real client, goldens, matrix row "parity" with evidence), 3.3a (function-call record). G1 `tool_calls`/`AddToolCall` row: 3.3a. Timeline (G1 tail, D-0.5): 3.4. Master Review Focus 3: Review Focus 1 here. Required test layers: L1 every task; L2 3.1 (pipeline skeleton), 3.2a (wire: single, failing, parallel, barge-in, async, privacy), 3.3a (API/DB incl. barge-in and async), 3.4 (failure injection via `/debug/timeline`); L3 3.2b; L4 3.3b; L5 stated (no offline case). Postgres: `TestToolCallRowsFollowTheirTurn` through `eachStore`.
- **Placeholder scan.** Every code step holds the code or an exact text-anchored replacement. Anchors are by text; `:NNN` numbers refer to 03eb695 and are hints. One-line factory edits in 3.4 (d) name the exact statement they follow. No "similar to Task N".
- **Type consistency.** `WithFrames(func(core.Frame)) *ToolLoop`, `announce(ToolCall, time.Time) bool`, `report(ToolCall, ToolResult, error)`, `finish(ctx, call, started, announced, gate) (res ToolResult, err error)`, `startAsync(call, def, started, announced, commit) (ToolResult, bool, error)`, `runAsync(call, def, started, announced)` are used identically in `loop.go`, `parallel.go` and the tests. `FunctionCallStarted(string) []string` / `FunctionCallStopped(string, bool) []string` match their callers in `transport/ws`, the rtvi tests and `TestParityFunctionCallMessages`. `fnEvent{Type, Data, Raw, TextsBefore}` is defined once (`flow_e2e_test.go`) and used by `fnShape` (3.2a) and the client test (3.2b), whose JSON events carry `type`/`data`. `newTurnRecorder`'s six parameters match `resolveCall`, the three updated test call sites and `TestTurnRecorderStoresToolCallsWithTheirTurn`. `api.TurnToolDTO{Name, Status, DurationMS}` matches `TestTurnsCarryTheirToolCalls`, `TestMCPCallToolCallsAreRecorded` and the zod `turnToolSchema` (`name`, `status`, `duration_ms`). `Policy.OnRetry func(int)` = `StageSpec.OnRetry`; `CallSpec.OnProviderRetry func(string, string, int)` matches `server.go`. `recordCircuit(string, string, resilience.State)` matches `Registry.onCircuit`'s pointee type.
- **Design-review edits (second pass, all applied).** (1) The real-client test dials `r.flowRigE2E.ts` (the rig's `ts` is the MCP stub). (2) A model-invented tool name is announced and stored as its category (`announce`, `settleLocked`), with loop and recorder tests; the per-frame `component` attribute is never exported, so nothing else carries it. (3) `runAsync` and `startAsync` settle an announced call `cancelled` when the loop closes; `TestAsyncToolIsSettledCancelledWhenTheLoopCloses`. (4) `ToolCallFrame` survives the fence too, the failover forwards it from finished attempts, the leniency case ignores another turn's call; the fence test, a failover test and a leniency test pin it. (5) Every e2e event sequence is re-checked after an ordering-barrier turn (Go, client-js, Playwright), and the record tests add a barrier before hanging up. (6) The third `turnRows(…, null)` caller is updated. (7) Timeout wording: an MCP timeout stops `cancelled: false`. (8) Late-row and full-queue losses are documented as accepted. (9) Circuit dedupe has its own `circuitMu`; out-of-order breaker reports documented. (10) Circuit attribution and the no-observer gap are in the docs and runbook. (11) The `docs/privacy.md` anchor quotes the wrapped lines. (12) 3.3a's RED is the compile failure, or the wiring line stashed. (13) Leniency coverage is stated (unit tests; why no e2e). (14) The report states the offline and Postgres facts as requirement notes. Open questions are replaced by the controller's rulings.
- **Type consistency after the second pass.** `startAsync` returns `(ToolResult, settledElsewhere bool, error)`; `finish`'s `async` flag means exactly that. `assertFnEvents(t, c, want) []fnEvent` and `(*e2eCall).barrier(t)` are defined in `server/tool_events_e2e_test.go` (3.2a) and used in 3.2a and 3.3a. `toolCaller.late` is used only by `TestToolCallOfAnAbandonedCandidateStillReachesTheSession`. `Server.circuitMu` guards only `circuitStates`.
- **Fixes made during self-review.** (1) The recorder originally wrote `tools_json` at turn time; late results would be missing, so `TurnDTO.tools` is now derived at read time (Decision 3). (2) Results for unannounced calls would have reached the client (stale `ToolCallFrame` dropped, `ToolResultFrame` surviving); the transport now stops only announced ids, and the recorder ignores unknown ids. (3) `turnRecorder.close` read run-loop maps from another goroutine and could race a wedged run loop (and send on a closed channel); a mutex and a `closed` flag now guard both. (4) The registry hook was first a plain field written after `go registry.WarmOnStart`; it is an `atomic.Pointer`. (5) `dialToken`'s parameter `token` would have shadowed a `token` method; the helper is `voiceToken`. (6) The failover pin test `TestToolCallIsTheFirstOutput` passes before the task; it is labelled a pin, not a failing test. (7) The async e2e uses a 1 s tool so "settled after the ack was answered" has a wide margin without sleeping.
