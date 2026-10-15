# Review Remediation Master Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. This is a **master plan**: it fixes scope, order, dependencies and acceptance tests for nine phases. Before starting a phase, expand it into its own step-level TDD plan (`docs/superpowers/plans/2026-10-15-phase-N-<name>.md`) using superpowers:writing-plans, then execute that. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Resolve every finding in the 2026-10-15 deep review (bugs, incomplete Pipecat-parity features, dead code, duplicates, server→UI gaps) without regressing the call path.

**Architecture:** Fix correctness first (small, independent, test-first), then the one large structural change (a shared STT runner in `core/` that also relocates the dial off the run loop), then close the highest-value Pipecat gap (tool-call events), and only then delete dead code, so that nothing is deleted that a later phase would have wired.

**Tech Stack:** Go 1.27 workspace (`core`, `server`, `providers/*`, `offline`, root module for `transport`/`vad`/`smartturn`/`tools`/`cmd`), React/TS/Vite (`ui/`), SQLite/Postgres via goose migrations, protobuf RTVI frames.

**Spec:** `docs/superpowers/specs/2026-10-15-deep-review.md` (sections A–G; item ids like A1, D1, G1 below refer to it). Reference behavior: `pipecat-impl/` and `docs/pipecat-parity-matrix.md`.

## Global Constraints

Copied from `AGENTS.md`; every task inherits them.

- `core/` never imports `server/`; everything added to `core/` must be consumed by `server/` on a call path (strict YAGNI).
- Every queue/channel/slice is bounded; every goroutine has a termination path and in `core/` is spawned via `session.Go`.
- No `context.Context` in new struct fields; `defer cancel()` immediately after derived contexts.
- Never hold a lock across I/O; hot audio path uses atomics and does not allocate per frame.
- Monotonic time only for durations.
- Errors wrapped with `%w`, inspected with `errors.Is/As`; transport writers and provider callbacks `recover()`.
- All code passes `go test -race` in every module (`make test`); `gofmt -w .` clean.
- Conventional commits, one logical change each; commit trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Provider secrets are read from the environment only; never write or store keys (user memory: provider-secrets-env-only).
- Wire protocol stays compatible with the unmodified Pipecat RTVI 2.1.0 client.
- **Per-phase gate (AGENTS.md "Mandatory Code Review Workflow"):** an independent adversarial reviewer agent audits the phase diff for YAGNI/bloat, races, goroutine leaks, time handling, cleanup. Phase is not done until `make verify` (or `go test -race ./...` in each module in `Makefile MODULES`) is green and the reviewer's findings are resolved.

## Review Focus

Failure modes the review implies that no single finding's fix naturally tests. Each is pinned by a named test in the owning task.

1. STT provider dial hangs for 3 s while a fallback STT is configured: call must survive and fail over, not die with `ErrProcessorWatchdog` (Task 2.3).
2. Caller audio keeps arriving while the STT stream is still dialing: no audio lost (pre-roll), no unbounded buffer (Task 2.2).
3. Tool call arrives during a barge-in: no `ToolCallFrame`/RTVI event from a stale generation (Task 3.2).
4. Client sends 48 kHz audio in chunks whose sample count is not a multiple of 3: output length and phase must match a single-shot resample (Task 1.4).
5. A raw transcript candidate for a segment the turn processor forgot: session must continue (Task 1.3).

---

## Phase 0 — Decisions (RESOLVED 2026-10-15: user chose "go with your recommendations")

Every recommendation below is the adopted decision. Execution style: subagent-driven; Phase 1 first.

