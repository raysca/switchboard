# Phase 9: Close-out (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. One task = one reviewable deliverable (one or a few conventional commits). Order: 9.0 → 9.1 → … → 9.10, sequential (9.1 and 9.2 both touch `go.mod`/`Makefile`; 9.7 measures the tree 9.1–9.6 produce; 9.8/9.9 describe it).

**Goal:** Close the review remediation: align `AGENTS.md` with what the remediation decided (D-0.8, parked/planned code, the gate), make the dead-code and doc-link checks permanent repo tools, add goroutine-leak assertions where goroutines are owned, close the two open flakes at their root cause, fix the small backlog items that are cheap and correct, measure the whole remediation against its baseline, write the release notes, and run one adversarial review over the entire remediation diff before the final gate.

**Architecture:** No new runtime component. Additions are test-only (goleak checks, regression tests), two tracked scripts (`scripts/deadcode.sh`, `scripts/check-links.mjs`), one Make target (`deadcode`), one advisory CI job, one doc (`docs/superpowers/remediation/release-notes.md`). Product changes are four small, test-first fixes (smartturn metric, Say race, run-loop RTVI write bound, resampler allocations) and one log line (VAD worker stderr).

**Tech stack:** Go 1.27.1 workspace (after Phase 8: `.`, `core`, `offline`, `providers/{cartesia,deepgram,echo,elevenlabs,google,nvidia,openai,xai}`, `server`), React/TS `ui/`, Playwright `e2e/`, GitHub Actions.

**Spec:** master plan `docs/superpowers/remediation/plans/2026-10-15-review-remediation.md` Phase 9 (Tasks 9.1–9.3, D-0.8) — written while it was still at `docs/superpowers/plans/`. Carried items: ledger `.superpowers/sdd/2026-10-15-review-remediation/progress.md` and `p5/p6/p7/p7b/pr-notes.md` "Carried backlog", Phase 8 plan R8/R9 and its Task 8.7 "Carried" list. Mapping: master 9.1 → 9.1; master 9.2 → 9.2 (leaks), 9.6 (race stress), 9.9 (review); master 9.3 → 9.7 (measure) + 9.10 (gate). 9.3–9.5 and 9.8 are the carried backlog and the release notes.

**Precondition:** Phase 8 is merged to `main`. This plan was written on `feat/phase-9-closeout` = `feat/phase-8-hygiene` 72cd2f1 (= main 0c0778b + the Phase 8 plan only). Every fact below that Phase 8 changes is marked **(P8)** and is re-verified in Task 9.0. Phase 8 is now merged (0b84d7d) and this plan sits at `docs/superpowers/remediation/plans/`; the design review (executing 9.0–9.5 in a scratch clone) confirmed the (P8) facts and its findings are folded in ("design review" below).

**Leave alone:** the untracked `IDEA.md` at the repo root (never touch, stage or delete; stage explicit paths, never `git add -A`).

---

## Decisions for the user (ANSWERED 2026-10-21: U1 goleak, U2 option B, U3 yes, U4 option A; final)

| # | Question | Recommendation |
|---|---|---|
| U1 | **Goroutine-leak checks: `go.uber.org/goleak` or a hand-rolled `runtime.NumGoroutine` helper?** goleak v1.3.0 is already in the workspace build graph (`server/go.sum:133-134`, pulled by the OTel SDK tests) and in the module cache. Adding it as a test-only `require` to the root and `core` modules is a hand edit of 1 line + 2 `go.sum` lines each; `go list -m all` stays byte-identical (Verified fact 4). A NumGoroutine delta cannot say *which* goroutine leaked and is noisy with `-race`. | **goleak** (AGENTS.md §1: established library over hand-rolled; zero new modules in the build list). |
| U2 | **D-0.8 scope.** D-0.8 adopted "document the exception for trace-parent contexts only; fix the rest when touched". The tree has 14 struct fields of type `context.Context` (plus one `map[core.TurnID]context.Context`, `server/otelexport/otelexport.go:191`, per-turn span parents): 8 (+ the map) are trace parentage, 6 are an object's own lifetime context paired with its `cancel` (session, ws connection, google/xai STT streams, OTel export queue, async tool state) (Verified fact 14). Option A: document trace parentage only; the 6 stay as listed violations. Option B: also document a narrow second exception ("an owner's lifetime context, created with its cancel in the constructor and cancelled by the owner's Close"). Option C: refactor the 6 to pass `ctx` (touches session, transport and two providers' stream code; no behaviour gain). | **B.** Both patterns are the standard Go shape for long-lived objects with `Close`; the rule's intent (no request context smuggled across calls) is kept. Every one of the 14 sites (and the `turnCtxs` map) gets a one-line tag comment so a reviewer can grep them. |
| U3 | **Carried backlog in scope.** Fix in Phase 9 (each test-first, ≤ ~40 lines): (a) smartturn counts a superseded or closed job as a `timeout` fallback (product metric bug, the likely root cause of the open flake; Verified fact 5), (b) delete `Connection.writeMu` (coder/websocket already serialises writes with a ctx-aware lock) and bound run-loop RTVI writes below the watchdog so a stuck client ends as `slow_client` (1008 when the close frame can still be sent, else 1006) instead of `ErrProcessorWatchdog` (behaviour change), (c) `Say` vs model-turn race, (d) resampler general path 7 → 1 allocs/frame, (e) log the VAD worker's stderr tail on a crash. Everything else is carried or kept (Rulings R6/R7). | **Yes, all five.** (a) and (b) are correctness; (c)–(e) are one guard, one loop rewrite and one log line. |
| U4 | **Where the release notes and the before/after results live.** Option A: one tracked doc `docs/superpowers/remediation/release-notes.md` (breaking changes, behaviour changes, measured results, carried items), linked from `docs/superpowers/README.md`; the PR description is its summary. Option B: a root `CHANGELOG.md` (the repo has none; would start a convention for one entry). | **A.** No new convention; the notes sit next to the plans they summarise. |

## Rulings (mine; recorded in the ledger; the reviewer may challenge them)

