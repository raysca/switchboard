# Phase 6: De-duplication (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. One task = one reviewable deliverable; a task may be several commits, and **every commit** keeps `go build` + `go vet` green in every `go.work` module in all three tag sets (see "Per-commit compile check"). Order: 6.0 → 6.1a → 6.1b → 6.1c → 6.1d → 6.1e → 6.2 → 6.3 → 6.4 → 6.5 → 6.6 (behaviour, U1) → 6.7 (behaviour, U2) → 6.8 (gate + review). Nothing runs in parallel: 6.1–6.3 and 6.6–6.7 all compile against the local `core` through `go.work`.

**Goal:** Collapse the duplicates of review §D that still exist on main 52db4be **and** whose shared form is smaller and no less safe; leave the rest with a measured reason. Two behaviour changes the master plan and the ledger assigned here (idle lines through `Say`, D9; unbound-final loss in the STT runner) are isolated tasks with failing tests first.

**Architecture:** The unary TTS processors of Google and xAI become `ttsunit.Attempt`s like every other TTS path; `ttsunit` gains a look-ahead mode (Google unary synthesizes up to 2 units ahead today, and the echo provider, every free e2e suite, `loadtest-smoke` and `perf-gate` run on that path) and stops copying each 20 ms chunk. The SSRF IP rule moves to `core/netpool`. The three failover wrappers share three small helpers. `tools/loadtest` and `tools/livecheck` share a new `tools/internal/stack` (spawn/stop/API/token/handshake) and `tools/internal/wav`. Server: one request-id helper, a table instead of the prefix ladder.

**Tech stack:** Go 1.27.1 workspace (`go.work`: `.`, `core`, `server`, `providers/{cartesia,deepgram,echo,elevenlabs,google,nvidia,openai,xai}`, `offline`, `offline/contract`). `core/go.mod` has **no requirements** (stays that way). `tools/` is in the root module `govoice`; `server` is its own module and cannot import `govoice/tools/internal/...`.

**Spec:** `docs/superpowers/specs/2026-10-15-deep-review.md` §D (D1–D11), §E. Master: `docs/superpowers/plans/2026-10-15-review-remediation.md` Phase 6 (Tasks 6.1–6.6). Ledger: `.superpowers/sdd/2026-10-15-review-remediation/progress.md` (items deferred to Phase 6) and `p5/pr-notes.md` §(e). Mapping: master 6.1 → 6.1a–e; 6.2 → 6.2 (SSRF only); 6.3 → skipped (see Skipped); 6.4 → 6.3 (helpers) + 6.6 (D9); 6.5 → 6.4; 6.6 → 6.5; ledger "unbound-final" → 6.7; gate → 6.8.

---

## Decisions for the user (ANSWERED 2026-10-19: U1 yes, U2 yes — final)

| # | Question | Recommendation |
|---|---|---|
| U1 | **Idle lines through `AssistantContextProcessor.Say` (D9, master 6.4).** Still true on main: `IdleController.say` (`core/idle.go:547-560`) re-enters a raw `SynthesisUnitFrame` (turn `idle-say-N`, gen 0) at the TTS index, so the idle prompt (`prompt_mode: say`), the bot-stuck fallback line and the goodbye never reach the LLM context, the stored turns, or the recording sidecar, and the client gets audio but no `bot-llm-text`/transcript for them. Routing through `Say` (the greeting path, `core/assistant_context.go:151-174`) changes all four: the model sees "Are you still there?" in history, operators see it in call records, the sidecar (redacted by `sidecarText`, `server/recording.go:47-55`) and the client get the text. Privacy: the lines are operator-configured static text, not caller content; they pass the same redaction as a greeting. | **Answered: yes. Do it (Task 6.6).** Pipecat parity: a `TTSSpeakFrame` is appended to the context (`pipecat-impl/.../llm_response_universal.py:2218-2228`, `append_to_context`). Net −15 lines (`SetSpeakIndex`/`SpeakIndex` and the unit-id dance go). Say "no" to keep idle speech out of history; then 6.6 is dropped and nothing else changes. |
| U2 | **Unbound-final loss in the STT runner (ledger P2 "Minor2").** Text a provider finalizes while the stream is unbound (between turns) is dropped by the runner by design (`core/sttunit/runner.go:714`); each Conn resurfaces it cumulatively in its next result. If the provider sends **no** further result after adoption + Finish (Riva may answer `CloseSend` with nothing more), `lost()` (`runner.go:658-663`) sees `lastText == ""` and the words vanish quietly: no A8 invalidation, no failover replay. | **Answered: yes. Do it (Task 6.7)**, +~6 lines in one place for all four runner providers: on adoption, seed `lastText` with the last unbound text (`settled=false`) so such an end is an A8 loss (invalidation → failover replay). Say "no" to carry it to Phase 9 unchanged. |

## Rulings (mine; recorded in the ledger; the reviewer may challenge them)