| # | Decision | Needed by | Recommendation |
|---|----------|-----------|----------------|
| D-0.1 | Whisker (G3): retire, or keep behind a build tag outside `core/`? | Phase 5, 8 | Keep as `tools/compat/whisker` with its extra `DebugEvent` fields moved to a `whisker`-tagged file; delete `Session.DebugDropped`. Retiring loses a documented P6-2 deliverable. |
| D-0.2 | DTMF output: frame path (`OutputDTMFFrame`) or direct `out.send`? | Phase 5 | Frame path, because it matches Pipecat and lets redaction/observers see digits; `send_dtmf` posts the frame and waits on a completion signal. If that is more than ~60 lines, delete the frame instead. |
| D-0.3 | Keep `Semantic` turn evidence (needs an eager-stop speculation gate on the roadmap)? | Phase 5 | Delete; re-add with the gate if/when it is scheduled. |
| D-0.4 | Provider voice settings (ElevenLabs stability/similarity/style/speed, Cartesia min_volume): expose in flow config or delete the fields? | Phase 5 | Expose ElevenLabs four (Pipecat parity, cheap); delete the rest. |
| D-0.5 | Timeline events (G1): wire retry/circuit-state only, or all six? | Phase 3 | Wire `ProviderRetry` and `CircuitState` only; delete the other four. |
| D-0.6 | Flow version history API (B2): build, or leave as storage-only? | Phase 7 | Leave; no consumer. |
| D-0.7 | `docs/superpowers/` (23k stale lines): leave, prune, or move out of tree? | Phase 8 | Move the 35 historical files to `docs/history/` with a README note; keep `plans/` and `specs/` new. |
| D-0.8 | "No context in structs": enforce (rework failover journals, provider streams) or document an exception for trace parentage? | Phase 9 | Document the exception in `AGENTS.md` for trace-parent contexts only; fix the rest opportunistically when touched. |

---

## Phase 1 — Independent correctness fixes

No dependencies; tasks parallelizable. Each task: write failing test → implement → `go test -race` the module → commit.

### Task 1.1 — Single validated publish path (A2, D7)
**Files:** Modify `server/storage/flows.go:23-95`, `server/storage/repositories.go:117,608-640`, `server/flow/service.go` (`SetMCPTools`, `Duplicate`), `server/api/flows.go:54-90` and route registration for `PUT /flows/{id}/mcp-tools`; Test `server/storage/flows_test.go`, `server/api/flows_test.go`.
**Interfaces:** Produces: `FlowRepository.CreateWithVersion…` calls `r.s.validateSelections(ctx, tx, selections)` (same helper `MutateAndPublish` uses). `SetMCPTools` becomes `Update{Config: current, MCPTools: &tools}`; `PublishVersion` and its route are deleted; `ui/src/api/client.ts` already sends config+`mcp_tools` on `PUT /flows/{id}` and must stop calling the removed route.
**Tests (red first):** `TestCreateFlow_DisabledMCPServer_Returns400`, `TestCreateFlow_SchemaDrift_Returns400`, `TestCreateFlow_TooManySelections_Returns400`, `TestCreateFlow_UnknownServer_Returns400_Not404`, `TestDuplicateFlow_SchemaDrift_Returns400`.
**Also:** merge `countMCPReferences`/`mcpReferenceCounts` into one function taking the existing `queryer` interface.
**Commit:** `fix(server): create and duplicate flows validate MCP selections like update` then `refactor(server): fold PublishVersion into MutateAndPublish`.

### Task 1.2 — Stop leaking error text into stored traces (A4)
**Files:** Modify `server/trace/adapter.go:454`; Test `server/trace/adapter_test.go`.
**Behavior:** `End(err)` stores `fmt.Sprintf("%T", err)` (matching `otelexport.go:394`), never `err.Error()`.
**Test:** `TestSpanEnd_ErrorCategoryNeverContainsMessage` with an error whose text contains `api_key=SECRET`.
**Commit:** `fix(server): trace spans record error type, not message`.

### Task 1.3 — Transcript pending-turn limit is not session-fatal (A5)
**Files:** Modify `core/transcript/processor.go:166-206`, `core/transcript/finalizer.go:172-174,256,351`; Test `core/transcript/processor_test.go`.
**Behavior:** on `ErrRetiredTurnLimit`, reset the finalizer (as `turn/processor.go:484` does with `mgr.Reset`), emit a trace event, continue; never return it from `ProcessFrame`.
**Test:** `TestProcessor_PendingTurnLimit_ResetsAndContinues` feeds 129 never-completed turns then a normal turn and asserts the normal turn finalizes and `ProcessFrame` returned nil throughout.
**Commit:** `fix(core): transcript turn-limit resets instead of failing the session`.