- **R1 Baseline commit = `1993240`** ("docs: deep review (spec) and remediation master plan", ledger `progress.md:3` "base 1993240"). Every before/after in 9.7 is `1993240` vs the branch head. The two non-phase merges in that window (9f37b84 offline drain, 3a243fd superseded reply turn row) came out of the remediation gates and are counted.
- **R2 The offline long-speech flake is closed, not A/B'd.** Root cause found and fixed in Phase 3's window (4716792: the xAI STT stream cancelled a half-closed stream 2.000 s after `audio.done`; Whisper on CPU needs up to 5.5 s; offline now waits 6 s). Since then: 104 ledger logs with `--- PASS: TestOfflineStackLongCallerSpeechIsOneTurn`, 0 with `--- FAIL` (Verified fact 7). The Phase 8 gate's "known intermittent" wording and its carried "A/B in 9.3" are stale; 9.7 records the evidence instead of 30+ runs. The 5× rerun rule stays in the gate as a safety net.
- **R3 The master plan's "≥ −5,000 lines net" is reported, not chased.** At main, non-test Go is net +1 line over the baseline (Verified fact 2); Phase 8 removes ≈ −33,000, of which −30,184 vendored headers and −1,879 SBOM. 9.7 reports the delta by category (product Go, test Go, UI, tests, docs records, vendored/generated); no code is deleted to hit a number. After Phase 8 (design review): 553 files, +37,819/−41,063; non-test Go net −286, test Go +12,370, vendored −30,186, generated −1,879; net without vendored/generated −3,244.
- **R4 `deadcode` is a tracked script with a pinned tool and a reasoned allowlist.** `golang.org/x/tools/cmd/deadcode@v0.52.0` (in the module cache; `@latest` drifts and needs the network), wide roots (`./cmd/... ./examples/... ./tools/... ./offline/cmd/...`, so tool-only exports such as `netpool.Requests` and `core.SplitNext` count as live), `-test=false`, `-tags xaispeech`. The allowlist holds `grep -E` patterns, each with a reason; after Task 9.1 it has exactly one: `core/coretest/` (test doubles shared across packages). `make deadcode` is not part of `make verify` (the master plan wants it advisory in CI); `AGENTS.md` lists it in the gate.
- **R5 The Phase 8 link checker becomes `scripts/check-links.mjs`, without `--moves`; the Phase 8 tripwire is not tracked.** `--moves` checked a one-off move; after Phase 8 an old path inside backticks is already a missing path (`--paths`). The tripwire's input (`deleted-paths.txt`) is history; a deleted Go symbol that reappears is caught by review and by `deadcode`, a deleted file named in a doc by `--paths`. `make verify` runs `node scripts/check-links.mjs --paths`.
- **R6 Kept with reason (no task):** ping-failure close counted `disconnect_client` (intentional: `transport/ws/transport.go:696-699` "a client that stopped answering pings stays client"; the failures have their own counter `ws_ping_failures`); demo install base context (already done: `server/demo_api.go:50-81` runs the flight on its own bounded context that `Shutdown` cancels and waits for); `SessionController` optional-interface probes (8 sites of `interface{ EmitDebug(...) }` / `TailIndex()`: the standard optional-interface idiom; folding them into the exported interface is a library break for test fakes, no runtime gain); VAD worker crash noticed only on its next batch (the batch is handed to a live worker by `handOff`; a `cmd.Wait` watcher goroutine per worker buys one batch's latency); "refused goodbye is dropped" (documented: `docs/api/v1.md:67`, `docs/privacy.md:177-178`); test gaps "no injected write error for `OnWriteComplete` err != nil" and "denied microphone is Vitest-only" (no ≤ 40-line deterministic test exists; recorded in the release notes as known gaps).
- **R7 Carried (not Phase 9):** R8 LLM sampling parameters (own phase, Phase 8 U2); macOS ORT sha pin (needs a verified download: take the hash from the `fetch-ort: WARNING no pinned sha256` line of the next CI `make ort` run); Gemini consecutive user turns after a refused LLM turn and the Riva unbound-interim seed false positive (both need live providers; low risk recorded in the ledger, Phase 4 / Phase 6); `turn_count` counting idle and greeting lines (documented); audit R4/R5c/R7 (Phase 7b carried).
- **R8 One race-stress run, not one per task.** `go test -race -count=5 -shuffle=on` over every module runs once (Task 9.6) with a 45-minute budget; per-task gates keep `make verify` (count=1).
- **R9 Flake rule.** A flake is closed only by (1) a root cause shown by a deterministic test that fails before the fix and passes after, plus (2) a stress run of the original test with zero failures. If no root cause is found inside the task's time box, the task adds diagnostics to the failing assertion (actual vs expected values, the `-test.shuffle` seed) and the flake stays OPEN in the release notes. Never `t.Skip`, never widen a timeout without a cause.
- **R10 Final review is split by area** (four independent reviewers on `git diff 1993240..HEAD`, plus a mechanical inventory the controller produces first), because the whole diff is ~550 files.

---

## Verified facts (2026-10-21, worktree at 72cd2f1 = main 0c0778b + Phase 8 plan; commands actually run)

`S` = the planner's scratchpad; `$S/p9-exp` was a `git clone --local` used for every mutating experiment (goleak trial, smartturn probe).

1. **Baseline.** `git log -1 1993240` → `docs: deep review (spec) and remediation master plan` (2026-10-15). `git rev-list --count 1993240..HEAD` → 282. Merges in the window: 946f646 (P1), 2e55347 (1.7), 03eb695 (P2), 318d1cd (P3), 9f37b84 (offline 6 s drain), 3a243fd (superseded reply keeps its turn row), 4cc6058 (P4), 52db4be (P5), dbcb21f (P6), dda9f3f (P7), 0c0778b (P7b). Phases 1–4 have no `pr-notes.md`; their merge messages (`git show -s --format=%B <merge>`) and `task-*-report.md` are the release-note sources.
2. **Line delta at main** (`git diff --numstat 1993240 HEAD`, categorised by path): non-test Go +5,632/−5,631 (net **+1**); test Go +13,901/−1,463 (+12,438); UI non-test +1,126/−302 (+824); UI tests + e2e +2,009/−187 (+1,822); `docs/superpowers` +13,672/−1; other docs +115/−75; other +649/−152. Total `git diff --shortstat`: 435 files, +37,104/−7,811. **(P8)** adds ≈ −33,030 (Phase 8 plan, "Estimated line delta").
3. **deadcode** (`go run golang.org/x/tools/cmd/deadcode@v0.52.0 -tags xaispeech -test=false …`, 3.1 s warm; `GOPROXY=off` fails on the deprecation lookup, so run with the default proxy; the module is cached): master roots (`./cmd/... ./examples/...`) **7** lines (`core/netpool.Requests`, `core.SplitNext`, `server.LoadConfig`, `mcptest.BlackHole.Accepted`, `retention.WithClock`, `server.Server.PublicAddr`, `ws.DefaultServerConfig`); wide roots (+ `./tools/... ./offline/cmd/...`) **28**: 16 `core/coretest` doubles + 12. **(P8)** Task 8.1 deletes 7 of the 12 → wide 21: 16 coretest + `server/config_load.go:14 LoadConfig`, `server/mcp/mcptest/mcptest.go:310 BlackHole.Accepted`, `server/retention/job.go:33 WithClock`, `server/server.go:1035 Server.PublicAddr`, `transport/ws/transport.go:63 DefaultServerConfig`. Callers: `LoadConfig` only from `package server` tests (`cloudrun_config_test.go:12`, `config_defaults_test.go:18`, `deploy_contract_test.go`); `PublicAddr` only `server/lifecycle_test.go:224` (`package server`); `WithClock` only `server/retention/retention_test.go:230,339,414,515` (`package retention_test`); `Accepted` only `server/mcp/mcptest/mcptest_test.go:134-135` (`package mcptest_test`); `DefaultServerConfig` ("returns a test/dev default: it allows any origin") only `transport/ws/*_test.go` (`package ws_test`, e.g. `client_safety_test.go:23`) and `tools/compat/client_compat_test.go:82`. The `scripts/deadcode.sh` of Task 9.1b, run as written in `$S/p9-exp` with only `^core/coretest/` allowed, exits 1 listing exactly the 12 non-coretest symbols; with everything allowed it prints `deadcode: OK (28 allowed)`, rc 0. Docs naming them: `docs/pipecat-parity-matrix.md:152,158` (`LoadConfig`; **(P8)** 8.6 deletes the line-152 bullet).
4. **goleak.** `server/go.sum:133-134` has `go.uber.org/goleak v1.3.0`; `cd server && go mod why -m go.uber.org/goleak` → `govoice/server/otelexport → go.opentelemetry.io/otel/sdk/trace.test → go.uber.org/goleak`; root `go.sum` and `core` have none; `$GOMODCACHE/go.uber.org/goleak@v1.3.0` exists. In `$S/p9-exp`: `go get go.uber.org/goleak@v1.3.0` at the root **rewrites 35 lines of `go.mod`** (bumps jwt, otel, x/net, protobuf… to the workspace build list) → forbidden. Hand edit instead (one `require` line after `github.com/yalue/onnxruntime_go v1.36.0` + the two `go.sum` lines copied from `server/go.sum`): `go list -m all` before/after **identical**. Trial results: `core/sttunit`, `core/ttsunit` with `goleak.VerifyTestMain` under `-race`: **ok** (5.3 s, 1.8 s); `vad` with `goleak.Find()` after `m.Run()` in its existing `TestMain` (`vad/export_test.go:11`): **ok** (needs the ORT dylib); `transport/ws` with `VerifyTestMain`: **FAIL, 4 leaked connections**, 28 goroutines = 4 × {`Server.ServeHTTP`, `Connection.readLoop` (blocked in `websocket.(*Conn).Read`), `Connection.pingLoop`, `PacedWriter.Run`, `PacedWriter.Run.func1`, `core.(*Session).runLoop`, `core.(*Session).watchdog`} (log `$S/ws-leak.log`). Server: `server/turn_stop_test.go:22` is the package `TestMain` (`m.Run(); vad.ShutdownORT(); os.Exit`); 71 `newFlowRigE2E(` uses; 1 `t.Parallel()` in `server/*_test.go`.
5. **smartturn flake root cause (reproduced deterministically).** Recorded failure: ledger `flake/p7b-7b.11-verify.log`: `--- FAIL: TestAnalyzerTimeoutFallsBackToSpeechTimeout … analyzer_test.go:201: timeout fallback not counted` (`Fallbacks(ReasonTimeout) != before+1`). `smartturn/analyzer.go`: `startLocked` (≈ :171-176) cancels the previous job without marking it done; `Close` (≈ :147-152) the same; the job goroutine's `finish` (≈ :199-214) maps `context.Canceled` to `ReasonTimeout` and calls `failLocked` → `countFallback` on the **process-wide** `stats.fallbacks`. Probe in `$S/p9-exp/smartturn/probe_supersede_test.go` (job 1 started, superseded by a new silence key, job 2 closed; never-released fake backend): `superseded/closed jobs counted 2 timeout fallbacks, want 0` → FAIL. So (i) production `voice_smartturn_fallbacks_total{reason="timeout"}` counts every superseded silence window and every close, and (ii) any earlier test that supersedes or closes a job can bump the counter asynchronously inside `TestAnalyzerTimeoutFallsBackToSpeechTimeout`'s `before`…check window.
6. **core flake.** Recorded: ledger `progress.md:220` `TestFlushStaleInterruptionCannotCancelCurrentTurn lifecycle_test.go:176 'got 612 want 722'`. `core/frame_values_test.go:15-16`: 612 = `FrameUserMessage`, 722 = `FrameFlush`: the user message reached `OnOutput` before the flush. The test posts `FlushFrame` (control lane, `isControlFrame`, `core/session.go:1228`) then `UserMessageFrame` (data lane) back to back and `awaitType`s them in that order (`core/lifecycle_test.go`, `awaitType` at :555). `Session.runLoop` (`core/session.go:681-710`): outer `select` takes control if ready, else `default:` → inner `select` over control **and** data; if both become ready between the outer `default` and the inner `select`, Go picks at random. Generation correctness does not depend on that order (the flush commits its generation at admission, `session.go:372-385`). `TestFlushInlineStaleInterruptionCannotCancelCurrentTurn` has the same post-post-await shape.
7. **Offline long speech.** Fix 4716792 `fix(offline): the STT stream waits 6 s for a final after audio.done …` merged 9f37b84 (2026-10-17 20:03). Ledger: `find flake p* -newer flake/offline-ab/summary.log -type f -exec grep -l -- '--- PASS: TestOfflineStackLongCallerSpeechIsOneTurn' {} +` → 104 files; same with `--- FAIL:` → 0. The test lives in `server/offline_stack_long_e2e_test.go:131`.
8. **writeMu.** `transport/ws/transport.go:102 writeMu sync.Mutex`, taken by the pacer's write function (:461-463) and by `WriteWireMessage` (:1164-1165, after building a `WriteTimeout` context, so the lock wait itself is unbounded). 21 `WriteWireMessage` call sites; those in `Connection.ProcessFrame` (:943 onward: :980-1045 RTVI messages, :1079 `bot-interrupted` on the flush barrier, :1100 error frame) run on the session run loop. Watchdog `MaxFrameProcessing` = 2 s (`core/budget.go:26`); `PacerConfig.WriteTimeout` = 2 s (`transport/ws/pacing.go:36`). A pacer write stuck up to 2 s + the run-loop write's own up to 2 s > watchdog → `ErrProcessorWatchdog`. coder/websocket v1.8.15 `conn.go:30`: "All methods may be called concurrently except for Reader and Read"; `Conn.write` takes `msgWriter.mu` via `mu.lock(ctx)` (`write.go:103-108`, `conn.go:286-300`: selects on `ctx.Done()`). The pacer's slow-client close is at `transport.go:592-597` (`s.observe("slow_client",1)`, `closeBounded(StatusPolicyViolation, "slow client terminated", 500ms)`, `cancel`).
9. **Say race.** `core/assistant_context.go:164-191` `Say` checks `p.modelActive.Load()` off the run loop (:173) then `Reenter`s `UserMessageFrame{Turn: "say-N"}`; `ProcessFrame` `FrameUserMessage` (:197-222) adopts any user message unconditionally, so a model turn admitted between the check and the say frame's arrival is reset by the say turn (its text then fails `isStaleTurn`, :429-437, and is dropped). Ledger `p6/pr-notes.md` "Review notes M3".
10. **Resampler.** `cd core && go test -run '^$' -bench 'Resampler_20msChunk|Downsample48To16' -benchmem -benchtime 2000x ./transport/resample/` → `BenchmarkResampler_20msChunk` (16 k→24 k, 20 ms) **1435 ns/op, 3648 B/op, 7 allocs/op**; `BenchmarkDownsample48To16` 1284 ns/op, 640 B/op, 1 alloc/op. General path (`core/transport/resample/resampler.go:79-135`): `samplesIn := make([]int16, n)`, `samplesOut` grown by `append`, then `out := make([]byte, …)`. Callers that hit it per frame: ws input at a client rate other than 16/48 k (`transport/ws/transport.go:831`), output `providerRate → FixedPlayerRate` (:953), `core/record/recorder.go:315`, `providers/nvidia/tts.go:135`. Output slices are retained by callers (Phase 1 ruling), so `out` stays a fresh allocation.
11. **VAD worker stderr.** `vad/worker_executor.go`: `boundedBuffer` (:73) with `String()`; `wp.stderr` assigned to `cmd.Stderr` (:263); `String()` is never called outside tests (`git grep` hits only `vad/env_test.go:81,85` on a different buffer). Package `vad` imports no logger. `smartturn/worker.go:304` logs respawn failures with `slog.Warn` (Phase 1.6 pattern).
12. **Ping failure** → `setCloseCause("peer_unresponsive")` (:659) and the read loop keeps `cause = "client"` on purpose (:696-699 comment); `server/server.go:1215` maps `disconnect_client`; `ping_failure` has its own counter (`server.go:1207`). **Demo flight**: `server/demo_api.go:50-81` (own `context.WithTimeout(context.Background(), demoInstallTimeout)`, `stopFlight` cancelled by `Shutdown`, `demoWG`).
13. **SessionController probes** (`git grep 'interface{ EmitDebug\|interface{ TailIndex'` in core, non-test): 8 sites (`assistant_context.go:134,612`, `audio/dtmf/processor.go:22`, `failover.go:42`, `idle.go:524,573`, `turn/mute.go:245`, `turn_completion.go:223`).
14. **`context.Context` struct fields** (`git grep -P '^\t+\w+(, \w+)*\s+context\.Context(\s|$|//)' -- '*.go' ':!pipecat-impl' ':!*_test.go'`) → 14. Trace parentage (8): `core/llm_failover.go:94`, `core/stt_failover.go:112`, `core/tts_failover.go:97`, `core/session.go:129` (`traceCtx`), `:130` (`traceRootCtx`), `core/trace_async.go:22`, `vad/processor.go:86` (`traceCtx`), `server/otelexport/otelexport.go:189` (`rootCtx`); also the map `:191 turnCtxs map[core.TurnID]context.Context` (not matched by the grep; trace parentage). Owner lifetime with its `cancel` (6): `core/session.go:77`, `transport/ws/transport.go:111`, `providers/google/stt.go:243`, `providers/xai/stt.go:204`, `server/otelexport/queue.go:23`, `core/tools/parallel.go:22`.
15. **Makefile / CI.** `verify` = `test` + gofmt + `validate_manifest.js` + `verify_fixtures.js` + `probe_playback_clear.js` (**(P8)** + fixture regeneration). `MODULES` (:48) **(P8)** loses `offline/contract`. `.github/workflows/ci.yml` jobs `linux`, `graph-e2e`, `macos`, `postgres`, `image-smoke`; `server/deploy_contract_test.go:102 TestCIWorkflowCoversTheSafetyNet` requires `make verify` in linux/macos, `make test-postgres`, `deploy/smoke.sh`, all blocking, and `perf-gate` `continue-on-error`. No `deadcode` target or job exists.
16. **Checker (P8).** `.superpowers/sdd/2026-10-15-review-remediation/p8/check-links.mjs` exists in the ledger (git-ignored) with the code in Phase 8 Task 8.0 Step 1; Phase 8 R9 leaves wiring it into `make verify` to Phase 9.
17. **Measurement tooling.** Installed `staticcheck` 2024.1.1 fails on Go 1.27 export data (`unsupported version: 4`); `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2` (cached) runs clean on `core/transport/resample`. `benchstat`: cached `golang.org/x/perf@v0.0.0-20261009192801-be2c69fb417e`. `tools/loadtest` and `tools/eval/cmd/govoice-bench` accept `-json-out` at both `1993240` and HEAD; loadtest JSON has `results.voice_to_voice_client_ms.{p50,p95,p99}_ms`, `results.server_stage_ms.<stage>.*`, `results.resources.{cpu_s_per_session_min,mb_per_session,goroutines_after_drain}`; bench JSON has `Overhead.{P50,P95,P99}`. Benchmarks at base: 7 (`Detector20msFrame`, `Denoise20ms`, `EmitFrameDebugAudio`, `STTFailoverAudioFrame`, `Resampler_20msChunk`, `Features`, `UnderlayIdleWriters`); at HEAD: 11 (+ `UnitWrite`, google/xai `UnaryUnit`, `Downsample48To16`). `git diff --stat 1993240 HEAD -- ui/package*.json e2e/package*.json tools/compat/package*.json` → empty (a baseline worktree can symlink `node_modules`). `cmd/server` exists at both.
18. **Inventories for the final review** (non-test unless noted): `go` statements in `core/` (excl. coretest) 14; `UnixNano()` 2; `time.Now().Sub(` 0; Playwright `waitForTimeout` 5 (`e2e/lib/mcp-helpers.ts:46` `pace`, specs 26, 34 ×2, oneport 10); `time.Sleep` in `server/*e2e*_test.go` 48; `t.Skip` in Go tests 56.

---

## Global Constraints

Inherited from `AGENTS.md`, the master plan and user memory.

- `core/` never imports `server/`; nothing new in `core/` that the server does not reach (test files excepted). Every queue bounded; every goroutine has a termination path (in `core/`, `session.Go`); no new `context.Context` struct field outside the documented exceptions; no lock across I/O; monotonic time for durations; hot audio path does not allocate more than before.
- `go build`/`go vet` with and without `-tags xaispeech` in every module; `gofmt -l` empty; `go test -race` green in every module.
- `go.mod` edits by hand only (Verified fact 4); `go list -m all` identical before/after in every module except for the added `go.uber.org/goleak` line where it was not yet listed.
- Conventional commits; trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Stage explicit paths only.
- Provider secrets: environment only. Never run paid/live targets (`e2e`, `e2e-xai`, `live-check`, `agent-matrix`, `live-critical`, `provider-matrix`). Never `offline-up`/`offline-models` or any model download. Never kill processes by pattern (kill only PIDs your own script started). Private temp paths only (`$P/tmp`). Leave a developer's own `make dev`/`bin/server` alone.
- Do not push or merge.

### Gate block (after EVERY task; substitute the task id for `9.X`)

```bash
P=.superpowers/sdd/2026-10-15-review-remediation/p9; L=$P/logs; mkdir -p "$L" $P/tmp; set -o pipefail
gofmt -l $(git ls-files '*.go' | grep -v '^pipecat-impl/') | tee "$L/9.X-gofmt.log"; test ! -s "$L/9.X-gofmt.log"
node scripts/check-links.mjs --paths 2>&1 | tee "$L/9.X-links.log"       # before 9.1d: node .superpowers/sdd/2026-10-15-review-remediation/p8/check-links.mjs --paths
make deadcode 2>&1 | tee "$L/9.X-deadcode.log"                           # from 9.1b on
make verify 2>&1 | tee "$L/9.X-verify.log"
make e2e-echo 2>&1 | tee "$L/9.X-e2e-echo.log"
make provider-matrix-dry 2>&1 | tee "$L/9.X-provider-matrix-dry.log"
# when transport/, core/, vad/, smartturn/ or server/ changed (9.2-9.5):
make loadtest-smoke 2>&1 | tee "$L/9.X-loadtest-smoke.log"
# when tools/ changed (9.1a):
make live-check-selftest 2>&1 | tee "$L/9.X-live-check-selftest.log"
# when ui/ or docs links changed: (9.1, 9.8)
(cd ui && npx tsc --noEmit -p . && npx vitest run && npx --yes knip@5 --no-progress) 2>&1 | tee "$L/9.X-ui.log"
# offline stack: never start it. Both must answer, else STOP and report BLOCKED (user requirement).
if curl -fsS http://127.0.0.1:11434/api/tags >/dev/null && curl -fsS http://127.0.0.1:8099/healthz >/dev/null; then
  make offline-test 2>&1 | tee "$L/9.X-offline-test.log"   # its own rc: a test failure is a FAIL, never "BLOCKED"
else echo "BLOCKED: offline stack down (Ollama :11434 or adapter :8099)"; fi
# 9.1 (retention.WithClock moves; test-postgres runs ./storage/ ./retention/), 9.2, 9.5 and 9.10 only:
make test-postgres 2>&1 | tee "$L/9.X-test-postgres.log"
```

Expected: gofmt log empty; `check-links: 0 problem(s)`; `deadcode: OK (16 allowed)`; `=== All verify checks passed ===` (the recorded `PLAYBACK_CLEAR_REQUIREMENT_FAILED … relax-unmodified-client-playback-clear` line is expected); `make e2e-echo` exits 0 with every Playwright test passed; `provider-matrix-dry`, `loadtest-smoke`, `live-check-selftest`, `test-postgres` exit 0; ui: 0 tsc errors, all vitest pass, knip prints nothing; `make offline-test` prints `ok` for both packages. On failure: name the failing test (`grep -nE -- '--- FAIL|^FAIL|✘' <log>`), keep the log, never overwrite it with a rerun. If `TestOfflineStackLongCallerSpeechIsOneTurn` fails: rerun it alone 5× (`cd server && OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -count=1 -v -tags "xaispeech offlinestack" -run TestOfflineStackLongCallerSpeechIsOneTurn .`), report all results, and reopen R2. Run gates on an otherwise idle machine.

Per-commit compile check (before every commit):

```bash
for t in "" xaispeech; do for m in $(sed -n 's/^MODULES := //p' Makefile); do (cd $m && go build -tags "$t" ./... && go vet -tags "$t" ./...) || echo "FAIL $m tags=$t"; done; done
```

---

## Task 9.0 — Re-verify after Phase 8, index this plan (ledger + one README commit)

- [ ] Step 1: the branch already sits on main with Phase 8 merged (0b84d7d) and this plan is already at `docs/superpowers/remediation/plans/` (no move). Add one row for it to the table in `docs/superpowers/README.md` ("Phase 9: close-out"; today `grep -c phase-9 docs/superpowers/README.md` = 0). Commit `docs: Phase 9 plan in the remediation index`.
- [ ] Step 2: re-verify every **(P8)** fact and record in `$P/base-facts.txt`: `sed -n 's/^MODULES := //p' Makefile` (no `offline/contract`); `make -n offline-test` (first line `./offlinecontract/`); the wide deadcode run of fact 3 → 21 lines (save as `$P/deadcode-wide-before.txt`); `ls .superpowers/sdd/2026-10-15-review-remediation/p8/check-links.mjs`; `node …/p8/check-links.mjs --paths` → `0 problem(s)`; `grep -c 'LoadConfig' docs/pipecat-parity-matrix.md` → 0 (Phase 8 removed both mentions); the line numbers cited in facts 4–14 (re-grep each symbol; Phase 8 does not touch Go product code, so only comment lines may shift). Any mismatch: update the task text in this plan first (commit `docs: Phase 9 plan re-verified after Phase 8`).
- [ ] Step 3: `git rev-parse HEAD > $P/base`; `echo 1993240 > $P/remediation-base`.

## Task 9.1 — AGENTS.md alignment and permanent checks (D-0.8, master 9.1)

### 9.1a Deadcode pass: move test-only helpers out of production code (≈ −40 / +40 lines, net ≈ 0)

Per symbol (fact 3), the standard Go idioms: a helper used only by its own package's tests moves to a `_test.go` file; one used by an external `_test` package of the same directory moves to `export_test.go` (package-internal test file, visible to the `_test` package).

- [ ] `server/config_load.go:13-14` `LoadConfig` → `server/config_load_test.go` (new, `package server`, the function and its doc comment verbatim). Production comments must not name a test helper: `server/config_defaults.go:9,11` and `server/llm_failover.go:145` say `LoadConfig` → reword to `LoadConfigProfile` (`git grep -nw LoadConfig -- 'server/*.go' ':!*_test.go'` → 0 after the move). (The parity matrix no longer mentions it.)
- [ ] `server/server.go:1034-1035` `PublicAddr` → `server/lifecycle_test.go` as an unexported helper `func publicAddr(s *Server) string { return s.publicAddr }`; the one caller (`:224`) uses it. The `publicAddr` field stays (`Start` writes it; `git grep -n 'publicAddr' server/*.go` lists its readers in the report).
- [ ] `server/retention/job.go:30-38` `WithClock` → `server/retention/export_test.go` (`package retention`); `Option` stays (used by `NewJob`).
- [ ] `server/mcp/mcptest/mcptest.go:310-314` `BlackHole.Accepted` → `server/mcp/mcptest/export_test.go` (`package mcptest`). `accepted` field keeps its writer.
- [ ] `transport/ws/transport.go:60-82` `DefaultServerConfig` → `transport/ws/export_test.go` (`package ws`; create it; it must import `govoice/transport/token` for the `*token.Validator` parameter). `tools/compat/client_compat_test.go:82` builds its config inline: copy the literal from the moved function with every type and constant `ws.`-prefixed (`ws.ServerConfig`, `ws.SecurityConfig`, `ws.DefaultPacerConfig()`, `ws.FixedPlayerSampleRate`) (it is a test; the copy is the test's own setup). The design review compiled and passed both.
- [ ] Pins: `bash scripts/deadcode.sh` (written in 9.1b; run it after 9.1b) lists only `core/coretest/`; `go test ./server/ ./server/retention/ ./server/mcp/mcptest/ ./transport/ws/ ./tools/compat/` ok (`-tags xaispeech` for server).
- [ ] Commits: `refactor(server): test-only helpers live in test files`, `refactor(transport): the permissive dev ServerConfig is a test helper`.

### 9.1b `make deadcode` (tracked script, pinned tool, reasoned allowlist; R4)

- [ ] `scripts/deadcode.sh` (mode 755):

```bash
#!/usr/bin/env bash
# Fails when a function that no binary, example or tool reaches is not allowlisted.
# Tool: golang.org/x/tools/cmd/deadcode, pinned (bump deliberately). Run: make deadcode.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
out=$(go run golang.org/x/tools/cmd/deadcode@v0.52.0 -tags xaispeech -test=false \
  ./cmd/... ./examples/... ./tools/... ./offline/cmd/...)
# "path/file.go:12:6: unreachable func: Name" -> "path/file.go Name" (line moves are not new findings)
norm=$(printf '%s\n' "$out" | sed -nE 's/^([^:]+):[0-9]+:[0-9]+: unreachable func: (.+)$/\1 \2/p' | sort -u)
allow=$(sed -E -e '/^[[:space:]]*(#|$)/d' -e 's/[[:space:]]+#.*$//' scripts/deadcode-allow.txt)
new=$norm
if [ -n "$allow" ]; then new=$(printf '%s\n' "$norm" | grep -vE -f <(printf '%s\n' "$allow") || true); fi
new=$(printf '%s\n' "$new" | sed '/^$/d')
if [ -n "$new" ]; then
  echo "deadcode: unreachable and not in scripts/deadcode-allow.txt (delete it, move it to a _test.go file, or allowlist it with a reason):"
  printf '%s\n' "$new"; exit 1
fi
echo "deadcode: OK ($(printf '%s\n' "$norm" | sed '/^$/d' | wc -l | tr -d ' ') allowed)"
```

- [ ] `scripts/deadcode-allow.txt`:

```
# One grep -E pattern per line, matched against "path/file.go Symbol"; text after " #" is the reason.
^core/coretest/   # test doubles shared by tests in many packages (core, server, providers); no binary uses them
```

- [ ] `Makefile`: after `verify`, add

```make
deadcode: ## build|Unreachable functions not in scripts/deadcode-allow.txt (advisory in CI)
	@scripts/deadcode.sh
```
and `deadcode` to the `.PHONY` line of the build targets.
- [ ] `.github/workflows/ci.yml`: new job (advisory, like perf-gate):

```yaml
  deadcode:
    runs-on: ubuntu-latest
    continue-on-error: true   # advisory: a new unreachable function is a review question, not a red build
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: make deadcode
```
(the same action tags and `go-version-file` as the `linux` job, `.github/workflows/ci.yml:28-33`; if those change before this task, copy them verbatim).
- [ ] `server/deploy_contract_test.go` `TestCIWorkflowCoversTheSafetyNet`: add one assertion: job `deadcode` exists, runs `make deadcode`, `ContinueOnError == true`.
- [ ] Pins (negative, then restore): add `func unusedProbe() {}` to `core/session.go` → `make deadcode` exits 1 naming `core/session.go unusedProbe`; remove it → `deadcode: OK (16 allowed)`. `go test -run TestCIWorkflowCoversTheSafetyNet ./server/` ok.
- [ ] Commit `build: make deadcode with a pinned tool and a reasoned allowlist; advisory CI job`.

### 9.1c AGENTS.md (D-0.8 per U2, the parked rule, the gate)

Exact edits (line numbers at 72cd2f1; Phase 8 edits only lines 23, 47, 50, 51, re-grep the quoted text):

- [ ] §Development Principles 2, after the "Server Consumption Requirement" bullet (line 63), insert:

```markdown
- **Planned or parked library code:** a `core/` (or `transport/`, `vad/`, `smartturn/`) capability that no server call path reaches may stay only while its row in `docs/pipecat-parity-matrix.md` has the status `planned (hook exists)` or `parked` with the reason. Otherwise it is deleted. `make deadcode` lists every unreachable function; each one it keeps is named with a reason in `scripts/deadcode-allow.txt`, and a helper only tests use lives in a `_test.go` file (`export_test.go` for an external test package).
```

- [ ] §Resource Bounds 3, after "Zero Race Conditions" (line 70), insert:

```markdown
- **No goroutine leaks:** the listed packages that own goroutines (`core/sttunit`, `core/ttsunit`, `transport/ws`, `vad`, `server`) end their tests with a `go.uber.org/goleak` check; a new package that starts goroutines adds one. (Not yet covered: `core` itself, whose tests leave one deliberately hung test processor, `core.hungProcessor`, and `smartturn`; recorded as a gap.)
```

- [ ] §Go Idioms 1, replace the "No Context in Structs" bullet (line 81) with (U2 = B; with U2 = A drop the second exception and list the six sites as known violations under it):

```markdown
- **No Context in Structs:** Never store a `context.Context` inside a struct field. Pass context as the first argument (`ctx context.Context`) to methods and functions. Two exceptions, each tagged with a comment at the field:
  - *Trace parentage* (`// trace parentage`): a context kept only so spans started later, off the caller's stack (failover replays, the async tracer, session trace roots), have the right parent. It is never used for cancellation or deadlines.
  - *Owner lifetime* (`// owner lifetime`): the context of a long-lived object that owns goroutines, created together with its `cancel` in the constructor (or the stream open) and cancelled by the object's own `Close`/teardown (session, WebSocket connection, provider streams, export queue). Never a caller's request context.
```

- [ ] §Mandatory Code Review Workflow 4, replace lines 123-126 with:

```markdown
4. **Verification Gate:**
   - `gofmt -w .`
   - `make verify`: vet and `go test -race` in every module of the Makefile's `MODULES`, the Go end-to-end tests, gofmt, the protobuf fixture and manifest checks, and the docs link checker (`scripts/check-links.mjs`).
   - `make deadcode`: no unreachable function outside `scripts/deadcode-allow.txt` (advisory in CI).
   - `make e2e-echo` (Go and browser end-to-end on the free echo provider) and, when the offline stack is running, `make offline-test`. Paid/live targets only when asked.
   - Clean git status with descriptive conventional commit messages.
```

- [ ] Tag comments on the 14 fields of fact 14 plus the one map of contexts, `server/otelexport/otelexport.go:191 turnCtxs map[core.TurnID]context.Context` (per-turn span parents: `// trace parentage`) (append `// trace parentage` or `// owner lifetime`, keeping any existing comment text after it, e.g. `ctx context.Context // trace parentage: the user message's, for replays`). Pin: `git grep -P '^\t+\w+(, \w+)*\s+context\.Context(\s|$|//)' -- '*.go' ':!pipecat-impl' ':!*_test.go' | grep -vcE '// (trace parentage|owner lifetime)'` → `0`.
- [ ] Commits: `docs: AGENTS.md rules for parked code, leak checks, context exceptions and the gate`, `chore: tag the context fields AGENTS.md allows`.

### 9.1d Link checker in `make verify` (R5)

- [ ] `scripts/check-links.mjs` = the Phase 8 checker (`p8/check-links.mjs`) with the `--moves` branch, its argument parsing and the header line about it removed; header first line becomes `// Docs reference checker (make verify): relative markdown links resolve; with --paths, backticked repo paths exist.`. The allowlist and `top` regex unchanged.
- [ ] `Makefile` `verify`, before "All verify checks passed":

```make
	@echo "=== Docs links and paths ==="
	node scripts/check-links.mjs --paths
```
- [ ] Pins: `node scripts/check-links.mjs --paths` → `check-links: 0 problem(s), N markdown files`; negative: append `` `core/nope.go` `` to `README.md` → exit 1 naming it; `git checkout README.md`.
- [ ] Commit `build: make verify checks docs links and repo paths`.
- [ ] Gate (+ `live-check-selftest`, ui, `test-postgres`).
- **Rollback:** revert per commit.

## Task 9.2 — Goroutine-leak assertions (U1; ≈ +60 lines, + ≈ 40–100 lines of server test-harness teardown)

- [ ] Step 1 (deps, hand edit, fact 4): root `go.mod`: add `go.uber.org/goleak v1.3.0` to the first `require` block (alphabetical, after `github.com/yalue/onnxruntime_go`); root `go.sum`: append the two `go.uber.org/goleak v1.3.0` lines from `server/go.sum`. `core/go.mod` **(P8: now has `require google.golang.org/protobuf`)**: add `go.uber.org/goleak v1.3.0` to it; `core/go.sum`: the same two lines. `server/go.mod`: move goleak from `// indirect` (if listed; else add) to direct. Pins: `go list -m all` per module identical to `$P/tmp/mods-before-<m>.txt` saved first (except the goleak line where it was absent); `cd core && GOWORK=off go build ./... && GOWORK=off go vet ./...`.
- [ ] Step 2 (`core/sttunit`, `core/ttsunit`): new `leak_test.go` in each (package of the existing tests):

```go
package sttunit // ttsunit in core/ttsunit

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
```
Pin: `cd core && go test -race -count=3 ./sttunit/ ./ttsunit/` ok (trial: ok). Mutation (the runner spawns through `session.Go`, so there is no local join to remove): plant `go func() { select {} }()` in `Runner.Attach` → the package fails with a goleak report naming it; remove it.
- [ ] Step 3 (`vad`): in `vad/export_test.go` `TestMain`, after `ShutdownORT()`:

```go
	if code == 0 {
		if err := goleak.Find(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
```
Pin: `go test -count=1 ./vad/` ok and `go test -race -count=1 ./vad/` ok.
- [ ] Step 4 (`transport/ws`): `transport/ws/leak_test.go` (`package ws_test`, `TestMain` with `goleak.VerifyTestMain(m)`). It FAILS today with 4 leaked connections (fact 4). All four come from the shared helper `dialReady` (`transport/ws/client_safety_test.go:19`), which never closes its client: one line after the dial, `t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })`, fixes all of them (design review: `-race -count=3` passes). If any leak remains after that, find it per test (`for t in $(go test -list '.*' ./transport/ws/ | grep ^Test); do go test -count=1 -run "^$t\$" ./transport/ws/ >/dev/null 2>&1 || echo LEAK $t; done`); a leak that survives the client closing and the server's `Close` is a product leak: STOP, write a failing test, report (do not ignore it with an option). No `goleak.IgnoreTopFunction` for project code. Pin: `go test -race -count=3 ./transport/ws/` ok.
- [ ] Step 5 (`server`): `server/turn_stop_test.go` `TestMain` gets the same `goleak.Find` block as Step 3, after `vad.ShutdownORT()`. Run `cd server && go test -count=1 -tags xaispeech . 2>&1 | tee $P/tmp/server-leak.log` (≈ 230 s). Expected from the design review's trial: FAIL with 11 goroutines from about 5 sources: 2 sessions never closed (`core.(*Session).runLoop` + `watchdog`), `Server.pruneTimelines` (a server a test never shut down), the `waitUntil` test helper's goroutine, an MCP `streamableServerConn.Read`, 2 `net/http` `persistConn`s. First list every source with the test that creates it (`$P/tmp/server-leak-sources.md`). Decision rule per source: (a) a third-party background goroutine that lives for the process by design (`database/sql.(*DB).connectionOpener` of a DB a test never closed is NOT this: fix the test; OTel/gRPC global workers, `net/http` keep-alive `persistConn` of `http.DefaultClient`) → `goleak.IgnoreTopFunction("<exact func>")` with a comment naming why; (b) project code (`govoice/...` on top or in the creating frame) → fix the test's teardown, or, if teardown is correct, a product leak: failing test + fix in this task. The expected sources are test-harness ones (unclosed session/server/client, a helper goroutine without a stop): fix them in the harness (`newFlowRigE2E` and the rigs of fact 4 register `Shutdown`/`Close` in `t.Cleanup`; `waitUntil` stops its goroutine on return). STOP and report (user decides scope) only if a source is product code with correct teardown (a real leak: record it with its stack, write the failing test, do not fix it in this task unless ≤ ~30 lines), or if the harness fixes exceed ~10 test files. Pin: `cd server && go test -race -count=1 -tags xaispeech .` ok; `make test-e2e-go` ok.
- [ ] Commits: `test: goleak checks in sttunit, ttsunit and vad`, `test(transport): ws tests close their connections; goleak check`, `test(server): goleak check at the end of the server package tests`.
- [ ] Gate (+ `loadtest-smoke`, `test-postgres`).
- **Rollback:** revert per commit; `go.mod` edits revert with them.

## Task 9.3 — smartturn: a superseded or closed job is not a timeout (flake 1; U3a)

**Files:** `smartturn/analyzer.go`, `smartturn/analyzer_test.go`.

- [ ] Step 1 (RED): add to `smartturn/analyzer_test.go` (the probe of fact 5, kept):

```go
// A job cancelled because a new silence superseded it, or because the analyzer closed, is not a
// timeout: only a verdict missing its latency budget is (voice_smartturn_fallbacks_total{reason="timeout"}).
func TestSupersededOrClosedJobCountsNoFallback(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	be := newFake(0, nil) // never released: Infer returns ctx.Err() when cancelled
	a := newTestAnalyzer(be, clk)
	before := Fallbacks(ReasonTimeout)
	a.verdict(input(clk, 100*time.Millisecond, "t1")) // job 1, inside the budget
	clk.now = clk.now.Add(10 * time.Millisecond)
	a.verdict(input(clk, 50*time.Millisecond, "t2")) // new silence key: job 1 superseded
	a.Close()                                        // job 2 cancelled
	eventually(t, func() bool { return be.cancels.Load() == 2 })
	// finish runs right after Infer returns; give both goroutines a bounded window to (wrongly) count.
	for end := time.Now().Add(100 * time.Millisecond); time.Now().Before(end); runtime.Gosched() {
		if got := Fallbacks(ReasonTimeout) - before; got != 0 {
			t.Fatalf("superseded/closed jobs counted %d timeout fallbacks, want 0", got)
		}
	}
}
```
(`runtime` import added.) `go test -count=1 -run TestSupersededOrClosedJobCountsNoFallback ./smartturn/` → FAIL `counted 1` or `2` (fact 5's probe, same steps, failed with 2). The window is a bound on how long a wrong count may take to show up, not a synchronisation sleep: the GREEN code never counts, so the loop always runs its 100 ms.
- [ ] Step 2 (GREEN): in `analyzer.go` add

```go
// abandonLocked drops a job nobody waits for any more (superseded or closed): its late result,
// including the context.Canceled its own cancel causes, is ignored and counted nowhere.
func (a *Analyzer) abandonLocked(j *job) {
	j.done = true
	j.cancel()
}
```
and use it in `startLocked` (`if a.job != nil { a.abandonLocked(a.job) }`) and `Close` (`a.abandonLocked(a.job)` under the existing nil check). `finish` already returns early on `j.done`. `failLocked` is unchanged (a real budget timeout still counts once).
- [ ] Step 3: the existing `TestAnalyzerTimeoutFallsBackToSpeechTimeout` assertion becomes exact-diagnostic: `if got := Fallbacks(ReasonTimeout) - before; got != 1 { t.Fatalf("timeout fallbacks counted %d, want 1", got) }` (R9 diagnostics; still exact).
- [ ] Step 4 (stress, R9): `$P/stress.sh` (ledger, reused by 9.4):

```bash
#!/bin/bash
# stress.sh <pkg-dir> <test-regex> <runs>: race-built test binary, run under 8 CPU burners with shuffle.
set -u; P=.superpowers/sdd/2026-10-15-review-remediation/p9; pkg=$1; re=$2; n=$3; out=$P/tmp/stress-$(basename $pkg)
mkdir -p $out; go test -c -race -o $out/t.test ./$pkg || exit 2
pids=(); for i in 1 2 3 4 5 6 7 8; do yes >/dev/null & pids+=($!); done
trap 'kill "${pids[@]}" 2>/dev/null' EXIT          # only the PIDs this script started
fail=0; for i in $(seq 1 $n); do
  (cd $pkg && $OLDPWD/$out/t.test -test.run "$re" -test.count=1 -test.shuffle=on -test.cpu 1,2,8) > $out/run-$i.log 2>&1 \
    || { fail=$((fail+1)); grep -m1 -- '-test.shuffle' $out/run-$i.log; }
done; echo "stress $pkg '$re': $fail/$n failed (logs $out)"; [ $fail = 0 ]
```
Run `bash $P/stress.sh smartturn . 200` (whole package, shuffled: the original failure needed another test's job to finish inside the window). Expected `0/200 failed`. Then `git stash` the fix (keep the new test), `bash $P/stress.sh smartturn 'TestAnalyzer|TestSuperseded' 50` → the new test fails every run (proves the RED is not flaky in the other direction); `git stash pop`.
- [ ] Commit `fix(smartturn): a superseded or closed analysis is not counted as a timeout fallback` (body: production metric was inflated by every superseded silence window; the likely root cause of the open TestAnalyzerTimeoutFallsBackToSpeechTimeout flake).
- R9 is met by the new deterministic test (design review: fails 3/3 with `counted 2 timeout fallbacks, want 0` before the fix, the `abandonLocked` fix passes `-race -count=3`). The original flake itself did not reproduce (0/150 shuffled runs without burners in the review, hundreds before): the release notes say its link to this bug rests on reasoning (the only way the counter moves by other than +1 in that window), not on a reproduction.
- [ ] Gate (+ `loadtest-smoke`).

## Task 9.4 — core: tests do not assume order across the control and data lanes (flake 2)

**Files:** `core/lifecycle_test.go` (and any other test the inventory finds).

- [ ] Step 1 (mechanism, shown by a probe): write `core/lane_order_probe_test.go` (temporary, not committed). A fresh session per iteration does NOT reproduce it (design review: 0/2000 in 9 runs, with or without `-race`); the run loop must be busy when both frames land. Use ONE session whose pipeline has a processor that, on a "tick" `UserMessageFrame`, spins 0–3 µs (busy loop on `time.Since`, random per tick) before pushing it on; then, 50,000 times: post tick, post `FlushFrame{Gen: s.CurrentGeneration()}`, post a `UserMessageFrame`, and record whether the user message reached `OnOutput` before the flush. Run WITHOUT `-race` at `-cpu 2,8`: the design review saw the user message first 46/50,000 and 49/50,000 times. Expected: > 0 occurrences → mechanism confirmed (Step 2 fixes the tests). The fix is test-side by design: a production change (poll `s.control` again after taking from `s.data`) ended the inversion but broke `TestPhase2_WriteCompletionRacingBarrier_DeterministicOrdering` (`core/context_test.go:421`, `-count=20`), which relies on the opposite order. The contract is "control overtakes queued data; no order across lanes" (Pipecat's SystemFrame priority path gives no cross-lane order either).
- [ ] Step 2: inventory tests with the same shape: `git grep -n -A3 'Post(&core.FlushFrame' -- 'core/*_test.go' | grep -B1 'Post(&core\.\(UserMessage\|LLM\)'` plus any test that `Post`s a control frame (`End`, `Error`, `Interruption`, `Flush`, `InputMute/Unmute`) and then a data frame and asserts their output order, plus tests that post data then control and use a sleep to order them (e.g. `core/context_test.go:421-443`, `time.Sleep(10 * time.Millisecond)` between `FinalTranscriptionFrame` and the rest): fix if a wait on an observed output can replace the sleep, else record each as timing-sensitive in the ledger and the release notes' known gaps. For each: await the control frame's output before posting the data frame (the contract is "control frames overtake queued data", not "two independent Posts keep their order"). Example for `TestFlushStaleInterruptionCannotCancelCurrentTurn`:

```go
	_ = s.Post(&core.FlushFrame{Gen: 1})
	awaitType(t, seen, core.FrameFlush)
	_ = s.Post(&core.UserMessageFrame{Turn: "B", Gen: 2, Content: "current"})
	awaitType(t, seen, core.FrameUserMessage)
```
- [ ] Step 3 (stress, R9): `bash $P/stress.sh core 'TestFlush' 300` → `0/300`; delete the probe file.
- [ ] Commit `test(core): flush tests wait for the flush before posting data (no cross-lane order assumption)`.
- [ ] Gate.

## Task 9.5 — Small backlog fixes (U3 b–e; each test-first, one commit each)

### 9.5a Run-loop RTVI writes bounded below the watchdog; `writeMu` deleted (fact 8)

- [ ] RED `transport/ws/write_bound_internal_test.go` (`package ws`, internal like `enqueue_internal_test.go`): start a real server + client pair (reuse the helper `enqueue_internal_test.go` uses, or `httptest` + `websocket.Dial`); make the server's socket writes stall the way production does: the client stops reading and the server writes until the kernel buffers are full, so a write is stuck mid-frame holding coder/websocket's frame lock (`writeFrameMu`). If that is not reachable deterministically in a unit test, hold `w, _ := c.conn.Writer(ctx, websocket.MessageBinary)` instead (the message lock only) and say in the test comment that it covers the wait, not the close code. Then call `c.ProcessFrame(ctx, f, core.Downstream, emitter)` with a frame that writes several RTVI messages (an `LLMTextDeltaFrame`: `for _, msg := range c.rtvi.BotLLMText(...)`, `transport.go:1010`; also `:1019,1026,1033`) and assert: it returns in < 1.5 s in total (today: each message waits up to `WriteTimeout` on the library lock), the observer saw `slow_client` exactly once, and the connection's context is cancelled. Do not assert close code 1008 in the frame-lock case: when the stuck write's own context expires, coder/websocket v1.8.15 closes the connection without a close frame (`conn.go:171-179`, `setupWriteTimeout` → `context.AfterFunc(ctx, c.close)`), and `closeBounded`'s close frame cannot get the frame lock either, so the client sees 1006.
- [ ] GREEN: delete `writeMu` (field, the pacer write function's lock/unlock, `WriteWireMessage`'s lock/unlock; the comment above the pacer write function says coder/websocket serialises writes, `conn.go:30`). Add:

```go
// loopWriteTimeout bounds an RTVI write made on the session run loop: it must give up well before
// the run loop's watchdog (core MaxFrameProcessing, 2 s) so a client that stopped reading ends as a
// slow client, not as a session failure.
const loopWriteTimeout = time.Second

// writeOnLoop is WriteWireMessage for ProcessFrame: a write that cannot finish in loopWriteTimeout
// (the socket is held by a stalled audio write) terminates the connection as a slow client.
func (c *Connection) writeOnLoop(ctx context.Context, msg string) {
	if c.ctx.Err() != nil {
		return // the connection is ending (e.g. terminated as a slow client by the previous message): don't wait again
	}
	wctx, cancel := context.WithTimeout(ctx, min(loopWriteTimeout, c.cfg.Pacer.WriteTimeout))
	defer cancel()
	if err := c.WriteWireMessage(wctx, msg); errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		c.terminateSlowClient()
	}
}
```
Extract `terminateSlowClient()` from `transport.go:594-597` (the pacer goroutine there calls the Server's `s.observe`; on the Connection use `c.observe("slow_client", 1)`, `transport.go:841`, which forwards to `c.cfg.Observe`, the same hook; then `closeBounded(StatusPolicyViolation, "slow client terminated", 500*time.Millisecond)`, `cancel`), guarded by a `sync.Once` field so the pacer path and this one count once; the pacer goroutine calls it too. Replace every `WriteWireMessage(ctx, …)` inside `ProcessFrame` (fact 8 sites from :943 on) with `c.writeOnLoop(ctx, …)`. `WriteWireMessage` keeps its `WriteTimeout` context (its other callers are off the run loop). Worst case on the run loop: 1 s + 0.5 s close < 2 s watchdog; every later message of the same `ProcessFrame` returns at once on `c.ctx.Err()` (without that guard: 1 + 0.5 + 1 s > 2 s, because `Close` is still in its handshake and the next write waits on the library lock again).
- [ ] Pins: RED test green; `go test -race -count=3 ./transport/ws/` ok (incl. the 9.2 goleak check); `BenchmarkUnderlayIdleWriters` and `BenchmarkUnitWrite` allocs unchanged (`-benchmem`, compare to `$P/tmp/bench-before.txt` taken first).
- [ ] Commit `fix(transport): a client that stalls an RTVI write ends as slow_client, not a watchdog failure; rely on the websocket library's write lock`. Release-note: behaviour change: a client that stops reading during an RTVI write is terminated as `slow_client` (counted in `voice_transport_slow_client_terminations_total`; close code 1008 "slow client terminated" when the close frame can still be sent, else the socket just drops: 1006) where a watchdog session error was logged before.

### 9.5b `Say` cannot reset a model turn (fact 9)

- [ ] RED in `core/assistant_context_test.go` (or the file holding the Say tests: `git grep -ln 'func TestSay' core/`): drive `ProcessFrame` directly in run-loop order: `UserMessageFrame{Turn: "u1"}` (model turn), then the say frames for `"say-1"` (`UserMessageFrame`, `LLMTextDeltaFrame`, `LLMTextFrame`), then `LLMTextDeltaFrame/LLMTextFrame` for `u1`; assert the committed assistant message is the model's text and the say line is neither emitted downstream nor committed.
- [ ] GREEN, `ProcessFrame` `case FrameUserMessage` first lines:

```go
		um := f.(*UserMessageFrame)
		if IsSayTurn(um.Turn) && p.modelActive.Load() {
			// A model turn started after Say checked: the scripted line loses (its later frames are stale).
			return nil
		}
```
- [ ] Pins: `go test -race ./core/ -run 'Say|Idle|AssistantContext'` ok; `make test-e2e-go` (CallBehavior idle lines) ok.
- [ ] `Say` has already returned nil for the dropped line, so an idle prompt that loses this race counts as spoken (spent) without being heard. Note it in the idle controller's retry comment (`core/idle.go:529-533`, "a prompt refused because a model reply is in progress…": add "or dropped by the run loop when the model turn wins the race; that one is spent") and in the release notes (§4).
- [ ] Commit `fix(core): a scripted line that loses the race to a model turn is dropped, not spoken over it`.

### 9.5c Resampler general path: one allocation per chunk (fact 10)

- [ ] RED: `BenchmarkResampler_20msChunk` stays as is; add `TestResamplerGeneralPathAllocs`: `testing.AllocsPerRun(100, …)` on a warmed 16 k→24 k resampler with a 20 ms chunk → `<= 1` (today 7). Add `TestResamplerGeneralPathChunkingInvariant` mirroring `TestDownsample48To16_ChunkingInvariant` for 16 k→24 k and 24 k→16 k (random chunk sizes 1..960 samples, plus 1-byte (half-sample) chunks after 2–10 bytes of history, which hit the `phase < 0` branch: concatenation equals the output of the **current** implementation byte for byte: generate the expected bytes with the pre-change function copied into the test as `refResample` before editing).
- [ ] GREEN: in `ResamplePCM16`'s general path read samples straight from `pcmIn` (`int16(binary.LittleEndian.Uint16(pcmIn[2*i:]))`, via a small `sample(i int) float64` closure over `pcmIn` and `r.lastSample`), compute the output count first with the existing loop's exact branch order: `n := 0; for ph := phase; ; ph += r.ratio { idx := int(math.Floor(ph)); if idx >= 0 && idx >= numSamplesIn-1 { break }; n++ }` (the old loop takes the `idx < 0` branch before testing `idx >= len-1`, so it emits samples even for a 0-sample chunk when phase < 0; a plain `idx < numSamplesIn-1` condition gives `got 0 bytes want 4; phase -1 vs 0.333` on a `[]byte{7}` chunk, design review), allocate `out := make([]byte, 2*n)` once and write with `binary.LittleEndian.PutUint16`. Same arithmetic, same rounding and clamping, same state update.
- [ ] Pins: both tests green; benchmark ≤ 1 alloc/op (design review's implementation: 1378 ns, 1024 B, 1 alloc vs 2324 ns, 3648 B, 7 allocs); `go test ./core/transport/resample/ ./core/record/ ./core/audio/... ./transport/ws/` ok; `cd providers/nvidia && go test ./...` ok.
- [ ] Commit `perf(core): the general resampler path allocates only its output`.

### 9.5d Log the VAD worker's stderr tail when it fails (fact 11)

- [ ] RED in `vad/resilience_test.go` (the hard-kill test already kills a worker): install `slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))` (restore the previous default in `t.Cleanup`), kill the worker, wait for `Restarts >= 1`; assert `buf` contains `vad: worker failed` and a `stderr=` attribute. (The worker writes its startup line to stderr; if it writes nothing, assert the attribute is present, possibly empty.)
- [ ] GREEN in `handleWorkerFailure` after `killAndReapLocked` (outside `wp.mu`):

```go
	slog.Warn("vad: worker failed", "worker", wp.id, "error", err, "stderr", wp.stderr.tail(4096))
```
`wp.stderr` is shared across respawns and never reset, so add to `boundedBuffer` (`vad/worker_executor.go:73-110`) one method `tail(n int) string` that returns the last `n` bytes and resets the buffer (under its own lock): each failure logs only what the dead process wrote, at most 4 KiB. It carries ORT/runtime diagnostics, never audio. The RED test asserts a second kill does not re-log the first failure's text.
- [ ] Commit `fix(vad): log the worker's stderr tail when it fails`.
- [ ] Gate (+ `loadtest-smoke`, `test-postgres`) once for 9.5a–d.
- **Rollback:** revert per commit.

## Task 9.6 — Race stress across all modules (R8)

- [ ] On an idle machine, with the timing recorded:

```bash
start=$(date +%s); for m in $(sed -n 's/^MODULES := //p' Makefile); do
  (cd $m && go test -race -count=5 -shuffle=on -timeout 30m -tags xaispeech ./...) > $L/9.6-race5-$(echo $m | tr / _).log 2>&1 \
    || echo "FAIL $m"; done; echo "elapsed $(( $(date +%s) - start ))s"
```
Budget: 45 min wall. Over budget: report the per-module times; do not cut modules.
- [ ] Every failure: name it, record the `-test.shuffle` seed line from the log, rerun that package with `-test.shuffle=<seed> -count=5` once. Reproduced → R9 (root cause or OPEN with diagnostics; a race report is always a bug: fix in this task with a test). Not reproduced → OPEN in the release notes with the log path and seed.
- [ ] No commit unless a fix is needed (then `fix(<area>): …` with its test).

## Task 9.7 — Measure the remediation: 1993240 vs head (master 9.3; results into 9.8)

All raw outputs in `$P/measure/`; the summary table goes into the release notes (9.8). Run on an idle machine; interleave base/head runs.

- [ ] Step 1 (baseline tree): `git worktree add --detach $P/tmp/base 1993240`; copy the ignored ORT library: `mkdir -p $P/tmp/base/third_party/onnxruntime-osx-arm64-1.29.0 && cp -R third_party/onnxruntime-osx-arm64-1.29.0/lib $P/tmp/base/third_party/onnxruntime-osx-arm64-1.29.0/` (local copy, no download); symlink `node_modules` for `ui`, `e2e`, `tools/compat` (lockfiles identical, fact 17). `(cd $P/tmp/base && make build)` ok. At the end: `git worktree remove $P/tmp/base`.
- [ ] Step 2 (lines): `git diff --numstat 1993240 HEAD` categorised by the awk of fact 2, plus categories `vendored` (`third_party/`), `generated` (`*.pb.go`, `deploy/sbom.json`), `docs/history` (moves): table of +/−/net per category; `git diff --shortstat`.
- [ ] Step 3 (deadcode): `deadcode@v0.52.0` master and wide at both trees (`-test=false`), and `-test` wide (test helpers dead even to tests) at both; counts.
- [ ] Step 4 (tests): per module `go test -list '^(Test|Benchmark|Fuzz|Example)' -tags xaispeech ./... | grep -cE '^(Test|Benchmark|Fuzz|Example)'` at both; `cd ui && npx vitest list 2>/dev/null | wc -l` (if `vitest list` is unavailable in vitest 5.0.3: `npx vitest run --reporter=json --outputFile $P/measure/<rev>-vitest.json` and count `numTotalTests`); `cd e2e && npx playwright test --list | tail -1` (both configs the Makefile uses: `e2e-fake` and `e2e-oneport`).
- [ ] Step 5 (static): per module `go vet -tags xaispeech ./...` (expected clean at both) and `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -tags xaispeech ./...` → finding counts per check id (`awk '{print $NF}' | sort | uniq -c`).
- [ ] Step 6 (artifacts): `go build -trimpath -tags xaispeech -o $P/tmp/<rev>-server ./cmd/server` → `stat -f %z`; UI: `cd ui && npx vite build --outDir $P/tmp/<rev>-ui --emptyOutDir` → total bytes and gzip bytes of `assets/*.js` and `*.css` (`gzip -c f | wc -c`).
- [ ] Step 7 (benchmarks): at both trees, for the 7 shared benchmarks (fact 17) `go test -run '^$' -bench '<name>$' -benchmem -count 10 <pkg>` → `$P/measure/<rev>-bench.txt`; `go run golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e $P/measure/base-bench.txt $P/measure/head-bench.txt`. HEAD-only benchmarks (`UnitWrite`, google/xai `UnaryUnit`, `Downsample48To16`, `Resampler_20msChunk` after 9.5c): absolute numbers plus the phase-recorded befores (`p6/bench-*-before.txt`, `p7/bench-ttsunit-before-7.4.txt`).
- [ ] Step 8 (load and perf, ×3 interleaved base/head): in each tree `go run ./tools/loadtest -sessions 4 -duration 30s -ramp 3s -turns-per-session 3 -sample-interval 2s -max-p95-v2v 1500 -json-out $P/measure/<rev>-load-<i>.json` and `go run ./tools/eval/cmd/govoice-bench -sessions 4 -turns 200 -json-out $P/measure/<rev>-bench-<i>.json`. Summarise (median of 3): client V2V p50/p95/p99, server `stt_final` and `voice_to_voice` p99, `cpu_s_per_session_min`, `mb_per_session`, `goroutines_after_drain`, bench `Overhead` P50/P95/P99. Plus `make loadtest-smoke` and `make perf-gate` once each at head (pass/fail vs the committed baseline).
- [ ] Step 9 (acceptance, master 9.3): no p99 regression beyond the perf-gate tolerance table (`tools/loadtest/compare.go`); deadcode wide = allowlisted entries only. A regression: STOP and report with the numbers (do not tune).

## Task 9.8 — Release notes (U4; ≈ +250 lines of docs)

**File:** `docs/superpowers/remediation/release-notes.md` (+ one row in `docs/superpowers/README.md`).

- [ ] Sources: merge messages of Phases 1–4 and the two side merges (`git show -s --format=%B 946f646 2e55347 03eb695 318d1cd 9f37b84 3a243fd 4cc6058`), `p5/pr-notes.md`, `p6/pr-notes.md`, `p7/pr-notes.md`, `p7b/pr-notes.md`, `p8/pr-notes.md`, this phase's ledger.
- [ ] Sections (each item one line, with the commit or PR merge it came from):
  1. **Breaking for operators** — verify each against the tree before writing it (command in brackets): `critical_output` mute strategy removed: a server YAML listing it under `turn.mute` fails at start-up with its own message (`git grep -n critical_output core/turn/mute.go` → `removedMuteCriticalOutput`, `:46`); REST `arguments_summary`/`result_summary` removed from traces (`git grep -n arguments_summary -- server/api ui/src` → none); metric `voice_stt_deduplicated_results_total` removed (`git grep -n deduplicated -- server` → none); voice token `token.max_lifetime` default 1 m, above the cap refused (`server/config_validate.go:21-60`); flow create/duplicate validate MCP selections (400); xAI LLM requires an explicit key (no `XAI_API_KEY` fallback in the adapter); ws read limit (over-limit closes with 1009 and counts `protocol_error`); a client stalling an RTVI write is closed as `slow_client` (9.5a); `voice_smartturn_fallbacks_total{reason="timeout"}` no longer counts superseded or closed analyses (9.3: expect a drop); the automatic trace-content purge is deleted: operators whose databases were written by builds between 5be193e and 12a7884 with trace content mode configured must purge stored trace content by hand (SQL in `docs/operations/retention.md`), and retention results `ToolCallsPurged`/`TracesSanitized` are gone (p5 notes :9, :22); new flow publish refusals (400): `turn` settings while `turn.manager` is off, `tts.clean_text`/`failover` on a non-start step, unknown route or hold cue, `max_mute_ms` above the server's `turn.max_mute`, `max_call_duration_seconds` above `limits.max_call_duration`, `smart` without the model (p7b notes :19); `deploy/sbom.json` is no longer tracked (written by `make release-security`), the `offline/contract` module is gone (tests in `server/offlinecontract`, `make offline-test` path changed) (p8 notes).
  2. **Wire (RTVI) additions** — `bot-interrupted` on barge-in, `bot-tts-text` after the unit's audio, tool-call `llm-function-call-*` events (Phase 3), non-fatal error categories.
  3. **Breaking for library users of `govoice/core`** — p5 "Library API changes" + p6 "Removed/Added" lists verbatim, p6 R5 (it sits under "Decisions" in the p6 notes, :32: breaker-open and attempt-timeout errors from `resilience.Execute` are no longer `*ProviderError`; use `errors.Is` and the failover class), the `tools/eval/scenarios` API deleted in Phase 8 (`SevenScenarios`, `ExtendedParityScenarios`, `AllScenarios`, `GetScenario`, `Scenario`), plus Phase 9 moves (`DefaultServerConfig`, `LoadConfig`, `PublicAddr`, `WithClock`, `BlackHole.Accepted` are test helpers now).
  4. **Behaviour changes** — p6 list 1–9, p7/p7b behaviour items, Phase 4 (sync LLM failover, per-rate output resampler, VAD worker respawn/routing), Phase 2 (STT dial off the run loop, pre-roll, drop invalidates the turn); 9.5b (an idle prompt that loses the race to a model turn is dropped and counts as spent).
  5. **Privacy and security** — trace spans record error type only (1.2); env-only secrets; redaction of idle lines in the sidecar; SSRF rule in `netpool.AllowedIP`.
  6. **Measured** — the 9.7 tables.
  7. **Known gaps, flakes and carried items** — R6, R7, any OPEN from 9.4/9.6, `PLAYBACK_CLEAR_REQUIREMENT_FAILED` under `relax-unmodified-client-playback-clear`, U1 columns kept (Phase 8); the smartturn flake's link to the 9.3 bug rests on reasoning (no reproduction of the original); tests recorded as timing-sensitive in 9.4; goleak not yet on `core` and `smartturn` (9.1c).
- [ ] Pins: `node scripts/check-links.mjs --paths` 0 (the doc is under `docs/superpowers`, excluded from path checks; check its relative links manually: `node scripts/check-links.mjs docs/superpowers/remediation/release-notes.md`).
- [ ] Commit `docs: remediation release notes with measured before/after`.

## Task 9.9 — Final whole-remediation adversarial review (master 9.2; R10)

- [ ] Step 1 (controller, mechanical inventory → `$P/review/inventory.md`, each with the command): the 14 tagged context fields and the `turnCtxs` map (9.1c pin); `go` statements in `core/` not via `session.Go` (`git grep -nE '\bgo (func|[a-z][A-Za-z.]*\()' -- 'core/*.go' 'core/**/*.go' ':!*_test.go' ':!core/coretest'`, fact 18: 14); `make(chan` without capacity and `append` in loops on audio/token paths (`git grep -n 'make(chan [^,]*)$'`); `UnixNano()` and `time.Now().Sub(` uses; `.Lock()` sites in files that also call `Write(`/`Exec(`/`Query(`/`Do(` (manual follow-up list); `recover()` presence in `transport/ws` writers and provider callback goroutines (`git grep -n 'recover()' -- transport providers`); log/trace statements that take transcript, prompt, tool arguments or DTMF digits (`git grep -nE 'slog\.(Info|Warn|Error|Debug)\(.*(Text|Transcript|Content|Arguments|Digits)' -- '*.go' ':!*_test.go'`); `time.Sleep` in tests (fact 18) and `waitForTimeout` in e2e; `t.Skip` list; the 9.6 flake list.
- [ ] Step 2: dispatch four independent reviewers (opus, read-only, run in parallel), each on `git diff 1993240..HEAD -- <area>` plus the inventory and `AGENTS.md`:
  - **A core/** (`core/`), **B media and providers** (`transport/`, `vad/`, `smartturn/`, `providers/`, `offline/`), **C server** (`server/`, `cmd/`, `config/`, `deploy/`, `examples/`, `proto/`), **D UI, tests, tools, docs, build** (`ui/`, `e2e/`, `tools/`, `docs/` outside `docs/superpowers` and `docs/history`, `Makefile`, `.github/`, `scripts/`, `AGENTS.md`, `README.md`, root `go.mod`/`go.sum`/`go.work`/`.gitignore`/`.dockerignore`, and the `third_party/` deletions: check nothing still needs the removed files).
  - Brief (same for all, area substituted):

> You are the final adversarial reviewer of the review remediation (Phases 1–9) for area **<X>**, diff `git diff 1993240..HEAD -- <paths>`, rules in `AGENTS.md`, decisions in `docs/superpowers/remediation/plans/2026-10-15-review-remediation.md` (D-0.1–D-0.8) and each phase plan's Rulings, inventory in `.superpowers/sdd/2026-10-15-review-remediation/p9/review/inventory.md`. Hunt, with file:line and a reproduction for each finding: (1) AGENTS.md violations anywhere in the area, old or new: unbounded queues or slices growing with audio/tokens, `context.Context` struct fields outside the two tagged exceptions, locks held across I/O or blocking channel operations, wall-clock time used for durations, goroutines without a termination path (and in `core/` not via `session.Go`), transport writers and provider callbacks without `recover()`, errors not wrapped with `%w` or compared with `==`; (2) races and goroutine leaks (read the 9.2 goleak coverage: what owns goroutines and is not checked?); (3) generation fencing: any async result, frame or RTVI event that can arrive from a stale generation; (4) privacy: transcript, prompt, tool arguments/results, DTMF digits (letters too) or secrets in logs, traces, metrics labels, error strings sent to clients or stored rows; secrets read anywhere but the environment; redaction coverage; (5) deleted-but-referenced: symbols, config keys, REST fields, metrics, Make targets, files still named by code, tests, docs, dashboards, `config/*.yaml`, `deploy/`; (6) docs truthfulness: every claim in `docs/` (outside history and the remediation records), `README.md`, `AGENTS.md`, the parity matrix rows and Known gaps, true against the code (open each cited path); (7) tests: assertions that pass vacuously, sleeps used as synchronisation, timing-sensitive e2e steps, skipped tests without a reason; (8) YAGNI/bloat: code added by the remediation that nothing on a call path uses, abstractions with one implementation, knobs nobody sets; (9) public API and wire changes missing from `docs/superpowers/remediation/release-notes.md`. Rank Critical / Important / Minor. Do not edit files.

- [ ] Step 3: merge the four reports into `$P/review/findings.md` (dedupe; each finding: area, severity, file:line, repro). Fix round for every Critical and Important (each test-first, own commit, its gate); Minors: fix if ≤ 10 lines and obviously safe, else list under "Known gaps" in the release notes. Re-review the fix commits (one scoped reviewer, sonnet) until no Critical/Important remains.

## Task 9.10 — Final gate and PR description

- [ ] Step 1: full gate block on the branch head including every conditional line (`loadtest-smoke`, `live-check-selftest`, ui, `test-postgres`, `offline-test`), `make e2e-echo` **twice** (second run on a fresh terminal after the first exits; both green), `make perf-gate` once, `make deadcode`, and `node scripts/check-links.mjs --paths`. Paid/live targets: never.
- [ ] Step 2: `git status --porcelain` shows only `?? IDEA.md`; `git log --oneline main..HEAD` all conventional with the trailer.
- [ ] Step 3: PR description (`$P/pr-description.md`, not committed): summary (5 lines), "Breaking changes" copied from release notes §1–3, the measured table (§6), the gate results (each target with pass and log path), link to `docs/superpowers/remediation/release-notes.md`. Ends with the attribution lines the session's system reminder prescribes for PR descriptions.
- [ ] Step 4: ledger `progress.md`: `PHASE 9 gate … ALL RC=0` line with the log directory, the OPEN list (should be empty or name each flake with its seed), "REMEDIATION COMPLETE pending merge". Do not push or merge.

---

## Estimated line delta per task

| Task | Delta | Note |
|---|---|---|
| 9.0 | +1 | README row |
| 9.1 | ≈ +60 | deadcode script/allowlist/target/CI ≈ +45, AGENTS.md ≈ +12, 14 tag comments (0 net), helper moves net 0, checker tracked ≈ +45 −0 |
| 9.2 | ≈ +100–160 | goleak TestMains, go.mod/go.sum +7, one `dialReady` cleanup, server test-harness teardown (≈ 5 sources) |
| 9.3 | ≈ +30 | 6 product lines, the rest test |
| 9.4 | ≈ ±10 | test reorders |
| 9.5 | ≈ +90 / −30 | writeMu −8, writeOnLoop/terminateSlowClient +25, Say +4, resampler ≈ −10 net, VAD +1; tests ≈ +100 |
| 9.6 | 0 | unless a race fix |
| 9.7 | 0 | ledger only |
| 9.8 | ≈ +250 | docs |
| total | ≈ +550–600 | of which product Go ≈ +30 net (9.5d adds `tail`) |