- **R1 Unify only what measures.** A duplicate is collapsed only when ≥2 copies exist on 52db4be and the shared form is smaller and no less safe; hot-path allocation and every bound must not regress; provider wire shapes stay in providers. Everything else is in "Skipped" with its measured reason.
- **R2 Google unary keeps its look-ahead.** `providers/google/tts.go:153-156,561-563`: units run on `ttsLookAhead+1 = 3` serial queues, so unit N+1 synthesizes while N plays; `tts_lookahead_test.go` pins it. `ttsunit` has one queue (attempt only on the unit's turn). Dropping it would add a full synthesis round-trip between sentences on the path every free suite runs (echo TTS uses `googleTTSProcessor`, `server/registry_echo.go:31-40`, voice `fake` = unary). So `ttsunit.Config.LookAhead` (0 = today, byte-for-byte). xAI unary has no look-ahead today (one `SerialQueue`) and keeps `LookAhead: 0`.
- **R3 No per-chunk copy in `ttsunit`.** `Unit.emit` copies every 20 ms chunk (`core/ttsunit/ttsunit.go:316`, `append([]byte(nil), pcm...)`); Google unary sub-slices without copying (`tts.go:465`). Moving Google onto ttsunit as is would add one heap alloc per output frame (50/s/call) on the free-suite path. 6.1b emits `u.buf[:n:n]` (3-index slice: frames never share capacity, an `append` on `Samples` reallocates; later `Write` appends only past the emitted bytes) — benchmark-gated, all streaming providers gain the same.
- **R4 Pure refactors keep tests unchanged except listed mechanical edits.** Allowed edits: a renamed counter field (`google.TTSProcessor.Errors` → `ErrorsTotal`, `providers/google/tts_handoff_test.go:73`), a moved unexported function's call sites in tests (6.2: `providers/openai/llm_test.go:302`, `providers/nvidia/stt_test.go:272,382`). 6.4: qualifying/moving the WAV helpers' tests in `tools/provtest/provtest_test.go`. 6.5/6.6 replace or add tests as their steps say. Anything else that needs a test edit is a behaviour change: stop and report.
- **R5 Error identity = `errors.Is` + failover class, not the concrete type.** Google unary sends `MapError("tts", err)` in its `ErrorFrame` (`tts.go:430`); the google attempt in 6.1e returns `MapError("tts", err)` for client errors, so errors **the client returns** still arrive as `*ProviderError`. Errors `resilience.Execute` raises itself (breaker open, attempt-timeout `ErrTimeout`) never pass through the attempt: on base they are `*ProviderError` ("provider circuit breaker is open (tts: circuit breaker open)"), after the port they are the raw resilience errors ("provider circuit breaker is open") — measured by the design review. `errors.Is` sentinels and the class (`classify`, server `flow.ClassOf`/`ttsClassify`) are identical, which is all the failover reads. Characterization (6.1a) pins, per failure kind, `errors.Is(…, ErrProviderTimeout)`, `errors.Is(…, ErrCircuitOpen)`, `errors.Is(…, context.Canceled)` and `classify(err)`; it uses `errors.As(*ProviderError)` only on rows whose error comes from the client, never on the breaker and timeout rows.
- **R6 Accepted, documented drifts in 6.1e (Google unary):** (a) a unit longer than `MaxSynthesisUnitChars` (250) is retried as a whole (today per piece) — the streaming fallback already does so; sentences are rarely split; (b) `Requests` counts at admission (ttsunit) instead of at synthesis: a unit that goes stale while queued counts as a request; (c) `OutputAudioFrame.Seq` is set and `Duration` is computed from the bytes (today `Seq` 0, `Duration` fixed 20 ms on a short tail) — the ws pacer already handles both from the other providers. (d) a lone trailing byte is dropped (Google base emits it in its 1-byte final frame: `A960 A1(final)` → `A960 A0(final)`, total 961 → 960); xAI's held-back tail becomes a separate empty final frame (base: one final 960-byte frame; port: a 960-byte frame + an empty final frame) — measured by the design review; the ws pacer treats an empty final frame as the drain marker already (the empty-audio path). No test pins any of the four (checked in 6.1a Step 3); if one does, stop.
- **R7 `perf-gate` against base, not a regenerated baseline.** The committed baseline `docs/benchmarks/baselines/darwin-arm64-12c*.json` is from 2026-10-13 (pre-Phase 1). 6.0 runs `make perf-gate` on base and records the result; 6.1e and 6.8 compare against both the committed baseline and the 6.0 base numbers. Never regenerate or commit a baseline in this phase.
- **R8 Out-of-scope ledger items carried, not dropped:** ws flush/RTVI writes waiting on `writeMu` on the run loop (`transport/ws/transport.go:1072-1074` → `WriteWireMessage` `:1148-1166`, lock shared with the pacer `:460-463`; a pacer write can hold it `WriteTimeout`, ≥ the 2 s watchdog) is a transport design change, not a duplicate → Phase 9 backlog; resampler general-path allocation (Phase 1 ruling) → Phase 9; `SessionController` type assertions (§E, 12 sites) → Phase 9. The latent flake `TestFlushStaleInterruptionCannotCancelCurrentTurn` (`core/lifecycle_test.go:165`, failed once in 5.1b, never reproduced) is stress-run in 6.0 and 6.8; it is fixed only if it reproduces (then superpowers:systematic-debugging, its own commit).

---

## Verified facts (2026-10-19, `feat/phase-6-dedup` = main 52db4be, commands actually run)

1. **Module graph.** `go.work` uses 13 modules (above). `core/go.mod`: `module govoice/core` / `go 1.27.1`, no `require`. `providers/nvidia/go.mod` requires `google.golang.org/grpc`; `providers/openai` does not. Build tags in tree: `xaispeech` (29 files), `integration` (8), `xaispeech && offlinestack` (4), `live` (1), `!xaispeech` (1).
2. **Line counts (before):** `wc -l` → `providers/google/tts.go 608`, `providers/xai/tts.go 542`, `core/ttsunit/ttsunit.go 377`, `providers/openai/dial.go 62`, `providers/nvidia/dial.go 189`, `core/netpool/netpool.go 471`, `core/llm_failover.go 447`, `core/stt_failover.go 536`, `core/tts_failover.go 529`, `core/session.go 1290`, `core/idle.go 566`, `core/sttunit/runner.go 745`, `tools/loadtest/server.go 509`, `tools/loadtest/client.go 418`, `tools/loadtest/audio.go 132`, `tools/livecheck/server.go 485`, `tools/livecheck/rtvi.go 338`, `tools/livecheck/clips.go 349`, `tools/provtest/wav.go 202`, `server/api_routes.go 215`.
3. **D2 unary TTS schedulers still wired.** `server/registry.go:244-249` (`googleTTSProcessor`: unary unless Chirp 3 HD and streaming enabled), `server/registry_echo.go:31-40` (echo TTS = google processor with an echo client), `server/registry_xaispeech.go:60-63` (xAI unary when `DisableStreaming`). Duplicated scheduler bodies: Google `tts.go:141-253` (113 lines: struct, counters, Attach/Close/cancel/ProcessFrame) + `:281-602` (322: `handleSynthesisUnit`, `waitSeq`, `staleGen`, `abandon`); xAI `tts.go:150-542` (390). Both re-implement `ttsunit.Runner.synthesizeUnit` (`core/ttsunit/ttsunit.go:117-243`): seq/cond ordering, cancel list, `abandon`, refused-task `ErrorFrame`, trace span, 20 ms framing, usage frame. `google/tts_streaming.go:52-79` already does a unary retry as a ttsunit attempt.
4. **D2 semantic differences found by reading** (each pinned in 6.1a): Google unary has look-ahead 2 (R2), emits chunks without copying (R3), maps the error frame with `MapError` (R5), retries per piece (R6a), counts `Requests` after the look-ahead wait (R6b), leaves `Seq` unset with a constant `Duration` (R6c), keeps a lone trailing byte (R6d), and drops a unit silently when `cfg.Client` is nil (`tts.go:313-317`; only `tts_test.go:300` builds one without a client and never posts a unit; the server always sets a client, `server/registry.go:327-331`, `registry_echo.go:38`). xAI unary already matches ttsunit: it sets `Seq` (`tts.go:428`), copies each chunk (`:423-424`), labels `"xai"` and reports usage `Model: cfg.Voice` — i.e. `ttsunit.Config{Provider: "xai", Model: cfg.Voice}`; it synthesizes one unit at a time (one `SerialQueue`).
5. **D2 tests touching processor internals:** `git grep` for `Requests./Errors./Retries./Stale./cancels/finishedSeq/nextSeq/queues/waitSeq/ttsLookAhead` in google/xai TTS tests → only `providers/google/tts_handoff_test.go:73 proc.Errors.Load()`; xAI tests use `ErrorsTotal`/`AudioFrames` (`tts_handoff_test.go:83`, `tts_test.go:135`, `task9_round5_probe_test.go:130`), which ttsunit's embedded `Runner` exports under the same names. Unary constructor callers: 3 examples, 9 google + 6 xai test files.
6. **D6 SSRF rule duplicated verbatim:** `providers/openai/dial.go:18-35` (18 lines) ≡ `providers/nvidia/dial.go:163-181` (19; comment "separate module, so copied"). Tests: `providers/openai/llm_test.go:302` (range table), `providers/nvidia/stt_test.go:272` (`dialAllowed`), `:382`.
7. **D6 warm loop:** `providers/google/warm.go:29-53` vs `providers/nvidia/dial.go:137-161`: 25 lines each, 14 differing lines after renaming (`diff | grep -c '^[<>]'`), both take a `*grpc.ClientConn`.
8. **D5:** `providers/xai/sse.go` (168 lines, typed `event:`/`data:` SSE with xAI sentinels `ErrOversized*`) vs `providers/openai/llm.go:567-629` (inline `bufio.Scanner` over `data:` lines, chat-completions); tool-call assembly: openai `callAcc` by `index` (`llm.go:484-489,604-618,640-657`) vs xAI Responses output items by `output_index`/`item_id` (`tools.go:157-302`); history: openai folds calls into one assistant message (`llm.go:292-334`), xAI emits `function_call` items (`llm.go:449-474`); tool result: openai fallback `string(r.Status)` + blacklist sanitizer (`llm.go:272-290`) vs xAI fallback `tool_failed`/`cancelled` + whitelist (`tools.go:58-92`).
9. **D8 failover scaffolding:** proxy session types `core/llm_failover.go:117-126`, `stt_failover.go:158-166`, `tts_failover.go:128-136` (identical bodies) + identical `Attach` loops (`llm :128-138`, `stt :168-178`, `tts :138-148`); `classOf` identical ×3 (`llm :430-435`, `stt :367-372`, `tts :524-529`); `noteFailover` ×3 (`llm :421-428`, `stt :528-536`, `tts :506-522`), differing only in debug name and turn/gen source. `*Counts` differ in arity (`STTCounts` 3 values, LLM/TTS 4): left alone.
10. **D8 clock seams:** `turn.Clock`/`turn.Timer` (`core/turn/processor.go:16-31`) ≡ `transcript.PolicyClock`/`PolicyTimer` (`core/transcript/processor.go:13-31`) (Now + `NewTimer(d)`); `IdleClock` (`core/idle.go:27-48`) uses `TimerAt(abs)` (fake clocks arm against their own now). Fakes in 9 test files (`core/turn/{commit,mute,processor,stop}_test.go`, `core/transcript/{processor,task9_review_probe,task9_round3_probe,task9_round4_probe}_test.go`, `smartturn/analyzer_test.go`, `tools/compat/parity_fixtures_test.go`). Server sets none.
11. **`alreadyAsync`** `core/session.go:279-286`: `SetTracer` callers pass `*AsyncTracer` nowhere (`git grep "SetTracer("`: server passes the otel session tracer, tests pass recorders), so the branch is never taken and `ownsTraceQueue` (`:134,285,670`) is always true when `traceQueue != nil`.
12. **D9:** `IdleController.say` called from `idle.go:346` (bot fallback after the interruption returns), `:489` (goodbye), `:529` (`prompt_mode: say`). Server wires the TTS index at `server/registry.go:854-855`; `server/idle_test.go:347-389` pins it. `AssistantContextProcessor.Say` is used by the greeting (`server/registry.go:790-797`). Synthetic-notice paths: `idle.go:532-543` and `turn_completion.go:287-298` share 5 lines; `async_trigger.go:369` has no history write.
13. **D3/D4 tools:** identical `randHex` (`tools/loadtest/server.go:29-33` ≡ `tools/livecheck/server.go:81-85`), `freePorts` (`:38-50` ≡ `:87-98`); spawn + ready-poll (`loadtest :166-202` vs `livecheck :218-253`: same `Setpgid`, `GOVOICE_ROOT/DEV_DIR/MANAGEMENT_API_KEY/TOKEN_SECRET/ENV_FILE=/dev/null`, 60 s `/ready` poll); `Stop` (`:254-266` ≡ `:255-269` + data dir removal); management call (`:269-289` ≡ `:271-294`); token (`:307-318` ≡ `:312-323`): ~100 lines per side. Env differs on purpose: livecheck passes an allowlist (`childEnv`, `:191-199`), loadtest the full environ. RTVI handshake (dial, `client-ready`, wait `bot-ready`): `tools/loadtest/client.go:224-262` vs `tools/livecheck/rtvi.go:39-77`; the readers differ (atomics/channels vs an event log).
14. **D10:** WAV readers `tools/loadtest/audio.go:37-66` (strict chunk walk, PCM16 mono 16 k), `tools/livecheck/clips.go:63-76` (finds `"data"` anywhere, rate at fixed offset 24), `tools/provtest/wav.go:136-186` (strict, also IMA ADPCM). `tools/provtest` imports `govoice/server` (so loadtest must not import provtest). Markdown tables are inline `Fprintf` strings (6 sites), no renderer to share.
15. **D11:** request-id idiom (`RequestIDFromContext`, `uuid.NewString()` fallback, `X-Request-ID`) ×5: `server/api/flows.go:35-39`, `server/api/providers.go:46-50`, `server/api/traces.go:141-145`, `server/mcp/http.go:286-290`, `server/probe_routes.go:57-61`. Prefix ladder: 4 identical 9-line blocks in `server/api_routes.go:132-176` (flows, providers, mcp-servers, sessions). UI: `ui/src/routes/dashboard/DashboardView.tsx:45` key `["operations-status"]` and `ui/src/routes/server/ServerView.tsx:30` key `["server-operations-status"]`, same `client.getOperationsStatus()`.
16. **D7 already done:** `PublishVersion`/`mcpReferenceCounts` gone (Phase 1.1); `SetMCPTools` remains as an `Update` wrapper used by tests (Phase 5 ruling).
17. **Gate infrastructure:** `make help` lists `test`, `verify`, `test-e2e-go`, `e2e-echo`, `provider-matrix-dry`, `loadtest-smoke`, `live-check-selftest`, `perf-gate`, `test-postgres`. `hw.ncpu` 12 → perf label `darwin-arm64-12c`; baseline files exist (committed 2026-10-13). Offline stack up: `docker ps` shows `offline-adapter-1`, `offline-speaches-1`; `curl -fsS http://127.0.0.1:8099/healthz` OK. `benchstat` not installed (use `go run golang.org/x/perf/cmd/benchstat@latest`, as Phase 5 used `go run …/deadcode@latest`).

---

## Global Constraints

Inherited from `AGENTS.md` and the master plan.

- `core/` never imports `server/` or a provider; `core/go.mod` gets no requirement. Everything added to `core/` is consumed by the server on a call path (`ttsunit.LookAhead` by Google unary TTS; `netpool.AllowedIP` by openai/nvidia, which the server builds).
- Every queue/slice bounded (ttsunit look-ahead holds at most `LookAhead+1` units of audio, as Google does today); goroutines only via `session.Go`/`SerialQueue`; no `context.Context` in new struct fields; no lock across I/O; monotonic time for durations.
- Hot path: no new per-frame allocation (R3; benchmarks in 6.0/6.1b/6.1e).
- Errors wrapped `%w`, inspected with `errors.Is/As`; transport writers/provider callbacks keep their `recover()`.
- `go test -race` in every module (`make test`), `gofmt -l` empty, `go vet` in all three tag sets.
- Conventional commits, one logical change each; trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Provider secrets: environment only; never write keys. Never run paid/live targets (`make e2e`, `e2e-xai`, `live-check`, `live-critical`, `agent-matrix`, `provider-matrix`, or `-tags integration/live` **tests**; compiling them is fine). Never `offline-up`/`offline-models`/`offline-down`.
- Never kill processes by pattern; leave a developer's `make dev`/`bin/server` alone. Scratch files go under the session scratchpad or `.superpowers/` (git-ignored), never `/tmp/<generic-name>`.
- A removed symbol takes its doc comment, its tests that only exercise it, and its mentions in `docs/` (outside `docs/superpowers/`), `examples/`, `tools/`.

### Gate block (after EVERY task; substitute the task id for `6.X`)

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
B=$(cat .superpowers/sdd/2026-10-15-review-remediation/p6/base-6.X)   # recorded at task start: git rev-parse HEAD
gofmt -l $(git ls-files '*.go' | grep -v '^pipecat-impl/') | tee "$L/6.X-gofmt.log"; test ! -s "$L/6.X-gofmt.log"
make verify 2>&1 | tee "$L/6.X-verify.log"
make e2e-echo 2>&1 | tee "$L/6.X-e2e-echo.log"
make provider-matrix-dry 2>&1 | tee "$L/6.X-provider-matrix-dry.log"
# when tools/, transport/ or a TTS/STT path changed (6.1*, 6.4, 6.7, 6.8):
make loadtest-smoke 2>&1 | tee "$L/6.X-loadtest-smoke.log"
# when tools/livecheck changed (6.4, 6.8):
make live-check-selftest 2>&1 | tee "$L/6.X-live-check-selftest.log"
# 6.1e and 6.8 (R7):
make perf-gate 2>&1 | tee "$L/6.X-perf-gate.log"
# 6.8 only (server touched in 6.5/6.6):
make test-postgres 2>&1 | tee "$L/6.X-test-postgres.log"
# last, so a down stack skips only this target (report it; never start the stack):
if curl -fsS http://127.0.0.1:8099/healthz >/dev/null; then make offline-test 2>&1 | tee "$L/6.X-offline-test.log"; else echo "offline stack is down: offline-test NOT run, report it"; fi
git diff --stat "$B"..HEAD | tail -1 | tee "$L/6.X-diffstat.log"                       # line delta, all files
git diff --stat "$B"..HEAD -- '*.go' ':!*_test.go' | tail -1 | tee -a "$L/6.X-diffstat.log"   # non-test Go
```

Every gate line runs for real (the design review could not run the Playwright half of `e2e-echo`, `offline-test`, `test-postgres` or `live-check-selftest`; the implementer must). Expected: `=== All verify checks passed ===` (the recorded `PLAYBACK_CLEAR_REQUIREMENT_FAILED … relax-unmodified-client-playback-clear` line is expected); `make e2e-echo` exits 0, every Playwright test passed; `provider-matrix-dry` exits 0; `loadtest-smoke` exits 0 (`Verdict: PASS`); `live-check-selftest` exits 0; `perf-gate: PASS (darwin-arm64-12c)` — or, if 6.0 recorded a FAIL on base, the same rows failing by no more than base (R7); `test-postgres` exits 0; `offline-test` prints `ok` for both packages. On failure: name the failing test (`grep -nE -- '--- FAIL|^FAIL|✘' <log>`), keep the log, never overwrite it with a rerun. Known intermittent `TestOfflineStackLongCallerSpeechIsOneTurn`: rerun alone 5× (Phase 4 command) and report every result. Run on an otherwise idle machine (overlapping runs caused a load flake before).

### Per-commit compile check (before every commit)

```bash
for t in "" xaispeech "xaispeech,offlinestack,integration,live"; do for m in . core server providers/cartesia providers/deepgram providers/echo providers/elevenlabs providers/google providers/nvidia providers/openai providers/xai offline offline/contract; do (cd $m && go build -tags "$t" ./... && go vet -tags "$t" ./...) || echo "FAIL $m tags=$t"; done; done
```

Expected: no `FAIL` line. (`go vet` compiles `integration`/`live` test files; it runs nothing.)

**Rollback (every task):** `git revert` the task's commits newest first; no schema, wire or stored-data change in this phase (6.6 changes what new calls store, not the schema).

---

## Task 6.0 — Safety net and baselines (no product change)

**Files (local, git-ignored):** `.superpowers/sdd/2026-10-15-review-remediation/p6/{base-6.0,linecounts-before.txt,cover-*.txt,bench-*-before.txt,flush-stress-before.log}`, `flake/6.0-perf-gate.log`.

- [ ] **Step 1: base + line counts.** `P=.superpowers/sdd/2026-10-15-review-remediation/p6; mkdir -p $P; git rev-parse HEAD > $P/base-6.0; wc -l <the files of fact 2> > $P/linecounts-before.txt`. Expected: the numbers of fact 2.
- [ ] **Step 2: coverage of the two unary schedulers.**
```bash
(cd providers/google && go test -race -count=1 -coverprofile=$OLDPWD/$P/cover-google.out -run 'TTS|Tts' . && go tool cover -func=$OLDPWD/$P/cover-google.out | grep -E 'tts.go' > $OLDPWD/$P/cover-google.txt)
(cd providers/xai && go test -race -count=1 -tags xaispeech -coverprofile=$OLDPWD/$P/cover-xai.out -run 'TTS|Tts' . && go tool cover -func=$OLDPWD/$P/cover-xai.out | grep -E '/tts.go' > $OLDPWD/$P/cover-xai.txt)
grep -E 'providers/google/tts.go:(2[89][0-9]|[3-5][0-9][0-9]|60[0-2])\.' $P/cover-google.out | awk '$NF==0' > $P/uncovered-google.txt   # zero-count blocks in handleSynthesisUnit..abandon
grep -E 'providers/xai/tts.go:(2[7-9][0-9]|[3-5][0-9][0-9])\.' $P/cover-xai.out | awk '$NF==0' > $P/uncovered-xai.txt          # synthesizeUnit..abandon
```
Record which of these behaviours have **no** covering test (each becomes a 6.1a characterization test): error frame for a client error / a deadline / a breaker-open refusal; barge-in mid-emission stops audio; gen-0 unit after a generation bump plays (google covered: `tts_test.go:320-385`); empty audio → one empty final frame; odd byte count; refused task (`ErrTooManyTasks`) → error frame, later units not wedged (google covered: `tts_budget_test.go`); Close during look-ahead wait; stale unit queued behind a slow one keeps order.
- [ ] **Step 3: benchmarks before.** Write (uncommitted now; committed in 6.1a) `providers/google/tts_bench_test.go` `BenchmarkUnaryUnit`: one session with `NewTTSProcessor` and a mock client returning 1 s of PCM16 at 24 kHz; each iteration posts one unit and waits for its `TTSStoppedFrame`; `b.ReportAllocs()`. Same for xAI in `providers/xai/tts_bench_test.go` (`//go:build xaispeech`). Run `(cd providers/google && go test -run '^$' -bench BenchmarkUnaryUnit -benchmem -count=10 .) > $P/bench-google-before.txt` and the xAI one with `-tags xaispeech` > `$P/bench-xai-before.txt`.
- [ ] **Step 4: latent flake stress.** `(cd core && go test -race -count=200 -run TestFlushStaleInterruptionCannotCancelCurrentTurn .) 2>&1 | tee $P/flush-stress-before.log` → expected `ok`; then the whole core suite alone, `(cd core && go test -race -count=3 ./...)` → `ok`. Record both.
- [ ] **Step 5: perf base (R7).** `make perf-gate 2>&1 | tee .superpowers/sdd/2026-10-15-review-remediation/flake/6.0-perf-gate.log`; record PASS/FAIL and the table. Also `make verify` and `make e2e-echo` on base (gate block without diffstat) so later failures are attributable.
- [ ] No commit (ledger only). Ledger line: base sha, counts, coverage gaps found, bench medians, flake stress result, perf-gate result.

---

## Task 6.1 — Unary TTS onto `ttsunit` (D2)

### 6.1a — Characterization tests (green on base)

**Files:** `providers/google/tts_characterization_test.go`, `providers/xai/tts_characterization_test.go` (`//go:build xaispeech` like the other xAI TTS tests — check `head -1 providers/xai/tts_test.go` and copy its tag line), the two `tts_bench_test.go` from 6.0 Step 3.

- [ ] **Step 1:** for every gap recorded in 6.0 Step 2, one table-driven test against the **public** surface only (`NewTTSProcessor`, `core.NewSession`, `Post`, `OnOutput`), never internals, so it survives the rewrite unchanged. Required rows (both providers unless noted):
  - client error kinds → the `ErrorFrame` (`Fatal == false`, `UnitID` = the unit) and the R5 tuple (`errors.Is` timeout / circuit-open / canceled, `classify(ef.Err)`); kinds: `&ProviderError{Retryable:false}`, `context.DeadlineExceeded`, `errors.New("x")`, breaker open (`resilience.NewBreaker` tripped first);
  - frames per unit for 3 sizes (0 bytes, 1 odd byte past a whole chunk, exactly 3 chunks): sequence `TTSStarted → OutputAudio… (exactly one IsFinal, the last; TurnFinal == unit.IsFinal) → TTSStopped → UsageMetrics`; total **sample-aligned** bytes (`total &^ 1`, R6d: the per-frame split of the tail and the lone byte are not asserted); no frame after `TTSStopped`;
  - barge-in (`InterruptionFrame`) while a unit is mid-emission: no `OutputAudioFrame` of that unit after the interruption is processed;
  - Google only: a unit queued behind a slow unit that becomes stale still lets the unit after it play, in order (look-ahead + stale);
  - Close closes the client: a mock client with a `Close() error` method (counting calls); `proc.Close()` closes it exactly once (Google; today `tts.go:211-222`). xAI: no row — its unary `Close` (`xai/tts.go:233-242`) never closed the client (the HTTP client comes from the shared `ttsPool()` netpool, `:59-63`), and the port keeps it that way;
  - Close while a unit waits for its turn: returns promptly (`< 1 s`), no goroutine left (`goleak` is not a dependency: compare `runtime.NumGoroutine()` before/after with a 1 s settle loop, as `providers/*/stt_test.go` leak checks do — grep `NumGoroutine` and copy the helper).
- [ ] **Step 2:** `cd providers/google && go test -race -count=5 -run 'Characterization|TTS' .` and the xAI equivalent with `-tags xaispeech` → all pass on base.
- [ ] **Step 3 (R6 check):** `git grep -nE 'Seq|Duration|Requests\.Load|TTSCounts' -- providers/google/*tts*_test.go providers/xai/*tts*_test.go` → expected: no assertion on unary `Seq`, chunk `Duration`, or the request count. If one exists, stop and report.
- [ ] **Commit:** `test(tts): characterize the unary Google and xAI schedulers before moving them onto ttsunit`. **Gate:** `make verify` only (tests only).

### 6.1b — `ttsunit` emits chunks without copying (R3)

**Files:** `core/ttsunit/ttsunit.go` (`Unit.Write`, `Unit.emit`), `core/ttsunit/ttsunit_test.go` (or a new `alloc_test.go`).

- [ ] **Step 1 (failing test first):** internal test (package `ttsunit`) with a stub `core.SessionController` (embed the interface; `Reenter` returns nil, `CurrentGeneration` 1, `ID` 1) and a no-op `core.Span` on the `Unit` (`emit` calls `u.span.AddEvent` on the first audio; a nil span panics). Body: a fresh `Unit` writes 1 s of 24 kHz PCM16 (48 000 bytes = 50 chunks) in 4 provider-sized pieces, then `Finish`. `BenchmarkUnitWrite` runs it with `b.ReportAllocs()`; `TestUnitEmitAllocatesOncePerFrame` asserts `testing.AllocsPerRun(50, body) <= 50+8` (one `OutputAudioFrame` per chunk; 8 covers buffer growth and the started/stopped/usage frames — confirm the slack against the base benchmark, which shows ≈ 2 per chunk, and tighten it to the measured after-value). Red on base. Guard (passes before and after): `TestUnitFramesDoNotShareCapacity` — every emitted `Samples` has `cap == len`, and `append` to frame 0's `Samples` leaves frame 1's bytes unchanged.
- [ ] **Step 2:** in `emit`, drop `pcm = append([]byte(nil), pcm...)`; callers pass `u.buf[:n:n]` (`Write`) and the tail `u.buf[:len(u.buf):len(u.buf)]` (`Finish`). Keep the odd-byte trim. `TestUnitEmitAllocatesOncePerFrame` now passes.
- [ ] **Step 3:** `cd core && go test -race -count=3 ./ttsunit/` and every streaming provider: `for m in providers/cartesia providers/elevenlabs providers/google providers/nvidia; do (cd $m && go test -race -count=2 ./...); done; (cd providers/xai && go test -race -count=2 -tags xaispeech ./...)` → `ok`. Bench: `go test -run '^$' -bench BenchmarkUnitWrite -benchmem -count=10 ./ttsunit/ | tee $P/bench-ttsunit-after.txt` and benchstat against the same benchmark run on the 6.1a commit: allocs/op drop by ≈ one per chunk (≈ 50 per 1 s unit).
- [ ] **Commit:** `perf(ttsunit): emit 20 ms chunks without copying them`. **Gate** (+`loadtest-smoke`).

### 6.1c — `ttsunit` look-ahead mode (R2)

**Files:** `core/ttsunit/ttsunit.go`, `core/ttsunit/lookahead_test.go` (new).

Interface (the only new API):
```go
type Config struct {
	// … existing fields …
	// LookAhead lets up to this many units past the one playing run Attempt early
	// (unary providers: synthesis overlaps playback). Until its turn a unit's Write
	// only buffers and Finish is deferred, so an early attempt may still retry.
	// 0: Attempt runs on the unit's turn (streaming providers).
	LookAhead int
}
```
Runner: `queue core.SerialQueue` → `queues []core.SerialQueue` sized `LookAhead+1` in `New`; unit `seq` runs on `queues[seq%len]` (as `google/tts.go:153-156`). Task body when `LookAhead > 0`: (1) wait until `seq <= finishedSeq+1+LookAhead` (or cancel/close); (2) unless stale, `resilience.Execute(Attempt)` with `u.held = true` (`Write` appends to `u.buf` without emitting; `Finish` sets `u.finishPending`); (3) **always** wait for the unit's turn (`finishedSeq+1 == seq`) — a stale or failed unit must still release its slot in order, else the next unit wedges; (4) stale → `StaleTotal`, return; error → the existing error path; else `u.held = false`, emit the buffered whole chunks, then `Finish` if pending. `LookAhead == 0` keeps today's code path untouched (same order of wait → stale check → Execute).
Bound: at most `LookAhead+1` units hold audio (the one playing plus the early ones), equal to Google today. Update the package doc (`ttsunit.go:1-4`, "shared by streaming TTS providers") to say unary providers use it too.

- [ ] **Step 1 (failing tests):** `TestRunnerLookAheadSynthesizesAheadInOrder` (port of `providers/google/tts_lookahead_test.go:14-…` onto a ttsunit `Runner` with `LookAhead: 2`: unit 2 and 3 attempt while unit 1 blocks, unit 4 does not; output order 1,2,3,4); `TestRunnerLookAheadStaleUnitKeepsOrder`; `TestRunnerLookAheadRetriesBeforeItsTurn` (attempt 0 fails retryably while held, attempt 1 succeeds, audio emitted once, `RetriesTotal == 1`); `TestRunnerLookAheadCloseWhileWaiting` (returns, no leak). They fail to compile on base (no field) — that is the red.
- [ ] **Step 2:** implement; `cd core && go test -race -count=20 ./ttsunit/` → `ok`. All existing ttsunit tests (`ttsunit_test.go`, `ttsunit_handoff_test.go`, `budget_test.go`, and the A12 abandon tests from Phase 4.5) unchanged and green — they prove `LookAhead: 0` equivalence; plus the streaming providers as in 6.1b Step 3.
- [ ] **Commit:** `feat(ttsunit): optional look-ahead so unary providers synthesize while earlier units play`. **Gate.**

### 6.1d — xAI unary on `ttsunit`

**Files:** `providers/xai/tts.go` (`//go:build xaispeech`, like all xAI speech files: the plain-tag compile does not see it, the per-commit check's `xaispeech` sets do).

- [ ] **Step 1:** replace `TTSProcessor` (`tts.go:150-542`: `ttsCancel`, struct, `TTSCounts`, `Attach`, `Close`, `cancelAllLocked`, `ProcessFrame`, `synthesizeUnit`, `abandon`) with
```go
type TTSProcessor struct {
	*ttsunit.Runner
	cfg    TTSConfig
	client TTSClient
}
// NewTTSProcessor: cfg = normalizeTTSConfig(cfg); client default as today; then
// p.Runner = ttsunit.New(ttsunit.Config{Provider: "xai", Model: cfg.Voice, SampleRate: cfg.SampleRate,
//	Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify, Attempt: p.attempt})
func (p *TTSProcessor) attempt(ctx context.Context, u *ttsunit.Unit, _ int) error {
	pcm, err := p.client.Synthesize(ctx, u.Frame.Text, p.cfg)
	if err != nil {
		return err
	}
	if err := u.Write(pcm); err != nil {
		return err
	}
	return u.Finish()
}
```
Keep `Warm` (`tts.go:66-75`) working on the new struct. `TTSCounts`, `RequestsTotal…StaleTotal`, `AudioFrames/AudioBytes` come from the embedded `Runner` (same names, fact 5).
- [ ] **Step 2:** `cd providers/xai && go test -race -count=5 -tags xaispeech ./...` → `ok`, **no test file changed** (`git diff --stat -- providers/xai/*_test.go` empty). `go test -run '^$' -bench BenchmarkUnaryUnit -benchmem -count=10 -tags xaispeech . | tee $P/bench-xai-after.txt`; benchstat vs before: allocs/op not higher, ns/op within noise.
- [ ] **Step 3 (real-stack coverage):** xAI unary has no free e2e path (the server builds it only with `xai_tts.disable_streaming`, a paid path; `offline.tts` builds `NewStreamingTTSProcessor`, `server/registry_offline_speech.go:53`). Add `offline/contract/tts_unary_test.go` (`//go:build xaispeech && offlinestack`, skip without `OFFLINE_ADAPTER_URL` like `stack_test.go:20-22`): `xai.NewTTSProcessor` with `Endpoint: base+"/v1/tts"`, `SampleRate: 16000` (as `stack_test.go:26-28`) in a `core.Session`; post one unit; expect `TTSStarted`, ≥1 s of sample-aligned audio, exactly one final frame, `TTSStopped`, a usage frame; no `ErrorFrame`. Write it **before** Step 1 and run it on base (`make offline-test` → `ok`), then again after. `make offline-test` already runs `offline/contract` with these tags.
- [ ] **Commit:** `refactor(xai): unary TTS runs on ttsunit` (the offline test as its own preceding `test(offline): …` commit). **Gate** (+`loadtest-smoke`; `offline-test` is the only real-stack proof of this path — if the stack is down, 6.1d is not done). Expected non-test delta ≈ −360.

### 6.1e — Google unary on `ttsunit` (look-ahead 2)

**Files:** `providers/google/tts.go`, `providers/google/warm.go` (the `TTSProcessor.Warm` receiver), `providers/google/tts_handoff_test.go:73` (R4 rename only).

- [ ] **Step 1:** replace `tts.go:141-253` and `:281-602` with a struct embedding `*ttsunit.Runner` (fields `cfg`, `client`), `NewTTSProcessor` building `ttsunit.Config{Provider: cfg.Provider, Model: cfg.Voice, SampleRate: cfg.SampleRate, Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify, Attempt: p.attempt, LookAhead: ttsLookAhead}` (keep `ttsLookAhead = 2` and its comment), and
```go
// attempt synthesizes the unit's pieces with the unary request and plays them as one unit.
func (p *TTSProcessor) attempt(ctx context.Context, u *ttsunit.Unit, _ int) error {
	for _, piece := range splitUnit(u.Frame.Text, p.cfg.MaxSynthesisUnitChars) {
		pcm, err := p.cfg.Client.SynthesizeSpeech(ctx, piece, p.cfg.Voice, p.cfg.SampleRate)
		if err != nil {
			return MapError("tts", err) // R5: error frames keep carrying *ProviderError
		}
		if err := u.Write(pcm); err != nil {
			return err
		}
	}
	return u.Finish()
}
```
Keep `splitUnit`, `appendPiece`, `normalizeTTSConfig`, `ttsRetryPolicy`, the client and `stripWAV`. **Keep closing the client** (today `tts.go:211-222`; the server builds one gRPC client per call stage, `server/registry.go:323-331`, so a promoted `Runner.Close` alone would leak one connection per call for non-Chirp voices, and no echo-based suite would notice — the echo client has no `Close`):
```go
func (p *TTSProcessor) Close() error {
	err := p.Runner.Close()
	if c, ok := p.cfg.Client.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return err
}
```
(same as `tts_streaming.go:118-123`). Checked the other embeddings of this phase: xAI unary's `Close` never closed its pooled client (keep promoted `Runner.Close`); 6.3/6.6/6.7 embed nothing new. Nil `cfg.Client` (fact 4): the attempt starts with `if p.cfg.Client == nil { return nil }` — no write, no `Finish`, so no frame is emitted, as today (`ponytail:` comment naming it a construction-without-client guard for the flush-ack test).
- [ ] **Step 2:** `tts_handoff_test.go:73`: `proc.Errors.Load()` → `proc.ErrorsTotal.Load()` (R4). No other test edit. `cd providers/google && go test -race -count=5 ./...` → `ok` incl. `tts_lookahead_test.go`, `tts_budget_test.go`, `tts_test.go` (gen-0), `provider_label_test.go`, `trace_lifecycle_test.go`, the 6.1a characterization tests. Bench vs 6.0: allocs/op and B/op within ±1% (measured by the design review: 155 allocs / 64.9 KB base vs 155–156 / 65.1 KB port — neutral, not lower), ns/op within noise.
- [ ] **Step 3:** server and free suites prove the call path (echo TTS = this processor): `cd server && go test -race -count=1 -tags xaispeech ./...`, then the **full** gate incl. `loadtest-smoke` and `perf-gate` (R7) — v2v/turn latency must not regress beyond base.
- [ ] **Commit:** `refactor(google): unary TTS runs on ttsunit with look-ahead`. **Gate** (+`loadtest-smoke`, `perf-gate`). Expected non-test delta ≈ −370 (design review measured `providers/google/tts.go` 608 → 226, `providers/xai/tts.go` 542 → 199; `perf-gate` PASS on both base and port, v2v p95 1003.6 vs 995.3 ms); whole 6.1 ≈ −690 non-test, +~250 test.

---

## Task 6.2 — SSRF IP rule in `core/netpool` (D6, SSRF part only)

**Files:** `core/netpool/netpool.go` (or new `core/netpool/ssrf.go`), `core/netpool/ssrf_test.go`, `providers/openai/dial.go`, `providers/nvidia/dial.go`, `providers/openai/llm_test.go`, `providers/nvidia/stt_test.go`.

```go
// AllowedIP is the dial-time SSRF rule for operator-supplied endpoints: link-local
// (incl. 169.254.169.254 and AWS IMDS fd00:ec2::254), multicast and unspecified are
// always refused; loopback, private and CGNAT (100.64/10) only with allowPrivate.
func AllowedIP(ip netip.Addr, allowPrivate bool) bool
```
- [ ] **Step 1:** move the range table test from `providers/openai/llm_test.go:~290-306` to `core/netpool/ssrf_test.go` as `TestAllowedIP` calling `netpool.AllowedIP` (red: undefined). Add the rows nvidia's test relies on (`stt_test.go:382` loop).
- [ ] **Step 2:** move the function + `metadataV6` + `cgnat` into netpool; delete both provider copies; `openai.guardedPool` and `nvidia.dialAllowed` call `netpool.AllowedIP`. R4 mechanical test edits: `providers/nvidia/stt_test.go:382` `allowedIP(` → `netpool.AllowedIP(`; `dialAllowed` stays in nvidia (plaintext rule is nvidia-only).
- [ ] **Step 3:** `cd core && go test -race ./netpool/`; `cd providers/openai && go test -race ./...`; `cd providers/nvidia && go test -race ./...` → `ok`.
Coverage note: unit tests only (openai/nvidia SSRF are not on a free e2e path besides the server build); acceptable — the rule is a pure function with a moved table.
- [ ] **Commit:** `refactor(netpool): one SSRF dial rule for openai and nvidia`. **Gate.** Expected ≈ −15 non-test (one copy of 18 lines, imports).

---

## Task 6.3 — Failover helpers and the dead tracer branch (D8 part, §E `alreadyAsync`)

**Files:** `core/failover.go` (new, small), `core/llm_failover.go`, `core/stt_failover.go`, `core/tts_failover.go`, `core/session.go`.

```go
// candidateSession shows a failover candidate the real session except for Reenter.
type candidateSession struct {
	SessionController
	idx  int
	from func(idx int, f Frame, dir Direction, resume int, gen uint64) error
}
func (c candidateSession) Reenter(f Frame, dir Direction, resume int, gen uint64) error { return c.from(c.idx, f, dir, resume, gen) }

// attachCandidates attaches every candidate processor through a candidateSession.
func attachCandidates(s SessionController, index, n int, proc func(i int) Processor, from func(int, Frame, Direction, int, uint64) error) error

// failoverClass is classify(err), or "" without a classifier.
func failoverClass(classify func(error) string, err error) string

// noteFailover reports one switch to the OnFailover hook and as a debug event.
func noteFailover(s SessionController, on func(from, to, class string), event, from, to, class string, turn TurnID, gen Generation)
```
- [ ] **Step 1:** replace the three proxy types + `Attach` loops, the three `classOf`, and the three `noteFailover` bodies with calls (keep each wrapper's method names if that keeps call sites unchanged; the TTS one keeps its `nil`-note and nil-unit guards at the call site). Debug event names (`DebugLLMFailover/STT/TTS`) and attribute keys stay identical.
- [ ] **Step 2:** `core/session.go:278-288`: always wrap a non-noop tracer: `s.traceQueue = NewAsyncTracer(tracer, defaultTraceQueueSize); s.tracer = s.traceQueue`; delete `ownsTraceQueue` (`:134`) and its check at `:670` (close `traceQueue` when non-nil).
- [ ] **Step 3 (equivalence):** unchanged tests: `cd core && go test -race -count=3 ./...` (failover suites `llm_failover_test.go`, `stt_failover_test.go`, `tts_failover_test.go`, `runtime_trace_test.go`, `session_teardown_test.go`); `cd server && go test -race -count=1 -run 'Failover|Trace|Redaction' -tags xaispeech .` → `ok`.
- [ ] **Commit(s):** `refactor(core): share failover candidate attach, classification and switch reporting`; `refactor(core): drop the never-taken already-async tracer branch`. **Gate.** Expected ≈ −50 non-test.

---

## Task 6.4 — `tools/` de-duplication (D3, D4 handshake, D10 WAV)

**Files:** `tools/internal/stack/stack.go` (new), `tools/internal/wav/wav.go` (= `git mv tools/provtest/wav.go`), `tools/loadtest/{server.go,client.go,audio.go}`, `tools/livecheck/{server.go,rtvi.go,clips.go}`, `tools/provtest/*` (callers of the moved WAV functions).

```go
package stack // spawns and drives a server child for the free harnesses (loadtest, livecheck)

type Spec struct {
	Root, Bin, Dir string                  // repo root, server binary, per-run work dir (config + data + log)
	Config         func(pub, adm int) []byte // server YAML on the chosen ports
	Env            []string                // child environment besides the generated secrets (caller decides: allowlist or environ)
	Args           []string                // extra server flags (loadtest: -vad-mode)
}
type Server struct{ Public, Admin, Key, Log, Data string /* + cmd, done */ }
func Start(ctx context.Context, sp Spec) (*Server, error) // freePorts(2), write config, Setpgid spawn, 60 s /ready poll
func (s *Server) Stop()                                    // SIGTERM group, 15 s, SIGKILL group, wait
func (s *Server) Call(method, path string, body any) (json.RawMessage, error) // management API, body never echoed
func (s *Server) Token(principal, flowID string) (string, error)
func RandHex(n int) string
func Handshake(ctx context.Context, conn *websocket.Conn, pushToTalk bool) error // client-ready, wait bot-ready
```
Callers keep their own concerns: loadtest `ConnectTarget`, proc stats, Prometheus parsing; livecheck `childEnv` allowlist (passed as `Env`), redactor registration of `Key`, data-dir removal after `Stop` (`os.RemoveAll(s.Data)` in its wrapper), its metric scrapes; loadtest's rejected/dial/handshake failure counters stay in `runCall` around `Handshake`.

- [ ] **Step 1:** create `stack` with a unit test `tools/internal/stack/stack_test.go` for `RandHex` length and `Handshake` against an `httptest` WebSocket server that answers `bot-ready` (no server build in the unit test).
- [ ] **Step 2:** loadtest adopts it (delete `randHex`, `freePorts`, spawn/poll, `Stop`, `api`, `Token` bodies; `StartServer` builds `Spec`); `go test ./tools/loadtest/` → `ok`; `make loadtest-smoke` → PASS.
- [ ] **Step 3:** livecheck adopts it; `go test ./tools/livecheck/` → `ok`; `make live-check-selftest` → exit 0.
- [ ] **Step 4:** `git mv tools/provtest/wav.go tools/internal/wav/wav.go` (package `wav`; exported names unchanged); provtest imports it (`wav.ReadWAV`, `wav.EncodePCMWAV`, …). R4 mechanical edits: `tools/provtest/provtest_test.go` calls `ReadWAV`/`EncodeIMAWAV`/`EncodePCMWAV`/`RMS` unqualified (`:61-102,239,257-273`) and `:61` `wav := EncodeIMAWAV(...)` would shadow the package — move the WAV-only tests (`TestReadWAVRobust`, the IMA round trip at `:61-78,239`) to `tools/internal/wav/wav_test.go`, rename the local to `w`, qualify the rest (`wav.RMS`); loadtest `parseWAV` → `wav.ReadWAV` + its own 16 k/mono check; livecheck `loadWAV` → `wav.ReadWAV` + rate check (stricter than today's header-offset read: the repo fixture `fixtureQuestion` and synthesized `.pcm` clips are what the free targets load — `live-check-selftest` proves it). `go test ./tools/... ` → `ok`; `make provider-matrix-dry` → exit 0.
- [ ] **Commits:** `refactor(tools): one server-child harness for loadtest and livecheck`; `refactor(tools): one WAV reader`. **Gate** (+`loadtest-smoke`, `live-check-selftest`). Expected ≈ −130 non-test.

---

## Task 6.5 — Server small duplicates (D11, §E ladder)

**Files:** `server/api/api.go`, `server/api/{flows,providers,traces}.go`, `server/mcp/http.go`, `server/probe_routes.go`, `server/api_routes.go`, `ui/src/routes/server/ServerView.tsx`.

- [ ] **Step 1:** `api.RequestID(w http.ResponseWriter, r *http.Request) string` (context id, else a new UUID; sets `X-Request-ID`); the five sites call it.
- [ ] **Step 2a (characterize first):** `server/api_routes_table_test.go`, written against the **existing** ladder and green on base: rows for each of the four prefixes — exact path (`/api/v1/flows`), trailing slash/sub-path (`/api/v1/flows/x`), a lookalike (`/api/v1/flowsX` → 404 `not_found`), 503 with the prefix's code when its handler is nil; `/api/v1/sessions/x/run` and `/x/context` → the live-call handler (not the session API); `/api/v1/demo` → demo; method behaviour (405 + `Allow`) unchanged for one route per handler; `X-Request-ID` present.
- [ ] **Step 2b:** `api_routes.go:132-176`: the four storage-backed prefixes become one table loop (`prefix`, `handler`, `code`, `message`); same 503 bodies, same `WithRequestID`. **Order:** demo and `liveCallRoute` (`server/live_calls_api.go:14-24` matches `/api/v1/sessions/{id}/run|context`) are checked **before** the loop — the live-call routes must win over the `/api/v1/sessions` prefix; neither overlaps flows/providers/mcp-servers. Ruling: a table, not `http.ServeMux` — `ServeMux` would add path cleaning/301 redirects the API never had. Step 2a's test passes unchanged.
- [ ] **Step 3:** `ServerView.tsx:30` key → `["operations-status"]` (Dashboard keeps its `refetchInterval`; the Server page shares the cache). `cd ui && npx tsc --noEmit -p . && npx vitest run` → 0 errors, all pass.
- [ ] **Step 4 (equivalence):** `cd server && go test -race -count=1 -tags xaispeech ./...` unchanged (API tests assert status codes, error codes and `X-Request-ID`).
- [ ] **Commit:** `refactor(server): one request-id helper and a route table for the management prefixes`. **Gate.** Expected ≈ −35 non-test.

---

## Task 6.6 — Idle lines through `Say` (D9; behaviour; U1 = yes)

**Files:** `core/idle.go`, `core/idle_test.go`, `server/idle.go`, `server/registry.go:812-855`, `server/idle_test.go`, `server/call_behavior_e2e_test.go`, docs mentioning idle speech (`git grep -n "idle-say\|SpeakIndex" -- docs ':!docs/superpowers'`).

- [ ] **Step 1 (failing tests):** core `TestIdleGoodbye_AppearsInHistory` and `TestIdleSayPrompt_AppearsInHistory` (an `IdleController` + `AssistantContextProcessor` in a session with a coretest TTS; after the user timeout the history holds an assistant message with the prompt text, and after the goodbye one with the goodbye text); `TestIdleBotFallback_AppearsInHistory` (stuck model → interruption → fallback line in history **and its audio reaches the sink**: `Say` reads `CurrentGeneration` when called, so the test must prove the line is spoken only after the interruption has bumped the generation and passed the TTS stage — i.e. the TTS stage's interruption handling (`cancelAllLocked`) does not cancel the fallback; run it with a coretest TTS **and** a ttsunit-based TTS, `-count=50`); `TestIdleGoodbyeThenHangup_Order` (goodbye audio delivered, then the end request; no hang-up before `TTSStopped` of the goodbye or `GoodbyeWait`) — keep or extend the existing goodbye-ordering test if one exists (`git grep -n Goodbye core/idle_test.go`). Server e2e `TestCallBehaviorIdleLinesAreInTheCallsTurns` (name matches `test-e2e-go`'s `CallBehavior` filter): the quiet-flow call of `TestCallBehaviorAFlowsQuietCallerSettingsPromptThenEndTheCall` (`:61-82`), then the session's stored turns via the same API `call_records_e2e_test.go` reads contain the prompt and the goodbye as assistant turns, and the caller-turn count is unchanged. All fail on base.
- [ ] **Step 2:** `IdleConfig.Speak func(text string) error` (server sets `asstCtxProc.Say`; the name `Say` is taken: `IdleConfig.Say bool` = `prompt_mode: say`, `core/idle.go:62,497`, `server/idle.go:78`); `say` calls `Speak` (no-op when nil); `beginEndLocked` and the bot-fallback check use `c.cfg.Speak != nil` instead of `c.speak >= 0`; delete `speak`, `SetSpeakIndex`, `SpeakIndex`, the `NextUnitID` assertion and the `idle-say-` turn naming (Say turns are `say-N`; `IsSayTurn` already keeps them out of caller-turn counting in `server/server.go:660,668` and `server/recording.go:47`). Server: pass `asstCtxProc.Say` into `idleCfg.stage(...)`; delete `registry.go:854-855`; replace `server/idle_test.go:347-389` (`TestIdleSpeakIndexFollowsFrontInsertions`) with a check that the idle stage gets the assistant's `Say` (Phase 5 R6 "kept with reason" no longer applies).
- [ ] **Step 3:** `goodbye wait`: the controller waits for the goodbye's `TTSStarted`/playback (`endAt`, `sawStart`); `Say` produces the same frames through TTS, so `TestCallPipelineIdleUserSilenceEndsCall`, `TestIdleSilentCallerIsHungUpWithNormalClose`, `TestCallBehaviorAFlowsQuietCallerSettingsPromptThenEndTheCall` (`probe.spoke == 2`) stay unchanged and green. `e2e/tests-flows/38-call-behavior.spec.ts` must pass in `e2e-echo`.
- [ ] **Step 4 (privacy):** `cd server && go test -race -run 'Redaction|NoSecretLeaves|Privacy|Recording' -tags xaispeech .` → `ok`; confirm by reading `server/recording.go:47-55` that assistant say-turns pass `sidecarText` (redacted). Docs: `docs/privacy.md` (or the call-records doc) gains one line: idle prompts/goodbyes are stored as assistant turns like the greeting.
- [ ] **Commit:** `feat(idle): idle prompts, fallbacks and goodbyes are spoken as assistant turns (in history and call records)`. **Gate.** Expected ≈ −15 non-test, +~90 test. **Overlap note:** Phase 7.5 adds an `idle.user_timeout` e2e; this task's e2e asserts storage, not firing.

---

## Task 6.7 — Unbound final is not lost after adoption (ledger P2 Minor2; behaviour; U2 = yes)

**Files:** `core/sttunit/runner.go` (`stream`, `emit` `:694-720`, `beginTurn` `:263-282`), `core/sttunit/runner_test.go`.

- [ ] **Step 1 (failing test):** `TestRunner_UnboundTextThenSilentEndIsALoss`: fake Conn yields `Result{Text:"hello", Final:true}` while unbound; the turn start the runner binds on adopts the stream (drive it exactly as `TestRunner_UnboundStreamAdoptedByNextTurn`, `runner_test.go:418`, does); the turn's end → `Finish`; the Conn then ends (`ErrStreamEnded`) with no further result. Expect the A8 path (the runner's invalidation `ErrorFrame`/`TranscriptInvalidationFrame` for the turn, exactly once — copy the assertion helper of the existing drop tests). On base: nothing (quiet). Plus `TestRunner_UnboundRetractionClearsSeed`: unbound interim "uh" then unbound empty Final → adoption → silent end is **not** a loss.
- [ ] **Step 2:** `stream.unboundText string` (one string, bounded by the provider's own text bound): set in `emit` when `turn == ""` and `res.Text != ""`, cleared by an empty Final while unbound; in `beginTurn`, when an unbound stream is adopted (`a.turn == ""`), `a.lastText, a.settled = a.unboundText, a.unboundText == ""`.
- [ ] **Step 3:** unchanged and green: all `core/sttunit` tests; per-provider resurfacing tests (`providers/deepgram/stt_test.go:625,661`, `providers/cartesia/stt_test.go:491,659`, `providers/elevenlabs/stt_test.go:855`, `providers/nvidia/stt_test.go:209`) — a provider that does resurface sets `lastText` itself, so nothing changes for it. `cd server && go test -race -run 'STTFailover|SttFailover' -tags xaispeech .`; `make offline-test` runs in the gate (xAI/Google STT are not on the runner, so it guards only the shared pipeline).
Coverage note (U2): the four runner providers (Deepgram, Cartesia, ElevenLabs, Riva) are paid, so only `core/sttunit` and provider unit tests cover the seed; no free e2e reaches it (echo/offline STT are not on the runner).
- [ ] **Commit:** `fix(sttunit): text heard before a stream was adopted is owed by the turn`. **Gate** (+`loadtest-smoke`). Expected ≈ +8 non-test, +~70 test.

---

## Task 6.8 — Phase gate, adversarial review, ledger

- [ ] **Step 1: whole-phase numbers.** `git diff --stat $(cat $P/base-6.0)..HEAD | tail -1`; non-test Go only (`-- '*.go' ':!*_test.go'`); per-task diffstats from the `flake/6.X-diffstat.log` files; `wc -l` of fact 2's files after. Expected order of magnitude: ≈ −900 non-test Go (6.1 −690, 6.2 −15, 6.3 −50, 6.4 −130, 6.5 −35, 6.6 −15, 6.7 +8).
- [ ] **Step 2: stress.** `cd core && go test -race -count=200 -run TestFlushStaleInterruptionCannotCancelCurrentTurn .`; `cd core && go test -race -count=20 ./ttsunit/ ./sttunit/`; `cd providers/google && go test -race -count=10 -run 'TTS' .`; `cd providers/xai && go test -race -count=10 -tags xaispeech -run 'TTS' .` → all `ok`.
- [ ] **Step 3: full gate** (every line of the Gate block incl. `loadtest-smoke`, `live-check-selftest`, `perf-gate`, `test-postgres`, `offline-test`). Benchmarks: rerun 6.0 Step 3 and `BenchmarkUnitWrite`; `go run golang.org/x/perf/cmd/benchstat@latest $P/bench-google-before.txt $P/bench-google-after.txt` (and xAI, ttsunit) → no allocs/op increase; ns/op deltas within benchstat noise or explained.
- [ ] **Step 4: adversarial review (independent agent, opus) of `main...HEAD`.** Brief — hunt for:
  1. **Behaviour drift in unified TTS paths:** unit order with look-ahead under interleaved stale/failed/refused units (the "always wait for your turn" rule); a held attempt that retries after a partial `Write`; `Finish` deferral when an attempt errors after `Finish`; gen-0 idle units; `InterruptionFrame` during a held unit; `Close` during look-ahead waits; `EndReply` (ElevenLabs) unaffected by `LookAhead: 0`; R6a–d drifts are the only differences (diff frame sequences from the 6.1a tests on base vs head).
  2. **Error classification:** google `MapError` on every attempt error (R5) incl. breaker-open from `resilience.Execute` itself (not from the attempt) — does the error frame still classify the same in `server` TTS failover (`flow.ClassOf`/`ttsClassify`)? xAI unchanged. In 6.3: class and `Allow` checks unchanged; debug-event names/attrs identical; candidate `Reenter` still routed per index.
  3. **Lost bounds:** look-ahead holds ≤ `LookAhead+1` units of audio; `queues` slice fixed at construction; no new channel/slice growth; `unboundText` single string; `stack` has no unbounded buffers beyond what the tools had.
  4. **Hot-path allocations:** 3-index slicing correctness (no aliasing between frames; `Write` after an emit never overwrites emitted bytes; retry clears `u.buf` without touching emitted frames); benchmarks attached.
  5. **Double-close / leaks:** Google unary still closes its client on `Close` (one gRPC conn per call stage); ttsunit `Close` vs held units; `candidateSession` attach errors; `AsyncTracer` closed exactly once after `ownsTraceQueue` removal; `stack.Server.Stop` idempotent and still sweeps the vadworker group; tools' readers terminate.
  6. **Tests that stopped covering provider-specific edges:** Google per-piece behaviour, empty-audio final marker, odd byte, `Seq`/`Duration` drift; the deleted `TestIdleSpeakIndexFollowsFrontInsertions` replaced by an equivalent wiring check; SSRF table rows all moved (count rows before/after).
  7. **tools/ regressions:** livecheck child env still an allowlist (no provider key leaks into the child beyond `sp.env`); key still registered with the redactor; data dir still removed; loadtest failure counters (rejected/dial/handshake) unchanged; WAV reader strictness vs the clips the free targets load.
  8. **D9/U1:** idle lines in history count against context budget/summarization; RTVI `bot-llm-text` for idle lines (client-visible); goodbye wait still ends the call; no idle line reaches the LLM as a *user* message; redaction of the sidecar.
  9. **YAGNI:** every new core API (`ttsunit.Config.LookAhead`, `netpool.AllowedIP`) consumed on a server call path; no helper with one caller.
- [ ] **Step 5:** resolve findings (fix round = own commits + scoped re-review). Ledger line + `p6/pr-notes.md`: (a) removed/renamed exported symbols (google/xai `TTSProcessor` fields `Requests/Errors/Retries/Stale` → `RequestsTotal/ErrorsTotal/RetriesTotal/StaleTotal`, xAI `TTSProcessor` internals, `IdleController.SetSpeakIndex/SpeakIndex`, new `ttsunit.Config.LookAhead`, `netpool.AllowedIP`, `api.RequestID`); (b) rulings R1–R8, U1/U2 answered yes; (c) behaviour changes (R6a–d drifts; idle lines in history/records/RTVI; unbound-final loss now replays); (d) skipped items (below) with reasons; (e) carried items (R8) for Phase 9.

---

## Skipped (measured on 52db4be; not unified)

| Item | Reason |
|---|---|
| D5 SSE reader | Different protocols: chat-completions `data:`-only stream (openai, ~25 inline lines) vs xAI Responses typed `event:` stream (168 lines with xAI-classified sentinels). A shared core reader saves ≤15 lines and moves xAI error identity into core. |
| D5 tool-call assembly, history mappers | Different wire shapes (per-`index` deltas vs output items by `output_index`/`item_id`; folded `tool_calls` vs `function_call` items). Provider wire shapes stay in providers. |
| D5 tool-result encoders | Model-visible strings differ (fallback category and sanitizer: blacklist vs whitelist); ~15 lines; unifying changes what models see. |
| D6 `warmConn` (google ≡ nvidia, 25 lines each) | Takes `*grpc.ClientConn`; `core` has zero requirements and would gain grpc. |
| D7 publish paths | Done in Phase 1.1 (fact 16). |
| D8 clock seams | `IdleClock` has a different shape (`TimerAt`, fact 10); `turn.Clock` ≡ `transcript.PolicyClock` saves ≤12 lines but needs a new exported API (core or turn) or a transcript→turn import, and churns fakes in 9 test files. Server sets none of them. |
| D8 `cloneMetadata` vs `cloneAttributes` | Different contracts (error on overflow vs fail-closed drop of strings). |
| D8 `closeSegments` vs `markDropped` | 3 shared lines. |
| D8 `*Counts` sums | Different arity (STT 3, LLM/TTS 4). |
| D9 synthetic-notice paths | ≤5 shared lines, different resume indexes and history handles; `async_trigger` writes no history. |
| D3 bash/TS copies (`tools/eval/pipecat/run.sh`, `e2e/global-setup-flows.ts`, `scripts/dev.sh`) | Different languages/runtimes. |
| D4 full RTVI client | Readers have different models (atomics + channels for load vs an event log for scenarios); only the handshake is shared (6.4). `server` e2e client is another module. |
| D10 `provtest/fake.go` vs `providers/echo` | Fault injection is test-only; folding it into the product provider adds knobs nobody ships; `provider-matrix-dry` (a gate target) needs it. |
| cartesia/elevenlabs `errors.go` (~40 shared lines) | Classification differs (transient keywords, ElevenLabs' 1008 handling); provider wire/classification stays in providers. |
| `examples/google_pipeline` vs `examples/xai_pipeline` | Examples are standalone teaching programs; sharing code between them hurts their purpose. |
| D10 markdown table renderers | Inline format strings, no renderer to share. |
| §E `SessionController` type assertions (12) | Not a duplicate; changes every fake session. Phase 9 (R8). |
| ws `writeMu` on the run loop; resampler general-path alloc | Not duplicates; carried to Phase 9 (R8). |