### Task 1.4 — Stateful 48→16 kHz resampler without per-frame allocation (A6)
**Files:** Modify `core/transport/resample/resampler.go:72-151`, `core/audio/filter/processor.go:34,43`, `core/audio/mixer/gain.go:69`; Test `core/transport/resample/resampler_test.go`, benchmark `BenchmarkDownsample48To16`.
**Behavior:** carry the 0–2 remainder samples in the struct across calls; take a caller-provided destination or reuse an internal buffer sized once.
**Tests:** `TestDownsample48To16_ChunkingInvariant` (random chunk sizes 1..960 concatenated equals single-shot output, byte for byte); benchmark must report 0 allocs/op after warm-up (`-benchmem`).
**Commit:** `fix(core): 48k→16k resampler carries remainder samples and reuses buffers`.

### Task 1.5 — Small server and transport fixes (A13–A19, A22)
One commit each, each with a regression test:
- A13 `server/server.go:553-578,687-710`: every `fail(...)` after `core.NewSession` closes the session and processors. Test: inject a failing `SetTracer`; assert provider fake's `Close` called once.
- A14 `server/mcp/http.go:52-63,126-130` and `Service.Get`: return `ErrNotFound` only for not-found; DB errors → 500. Test with a closed DB.
- A15 `server/server.go:842-852`, `server/demo_api.go:38`: replace `pingMu`-across-ping with a singleflight-style cached result (`golang.org/x/sync/singleflight` is already an indirect dep; promote it) and move the demo discovery/DB work outside `demoMu`. Test: 50 concurrent `/ready` calls issue one ping.
- A16 `server/api_routes.go:28-39`: check the limiter before `Authenticate`, but key it on client address and exempt a request that presents a valid key only if the limiter has capacity — decide against the code comment's intent and document it. Test: 100 wrong keys from one address then a right key from another address → 200.
- A17 `server/mcp/http.go:191`: `Server *ServerView`. Test: detach-delete JSON has no `server` key.
- A18 `transport/ws/transport.go`: `conn.SetReadLimit(MaxEncodedFrameSize+headroom)`. Test: 48 KB message accepted, 80 KB closed with a protocol error (not counted as client disconnect).
- A19 `providers/xai/llm.go:90`: remove the `os.Getenv("XAI_API_KEY")` fallback. Test: empty key → constructor error.
- A22 `ui/src/voice/VoiceClient.ts:420`: delete the `ws://127.0.0.1:9090/ws` branch; `VoiceClient.test.ts` asserts URL derivation uses `location`.

### Task 1.6 — Lifecycle nits (A20, A21)
`core/llm_failover.go:294`: stop `g.timer` on completion, interruption and `Close` (test with a fake clock/short timeout: no candidate call after `Close`). `smartturn/worker.go:277-302`: log the `start()` error with its stderr tail on each respawn failure (test: a worker binary that exits 1 yields a logged error).

**Phase 1 gate:** per-phase reviewer; `make verify`.

---

## Phase 2 — Shared STT runner, off-loop dial, drop recovery (A1, A8, D1)

The largest change; ships as a pilot plus migrations so each commit is green.

### Task 2.1 — Design the runner against ttsunit (no code merged)
Read `core/ttsunit/ttsunit.go` and the identical scaffolding in `providers/deepgram/stt.go:62-104,190-229,318-420`. Write the interface in the phase-2 plan before coding: a `core/sttunit.Runner` with hooks `Dial(ctx) (Conn, error)`, `Send(Conn, audio)`, `Decode(raw) []Result`, `Finish(Conn)`, owning `ProcessFrame` switch, stream lifecycle, turn binding, receive loop, close/drain. **Done when** the phase plan lists the hook set and which of the four ws adapters need which hooks.

