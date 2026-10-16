# Switchboard deep review — 2026-10-15

Scope: core, server, providers, transport, vad/smartturn, tools, offline, deploy, ui. Python excluded.
Method: five independent reviewer agents (one per area) + deadcode/staticcheck with the Go 1.27 toolchain; every headline bug below was re-verified by hand against the source. Lines are `path:line`.

**Revision 2 (same day):** section C ("dead code") was re-examined against Pipecat (`pipecat-impl/` @ the pinned commit) and `docs/pipecat-parity-matrix.md`. Not everything unreferenced is dead: some of it is Pipecat functionality whose hook exists but was never wired. See **section G** for the four-bucket reclassification, which supersedes the flat list in C wherever they disagree. **Correction:** revision 1 recommended deleting `tools/compat/whisker`; it is a documented deliverable (matrix row "Whisker", plan item P6-2), so it is *parked*, not dead (G3).

Verdict in one line: the call path is solid (pinning, fencing, pacing, secret handling, RTVI framing all check out) — the problems are a handful of real runtime bugs, a large dead surface in core that AGENTS.md's YAGNI rule forbids, and ~1,600 lines of copy-pasted provider scaffolding.

---

## A. Bugs (fix these)

| # | Sev | Where | What |
|---|-----|-------|------|
| A1 | **HIGH** | `providers/{deepgram,cartesia,elevenlabs,nvidia,google,xai}/stt.go` `ensureStream` (e.g. deepgram:194→318→349) | STT dial + retries run **synchronously inside `ProcessFrame`** on the session run loop. Retry policy allows 2–3 × 5 s; `core/budget.go:26` watchdog is 2 s and server never overrides it. A provider dial hanging >2 s kills the call with `ErrProcessorWatchdog` instead of letting `stt_failover` move on; anything under 2 s freezes VAD/barge-in. Dial must happen off-loop (`session.Go`) with audio buffered into the pre-roll. |
| A2 | **HIGH** | `server/storage/flows.go:45-67`, `api/flows.go:181` | `POST /flows` (and `Duplicate`) re-implements tool-selection validation inline; returns raw `errors.New("MCP schema hash changed")` → **500**, and bare `sql.ErrNoRows` → **404 "flow not found"**. `PublishVersion`/`MutateAndPublish` use `validateSelections` → 400. Inline copy also skips `MaxSelectedTools`. Fix: call `r.s.validateSelections(ctx, tx, selections)`. |
| A3 | **HIGH** | `server/storage/history.go:568` `AddToolCall` | **Never called.** `tool_calls` table is never written → `/sessions/{id}/traces` `tool_calls` always `[]`, `TurnDTO.Tools` always empty, retention purge of tool_calls operates on nothing. MCP spans do land in `trace_spans` but the adapter whitelist (`trace/adapter.go:218`) drops `mcp.tool_name` etc. Operators cannot see which tool ran. Either wire it from `core/tools/loop.go` or delete the whole column/DTO/purge path. |
| A4 | MED | `server/trace/adapter.go:454` | `End(err)` writes `err.Error()` verbatim into `attributes["error.category"]` and persists it — bypasses the redactor. Route build errors reach it (`route_obs.go:80`) and those are exactly the errors `logSetupFailure` refuses to log because they may contain endpoint/credential fragments. otelexport correctly exports only `%T`. |
| A5 | MED | `core/transcript/finalizer.go:173,257,352` → `processor.go:166+` → `session.go:812` | `ErrRetiredTurnLimit` (128 pending turns) propagates as a processor error and **fails the session**. A candidate for a segment the turn processor forgot (closed set reset at >64, `turn/processor.go:540`) creates a turn that never completes. `turn/processor.go:484` recovers from the same condition with `Reset`; transcript should too. |
| A6 | MED | `core/transport/resample/resampler.go:136-151` | 48k→16k path does `len/3` and **drops the 1–2 remainder samples of every chunk**; never uses `r.phase`. Client chunks of arbitrary size (`transport/ws/transport.go:799`) drift the clock. Also 3 heap allocs per frame on the 50 fps path (`:72,83,115,128`), against the zero-alloc rule; same in `filter/processor.go:34,43`, `mixer/gain.go:69`. |
| A7 | MED | `transport/ws/transport.go:1009` | Barge-in never sends the protobuf `interruption` frame (proto has `Frame_Interruption`; Pipecat's serializer emits it). The comment admits the client keeps playing buffered audio. Send it. |
| A8 | MED | `providers/google/stt.go:820`, deepgram:407, elevenlabs:467, cartesia:458, nvidia:463 | Receiver exits silently on EOF/timeout/NormalClosure with a turn bound and no final: no `TranscriptInvalidationFrame`/`ErrorFrame`, so `stt_failover.go:475` never replays. xai/stt.go:640 handles this correctly — copy it. |
| A9 | MED | `core/llm_failover.go:221` vs stt/tts failover | Synchronous error from primary LLM `ProcessFrame` is returned (session fails) rather than failing over; STT/TTS wrappers fail over. `openai/llm.go:386` returns `ErrTooManyTasks` synchronously → busy session kills the call. |
| A10 | MED | `transport/ws/transport.go:926-941` | `outResampler` keyed once from the first frame's rate, never re-keyed; input side keys by rate. Latent only because server pins all TTS to `GoogleTTS.SampleRate` (registry.go:214,403,446 — misnamed global). |
| A11 | MED | `vad/worker_executor.go:552-603,178` | Respawn backoff sleeps on the worker's own run loop; sessions pinned by `id % len(queues)` with no re-route → every session on a crash-looping worker is VAD-blind while the other worker idles. |
| A12 | LOW-MED | `core/ttsunit/ttsunit.go:246-265` + `tts_failover.go:404-452` | `abandon` assumes serialization with `synthesizeUnit`, but `TTSFailover.expire` calls `w.run` from an `AfterFunc` goroutine concurrently with the run loop. If the newer unit took `nextSeq` and the older one's `queue.Go` is refused, `finishedSeq` never reaches the abandoned seq → later units wait forever at `:185`. |
| A13 | LOW-MED | `server/server.go:553-578,687-710` | After `core.NewSession` succeeds, later `fail(...)` paths return without `session.Close()` → provider clients/VAD stream leak per attempt. |
| A14 | LOW | `server/mcp/http.go:52-63,126-130` | DELETE maps every non-`ErrMCPReferenced` error (incl. DB errors) to 404; `ErrMCPReferenced` branch after `Get` is unreachable because `Service.Get` collapses all errors to `ErrNotFound`. |
| A15 | LOW | `server/server.go:842` `storeHealthy`; `demo_api.go:38` | Mutex held across `PingContext` (1 s) / MCP discovery + DB — "never hold locks across I/O". xai/stt.go:485,677 `sendMu` across network writes, same. |
| A16 | LOW | `server/api_routes.go:28-39` | Auth fail-limiter runs **after** `Authenticate`, so it only swaps 401→429; every guess is still fully evaluated. Cosmetic. |
| A17 | LOW | `server/mcp/http.go:191` | `Server ServerView \`json:"server,omitempty"\`` — omitempty is a no-op on structs; detach-delete sends a zero `ServerView`. Make it a pointer. |
| A18 | LOW | `transport/ws/wire.go:15` | `MaxEncodedFrameSize=64KB` but no `SetReadLimit`; coder/websocket default is 32 KB → 32–64 KB messages close as 1009 and count as client disconnect. |
| A19 | LOW | `providers/xai/llm.go:90` | Falls back to `os.Getenv("XAI_API_KEY")`, masking a missing keyring credential. |
| A20 | LOW | `core/llm_failover.go:294` | First-token timer never stopped on completion/interrupt/Close. |
| A21 | LOW | `smartturn/worker.go:277-302` | Crash-loop respawn discards `w.start()` error (with stderr tail), logs nothing. |
| A22 | LOW | `ui/src/voice/VoiceClient.ts:420` | Fallback `ws://127.0.0.1:9090/ws` targets the admin listener which serves no `/ws`. Dead and wrong — delete. |

AGENTS.md "no context in structs" violations (deliberate but a rule is a rule — decide once): `transport/ws/transport.go:106`, all six provider `activeStream.ctx`, `vad/processor.go:86`, `core/session.go:77,131,132`, `stt_failover.go:115` (journal retains up to 4096 contexts), `tts_failover.go:93`, `llm_failover.go:93`, `tools/parallel.go:21`.

---

## B. Incomplete features (library → server → UI)

| # | Where | Gap |
|---|-------|-----|
| B1 | `server/live_calls_api.go` `POST /sessions/{id}/context|run` | Complete, documented API; **zero UI** (`CallView.tsx` is read-only). UI is the only missing piece. |
| B2 | Flow version history | Storage keeps versions; only `current_version` exposed. No list/rollback endpoint → API gap, not just UI. |
| B3 | Retention, recordings | Config-only; no API at all. |
| B4 | `server/flow/service.go:91` `readiness.missing_env` | Server sends it; UI zod schema strips it (`client.ts:166`), FlowsView shows only `reasons`. |
| B5 | `ui/src/api/client.ts:582` `getFlows` | Hard `limit=100`, no paging; Dashboard/CallsView/ToolPicker silently miss flow 101. |
| B6 | `server/catalog/data/xai.yaml` | Advertises stt/tts unconditionally; adapters exist only under `-tags xaispeech`. Console offers providers the binary can't run (only `management.go:113` rejects, late). |
| B7 | `providers/deepgram/stt.go` | No `STTCounts` → `server.go:1183` STT error metric is 0 for Deepgram. |
| B8 | `transport/rtvi` | No `bot-tts-text` emitted; stock client expects it. |
| B9 | `core/turn/manager.go:588` `TurnEventNoSpeechTimeout` | Can only fire from `Observe` (processor doesn't tick before a turn opens) → never fires in practice, yet idle/async_trigger/session all special-case it. |
| B10 | UI auth | No sign-out; `App.tsx:111 clearKey()` defined, never called; test asserts "no way to clear it but reloading". Otherwise the flow (fragment magic link → in-memory bearer, no cookie/CSRF) is sound. |

---

## C. Unreferenced code (verified zero non-test callers — but see G: not all of it should be deleted)

**core/** (~600 lines, zero server impact)
- 7 frame types never constructed in production: `CancelFrame`, `StartFrame`, `SystemMessageFrame`, `TTSAudioFrame`, `ToolCallFrame`, `ToolResultFrame`, `OutputDTMFFrame` — plus every handler branch for them in session.go (trace naming :974-990, :1066-1092), all three failovers, ttsunit, google/llm, google/tts, cartesia/stt, google/xai stt `StartFrame` cases, `audio/dtmf/processor.go:271` (the tool calls `out.send` directly). Only `tools/compat/whisker` posts them.
- `turn/stop.go:98-138` `TurnAnalyzer/AnalyzerStop` (server uses `smartturn.Stop`); `turn/manager.go` `Semantic*` evidence path (:72,121,247,594-623, detector.go:61), `retiredOrder` (write-only), `Manager.Interrupt`.
- `context.go` system-role branches (:149-182,566,587-606; providers take `cfg.SystemPrompt` directly), `AddToolCall/AddToolResult/EstimatedTokens` (:295-325,362,216).
- `session.go:117,873` `OnUpstream`; `observer.go:100` `DebugDropped`; `tool_pairing.go` `ValidateToolPairing`; `tools/loop.go:50` `OnAsyncResult`; `audio/filter/filter.go` `Delayer/Chain.Len/Chain.Delay`; `trace_async.go:85` `DroppedOperations`; `trace.go:24,197,261` `RedactToolContent/redactTraceText`; `idle.go:178` `SpeakIndex`; `stt_failover.go:149` `Buffered`; `debug.go:13-19` seven `DebugEvent` fields core never populates; `sentence_export.go SplitNext` (tools/ttssim only); `netpool.Requests`.

**server/**
- Trace-content purge machinery that can never match (adapter whitelist never admits `transcript/prompt/text/tool.*`, and the adapter isn't the session tracer — only otel is, `server.go:687`): `storage/retention.go:32-34,161-213,342-365` + `trace_spans/trace_events` EXISTS clauses. ~120 lines.
- `trace/adapter.go:79-100,216-257,282-339` — turn/session UUIDv7 inference, `session_label/turn_label`, most of the attribute whitelist, `copyStructuredMetadata`: unreachable since only `route.*`/`call.end`/`mcp.*` spans arrive. `?turn_id=` on `/traces` can never match (core ids are `turn-N`, adapter stores UUIDs → column always NULL).
- `flow/service.go:325,367` `StartSession/ResolveSession` (+ `history.go:119` nil-RuntimeTools branch); `storage/repositories.go:59` `FlowRepository.Create`; `flow/fallback.go:130-154` `BreakerOpen/Candidates`; `config.go:387` `VADProcessorConfig`; `config_load.go:14` `LoadConfig`; `timeline.go:21-32` six never-recorded event kinds + `TimelineEvent.Provider`; `endreason.go:58` `connection_lifetime` (lifetime hardcoded 0 at server.go:417); `mcp/service.go:93` three `Max*Bytes` never set; `server.go:370` `WorkerStatus=="degraded"` never assigned; `api/api.go:47` `Pagination` json tags; `catalog.DefaultEntry` (server/probe.go:208 re-implements it); `retention.WithClock`, `NewResolverWithFS`, `finalizePendingDeletesHook` test seams in prod structs; `Server.PublicAddr/AdminAddr`.

**providers/transport**
- `google/stt.go:949` `assembleLocked`; `:1037-1044` identical if/else branches; `:333` `DeduplicatedResults` never incremented.
- `transport/ws/pacing.go:467` `Flush`, `:1089` `QueuedFrames`; `transport.go:1123 RTVI()`, `:1133 Pacer()`; pacing.go:126 `AfterSelect/BeforeWriteAdmission` test hooks checked on every write; `DefaultServerConfig`; `transport/token.RedactToken`; `rtvi/wire_encoder.go` `EncodeDebugEvent` + helpers; `xai.DefaultConfig`; `smartturn/stats.go` `Fallbacks/Crashes`.
- ~30 config fields server never sets: elevenlabs `IncludeTimestamps`, `Stability/SimilarityBoost/Style/Speed`; cartesia `MinVolume/MaxSilenceSecs`; nvidia `RivaRate/CustomDictionary/Insecure/AllowPrivate/Conn`; google TTS `CredentialsFile/LanguageCode/ChunkDuration/MaxSynthesisUnitChars`, STT `Endpoint/CredentialsFile/StreamOverlap/MaxRequestBytes`; xai `MaxLineLength/MaxEventSize/MaxTotalSize`; `DrainTimeout/InterimResults/Version/Pool` on four adapters; `Now func()` seam on all six STT configs.
- core knobs likewise never set: `ContextConfig.MaxToolIdentities`, `TTSFailoverConfig.UnitTimeout`, `STTFailoverConfig.PreRollBytes`, `AsyncTriggerConfig.MaxQueue/Settle/Poll/MaxWait`, `IdleConfig.GoodbyeWait`, `RouterConfig.*`, `ToolLoopConfig.StaleCheckInterval/MaxAsyncInFlight/AsyncTimeout/Now`, `record.Config.*` except Dir, `transcript.Config.*` except defaults, every `turn.Config` field except `StartStrategies`.

**ui/**
- `client.ts:202-233` `stepConfigV2Schema/StepConfigV2/FlowConfigPayload` (schema never parses anything); exports `MCPDetachResultDTO`, `demoInstallSchema`, `AuthSnapshot`, `SessionFilterParams`; `App.tsx:111 clearKey`; `VoiceClient.ts:420` 9090 fallback. npm deps and components are all live.

**tools / repo** (~4,600 lines HIGH confidence)
- `tools/eval/{eval.go,scenarios/,cmd/govoice-eval}` (~1,150) — no Makefile/CI/script reference; keep `govoice-bench`, `govoice-turn-eval`, `pipecat/run.sh`. **`tools/compat/whisker/` is NOT in this list (revision 2): it is parked, see G3.** Caveat: `tools/eval/eval.go` is the only importer of whisker, so deleting it leaves whisker with no runner — decide G3 first.
- `deploy/sbom.json` (1,879; 147 modules stale, nothing reads it — generate in release job). `baseline/python/` (422). `scripts/plot.py` (191, reads nonexistent `results/`). `docs/linux-ci-report.md`. `tools/compat/generate_fixtures.js` (133). `tools/vadcompare/pipecat_probs.py`.
- `docs/superpowers/` — 23k lines of plans referencing removed packages/targets (`/api/v1/agents`, `server/credential`, `cmd/toolcheck`, `make e2e-free`…). Relocate.
- Stale refs: **`AGENTS.md:51` cites `server/credential` (doesn't exist)**; `docs/pipecat-parity-matrix.md:146-147` links two missing benchmark docs; `e2e/README.md` wrong fixtures path; `deploy/*` still says "Voice Framework V1 / spec §39"; `perf-gate.yml:27` hand-curls ORT without checksum instead of `make ort`; `offline/contract` as its own go.work module for build-tagged tests only; root `go.mod` requires `providers/echo` that no root package imports (`go mod tidy`).

---

## D. Duplicates (collapse)

| # | Lines | What |
|---|-------|------|
| D1 | ~1,000 | `deepgram/cartesia/elevenlabs/nvidia/stt.go` — ~330 lines each byte-similar: `stream` struct, Attach/Close, `ProcessFrame` switch (identical), `ensureStream`, `receive` defer, `finish/stopTurn/release/stop`. Only dial URL, decode, and transcript assembly differ. One core STT runner mirroring `core/ttsunit.Runner` (Dial/Decode/Finish hooks). This is also where A1/A8 get fixed once. |
| D2 | ~600 | `google/tts.go:139-611` and `xai/tts.go:150-551` — two full unary TTS schedulers (seq/cond/abandon/20 ms chunking/usage) re-implementing `core/ttsunit.Runner`, which every other TTS path already uses; `google/tts_streaming.go:66-79` shows a unary request is ~10 lines as a ttsunit Attempt. Both still wired (registry.go:233, registry_xaispeech.go:60). |
| D3 | ~500 | `tools/loadtest/server.go` vs `tools/livecheck/server.go` — same randHex/freePorts/build/writeConfig/StartServer/token/Prometheus parse. Plus bash/TS copies in `tools/eval/pipecat/run.sh`, `e2e/global-setup-flows.ts`, `scripts/dev.sh`. |
| D4 | ~250 | RTVI WS client: `tools/livecheck/rtvi.go` vs `tools/loadtest/client.go` (and server's e2e tests have a third). |
| D5 | ~200 | SSE + tool-call assembly: `openai/llm.go:546-626` vs `xai/sse.go`; `callAcc` vs `xaiFunctionCallAssembly`; `toolResultOutput` ≡ `XAIToolResultOutput`; `historyMessages` ≡ `xaiHistoryItems`. |
| D6 | ~100 | SSRF allowlist `openai/dial.go:23-54` ≡ `nvidia/dial.go:163-187` ("separate module, so copied"); `google/warm.go:29-53` ≡ `nvidia/dial.go:137-161`. Belongs in `core/netpool`. |
| D7 | — | Three publish paths: `storage/repositories.go:117 PublishVersion` (only via `PUT /flows/{id}/mcp-tools`), `flows.go:95 MutateAndPublish`, `flows.go:23 CreateWithVersion…`. Fold `SetMCPTools` into `Update{MCPTools:&tools}`, drop `PublishVersion` + the route; fixes A2 as a side effect. `countMCPReferences` vs `mcpReferenceCounts` identical SQL. |
| D8 | — | core: three failover `proxySession/classOf/noteFailover/*Counters` copies; three test-clock seams (`IdleClock`, `turn.Clock`, `transcript.PolicyClock`) none set by server; `cloneMetadata` vs `cloneAttributes`; `closeSegments` vs `markDropped` eviction. |
| D9 | — | Two "speak without the model" paths: `AssistantContextProcessor.Say` (tracked, in history, recorded) vs `IdleController.say` (raw `SynthesisUnitFrame`, bypasses all three) → **idle goodbyes never appear in history or recordings**. Two "synthetic user notice → Reenter at LLM" paths (`idle.go:527`, `turn_completion.go:287`, + `async_trigger.go:369`). |
| D10 | ~100 | `tools/provtest/fake.go` fake providers vs `providers/echo` (the one the server registers). Three WAV readers in tools (loadtest/audio.go, livecheck/clips.go, provtest/wav.go), four markdown table renderers. |
| D11 | — | Request-id-from-context idiom ×5 in server/api; `DashboardView`/`ServerView` two query keys for one endpoint. |

---

## E. Over-engineering

- `server/server.go:283-311` five exported `Set*ManagementAPI/SetStore` setters + four handler fields + prefix if-ladder in `handleAPI`; sole caller is `EnableManagement` in the same package. One `http.ServeMux` replaces it. Single-impl interfaces nothing substitutes: `api.FlowService`, `SessionRepository/TraceRepository`, `retention.Store` (+3 functional options, one caller), `dental.Servers/Flows`, xai `STTStream/STTStreamOpener/TTSClient`, `vad.SharedExecutor` forwarding wrapper. `trace/adapter.go:129` `enqueue` guards one non-blocking send with an atomic + RWMutex + recover.
- `SessionController` consumers type-assert for `EmitDebug/NextUnitID/TailIndex` in 8+ places instead of the interface carrying them.
- `session.go:279-290` `alreadyAsync` passthrough never true.
- `api_routes.go:179` `effectiveVoiceTokenLifetime` silently caps at 1 min; `token.max_lifetime` (5m in every yaml) mostly does nothing.

---

## F. Verified clean (no action)
Registry version pinning across publish; secret wiping and never-logged setup errors; SQL parameterization; migrations ↔ repository types; pacing media clock (monotonic, no negative sleep/burst); recover() on all writers/callbacks; response bodies closed; generation fencing on LLM deltas and TTS chunks; vad/smartturn children Kill+Wait, bounded stderr, deadlines on handshakes; RTVI message names vs client-js; retention job cadence and cutoff; UI↔API field names/enums/pagination; npm deps; `offline/` fully wired (provider "offline" → xai processors at configured endpoints); `config/*.yaml` keys all exist (strict loader).

---

## G. Pipecat-parity reclassification of the "dead" list (revision 2)

Method: for each unreferenced item, found the Pipecat counterpart in `pipecat-impl/src/pipecat/` and the matching row in `docs/pipecat-parity-matrix.md`. Four buckets:

- **G1 Yet to implement** — Pipecat has it, the Go hook exists, nothing connects it. Keep; wire it.
- **G2 Superseded** — Pipecat has it, Go solved it another way, the remnant is dead. Delete the remnant.
- **G3 Parked** — kept only for a documented deliverable. Decide, then either relocate or retire.
- **G4 Dead** — no Pipecat counterpart (or matrix says "not planned"). Delete.

### G1. Yet to implement
| Item | Pipecat | Gap in Go |
|------|---------|-----------|
| `ToolCallFrame` / `ToolResultFrame` | `FunctionCallInProgressFrame`/`FunctionCallResultFrame` (used by 10 files) → RTVI `llm-function-call-started/-in-progress/-stopped` (`rtvi/models.py:274-334`), function-call observer, latency observer, turn-completion tool-first leniency | `transport/rtvi` emits **no** function-call messages. `core/tools/loop.go` executes via a `ToolExecutor`; nothing posts the frames. Function-call *mute* is fine (wired through `ToolActivity.Wrap`, `server/registry.go:573`), so the missing piece is the client-visible tool events. Matrix known-gap: "No provider emits `ToolCallFrame`". |
| `tool_calls` table / `AddToolCall` (A3) | function-call observer records every call | Never written. Same root cause as the row above: one event source fixes both. |
| Timeline events `EventSTTRotation`, `EventProviderRetry`, `EventCircuitState`, `EventTTSAudio`, `EventLLMStart`, `EventContextEviction` | service-metrics / startup observers | Declared in `server/timeline.go:21-32`, never recorded. Retry and circuit-state are the operationally valuable ones. |
| Provider voice settings: ElevenLabs `Stability/SimilarityBoost/Style/Speed`, Cartesia `MinVolume` | `voice_setting_keys` in `services/elevenlabs/tts.py:101`, runtime-updatable | Library fields exist; flow config exposes provider/model/voice/language only (from grep of `server/flow`; confirm before wiring). |
| Mid-call context/settings updates | `LLMMessagesUpdateFrame`, `LLMSetToolsFrame`, `LLMSetToolChoiceFrame`, `LLMUpdateSettingsFrame` | Matrix "Remaining work" #4: replace-history and tools "not built". `ConversationContext.SetSystemInstruction*` and the system-role branches are leftovers of this. Flow step-swap covers prompt changes at stage boundaries only. |

### G2. Superseded (delete the remnant)
- `StartFrame`, `CancelFrame`: Pipecat lifecycle frames; Go uses ctx cancellation + `Session.Close`. Only the Whisker harness posts them. Inert handler branches in the three failovers, `assistant_context`, `ttsunit`, `google/llm`, `google/tts`, `cartesia/stt`, `google/xai stt`.
- `TTSAudioFrame`: Pipecat's `TTSAudioRawFrame` is special-cased by the transport (`base_output.py:854`); Go's `ttsunit` emits `OutputAudioFrame` directly. Handlers and the `tts.audio_chunk` trace name are dead.
- `OutputDTMFFrame` + `OutputProcessor.ProcessFrame` branch: Pipecat pushes DTMF as a frame; Go's `send_dtmf` calls `out.send` directly (`core/audio/dtmf/tool.go:44`). **Choose one path.** Frame path is the Pipecat-shaped one and would let observers/redaction see output digits; direct path is what ships.
- `AnalyzerStop`/`TurnAnalyzer` impl: `smartturn.Stop` (`smartturn/analyzer.go:271`) re-implements the same strategy; matrix already says "wire or delete".
- `AddToolCall`, `AddToolResult`: replaced by `ClaimToolCall` / `AddToolResultStatus`.

### G3. Parked
- **Whisker** (`tools/compat/whisker`, `tools/eval/eval.go` runner): matrix row "Whisker — intentionally different; semantic adapter and 11 scenarios kept; wire compatibility not pursued until a version is pinned" (P6-2 done). It is the *only* reason `CancelFrame`/`StartFrame` posting, the seven extra `DebugEvent` fields (`core/debug.go:13-19`) and `Session.DebugDropped` exist. That conflicts with AGENTS.md's rule that everything in `core/` is consumed by `server/`. **Decision needed:** (a) retire Whisker and delete those hooks, or (b) keep it and move its needs behind a build tag / out of `core/`.

### G4. Dead (no Pipecat counterpart)
`SystemMessageFrame` (Pipecat has no such frame); `TurnEventNoSpeechTimeout`/`InitialSilenceTimeout` (nothing in Pipecat's `turns/`; `idle.user_timeout` covers it); `Manager.Interrupt`, `retiredOrder`; `RedactToolContent` and tool-argument trace redaction (Pipecat's tracing does not capture tool args); trace-DB adapter machinery and retention content-purge (Pipecat is OTel-only; adapter only sees `route.*`/`call.end`/`mcp.*` spans); `ValidateToolPairing` (matrix: tool ordering "intentionally different" — either delete, or turn into an assertion in the provider request builders so orphaned tool results fail locally rather than as a provider 400).
**Judgment call:** `Semantic` evidence (`Observation.Semantic`, `DetectionInput.Semantic`). It is the natural bridge for Pipecat's eager end-of-turn speculation gate (matrix: "Eager stop has no speculation gate"), but that gate is not in the remaining-work list. Keep only if the gate is planned.

### Effect on the numbers
Of the ~600 "dead" core lines, roughly 40% are G1–G3. The seven never-constructed frames split: 2 → G1 (`ToolCallFrame`, `ToolResultFrame`), 4 → G2 (`StartFrame`, `CancelFrame`, `TTSAudioFrame`, `OutputDTMFFrame`), 1 → G4 (`SystemMessageFrame`).
New top item: **emit tool-call events from the tool loop → RTVI function-call messages → fill `tool_calls`**. One change closes a client-visible Pipecat gap, makes three dead paths live, and fixes A3.

---

## Suggested order (superseded by the remediation plan: `docs/superpowers/plans/2026-10-15-review-remediation.md`)
1. **A1 + D1 together**: build the shared STT runner, move the dial off-loop, add the invalidation-on-drop (A8). One change fixes the worst bug and deletes ~1,000 lines.
2. **A2 + D7**: single publish path through `validateSelections`.
3. **A3**: decide — wire `tool_calls` from the tool loop, or delete the table/DTO/purge. Then A4 (redact `error.category`), and delete the unreachable adapter/retention code (C server).
4. **A5, A6, A7, A9** — small, isolated.
5. **C core** dead-frame sweep (7 frame types + branches) and the rest of the core dead list; fix AGENTS.md:51.
6. **D2** (unary TTS onto ttsunit), **D5/D6** into core.
7. Repo hygiene: tools/eval dead half + whisker, sbom.json, baseline/, plot.py, relocate docs/superpowers.

`net: roughly -8,000 lines of Go/TS deletable or foldable (excluding docs), 0 deps.`