### Task 2.2 — Runner core with pre-roll and off-loop dial (A1)
**Files:** Create `core/sttunit/runner.go`, `core/sttunit/runner_test.go`; Modify `core/session.go` only if a new helper is required (prefer none).
**Behavior:** `ProcessFrame` never dials. First audio of a turn starts the dial via `session.Go`; audio received meanwhile is held in a bounded pre-roll (`MaxPreRollBytes`, default 5 s of 16 kHz PCM) and flushed in order when the stream opens; overflow drops oldest and counts a metric.
**Tests:** `TestRunner_DialDoesNotBlockProcessFrame` (dial sleeps 3 s; `ProcessFrame` returns < 10 ms; the 2 s watchdog does not fire), `TestRunner_PreRollFlushedInOrder`, `TestRunner_PreRollBounded`, `TestRunner_DialFailureReportsErrorFrame` (so `stt_failover` sees it).
**Commit:** `feat(core): sttunit runner dials off the run loop`.

### Task 2.3 — Drop recovery (A8) and failover proof
**Behavior:** on receiver exit (EOF, NormalClosure, idle timeout) while a turn is bound and no final arrived, the runner emits `TranscriptInvalidationFrame` for that turn then an `ErrorFrame`; xai's M3-09 logic (`providers/xai/stt.go:640-662`) is the template.
**Tests:** `TestRunner_MidUtteranceDrop_InvalidatesTurn`; integration in `core/stt_failover_test.go`: primary dials for 3 s, fallback answers → transcript delivered, session alive (Review Focus 1).
**Commit:** `fix(core): STT drop mid-utterance invalidates the turn so failover replays`.

### Task 2.4 — Migrate deepgram (pilot), then cartesia, elevenlabs, nvidia
One commit per adapter. Each keeps only `wsURL/dial`, message decode and result assembly; delete the copied scaffolding (`stream` struct, `Attach/Close`, `ProcessFrame`, `ensureStream`, `receive`, `finish/stopTurn/release/stop`, `reportError`, `frameGen`, `join`). Existing `stt_test.go` for each provider must pass unchanged; add `STTCounts` to deepgram (B7) in the deepgram commit.
**Expected:** −~250 lines per adapter (−~1,000 total); `go test -race ./...` in each provider module green.

### Task 2.5 — Google and xAI STT
They differ more (google gRPC bidi; xai reconnect and ordering rules). Only move them if the runner expresses their needs without new hooks; otherwise leave them and fix A8 locally (`google/stt.go:820`) and A1 by moving `ensureStream` dial to `session.Go`. Decision recorded in the phase plan. xai `sendMu` held across network writes (A15 second half): remove by relying on `coder/websocket`'s concurrent-write safety.

**Phase 2 gate:** reviewer focuses on goroutine termination, pre-roll bound, generation fencing on late finals. Run `make loadtest-smoke` before and after; p99 `stt_final` must not regress.

---

## Phase 3 — Tool-call events: close the Pipecat function-call gap (G1)

Depends on Task 1.2 (adapter) only loosely; independent of Phase 2.

### Task 3.1 — Tool loop posts frames
**Files:** Modify `core/tools/loop.go` (around the dispatch at ~:391), `core/tools/parallel.go`; Test `core/tools/loop_test.go`.
**Behavior:** before executing a call the loop emits `*core.ToolCallFrame` (generation = the call's generation) downstream; after it emits `*core.ToolResultFrame`; async tools emit the same pair for the background run. Frames carry only what the structs already define.
**Tests:** `TestLoop_EmitsCallThenResult`, `TestLoop_ParallelCalls_EmitPairsInDispatchOrder`, `TestLoop_StaleGeneration_EmitsNothing` (Review Focus 3), `TestLoop_FailedTool_ResultStatusFailed`.
**Commit:** `feat(core): tool loop emits ToolCallFrame and ToolResultFrame`.

### Task 3.2 — RTVI function-call messages
**Files:** Modify `transport/rtvi/rtvi.go` (add formatters), `transport/ws/transport.go` (handle `core.FrameToolCall`/`FrameToolResult` in the output switch, same pattern as `FrameTTSStarted`); Test `transport/rtvi/rtvi_test.go`, `tools/compat` fixtures.
**Behavior:** emit `llm-function-call-started`, `-in-progress`, `-stopped` with the payload shapes in `pipecat-impl/src/pipecat/processors/frameworks/rtvi/models.py:274-334`. Gate behind the same RTVI version the client negotiates; unknown to older clients is ignored by client-js.
**Tests:** golden-JSON tests per message; `tools/compat` conformance gets one new fixture generated from Pipecat (`generate_fixtures.js` — if D-0.7/Phase 8 would delete it, regenerate first).
**Commit:** `feat(transport): RTVI llm-function-call messages`.

### Task 3.3 — Persist tool calls; fix trace names (A3)
**Files:** Modify `server/call.go` (a small observer on the frame stream, or the existing `turnRecorder`), `server/storage/history.go:568` (`AddToolCall` now called), `server/trace/adapter.go:218` whitelist adds `mcp.server_id`, `mcp.tool_name`, `mcp.duration_ms`; Test `server/storage/history_test.go`, `server/api/traces_test.go`.
**Tests:** `TestCall_ToolCallPersisted`, `TestTraces_ToolCallsAndTurnTools_Populated`, e2e `server/flow_e2e_test.go` asserts `GET /sessions/{id}/traces` returns the MCP tool call.
**UI:** `CallView` already renders `tool_calls`; add a Vitest asserting a non-empty list renders.
**Commit:** `feat(server): record tool calls in session history and traces`.

### Task 3.4 — Provider retry / circuit-state timeline events (D-0.5)
**Files:** Modify `server/timeline.go`, the breaker hook in `core/resilience` consumers in `server/registry.go`; Test `server/timeline_test.go`.
**Behavior:** record `EventProviderRetry` and `EventCircuitState` where the breaker already reports; delete the other four unused event constants.
**Commit:** `feat(server): timeline records provider retries and circuit state`.

**Phase 3 gate:** run the Pipecat client (`examples/rtvi_client`, `make test-e2e-go`) against a tool-calling flow; confirm client-js surfaces function-call events without errors.

---

## Phase 4 — Failover and transport correctness

### Task 4.1 — Interruption frame on barge-in (A7)
`transport/ws/transport.go:1009`: serialize `Frame_Interruption` (already in `framespb`) to the client on `FrameInterruption`. Test: `TestBargeIn_SendsProtobufInterruption`; manual check with the pinned client that buffered playback stops (update the matrix row "RTVI wire" if it now does).
### Task 4.2 — LLM sync-error failover (A9)
`core/llm_failover.go:221`: treat a synchronous `ProcessFrame` error (including `ErrTooManyTasks`) like STT/TTS: try the next candidate. Test: primary returns `ErrTooManyTasks`, fallback answers.
### Task 4.3 — Output resampler keyed by rate (A10)
`transport/ws/transport.go:926-941`: map keyed by sample rate like the input side. Test: two frames at 24 k then 16 k produce correct lengths.
### Task 4.4 — VAD worker routing and respawn (A11)
`vad/worker_executor.go:552-603,178`: respawn backoff runs in its own goroutine; sessions whose pinned worker is down are re-routed to a live worker (new session state is created lazily). Test with a fake worker that crash-loops: sessions on it get VAD from the other worker within one frame interval.
### Task 4.5 — ttsunit abandon race (A12)
`core/ttsunit/ttsunit.go:246-265`: replace the `nextSeq--` assumption with an explicit "abandoned" set consumed by `waitSeq`. Test: concurrent `expire` + new unit with refused `queue.Go` never wedges (`-race -count=50`).
### Task 4.6 — Remaining config/registry cleanups
`effectiveVoiceTokenLifetime` (B/E): honor `token.max_lifetime` or delete the knob; `management.go:68-115` ordering so `unavailableFlowProviders` runs before `SetStore`; `probe.go:186` close `proc` on partial failure; `storage` `StopSecs` naming lint (ST1011).

**Phase 4 gate:** reviewer; `-race -count=20` on `core/ttsunit` and `vad`.

---

## Phase 5 — Dead-code removal (after Phases 2–3, so nothing wired is deleted)

Order matters: each task is one commit and keeps `go build ./... && go vet ./...` green in every module (`make test` compiles all).

### Task 5.1 — core frames and handlers (G2, G4)
Delete `StartFrame`, `CancelFrame`, `TTSAudioFrame`, `SystemMessageFrame` and every handler branch (list in review §C and `deadcode` output; use `go run golang.org/x/tools/cmd/deadcode@latest -tags xaispeech -test=false ./cmd/... ./examples/...` before and after). `OutputDTMFFrame` per D-0.2. If D-0.1 = keep Whisker, move its use of `StartFrame`/`CancelFrame` to whisker-local frame types.
### Task 5.2 — core turn and context (G2, G4)
`TurnAnalyzer`/`AnalyzerStop` impl (keep the `turn.TurnAnalyzer` interface only if `smartturn` still needs it), `Manager.Interrupt`, `retiredOrder`, `TurnEventNoSpeechTimeout`/`InitialSilenceTimeout` + their special cases in `idle.go:290`, `async_trigger.go:121`, `session.go:996,1047`; `Semantic` path per D-0.3; `ConversationContext` system-role branches, `AddToolCall/AddToolResult/EstimatedTokens`; `Session.OnUpstream`, `tools/loop.go OnAsyncResult`, `filter.Delayer/Chain.Len/Chain.Delay`, `DroppedOperations`, `RedactToolContent`/`redactTraceText`, `SpeakIndex`, `Buffered`, `ValidateToolPairing` (or convert per G4), `netpool.Requests`, `SplitNext` (move to `tools/ttssim` if only it uses it), `DebugDropped` + `DebugEvent` extras per D-0.1.
### Task 5.3 — core knobs nobody sets
Remove config fields listed in review §C ("core knobs") and the three duplicate clock seams (collapse `IdleClock`, `turn.Clock`, `transcript.PolicyClock` into one `core.Clock` only if tests need it; otherwise delete). Tests that set these fields are rewritten to use the defaults.
### Task 5.4 — server dead code
Trace-content purge (`storage/retention.go:32-34,161-213,342-365`); adapter machinery unreachable after Task 3.3 (UUIDv7 inference, `custom.*` copier, unused whitelist keys, `?turn_id=` filter and its UI params); `flow.Service.StartSession/ResolveSession` and the `RuntimeTools == nil` branch; `FlowRepository.Create`; `fallback.go BreakerOpen/Candidates`; `Config.VADProcessorConfig`, `LoadConfig`; `endreason` `connection_lifetime`; `mcp.ServiceConfig.Max*Bytes`; test-seam exports; `Server.PublicAddr/AdminAddr` if unused after tests move; `Pagination` json tags; `catalog.DefaultEntry` vs `probe.defaultEntry` (keep one).
### Task 5.5 — provider and transport dead code and knobs
`google/stt.go:949 assembleLocked`, `:1037-1044`, `DeduplicatedResults`; `pacing.go` `Flush`, `QueuedFrames`, test hooks moved behind an unexported test-only setter; `transport.go RTVI()/Pacer()`; `DefaultServerConfig`; `token.RedactToken`; `rtvi/wire_encoder.go` debug encoder; `xai.DefaultConfig`; `smartturn/stats.go Fallbacks/Crashes` (or wire them into metrics — wire is one line each and both are operationally useful: prefer wiring); provider config knobs per D-0.4; xai single-impl interfaces.
### Task 5.6 — UI dead code
`client.ts:202-233` `stepConfigV2Schema` and unused exports; `App.tsx` `clearKey` becomes a Sign-out button (B10) with a test replacing `App.test.tsx:83`; hard-coded color list in `noHardcodedColors.test.ts` becomes a glob.

**Phase 5 gate:** `deadcode` output is a strict subset of the previous run; `git diff --stat` reported in the PR; reviewer checks nothing deleted is referenced by `docs/` examples or `config/`.

---

## Phase 6 — De-duplication

### Task 6.1 — Unary TTS onto ttsunit (D2)
Rewrite `providers/google/tts.go` (unary) and `providers/xai/tts.go` (unary) as `ttsunit` attempts following `google/tts_streaming.go:66-79`. Existing TTS tests pass unchanged; delete the two private schedulers (−~600 lines).
### Task 6.2 — Shared SSRF dial guard and warm conn in `core/netpool` (D6)
Move `allowedIP/metadataV6/cgnat` and `warmConn` once; `openai`, `nvidia`, `google` import them. Test: table of blocked ranges (loopback, link-local, CGNAT, metadata) moves with the code.
### Task 6.3 — LLM SSE/tool-call helpers (D5)
Single SSE reader and function-call assembler in `core` (xai's `SSEReader` is the more bounded one); openai and xai adopt it. Keep the provider wire shapes in the providers.
### Task 6.4 — Failover scaffolding (D8)
One generic `proxySession`, `classOf`, `noteFailover` and counters struct for LLM/STT/TTS failover; unify "speak a line" paths (D9): `IdleController.say` routes through `AssistantContextProcessor.Say` so idle goodbyes enter history and recordings (behavior change — add `TestIdleGoodbye_AppearsInHistory`).
### Task 6.5 — tools/ de-duplication (D3, D4, D10)
New `tools/internal/stack` (build server, free ports, write config, spawn, token, scrape) and `tools/internal/rtviclient`; `loadtest` and `livecheck` adopt them; WAV helpers once; fold `provtest/fake.go` faults into `providers/echo` or drop dry mode. Verify with `make loadtest-smoke` and `make live-dry` (whatever targets exist in `Makefile`).
### Task 6.6 — server small dups
Request-id helper `api.RequestID(w, r)` used by the five handlers; Dashboard/Server views share one operations-status query key.

**Phase 6 gate:** line-count delta recorded; `make test` and `make perf-gate` unchanged within tolerance.

---

## Phase 7 — Server→UI completeness

### Task 7.1 — Live-call context injection UI (B1)
`ui/src/api/client.ts`: `postSessionContext`, `runSession` (match `server/live_calls_api.go`); `CallView.tsx`: operator-note box and "run" button visible only for active sessions; Vitest for success, 404 (call ended), validation errors.
### Task 7.2 — Readiness detail (B4) and flow paging (B5)
Add `missing_env` to `readinessSchema` and render variable names in `FlowsView`; replace `limit=100` with a paged `useFlows` (or an "all" helper that follows `offset`). Test with 101 flows.
### Task 7.3 — Catalog honesty (B6)
`server/catalog`: mark xAI STT/TTS entries `requires_build_tag: xaispeech` (or filter in `/providers` when the binary lacks the adapters, via the existing `registry_noxaispeech.go` hook); UI disables them. Test: `-tags ""` build lists no xAI speech.
### Task 7.4 — RTVI `bot-tts-text` (B8)
Emit from `transport/ws` on `AssistantMessage`/TTS text frames per Pipecat's observer; fixture-compare with `pipecat-impl/tests/test_rtvi_client_serializer.py`.
### Task 7.5 — Idle goodbye `NoSpeechTimeout` UX check (B9)
Confirm after Phase 5 that user-idle prompts still fire; add an e2e test (`idle.user_timeout`) so removal of `NoSpeechTimeout` cannot regress it.

---

## Phase 8 — Repo hygiene and documentation

### Task 8.1 — Delete orphaned tooling
`tools/eval/{eval.go,scenarios,cmd/govoice-eval}` (only if D-0.1 retires Whisker; otherwise keep the runner), `deploy/sbom.json` (generate in `release-security`), `baseline/python/`, `scripts/plot.py`, `docs/linux-ci-report.md`, `tools/vadcompare/pipecat_probs.py`; decide `tools/compat/generate_fixtures.js` (needed by Task 3.2 — keep and wire into `make verify`).
### Task 8.2 — Stale references
Fix `AGENTS.md:51` (`server/credential`), `docs/pipecat-parity-matrix.md:146-147`, `e2e/README.md` path, deploy file headers ("Voice Framework V1 / spec §39", pinned `govoice:v1.0.0`), `perf-gate.yml` to `make ort` and `-tags xaispeech`, `go mod tidy` at root (drops the `providers/echo` require, fixes `golang.org/x/time` comment), fold `offline/contract` into `offline/adapter` or `server` and drop its `go.work`/Dockerfile entries.
### Task 8.3 — Move history out of the way (D-0.7)
`git mv` every file under `docs/superpowers/` that predates 2026-10-15 into `docs/history/superpowers/` (list them with `git ls-files docs/superpowers | grep -v 2026-10-15`), add a `docs/history/README.md` saying they describe removed packages, and keep this plan, its phase plans and the copied spec under `docs/superpowers/{plans,specs}/`.
### Task 8.4 — Parity matrix gets the new vocabulary
Add a column or marker for **planned (hook exists)**, **parked**, **superseded**, and re-label: function-call RTVI → `parity` after Phase 3; `Whisker` → parked per D-0.1; DTMF output per D-0.2; mid-call context updates → planned. Update "Known gaps" (remove items fixed here; keep the rest honest).

---

## Phase 9 — Close-out

### Task 9.1 — AGENTS.md alignment
Apply D-0.8; add a line that `core/` additions must be consumed by `server/` *or* be tagged parked in the matrix; add `deadcode` to the verification gate (`make deadcode` target using the command above, advisory in CI).
### Task 9.2 — Final adversarial review
Independent reviewer on the combined diff (AGENTS.md workflow): YAGNI/bloat, races (`go test -race -count=5 ./...` all modules), goroutine leaks (`goleak`-style assertion in `core/sttunit` and `vad` tests, or a final `runtime.NumGoroutine` check in the e2e suite), monotonic time, resource cleanup.
### Task 9.3 — Measure
Re-run `make loadtest-smoke`, `make perf-gate`, `make test-e2e-go`, the UI `npm test`, and `make live-*` dry runs; paste before/after into the PR. Expected: ≥ −5,000 lines net, no p99 regression, `deadcode` list reduced to documented library-only exports.

---

## Dependency graph

```
Phase 0 ──┬─────────────► Phase 5 (decisions D-0.1..4), Phase 8 (D-0.7), Phase 9 (D-0.8)
          └─► Phase 3.4 (D-0.5), Phase 7 (D-0.6)
Phase 1 ──► (independent) ──► Phase 3.3 (adapter shrink uses 1.2)
Phase 2 ──► Phase 5.5 (provider cleanup after migration), Phase 6.3
Phase 3 ──► Phase 5.1/5.2 (frames must be wired before dead ones are deleted), Phase 8.1 (fixtures)
Phase 4 ──► independent
Phase 5 ──► Phase 6 (less to dedupe), Phase 8.4
Phase 7 ──► after Phase 5.4 (UI params)
```

Parallelizable: Phase 1 tasks with each other; Phase 1 ∥ Phase 2 ∥ Phase 4; Phase 6.1–6.3 with each other.

## Self-review

- **Coverage:** every review item has a home — A1→2.2/2.5, A2→1.1, A3→3.3, A4→1.2, A5→1.3, A6→1.4, A7→4.1, A8→2.3, A9→4.2, A10→4.3, A11→4.4, A12→4.5, A13–A19,A22→1.5, A20,A21→1.6, B1→7.1, B2→D-0.6, B3→(no change; config-only by design), B4,B5→7.2, B6→7.3, B7→2.4, B8→7.4, B9→7.5, B10→5.6, C→5.1–5.6, D1→2.4, D2→6.1, D3,D4,D10→6.5, D5→6.3, D6→6.2, D7→1.1, D8,D9→6.4, D11→6.6, E→5.4/5.5 (setter/mux collapse added there), F→none, G1→3, G2/G4→5, G3→D-0.1.
- **Gap noted:** review item E ("replace five exported setters with one `http.ServeMux` in `EnableManagement`") needs an explicit task: add to Task 5.4 as the last bullet when that phase is expanded.
- **Placeholders:** this is a master plan; step-level code lives in each phase plan, which must contain failing-test code and exact commands before execution. Phases 0 decisions are the only blocking unknowns.
- **Type consistency:** names introduced here (`core/sttunit.Runner`, `MaxPreRollBytes`, `api.RequestID`, `tools/internal/stack`) are defined in the phase plans that create them; Phase 2 and Phase 6.5 depend on those names being stable.
- **Risk ranking:** highest risk is Phase 2 (touches the live STT path of four providers) and Task 6.4's idle-goodbye behavior change; both have load-test gates.
