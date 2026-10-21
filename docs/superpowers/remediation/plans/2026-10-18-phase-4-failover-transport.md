# Phase 4: Failover and Transport Correctness (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. Each task is one independently reviewable deliverable with its own tests and its own commit. Tasks run in order 4.1 → 4.2 → 4.3 → 4.4 → 4.5 → 4.6a → 4.6b → 4.6c → 4.6d → 4.6e → 4.6f → 4.7. Hard dependencies: 4.3 and 4.6a edit `transport/ws/transport.go` after 4.1 (text anchors, no overlapping hunks); 4.3 reuses the `outputRig` test helper from 4.1; 4.4's server e2e reuses `e2eCall.wireLog`/`countOf` from 4.1; 4.6d and 4.6b edit files no other task touches. 4.2, 4.5 and 4.6b–4.6f depend only on main and may run on parallel worktrees (rebase before 4.7).

**Goal:** Barge-in tells the client its reply was cut off with the message the pinned client accepts (A7), a synchronous LLM failure fails over instead of killing the call (A9), output audio is resampled per frame rate (A10), a crash-looping VAD worker no longer leaves its calls VAD-blind (A11), the TTS failover's stall watchdog no longer drives TTS candidates from its timer goroutine, which broke the serialized calls the unit queues rely on and could wedge later units (A12), and the deferred transport/server cleanups land (read limit, disconnect accounting, token lifetime, management ordering, demo install context, MCP route codes, `StopSecs` lint).

**Architecture:** No new goroutine except the VAD worker's `respawnLoop` (one per down worker, ends on success or close). The transport sends RTVI `bot-interrupted` from its `FrameFlush` case (only a real barge-in emits a `FlushFrame`). `core.LLMFailover.begin` classifies a synchronous candidate error like an error frame; the three LLM providers turn a refused task (`ErrTooManyTasks`) into a non-fatal error for the turn. `transport/ws` rebuilds its output resampler when the frame rate changes. The VAD worker's run loop never sleeps: a down worker hands its queued jobs to a live one (state rides in the job) and restarts off the loop. `core.TTSFailover`'s unit watchdog re-enters an internal frame to itself (the `UnitFailureReporter.check` pattern) so the expiry's cancel and failover run on the session run loop.

**Tech Stack:** Go 1.27 workspace (`core`, `providers/*`, `server`, root module for `transport`/`vad`/`tools`), protobuf RTVI 2.1.0 frames, SQLite/Postgres (no migration), Node 22 with the pinned `@pipecat-ai/client-js@1.13.1` + `@pipecat-ai/websocket-transport@1.7.2` (`tools/compat`), the supervised `bin/vadworker` child (ONNX Runtime, `make ort`).

**Spec:** `docs/superpowers/remediation/specs/2026-10-15-deep-review.md` items **A7**, **A9**, **A10**, **A11**, **A12**; deferred cleanups from master Task 4.6 and the Phase 1 deferred list (read limit, 1007 disconnect accounting, stale comment, demo singleflight, MCP enable/disable codes). Master plan: `docs/superpowers/remediation/plans/2026-10-15-review-remediation.md` Phase 4 (Tasks 4.1–4.6, gate `-race -count=20` on `core/ttsunit` and `vad`). Mapping: master 4.1 → 4.1; 4.2 → 4.2; 4.3 → 4.3; 4.4 → 4.4; 4.5 → 4.5; 4.6 → 4.6a–4.6f; the phase gate is 4.7.

## Global Constraints

Copied from `AGENTS.md` and the master plan. Every task inherits them.

- `core/` never imports `server/`. Everything added to `core/` is consumed by `server/` on a call path: the `LLMFailover` sync-failure path by `server/llm_failover.go` `buildLLM`; the run-loop TTS expiry by `server/tts_failover.go` `buildTTS` (every flow with TTS fallbacks). No new exported core API (`ttsExpire` is unexported).
- Every queue, channel, map and slice is bounded: the transport keeps one output resampler (rebuilt on a rate change); a TTS expiry is one internal frame per watchdog firing, retried every 5 ms only while the inbox refuses it; the VAD hand-off reuses the existing worker queues (non-blocking sends).
- Goroutines: the only new one is `workerProcess.respawnLoop`, tracked by the worker's `wg`, ending on handshake success or `quit` (closed by `close`). No goroutine in `core/` is added (the TTS watchdog's `time.AfterFunc` callbacks already exist; they now only post a frame, ending on success or `ErrSessionClosed`).
- No `context.Context` in new struct fields (test fakes pass contexts over channels only). `defer cancel()` immediately after every derived context.
- Never hold a lock across I/O: a VAD worker's start and 3 s handshake run without `wp.mu` (`start`, 4.4); only installing the process takes it; the VAD hand-off holds `closeMu.RLock` only around non-blocking channel sends; the TTS expiry on the run loop does what `run` already does there (candidate `ProcessFrame` calls and re-entries), no I/O.
- Generation fencing: `bot-interrupted` is written only for a committed `FlushFrame`; the LLM sync failover re-checks `mayFailOver` (generation, side effects, budget) under `w.mu`; provider ErrorFrames for a refused task carry the turn's generation.
- Monotonic time only (`time.Now()` comparisons keep the monotonic reading; timers for waits).
- Errors wrapped with `%w`, inspected with `errors.Is/As`; callbacks recover as today.
- `go test -race` in every module (`make test`) and an empty `gofmt -l`.
- Conventional commits, one logical change each. Commit trailer: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Provider secrets come from the environment only. Never write or store keys. Every test uses fakes and placeholder keys.
- RTVI 2.1.0 stays compatible with the unmodified pinned client: `bot-interrupted` is a message the pinned client parses and ignores (`dist/index.js:2496`, "Unrecognized message type"); the protobuf `interruption` frame is not sent (Ruling R1).
- **Gate rules (user requirement, binding for every task).** Run on an otherwise idle machine, one target at a time, sequentially (a developer's own dev server, `make dev` / `bin/server`, may run on this machine: leave it alone). Tee every run's full output to `.superpowers/sdd/2026-10-15-review-remediation/flake/<task>-<step>-<target>.log` with `set -o pipefail`; on any failure name the failing test (`grep -nE -- '--- FAIL|^FAIL|✘' <log>`) in the task report; a rerun never replaces the failing log. After **every** task: `make verify`. After every task that **changes behaviour** (all but 4.6f): also `make e2e-echo` (Go e2e + Playwright flows + one-port) and `make offline-test` (offline stack up on 127.0.0.1:8099; never run `offline-up`, never download models; if the stack is down, stop and report, do not start it). After 4.2 and 4.7: `make provider-matrix-dry`. `make test-postgres` in 4.7 (no task touches `server/storage`; run it earlier if one does). **Never** run paid or live targets (`make e2e`, `e2e-xai`, `live*`, `agent-matrix`, `provider-matrix`, `integration`-tagged tests).
- **Known intermittent:** `TestOfflineStackLongCallerSpeechIsOneTurn` was root-caused and fixed on main (xAI STT drain 2 s → offline 6 s, 4716792). If it fails, keep the log, rerun it alone 5 times (command below) and report every result; never mask, skip or retry it inside a target.
- **Per-phase gate:** an independent adversarial reviewer agent audits the phase diff (Task 4.7) before the phase is done.

The gate block every task's "Gate" step runs (substitute the task id and step):

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
make verify 2>&1 | tee "$L/4.X-sN-verify.log"
# behaviour-changing tasks (all but 4.6f):
make e2e-echo 2>&1 | tee "$L/4.X-sN-e2e-echo.log"
curl -fsS http://127.0.0.1:8099/healthz >/dev/null || { echo "offline stack is down: report it, do not start it"; exit 1; }
make offline-test 2>&1 | tee "$L/4.X-sN-offline-test.log"
# 4.2 and 4.7 only:
make provider-matrix-dry 2>&1 | tee "$L/4.X-sN-provider-matrix-dry.log"
# only if TestOfflineStackLongCallerSpeechIsOneTurn failed:
for i in 1 2 3 4 5; do (cd server && OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -count=1 -v -tags "xaispeech offlinestack" -run '^TestOfflineStackLongCallerSpeechIsOneTurn$' .) 2>&1 | tee "$L/4.X-sN-longspeech-$i.log"; done
```

(`GET /healthz` is the adapter's liveness route, `offline/adapter/server.go:28`; `/health` answers 404 while the stack is up.) Expected: `=== All verify checks passed ===` (it still prints `PLAYBACK_CLEAR_REQUIREMENT_FAILED … Continuing only under recorded product decision relax-unmodified-client-playback-clear`, which is the recorded, accurate decision; see R1); `make e2e-echo` exits 0 with every Playwright test passed; `make offline-test` prints `ok` for both packages; `provider-matrix-dry` exits 0.

## Review Focus

These are the failure modes this phase must not ship. Each is pinned by named tests.

1. **Barge-in wire order (A7).** `bot-interrupted` is sent exactly once per barge-in that cut a reply off (the assistant's `handleInterruption` flush), not for a typed turn while the assistant has no active turn; after it, no queued audio of the cut reply (at most the one audio frame already admitted to the socket (one provider chunk, which can be longer than 20 ms; only echo and xAI emit 20 ms chunks); uninterruptible output such as cues continues by design); the next reply's audio comes after it; the pinned client handles it without logging any error, and the protobuf `interruption` frame is never sent. Pinned by `TestFormatBotInterruptedIsPipecatsMessage`, `TestBargeInSendsBotInterruptedAfterTheCutReplysQueuedAudio` (transport, real WebSocket), `TestCallBehaviorBargeInSendsBotInterruptedOnceAfterTheCutReplysAudio` (server e2e), `TestCallBehaviorPipecatClientTakesABargeInWithoutErrors` (real pinned client), `node tools/compat/probe_playback_clear.js` (in `make verify`).
2. **Failover error classes (A9).** A provider-side synchronous error before any output fails over and the call survives; the session's own task budget (`ErrTooManyTasks`) and teardown (`ErrSessionClosed`) never jump providers; a refusal by a later stage is not the candidate's failure; the user message reaches the assistant context exactly once. Pinned by `TestSyncErrorFromThePrimaryFailsOver` (both push variants), `TestLocalSyncErrorNeverFailsOver`, `TestADownstreamRefusalIsNotTheCandidatesFailure`, `TestLLMClassify`, the three `TestRefusedTaskLosesTheTurnNotTheCall` provider tests, `TestCallBehaviorASynchronousLLMFailureFailsOverAndTheCallSurvives`.
3. **Output resampling per rate (A10).** A frame is resampled with the ratio of its own rate; a rate change builds a new resampler (filter state restarts at a switch, which no server path makes today). Pinned by `TestOutputResamplerFollowsEachFramesRate` (real WebSocket, 16 k → 48 k → 16 k).
4. **VAD crash loop (A11).** The worker run loop never sleeps; while a worker is down or crash-looping its sessions are served by a live worker from the next chunk; a batch whose pipe failed (an external SIGKILL) is re-run on a live worker; a restarted worker is installed, counted in capacity and marked up in one critical section; the respawn goroutine ends on success or close; no lock is held across the handshake; no child process outlives `Close`. Pinned by `TestWorkerProcess_CrashLoopingWorkerHandsItsSessionsToALiveOne` (real worker children, `-race -count=20`), `TestWorkerRespawnFailuresDoNotDrainCapacity` (rewritten), the existing `TestWorkerProcess_HardKillAndRespawn` / `_SessionIsolation`, `TestCallBehaviorVADKeepsHearingCallersWhileAWorkerCrashLoops` (server e2e, real `vadworker` killed mid-call).
5. **TTS watchdog on the run loop (A12).** A unit watchdog expiry never calls a TTS candidate from the timer goroutine: its cancel and failover wait for the run loop; an expiry that meets a closed session is dropped without a leak; a stalled unit still fails over and later units are spoken. Pinned by `TestTTSWatchdogExpiryDrivesCandidatesOnlyFromTheRunLoop` (`-race -count=50`: 50/50 RED on today's `expire`, 50/50 GREEN after, in a scratch copy), `TestTTSWatchdogExpiryAfterCloseIsDropped`, `TestCallBehaviorAStalledTTSUnitFailsOverAndLaterUnitsAreSpoken` (server e2e).

---

## Architecture

### What the code does today (verified 2026-10-17 on `feat/phase-4-failover-transport` @ 3a243fd)

- **A7.** `transport/ws/transport.go:1039-1042` `case core.FrameInterruption:` only passes the frame on ("The pinned client still does not clear audio it already buffered."). `:1044-1061` `case core.FrameFlush:` sets the pacer's active generation, `BeginFlush` drops the generation's queued interruptible audio (`pacing.go:500-545 dropGeneration`) and acks with `FlushProcessedFrame`. The only producer of a `FlushFrame` is `core/assistant_context.go:694` (`handleInterruption`, i.e. a reply was active); a typed turn's `InterruptionFrame` from `core/user_context.go:158` passes the assistant untouched when nothing is active (`assistant_context.go:327-333`) and reaches the transport without a flush. `transport/rtvi/rtvi.go:19-44` has no `bot-interrupted` type. `docs/pipecat-compatibility.md:39` says "`bot-interrupted` … not emitted".
- **A7, the pinned client.** `tools/compat/node_modules/@pipecat-ai/websocket-transport/dist/index.js`: the protobuf `Frame` type knows text/audio/transcription/message only (`:2762-2840`); `ProtobufFrameSerializer.deserialize` (`:2873-2896`) throws `Unknown frame kind` for anything else, and the socket's `message` listener (`:3028-3037`) logs `Failed to deserialize incoming message` and drops it. Nothing in the transport calls `mediaManager.userStartedSpeaking()` (the only path to `WavStreamPlayer.interrupt()`, `:1573-1575`); `interrupt()` is called only from `disconnect()` (`:1570`, `:1787`). `client-js/dist/index.js:2341-2498` `handleMessage`: `user-started-speaking` fires the app callback only (`:2371`), unknown types (including `bot-interrupted`) hit `default:` ("Unrecognized message type", `:2496`). Experiment (scratch script feeding the real transport's listener, 2026-10-17): audio → `bufferBotAudio`; field 5 → two `Unknown frame kind` errors; `bot-interrupted`, `user-started-speaking`, `bot-stopped-speaking` → delivered to `handleMessage`; the media manager's `userStartedSpeaking` was never called.
- **A7, Pipecat (pinned `pipecat-impl` 2967e1c, 1.12.0).** `serializers/protobuf.py:47-63` serializes `InterruptionFrame` as `"interruption"`, and `transports/websocket/fastapi.py:508-513` writes it on interruption; `processors/frameworks/rtvi/observer.py:480-483` also sends `BotInterruptedMessage` (`models.py:637-646`, `{"label":"rtvi-ai","type":"bot-interrupted"}`, no data; CHANGELOG: "so clients can drop whatever the bot was mid-saying").
- **A7, the gate.** `Makefile:92-93` runs `node tools/compat/probe_playback_clear.js` in `make verify`. The probe (`:35-131`) deserializes field 5, inspects `WebSocketTransport.prototype` method names and calls `client.handleMessage(user-started-speaking)` directly; its "transport clears" result is hard-coded `false` (`:111`). `docs/compatibility/playback-clear-decision.json` records `relax-unmodified-client-playback-clear`.
- **A9.** `core/llm_failover.go:191-232` `begin` returns the first candidate's synchronous `ProcessFrame` error (`:231`) to the session, which fails the call (`core/session.go:817-824`). A synchronous error from a *fallback* already moves on (`failoverLocked` `:284-308`, class `server_error`). All three LLM processors push the user message first, then return `s.Go(...)`'s error (`providers/openai/llm.go:369-386`, `providers/xai/llm.go:308-354`, `providers/google/llm.go:373-404`), so `ErrTooManyTasks` kills a busy call with or without fallbacks. `server/llm_failover.go:61-67` `llmClassify` maps `ErrTooManyTasks` to `server_error`; `server/stt_failover.go:34-37` already treats it as local (`""`). Google/xAI TTS already turn a refused task into a non-fatal, re-entered `ErrorFrame` (`providers/google/tts.go:552-566`, `core/ttsunit/ttsunit.go:228-243`).
- **A10.** `transport/ws/transport.go:104-106` keeps `inResamplers map[int]*resample.Resampler` and one `outResampler`; `:941-958` creates `outResampler` from the first frame's rate and reuses it for every later rate. `server/tts_failover.go:91-99` (`ttsRate`) pins every TTS candidate to one rate because of it ("The transport builds its resampler from the first frame and cannot follow a rate change"); `core.NewTTSFailover` rejects mismatched candidate rates (`core/tts_failover.go:114`), and `server/dtmf.go:43-47` generates tones at the same rate. So no server path emits two rates today: the bug is latent, and the e2e lives at the transport boundary. `core/tts_failover.go:27-28` (`TTSCandidate.Rate`) repeats the stale "cannot follow a mid-session change".
- **A11.** `vad/worker_executor.go:178` pins a session to `sessionID % len(queues)`; `executeBatch` (`:363-375`) calls `respawnWithBackoff` (`:552-603`) on the worker's own run loop, which sleeps 20 ms … 1 s per batch and then fails the batch (`ErrWorker`) when the restart fails; `handleWorkerFailure` and the hang path do the same. `startAndHandshake` (`:231-307`) holds `wp.mu` for the whole 3 s handshake, so the run loop's own `wp.mu.Lock` (`:364`) also waits on it. A Silero job carries its own detector state (`Job.State`, set from the processor's `p.state` at `vad/processor.go:304`, written back at `:379`), and `PerSessionLimit` is 1 with one reusable job per processor (`processor.go:293`, `p.busy`), so any worker can serve any chunk in order. The test seam `Executor.HardKill` exists (`vad/export_test.go:26`). Metrics: `voice_vad_worker_restarts_total` from `Stats().Restarts` (`server/server.go:1204-1219`).
- **A12.** `core/tts_failover.go:258` arms each unit's watchdog with `time.AfterFunc(… w.expire(u, attempt))`; `expire` (`:404-452`) then calls the wedged candidate's `ProcessFrame(InterruptionFrame)` and `w.run(…)` (the next candidate's or the next unit's `ProcessFrame(SynthesisUnitFrame)`) on the timer goroutine, while the run loop may drive the same candidates (`ProcessFrame` for a unit when no unit is current or the current one is fenced, `InterruptionFrame`/`CancelFrame` to all). `core/serial_queue.go:19` documents "Callers must serialize Go calls (processors do: one run loop)", and `ttsunit.Runner` takes a playback slot before queueing the unit's job (`ttsunit.go:135-136`, `:146`); Google and xAI non-streaming TTS have the same slot-then-queue shape (`providers/google/tts.go:313,334`, `providers/xai/tts.go:300,314`). Two concurrent unit deliveries to one candidate can take slots n and n+1 and queue their jobs in the opposite order; the single drain then waits forever on job n+1 in front of job n (`ttsunit.go:185-188`). The design review proved this with the stress test of the first draft of this plan: an explicit abandoned set plus a locked `SerialQueue` still deadlocked 3/3 (`finished=1 next=41`, the drain in `cond.Wait`). With calls serialized again, `abandon`'s `nextSeq--` is correct as it is (`ttsunit.go:246-265`: the refused slot is always the newest). Other off-loop callers that remain: `fromCandidate` → `run` on a candidate's own task goroutine when a unit's final frame starts the next queued unit; it holds `cur` (exclusive with the run loop's `run`, which needs `cur == nil` or a fenced `cur`), and a unit fenced meanwhile is dropped by the providers before it takes a slot (`ttsunit.go:129-133`); `CancelFrame`, which clears `cur` without a new generation, has no production sender (deep review G2).
- **4.6.** `transport/ws/wire.go:15` `MaxEncodedFrameSize = 64 * 1024` while the largest valid frame is a 16 KB message (`:17`) plus a few bytes; the read limit is that + 1 KB (`transport.go:428-432`). `readLoop` (`transport.go:665-797`) returns without any `disconnect_*` on every server-initiated close (1007 at `:694-701` and `:738`, 1008 at `:718`/`:760`/`:790`, 1011 at `:735`, 1013 at `:729`, going away at `:723`) and on the client's `disconnect-bot` (`:742-745`), while `Close` always counts `client_disconnected` (`:923-925`). `:816` "// Close gracefully terminates the connection." sits above `observeDisconnect`. `server/api_routes.go:181-187` issues `min(1 m, token.max_lifetime)`; every `config/*.yaml` sets `max_lifetime: 5m`; only the server issues voice tokens (no other `SignedString` outside tests). `server/management.go:81-115` installs the store, flow service and routes before the production provider check (`:108-115`) and the recording sink (`:98-107`), whose `fail()` closes the store they point at. `server/probe.go:186-191` + `:61-90`: a pre-session failure needs `core.NewPipeline` or `core.NewSession` to fail, which they do only for a nil processor or an invalid default budget (unreachable; Ruling R10). `server/config.go:200` `StopSecs time.Duration` (ST1011). `server/demo_api.go:38-44` runs the singleflight under the first caller's `ctx`. `server/mcp/http.go:85-110` maps every disable error to 404 and every enable error to 409.

### Decision 1: barge-in sends RTVI `bot-interrupted`, not the protobuf frame (A7)

- **Key question settled with evidence: the pinned unmodified client does not clear its buffered playback for any wire signal.** Field 5 never reaches the client (deserializer throws, logged as an error); `bot-interrupted` reaches `handleMessage` and is ignored; `user-started-speaking` only fires the app callback. Nothing but `disconnect()` interrupts the player. The probe is rewritten to show this by delivering each signal through the real transport's `message` listener and watching the media manager (Task 4.1), instead of hard-coding the result.
- **Emission.** `bot-interrupted` is what Pipecat's `RTVIObserver` sends for an interruption and what the pinned client accepts quietly. No JS client available locally acts on it (client-js 1.13.1 here; the design review also checked 1.12.0 and 1.13.0); Pipecat's own Python consumers do: `serializers/rtvi_client.py:152` maps it to an `InterruptionFrame`, and the evals transport (`evals/client_transport.py:319-331`) drops the bot PCM it buffered. Pipecat sends it on every `InterruptionFrame` without checking whether the bot was speaking (`observer.py:480-483`) and its WebSocket transport writes the protobuf frame every time too (`fastapi.py:508-513`), so "parity" here is the same message, sent in fewer cases. The protobuf `interruption` frame would make every pinned client log `Failed to deserialize incoming message` on every barge-in for no effect, so it is not sent (declared difference; Q1).
- **Where.** In `case core.FrameFlush:` after `BeginFlush`, because the flush exists only when the assistant ran its barrier (`handleInterruption`, i.e. its `activeTurn` was set: a reply generating or playing), so a typed turn while the assistant is idle sends nothing (pre-existing caveat: after an LLM error the assistant never resets `activeTurn`, so the next turn's barrier sends a `bot-interrupted` with nothing cut off; deferred, 4.7); and because `BeginFlush` has just dropped the reply's queued audio and fenced later stale frames, so nothing of the cut reply follows except at most the one audio frame already admitted to the socket (one provider chunk, which can be longer than 20 ms; only echo and xAI emit 20 ms chunks): a flush does not cancel coder/websocket's write, `docs/output-pacing-and-flush.md:20`. Uninterruptible output (cues, goodbyes) keeps playing by design. Written synchronously with `WriteWireMessage` (the same writer lock as audio), before any audio of the next reply is enqueued (the next reply needs a new LLM round).
- **The decision file stays accurate.** `playback-clear-decision.json` keeps `relax-unmodified-client-playback-clear` and `client_cleared_buffered_playback: false`, gains the per-signal observations the probe now compares exactly, and records the server's emission. The V2 output-flush gate stays unresolved (`decision_is_a_pass: false`).

### Decision 2: a synchronous LLM error is classified like an error frame (A9)

| Error from the first candidate's `ProcessFrame(UserMessage)` | Today | After |
|---|---|---|
| A provider-side failure (e.g. `MapToolDefinitions` rejects this provider's schema; a processor whose client refuses the request) | session fails | fails over when `mayFailOver` allows (class, generation, side effects, budget); the call survives |
| `ErrTooManyTasks` (the session's own task budget) | session fails | the provider absorbs it: non-fatal `ErrorFrame` for the turn, re-entered (so a failover wrapper sees it), call survives; `llmClassify` maps it to `""`, so it never jumps providers |
| `ErrSessionClosed` (teardown) | returned (ignored after teardown) | unchanged; `llmClassify` maps it to `""` (no spurious failover metric) |
| a later stage refused the user message (`e.Push` error) | session fails | session fails (not the candidate's fault: the `pushTracker` sees it) |
| every candidate failed synchronously | session fails | session fails (unchanged; the remaining sync errors are configuration bugs) |

The wrapper forwards the user message itself when the failed candidate had not, so the assistant context sees it exactly once.

### Decision 3: rebuild the output resampler when the rate changes (A10)

The transport keeps its single `outResampler` and the rate it was built for (`outRate`), and builds a new one when a frame arrives at another rate. That is the smallest change that resamples every frame with its own ratio; no map, bound or `clear()` is needed, and `resampleInput` is untouched. Filter state restarts at a rate switch (a one-sample discontinuity), which no server path produces today. The server's single-rate rule (`ttsRate`) stays because `core.TTSFailover.expire` closes a stalled unit with a frame at the candidate's rate; only its comment (and `TTSCandidate.Rate`'s) changes.

### Decision 4: a down VAD worker hands its jobs over and restarts off its loop (A11)

- `executeBatch` on a worker that is not healthy: `markDown` (out of capacity, `up=false`, starts one `respawnLoop`), then `handOff` the batch to the next worker that is `up` (non-blocking send under `closeMu.RLock`, the same guard `Submit` uses); only a job no live worker can take fails with `ErrWorker`. Routing in `Submit` is not changed: the hand-off happens in one place, costs nothing on the healthy path, and the down worker's loop is now fast (it never sleeps).
- `respawnLoop(wait)` waits on a timer or `quit`, calls `start` (a process that passed its handshake, not yet installed), and on success installs it and restores `healthy`, `down=false`, capacity and `up` in one critical section of `wp.mu` (a crash noticed afterwards sees an up, healthy worker and starts a new respawn; installing and restoring in two sections would let a crash in between leave an up worker on a dead process); on failure backs off further (`noteCrashLocked`: 20 ms doubling to 1 s, 1 s in a crash loop, as today). `Restarts` is counted when the restart is scheduled (synchronously in `markDown`) and on every failed retry.
- `start` starts and handshakes without `wp.mu`; its caller installs the process under `wp.mu` (`installLocked`), or reaps it when the worker was closed meanwhile. `handleWorkerFailure` (a write, read or decode failure: an external SIGKILL lands here) hands its batch to a live worker too: the failed attempt changed neither the jobs' input nor their state. The hang path still fails its jobs (`ErrWorkerHung`: their deadline has passed).
- `wp.stderr` keeps collecting the child's last 64 KB as today; logging it on a crash is out of scope (the vad package has no logger; deferred, 4.7).
- Correctness of the hand-off: the job carries the session's detector state and the processor has at most one outstanding job, so a session's chunks stay in order across workers.

### Decision 5: the TTS watchdog expiry runs on the run loop (A12)

The watchdog's `time.AfterFunc` callback only posts `&ttsExpire{u, attempt}` (an unexported `FrameInternal` frame) to the TTSFailover's own index with generation 0 (`postExpire`), retrying every 5 ms while the inbox refuses internal frames (`ErrQueueFull`), exactly like `UnitFailureReporter.check`; `ErrSessionClosed` ends it. `TTSFailover.ProcessFrame` handles `*ttsExpire` by calling the unchanged `expire`, which re-checks `w.cur == u && u.attempt == attempt` under `w.mu`, so a stale or duplicate expiry is a no-op. `expire` on the loop does what `run` already does on the loop (candidate `ProcessFrame` calls, `Reenter` of an error and a closing frame): no I/O and no new blocking. The first draft's `ttsunit.Order`, its Google/xAI copies and the `SerialQueue` lock change are dropped: they did not prevent the deadlock (the reviewer's run) and `abandon` is correct once calls are serialized.

### Decision 6: cleanups

- 4.6a: `MaxEncodedFrameSize` 20 KB (largest valid frame ≈ 16.4 KB; read limit 21 KB, below the library's 32 KB default; worst-case read buffer per socket 65 KB → 21 KB). `readLoop` counts exactly one `disconnect_*` per connection with one deferred observe (`server` for every return that is not a failed `Read`, `normal` for `disconnect-bot`, the existing classification for a failed `Read`, except that a `Read` failing because the server cancelled the connection from outside the loop, `Close`/hang-up/ready timeout, is `server` rather than `client`; a client that stopped answering pings stays `client`); `observeDisconnect` and its stale comment are deleted.
- 4.6b: issued voice tokens live `token.max_lifetime` (whole seconds); the default and every shipped YAML become `1m`, so issuance stays 60 s and the validator ceiling tightens from 5 m to 1 m (only the server mints tokens); `ValidateConfig` rejects `token.max_lifetime` above 5 m (the token travels in the `/ws` URL query).
- 4.6c: everything that can fail runs before anything is installed: the production provider check runs on a local flow service, and the recording sink is created before `SetStore`.
- 4.6d: the demo install flight runs on a server-owned context (`context.Background()` with a 30 s bound) whose cancel Shutdown calls (`stopDemoLocked`), and registers on `demoWG` inside the flight under `s.mu` with the shutting-down check, so `waitDemo` still waits for it before the store closes; each caller waits on `DoChan` or its own context. (`context.WithoutCancel` of the caller's context, the first draft, would have let Shutdown close the store under a running flight.)
- 4.6e: disable/enable map not-found to 404, a store failure to 500, and enable's pending deletion to 409.
- 4.6f: `StopSecs` → `Hold` (YAML key `stop_secs` unchanged).

### Test strategy (layers)

- **L1 unit (TDD, every task):** `transport/rtvi/bot_interrupted_test.go`, `core/llm_failover_test.go`, `server/llm_failover_test.go`, provider tests (`providers/{google,openai,xai}`), `core/tts_failover_loop_test.go`, `vad/worker_capacity_test.go`, `transport/ws/wire_limits_test.go`, `server/management_test.go`, `server/demo_api_test.go`, `server/mcp/http_test.go`, `server/api_boundary_test.go`, `server/config_test.go`.
- **L2 transport e2e (real `ws.Server`, real WebSocket):** `transport/ws/output_rig_test.go` (`outputRig`: the Connection as the whole pipeline, a client logging every frame in order) for A7 and A10; `transport/ws/read_limit_test.go` for 4.6a.
- **L2 server e2e (real server, real WebSocket, echo providers, factory overrides like `server/provider_timeline_e2e_test.go`):** new tests are named `TestCallBehavior…`, so `make test-e2e-go` (regex `…|CallBehavior|…`) runs them with no Makefile change. Waits are `eventually` (8 s, 5 ms poll) on observable conditions, or `within` (same poll, own deadline) for the barge-in waits that span a whole reply on Google-like echo timing; ordering is asserted on the wire log (`e2eCall.wireLog`, added in 4.1), never by sleeping. A11's server e2e uses the real process VAD executor with a crash-loop wrapper script and kills a real `vadworker` child. A12's server e2e stalls the primary TTS of a flow with a TTS fallback (factory override) with the registry's `ttsUnitTimeout` shortened to 200 ms.
- **L2 VAD (real child processes):** `vad/resilience_test.go` with the wrapper script; `-race -count=20` in 4.7.
- **L3 protocol (real pinned client):** `tools/compat/test_rtvi_barge_in.js` driven by `server/barge_in_client_e2e_test.go`; `tools/compat/probe_playback_clear.js` in `make verify` compares observed client behaviour with the recorded decision.
- **L4 browser (Playwright):** none added. Nothing in this phase is UI-visible: the console's call view does not surface `bot-interrupted` (the pinned client ignores it), failover and VAD faults need server-side fault injection that the browser suites cannot reach. `make e2e-echo` runs as the regression gate after every behaviour task (Q4).
- **Stress/race:** `core/ttsunit` and `vad` `-race -count=20` (master gate), the A12 core tests `-count=50`, transport A7/A10 tests `-count=20`, the new server e2e `-count=3`.
- **Mutation checks** are listed per task: each names the line to break and the test that must fail; run them locally, do not commit them.

---

### Task 4.1: barge-in sends `bot-interrupted`; the playback-clear probe observes the real client (A7)

**Files:**
- Modify: `transport/rtvi/rtvi.go`, `transport/ws/transport.go`, `server/flow_e2e_test.go` (wire log), `tools/compat/probe_playback_clear.js`, `docs/compatibility/playback-clear-decision.json`, `docs/pipecat-compatibility.md`, `docs/pipecat-parity-matrix.md`, `docs/output-pacing-and-flush.md`
- Create: `transport/rtvi/bot_interrupted_test.go`, `transport/ws/output_rig_test.go`, `transport/ws/bot_interrupted_test.go`, `server/barge_in_e2e_test.go`, `tools/compat/test_rtvi_barge_in.js`, `server/barge_in_client_e2e_test.go`

**Interfaces:**
- Consumes: `rtvi.Handler.formatMessage`, `Connection.WriteWireMessage`, `PacedWriter.BeginFlush`, `core.FlushFrame`, the flow rig (`newFlowRigE2E`, `behaviorFlow`, `e2eCall.say`, `waitReplies`, `eventually`, `ended`), `createTestToken`/`performHandshake`.
- Produces: `rtvi.TypeBotInterrupted`, `(*rtvi.Handler).FormatBotInterrupted() (string, error)`; test helpers `outputRig`, `newOutputRig(t)`, `(*outputRig).events()`, `(*outputRig).waitFor(t, what, cond)`, `wireEvent{msg, bytes, rate, first}`, `constPCM(rate, d, v)`, `countAudio(log, first)`; `e2eCall.wire`, `(*e2eCall).wireLog() []string`, `countOf(log, entry)`, `indexFrom(log, entry, from)`, `longReply`, `bargeInRig(t)` (the flow rig on Google-like echo timing), `within(t, d, what, ok)`.

- [ ] **Step 1: Write the failing RTVI unit test.** Create `transport/rtvi/bot_interrupted_test.go`:

```go
package rtvi_test

import (
	"encoding/json"
	"testing"

	"govoice/transport/rtvi"
)

// Pipecat's BotInterruptedMessage (processors/frameworks/rtvi/models.py:637-646) is the label and the
// type, no data; the envelope adds the id every server message carries.
func TestFormatBotInterruptedIsPipecatsMessage(t *testing.T) {
	msg, err := rtvi.NewHandler().FormatBotInterrupted()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(msg), &got); err != nil {
		t.Fatal(err)
	}
	if got["label"] != "rtvi-ai" || got["type"] != "bot-interrupted" || got["id"] == nil || len(got) != 3 {
		t.Fatalf("bot-interrupted = %s", msg)
	}
}
```

Run: `go test -count=1 -run TestFormatBotInterrupted ./transport/rtvi/`. Expected: build failure `rtvi.NewHandler().FormatBotInterrupted undefined`.

- [ ] **Step 2: Add the message.** In `transport/rtvi/rtvi.go`, in the `const` block after `	TypeBotStoppedSpeaking  = "bot-stopped-speaking"` add:

```go
	TypeBotInterrupted      = "bot-interrupted" // a barge-in cut the bot's reply off (Pipecat BotInterruptedMessage)
```

After `func (h *Handler) FormatBotStoppedSpeaking() (string, error) {…}` add:

```go
// FormatBotInterrupted produces bot-interrupted: the bot's in-flight reply was
// cut off. Pipecat's RTVIObserver sends it on an InterruptionFrame.
func (h *Handler) FormatBotInterrupted() (string, error) {
	return h.formatMessage(TypeBotInterrupted, nil)
}
```

Run: `gofmt -w transport/rtvi && go test -race -count=1 ./transport/rtvi/`. Expected: `ok`.

- [ ] **Step 3: Write the failing transport e2e.** Create `transport/ws/output_rig_test.go`:

```go
package ws_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	"govoice/core"
	framespb "govoice/core/transport/v1/framespb"
	"govoice/transport/token"
	"govoice/transport/ws"
)

// wireEvent is one frame the client received: an RTVI message (msg set) or bot audio.
type wireEvent struct {
	msg   string // RTVI message type; "" for audio
	bytes int    // audio payload bytes
	rate  uint32 // audio sample rate on the wire
	first int16  // first audio sample: tests give each reply its own constant value
}

// outputRig is a real ws.Server whose pipeline is only the Connection (the
// transport's output side) and a client that logs every frame in arrival
// order. Tests post output frames into the session and read the wire. Audio
// is decoded from the protobuf directly, so a frame the client decoder would
// reject (a wrongly resampled one) is still counted.
type outputRig struct {
	sess *core.Session
	mu   sync.Mutex
	log  []wireEvent
}

func newOutputRig(t *testing.T) *outputRig {
	t.Helper()
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))
	sessCh := make(chan *core.Session, 1)
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, err := core.NewPipeline(conn)
		if err != nil {
			return nil, err
		}
		sess, err := core.NewSession(210, pipe, core.DefaultResourceBudget())
		if err == nil {
			sessCh <- sess
		}
		return sess, err
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+createTestToken(t, "jti-"+t.Name(), now, now.Add(time.Minute)), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	performHandshake(t, ctx, conn)
	r := &outputRig{sess: <-sessCh}
	t.Cleanup(func() { _ = r.sess.Close() })
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var pb framespb.Frame
			if proto.Unmarshal(data, &pb) != nil {
				continue
			}
			var ev wireEvent
			switch {
			case pb.GetAudio() != nil:
				a := pb.GetAudio()
				ev.bytes, ev.rate = len(a.Audio), a.SampleRate
				if len(a.Audio) >= 2 {
					ev.first = int16(binary.LittleEndian.Uint16(a.Audio))
				}
			case pb.GetMessage() != nil:
				var head struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(pb.GetMessage().Data), &head)
				ev.msg = head.Type
			default:
				continue
			}
			r.mu.Lock()
			r.log = append(r.log, ev)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *outputRig) events() []wireEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wireEvent(nil), r.log...)
}

// waitFor polls cond over the wire log every 5 ms for up to 5 s.
func (r *outputRig) waitFor(t *testing.T, what string, cond func([]wireEvent) bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(r.events()); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s; wire: %v", what, r.events())
		}
	}
}

// constPCM is d of PCM16 mono at rate with every sample v.
func constPCM(rate int, d time.Duration, v int16) []byte {
	b := make([]byte, 2*int(d*time.Duration(rate)/time.Second))
	for i := 0; i < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(v))
	}
	return b
}

// countAudio counts the audio frames whose first sample is first.
func countAudio(log []wireEvent, first int16) int {
	n := 0
	for _, ev := range log {
		if ev.msg == "" && ev.first == first {
			n++
		}
	}
	return n
}
```

Create `transport/ws/bot_interrupted_test.go`:

```go
package ws_test

import (
	"testing"
	"time"

	"govoice/core"
)

// A barge-in that cuts a reply off (the assistant's InterruptionFrame + FlushFrame) sends bot-interrupted
// once, after the paced writer dropped the reply's queued audio: of the cut reply only the frame that may
// already be on the socket can follow it, and the next reply plays after it.
func TestBargeInSendsBotInterruptedAfterTheCutReplysQueuedAudio(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	frame := func(g, unit, seq uint64, v int16, last bool) *core.OutputAudioFrame {
		return &core.OutputAudioFrame{Samples: constPCM(24000, 20*time.Millisecond, v), Encoding: core.EncodingPCM16LE,
			SampleRate: 24000, Channels: 1, Duration: 20 * time.Millisecond, Gen: g, Turn: "t", UnitID: unit, Seq: seq,
			IsFinal: last, TurnFinal: last}
	}
	posted := make(chan struct{})
	go func() { // the cut reply: 1 s of audio, posted at media pace by the session's output gate
		defer close(posted)
		for i := uint64(1); i <= 50; i++ {
			_ = r.sess.Post(frame(gen, 1, i, 1000, i == 50)) // after the flush these are stale and refused
		}
	}()
	r.waitFor(t, "the reply to start playing", func(log []wireEvent) bool { return countAudio(log, 1000) >= 3 })
	if err := r.sess.Post(&core.InterruptionFrame{Gen: gen}); err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Post(&core.FlushFrame{Gen: gen}); err != nil {
		t.Fatal(err)
	}
	<-posted
	for i := uint64(1); i <= 3; i++ { // the next reply
		if err := r.sess.Post(frame(gen+1, 2, i, -1000, i == 3)); err != nil {
			t.Fatal(err)
		}
	}
	r.waitFor(t, "the next reply", func(log []wireEvent) bool { return countAudio(log, -1000) == 3 })
	log := r.events()
	at := -1
	for i, ev := range log {
		if ev.msg == "bot-interrupted" {
			if at >= 0 {
				t.Fatalf("bot-interrupted twice: %v", log)
			}
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no bot-interrupted: %v", log)
	}
	before, after := countAudio(log[:at], 1000), countAudio(log[at:], 1000)
	if before < 3 || before+after >= 50 {
		t.Fatalf("the reply was not cut mid-play: %d frames before bot-interrupted, %d after", before, after)
	}
	if after > 1 {
		t.Fatalf("%d frames of the cut reply followed bot-interrupted (at most the one already on the socket may): %v", after, log)
	}
	if countAudio(log[:at], -1000) != 0 {
		t.Fatal("the next reply played before bot-interrupted")
	}
}
```

Run: `go test -race -count=1 -run TestBargeInSendsBotInterrupted ./transport/ws/`. Expected: `FAIL … no bot-interrupted`.

- [ ] **Step 4: Send it on the flush.** In `transport/ws/transport.go` replace

```go
	case core.FrameInterruption:
		// The following flush drives an output-derived stopped transition. The
		// pinned client still does not clear audio it already buffered.
		return e.PushDirection(f, dir)
```

with

```go
	case core.FrameInterruption:
		// An interruption that cut a reply off is followed by its flush, which
		// tells the client (bot-interrupted below); one with nothing to cut (a
		// typed turn while the bot is quiet) sends nothing.
		return e.PushDirection(f, dir)
```

and in `case core.FrameFlush:` replace the block's last line `		return e.PushDirection(f, dir)` (the one after `		})` that closes the `BeginFlush` callback; anchor: the text `				_ = session.Reenter(&core.FlushProcessedFrame{Gen: ff.Gen}, core.Upstream, session.TailIndex(), 0)\n			}\n		})\n		return e.PushDirection(f, dir)`) with

```go
				_ = session.Reenter(&core.FlushProcessedFrame{Gen: ff.Gen}, core.Upstream, session.TailIndex(), 0)
			}
		})
		// The flush is the barrier that cut a reply off (only the assistant's
		// handleInterruption emits one): tell the client, as Pipecat's
		// RTVIObserver does. BeginFlush has just dropped the reply's queued
		// audio, so only the audio frame already admitted to the socket (one
		// provider chunk) can follow this message. The pinned client ignores it
		// (docs/compatibility/playback-clear-decision.json); the protobuf
		// interruption frame is not sent: the pinned client rejects it.
		if msg, err := c.rtvi.FormatBotInterrupted(); err == nil {
			_ = c.WriteWireMessage(ctx, msg)
		}
		return e.PushDirection(f, dir)
```

Run: `gofmt -l transport && go test -race -count=20 -run 'TestBargeInSendsBotInterrupted|TestRTVI_BargeIn' ./transport/ws/`. Expected: `ok`.

Mutation checks: (1) delete the `FormatBotInterrupted` block → `no bot-interrupted` here and both server tests below fail; (2) write it twice → `bot-interrupted twice` here, `bot-interrupted 2 times, want 1` in both server tests (verified in a scratch copy); (3) move it into `case core.FrameInterruption:` → the server e2e below fails (`bot-interrupted with nothing cut off`). Where in the flush case the message is written is not observable by these tests (moving it before `BeginFlush` still passes: `beginWrite`'s stale check drops the queued frames either way); the order is argued in Decision 1 and R2, not tested.

- [ ] **Step 5: Wire log in the server e2e client.** In `server/flow_e2e_test.go`, in `type e2eCall struct` after `	fn        []fnEvent // llm-function-call-* messages, in arrival order` add `	wire      []string  // every RTVI message ("msg:<type>") and bot audio frame ("audio"), in arrival order`. In `read()` replace

```go
			case *ws.WireMessageFrame:
				if strings.Contains(m.Data, "bot-llm-text") {
```

with

```go
			case *ws.WireMessageFrame:
				var head struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(m.Data), &head)
				c.mu.Lock()
				c.wire = append(c.wire, "msg:"+head.Type)
				c.mu.Unlock()
				if strings.Contains(m.Data, "bot-llm-text") {
```

and replace `				c.audio = append(c.audio, audioArrival{at: time.Now(), peak: peak, dur: m.Duration})` with

```go
				c.audio = append(c.audio, audioArrival{at: time.Now(), peak: peak, dur: m.Duration})
				c.wire = append(c.wire, "audio")
```

- [ ] **Step 6: Write the failing server e2e.** Create `server/barge_in_e2e_test.go`:

```go
package server

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// wireLog is what the client received so far: "msg:<type>" per RTVI message, "audio" per bot audio frame.
func (c *e2eCall) wireLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.wire)
}

func countOf(log []string, entry string) int {
	n := 0
	for _, e := range log {
		if e == entry {
			n++
		}
	}
	return n
}

// indexFrom is the index of entry in log at or after from; -1 if absent or from < 0.
func indexFrom(log []string, entry string, from int) int {
	if from < 0 {
		return -1
	}
	for i := from; i < len(log); i++ {
		if log[i] == entry {
			return i
		}
	}
	return -1
}

// longReply is ten short sentences: with Google-like echo timing (bargeInRig) the first sentence is spoken
// within a second and the whole reply plays for several seconds, so a later turn cuts it off mid-play.
var longReply = strings.TrimSpace(strings.Repeat("This sentence keeps the reply going. ", 10))

// bargeInRig is the flow rig with the echo providers on Google-like timing: "instant" synthesis makes two
// 20 ms frames per piece, too little audio for a reply to still be playing when the next turn arrives.
func bargeInRig(t *testing.T) *flowRigE2E {
	return newFlowRigE2E(t, newProbe(), func(c *Config) { c.Providers.Echo.Mimic = "google" })
}

// within is eventually with its own deadline: on Google-like timing one barge-in test takes about 7 s, so the
// rig's 8 s limit is too close for the waits that span a whole reply.
func within(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
	}
}

// A typed turn that cuts a playing reply off: bot-interrupted arrives once, no audio of the cut reply follows
// it (at most the frame already on the socket) and the new reply comes after it. The first turn, sent while
// nothing played, sends none (its InterruptionFrame reaches the transport without a flush).
func TestCallBehaviorBargeInSendsBotInterruptedOnceAfterTheCutReplysAudio(t *testing.T) {
	r := bargeInRig(t)
	c := r.dial(behaviorFlow(t, r, "Talker", "talkvoice", ``))
	c.say(longReply)
	within(t, 20*time.Second, "the long reply to play", func() bool { return countOf(c.wireLog(), "audio") >= 10 })
	if n := countOf(c.wireLog(), "msg:bot-interrupted"); n != 0 {
		t.Fatalf("bot-interrupted with nothing cut off: %d", n)
	}
	c.say("barrier")
	c.waitReplies("barrier", 1)
	within(t, 20*time.Second, "the barrier reply to finish playing", func() bool {
		log := c.wireLog()
		return indexFrom(log, "msg:bot-stopped-speaking", indexFrom(log, "msg:bot-tts-started", indexFrom(log, "msg:bot-interrupted", 0))) >= 0
	})
	log := c.wireLog()
	if n := countOf(log, "msg:bot-interrupted"); n != 1 {
		t.Fatalf("bot-interrupted %d times, want 1: %v", n, log)
	}
	at := indexFrom(log, "msg:bot-interrupted", 0)
	next := indexFrom(log, "msg:bot-tts-started", at)
	if n := countOf(log[at:next], "audio"); n > 1 {
		t.Fatalf("%d audio frames between bot-interrupted and the next reply (at most the one already on the socket): %v", n, log[at:next+1])
	}
	if c.ended() {
		t.Fatal("the barge-in ended the call")
	}
}
```

Run: `cd server && go test -race -count=3 -tags xaispeech -run TestCallBehaviorBargeInSendsBotInterrupted .` — without Step 4 it fails (`bot-interrupted 0 times, want 1`, or the barrier wait times out); with Step 4: `ok`, about 7 s per run (measured in a scratch copy: 6.8 s; the 20 s `within` waits leave room on a loaded machine). (If Step 4 is already applied, stash it to see the failure.)

- [ ] **Step 7: The real pinned client takes the barge-in.** Create `tools/compat/test_rtvi_barge_in.js`:

```js
/**
 * test_rtvi_barge_in.js
 *
 * The released @pipecat-ai/client-js@1.13.1 with @pipecat-ai/websocket-transport@1.7.2 against a running
 * Switchboard server: a long reply is cut off by a typed turn while its audio plays. Prints, as one JSON line
 * after "EVENTS ", what the client handled in order ("msg:<type>" for every RTVI message, read from the
 * client's own debug line "[RTVI Message]", and "audio" for every chunk handed to the media manager), how
 * often it asked the media manager to interrupt playback, and every error it logged before disconnecting.
 * Driven by server/barge_in_client_e2e_test.go.
 *
 * Usage: node test_rtvi_barge_in.js --url <wsUrl> --token <token> --long <text> --barge <text>
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

async function until(cond, ms, what) {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

async function main() {
  const log = [];
  const errors = [];
  const said = [];
  let interrupts = 0;
  const debug = console.debug;
  const error = console.error;
  // The client logs every RTVI message it handles as console.debug('[RTVI Message]', msg) (its logger is at
  // DEBUG by default), types it does not recognise included; WebSocketTransport logs a frame it cannot
  // deserialize with console.error. Both are observed here, the packages are not changed.
  console.debug = (...a) => {
    if (a[0] === '[RTVI Message]' && a[1] && a[1].type) log.push('msg:' + a[1].type);
  };
  console.error = (...a) => errors.push(a.map(String).join(' '));
  const mediaManager = {
    initialize: async () => {}, connect: async () => {}, disconnect: async () => {},
    setUserAudioCallback: () => {}, setClientOptions: () => {}, tracks: () => ({}),
    userStartedSpeaking: async () => { interrupts++; }, bufferBotAudio: () => { log.push('audio'); },
  };
  const client = new PipecatClient({
    transport: new WebSocketTransport({ mediaManager }),
    enableMic: false,
    enableCam: false,
    callbacks: {
      onBotLlmText: (d) => said.push((d && d.text) || ''),
      onError: (e) => errors.push('onError ' + JSON.stringify(e)),
    },
  });
  let seen;
  try {
    await Promise.race([
      client.connect({ wsUrl: arg('--url'), token: arg('--token') }),
      new Promise((_, reject) => setTimeout(() => reject(new Error('connect timed out')), 5000)),
    ]);
    await client.sendText(arg('--long'));
    await until(() => log.filter((e) => e === 'audio').length >= 10, 20000, 'the long reply to play');
    await client.sendText(arg('--barge'));
    // The barge-in's flush stops the speaking state once, the barge-in reply's end stops it again.
    await until(() => said.join('').includes(arg('--barge')) && log.includes('msg:bot-interrupted')
      && log.filter((e) => e === 'msg:bot-stopped-speaking').length >= 2, 20000, 'the barge-in reply to finish');
    seen = errors.slice();
    await client.disconnect();
  } finally {
    console.debug = debug;
    console.error = error;
  }
  console.log('EVENTS ' + JSON.stringify({ log, interrupts, errors: seen }));
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
```

Create `server/barge_in_client_e2e_test.go`:

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

// The unmodified pinned client, connected to the real server, takes a barge-in without an error: it handles
// bot-interrupted once (as a type it does not recognise) after the cut reply's audio, and logs no frame it
// could not deserialize (the protobuf interruption frame would be one). It does not clear its player on it:
// that limitation is recorded in docs/compatibility/playback-clear-decision.json.
func TestCallBehaviorPipecatClientTakesABargeInWithoutErrors(t *testing.T) {
	compat := filepath.Join("..", "tools", "compat")
	if _, err := os.Stat(filepath.Join(compat, "node_modules", "@pipecat-ai", "client-js")); err != nil {
		t.Fatalf("the pinned Pipecat client is not installed (run: npm ci --prefix tools/compat): %v", err)
	}
	r := bargeInRig(t)
	flowID := behaviorFlow(t, r, "Talker", "talkvoice", ``)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "node", filepath.Join(compat, "test_rtvi_barge_in.js"),
		"--url", "ws"+strings.TrimPrefix(r.ts.URL, "http"), "--token", r.voiceToken(flowID),
		"--long", longReply, "--barge", "barrier").CombinedOutput()
	if err != nil {
		t.Fatalf("client-js run failed: %v\n%s", err, out)
	}
	_, line, ok := strings.Cut(string(out), "EVENTS ")
	if !ok {
		t.Fatalf("no EVENTS line:\n%s", out)
	}
	line, _, _ = strings.Cut(line, "\n")
	var got struct {
		Log        []string `json:"log"`
		Interrupts int      `json:"interrupts"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("EVENTS %s: %v", line, err)
	}
	if len(got.Errors) != 0 {
		t.Fatalf("the pinned client logged errors: %q", got.Errors)
	}
	if n := countOf(got.Log, "msg:bot-interrupted"); n != 1 {
		t.Fatalf("the client handled bot-interrupted %d times, want 1: %v", n, got.Log)
	}
	if at := slices.Index(got.Log, "msg:bot-interrupted"); countOf(got.Log[:at], "audio") < 10 {
		t.Fatalf("bot-interrupted came before the cut reply played: %v", got.Log)
	}
	t.Logf("the pinned client asked its player to interrupt %d time(s) (0 is the recorded limitation)", got.Interrupts)
}
```

Run: `cd server && go test -race -count=1 -tags xaispeech -run 'TestCallBehaviorPipecatClient' .`. Expected: `ok` (about 7 s). Mutation check: additionally write the field-5 bytes `[]byte{0x2a, 0x00}` with `c.conn.Write(ctx, websocket.MessageBinary, …)` next to the `bot-interrupted` write → this test fails with `the pinned client logged errors: ["Failed to deserialize incoming message Error: Unknown frame kind"]`.

- [ ] **Step 8: Rewrite the probe so it observes the real client (RED first).** Replace `tools/compat/probe_playback_clear.js` with:

```js
/**
 * probe_playback_clear.js
 *
 * The hard playback-clear probe, against the exact unmodified released packages
 * @pipecat-ai/client-js@1.13.1 and @pipecat-ai/websocket-transport@1.7.2.
 *
 * Every wire signal a server could send at barge-in is delivered through the real
 * WebSocketTransport message listener (installed by initializeWebsocket(): deserialize, then
 * bufferBotAudio or PipecatClient.handleMessage). The media manager is a Proxy that records every
 * method the transport or the client calls on it; playback is cleared only by userStartedSpeaking()
 * (WavMediaManager: _wavStreamPlayer.interrupt()). Every console.error and console.warn line is
 * recorded too, so a client that fails differently does not match the recording.
 *
 * The observed facts must equal the ones recorded under "probes" in
 * docs/compatibility/playback-clear-decision.json: a pinned client that behaves differently fails
 * `make verify` until the decision is re-recorded from this output.
 */
if (typeof globalThis.window === 'undefined') {
  globalThis.window = { addEventListener: () => {}, removeEventListener: () => {} };
}
if (typeof globalThis.navigator === 'undefined') {
  globalThis.navigator = { mediaDevices: { addEventListener: () => {}, removeEventListener: () => {} } };
}

const fs = require('fs');
const path = require('path');

const { ProtobufFrameSerializer, WebSocketTransport } = require('@pipecat-ai/websocket-transport');
const { PipecatClient } = require('@pipecat-ai/client-js');

const DECISION_PATH = path.join(__dirname, '../../docs/compatibility/playback-clear-decision.json');
const FAILED_VERDICT = 'UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT';
const CLEARS_VERDICT = 'PINNED_CLIENT_CLEARS_BUFFERED_PLAYBACK';
const RELAX_DECISION = 'relax-unmodified-client-playback-clear';

// Each signal's bytes as a server puts them on the socket.
const SIGNALS = {
  // Pipecat's ProtobufFrameSerializer sends InterruptionFrame as Frame.interruption (field 5, empty).
  protobuf_interruption_field_5: () => new Uint8Array([0x2a, 0x00]),
  // Pipecat's RTVIObserver sends this on every InterruptionFrame; Switchboard on a barge-in that cut a reply.
  rtvi_bot_interrupted: (s) => s.serializeMessage({ id: 'probe-1', label: 'rtvi-ai', type: 'bot-interrupted' }),
  rtvi_user_started_speaking: (s) => s.serializeMessage({ id: 'probe-2', label: 'rtvi-ai', type: 'user-started-speaking' }),
};

const tick = () => new Promise((r) => setTimeout(r, 10));

async function probeSignal(bytes) {
  const calls = [];
  const logged = [];
  let delivered = false;
  const mediaManager = new Proxy({}, {
    get: (_, name) => {
      if (typeof name !== 'string' || name === 'then') return undefined;
      return (...args) => {
        calls.push(name);
        return name === 'tracks' ? {} : Promise.resolve();
      };
    },
  });
  const transport = new WebSocketTransport({ mediaManager, wsUrl: 'ws://127.0.0.1:9' });
  new PipecatClient({ transport, callbacks: {} }); // makes handleMessage the transport's message handler
  const ws = transport.initializeWebsocket(); // the socket wrapper and its listeners; nothing is dialed
  calls.length = 0; // setUserAudioCallback/setClientOptions at construction are not reactions to the signal
  const debug = console.debug;
  const error = console.error;
  const warn = console.warn;
  console.debug = (...a) => { if (a[0] === '[RTVI Message]') delivered = true; };
  console.error = (...a) => logged.push('error: ' + a.map(String).join(' '));
  console.warn = (...a) => logged.push('warn: ' + a.map(String).join(' '));
  try {
    ws.emit('message', new Blob([bytes]));
    // The listener awaits blob.arrayBuffer(): wait until it delivered or logged, then once more for a clear.
    for (let i = 0; i < 100 && !delivered && logged.length === 0; i++) await tick();
    await tick();
  } finally {
    console.debug = debug;
    console.error = error;
    console.warn = warn;
  }
  return {
    delivered_to_client: delivered,
    media_manager_calls: calls,
    logged,
  };
}

async function runPlaybackClearProbe() {
  console.log('=== Playback-clear probe (pinned unmodified client) ===');
  const serializer = new ProtobufFrameSerializer();
  const observed = {};
  for (const [name, make] of Object.entries(SIGNALS)) {
    observed[name] = await probeSignal(make(serializer));
    console.log(name + ': ' + JSON.stringify(observed[name]));
  }
  const cleared = Object.values(observed).some((p) => p.media_manager_calls.includes('userStartedSpeaking'));
  const verdict = cleared ? CLEARS_VERDICT : FAILED_VERDICT;
  console.log('verdict: ' + verdict);
  const decision = readProductDecision(verdict, observed);
  if (!decision.ok) {
    console.error(decision.reason);
    console.error('Playback-clear gate failed: re-record docs/compatibility/playback-clear-decision.json from this output.');
    process.exitCode = 2;
    return { verdict, observed };
  }
  if (!cleared) {
    console.error('PLAYBACK_CLEAR_REQUIREMENT_FAILED: pinned unmodified client cannot clear buffered playback.');
    console.error('Continuing only under recorded product decision ' + decision.decision + ' (this is not a passing clear).');
  }
  return { verdict, observed, product_decision: decision.decision };
}

function readProductDecision(verdict, observed) {
  let doc;
  try {
    doc = JSON.parse(fs.readFileSync(DECISION_PATH, 'utf8'));
  } catch (err) {
    return { ok: false, reason: 'cannot read product decision ' + DECISION_PATH + ': ' + err.message };
  }
  if (doc.probe_verdict !== verdict) {
    return { ok: false, reason: `recorded probe_verdict ${doc.probe_verdict} differs from the observed ${verdict}` };
  }
  if (verdict === FAILED_VERDICT && (doc.decision !== RELAX_DECISION || doc.client_cleared_buffered_playback !== false)) {
    return { ok: false, reason: `an unresolved clear needs decision ${RELAX_DECISION} with client_cleared_buffered_playback=false` };
  }
  const recorded = doc.probes || {};
  if (Object.keys(recorded).length !== Object.keys(observed).length) {
    return { ok: false, reason: 'recorded probes name other signals than this probe sends' };
  }
  for (const [name, got] of Object.entries(observed)) {
    if (JSON.stringify(recorded[name]) !== JSON.stringify(got)) {
      return { ok: false, reason: `recorded probe ${name} ${JSON.stringify(recorded[name])} differs from the observed ${JSON.stringify(got)}` };
    }
  }
  return { ok: true, decision: doc.decision };
}

if (require.main === module) {
  runPlaybackClearProbe().catch((err) => {
    console.error('Probe failed unexpectedly:', err);
    process.exit(1);
  });
}

module.exports = { runPlaybackClearProbe };
```

Run: `node tools/compat/probe_playback_clear.js; echo "exit=$?"`. Expected (RED, old decision file): the three observation lines

```
protobuf_interruption_field_5: {"delivered_to_client":false,"media_manager_calls":[],"logged":["error: Failed to deserialize incoming message Error: Unknown frame kind"]}
rtvi_bot_interrupted: {"delivered_to_client":true,"media_manager_calls":[],"logged":[]}
rtvi_user_started_speaking: {"delivered_to_client":true,"media_manager_calls":[],"logged":[]}
```

(verified in a scratch copy against the pinned packages, 2026-10-18)

then `recorded probes name other signals than this probe sends` and `exit=2`. If the observations differ, stop: the evidence in Decision 1 is wrong and the human must be told.

- [ ] **Step 9: Re-record the decision.** Replace `docs/compatibility/playback-clear-decision.json` with:

```json
{
  "probe_verdict": "UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT",
  "decision": "relax-unmodified-client-playback-clear",
  "client_cleared_buffered_playback": false,
  "client_packages": {
    "@pipecat-ai/client-js": "1.13.1",
    "@pipecat-ai/websocket-transport": "1.7.2"
  },
  "probes": {
    "protobuf_interruption_field_5": {
      "delivered_to_client": false,
      "media_manager_calls": [],
      "logged": ["error: Failed to deserialize incoming message Error: Unknown frame kind"]
    },
    "rtvi_bot_interrupted": {
      "delivered_to_client": true,
      "media_manager_calls": [],
      "logged": []
    },
    "rtvi_user_started_speaking": {
      "delivered_to_client": true,
      "media_manager_calls": [],
      "logged": []
    }
  },
  "server_barge_in_emission": {
    "rtvi_bot_interrupted": "sent once per barge-in that cuts a reply off (the transport's FlushFrame), after the paced writer dropped that reply's queued audio; at most the one audio frame already admitted to the socket (one provider chunk) follows it; Pipecat's RTVIObserver sends it on every InterruptionFrame",
    "protobuf_interruption_field_5": "not sent: the pinned transport rejects it and logs Failed to deserialize incoming message on every barge-in",
    "evidence": "transport/ws/bot_interrupted_test.go, server/barge_in_e2e_test.go, server/barge_in_client_e2e_test.go"
  },
  "v1_server_side_residual_playback": {
    "bounded_by_pacing": true,
    "maximum_queued_duration_ms": 100,
    "maximum_queued_bytes": 4800,
    "format": "24-kHz PCM16 mono",
    "scope": "server-side queued output only; this does not bound audio already delivered to or buffered by the client"
  },
  "v2_output_flush_gate": {
    "status": "unresolved",
    "pass_condition": "A compatible client/API or another tested mechanism must demonstrate that the actual client playback buffer is cleared.",
    "decision_is_a_pass": false
  },
  "evidence_command": "node tools/compat/probe_playback_clear.js"
}
```

Run: `node tools/compat/probe_playback_clear.js; echo "exit=$?"`. Expected: the three observation lines, `verdict: UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`, `PLAYBACK_CLEAR_REQUIREMENT_FAILED: …`, `Continuing only under recorded product decision relax-unmodified-client-playback-clear (this is not a passing clear).`, `exit=0`. Mutation check: add `"userStartedSpeaking"` to one recorded `media_manager_calls` (or empty one `logged`) → `exit=2`.

- [ ] **Step 10: Docs.** `docs/pipecat-compatibility.md`: in the bullet that starts `- The server emits V1 RTVI lifecycle`, replace ``  `bot-tts-text`, `bot-interrupted` and `vad-*` messages are not emitted.`` with `` `bot-tts-text` and `vad-*` messages are not emitted. A barge-in that cuts a reply off sends `bot-interrupted` (Pipecat's `RTVIObserver` message, no data) once, after the paced writer dropped that reply's queued audio; at most the one audio frame already admitted to the socket (one provider chunk, which can be longer than 20 ms) can follow it. Pipecat's `RTVIObserver` sends it on every interruption, whether or not the bot was speaking; here it is sent only when the assistant's barrier ran. The protobuf `interruption` frame (field 5) is not sent: the pinned transport rejects it and logs an error on every barge-in.``. Replace the bullet that starts `- The hard probe against the exact unmodified packages found` with:

```markdown
- The hard probe (`node tools/compat/probe_playback_clear.js`, part of `make verify`) delivers each barge-in signal through the pinned `WebSocketTransport`'s own message listener with a media manager that records every method called on it, and records every `console.error`/`console.warn` line: protobuf interruption field 5 never reaches the client (`Unknown frame kind`, logged as `Failed to deserialize incoming message`); `bot-interrupted` reaches `PipecatClient.handleMessage`, which ignores it (`Unrecognized message type`); `user-started-speaking` fires the app callback only. None of them makes the transport or the client call any media manager method (`userStartedSpeaking()` would interrupt the player), the only path that interrupts the player (besides `disconnect()`). The verdict is `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`; the probe fails `make verify` if these observations ever stop matching the recorded decision. `server/barge_in_client_e2e_test.go` runs the pinned client through a real barge-in.
```

`docs/pipecat-parity-matrix.md` row `| RTVI wire (pinned client) |`: replace its notes cell `Separate surface (`docs/pipecat-compatibility.md`). The unmodified client cannot clear already-buffered playback on a wire event.` with `Separate surface (`docs/pipecat-compatibility.md`). Barge-in sends `bot-interrupted` (Pipecat `BotInterruptedMessage`) after the cut reply's queued audio; the protobuf `interruption` frame is not sent (declared difference: the pinned transport rejects it). The unmodified client cannot clear already-buffered playback on any wire event (probe).` and its evidence cell to `` `tools/compat/test_rtvi_client.js`, `tools/compat/verify_fixtures.js`, `tools/compat/probe_playback_clear.js`, `server/barge_in_client_e2e_test.go`, `transport/ws/bot_interrupted_test.go` ``. `docs/output-pacing-and-flush.md`: replace the last paragraph (starts `The server guarantee ends at bytes already accepted by the socket.`) with `The server guarantee ends at bytes already accepted by the socket. After the flush the transport sends RTVI `bot-interrupted` (at most the one audio frame already admitted to the socket, one provider chunk, follows it). The pinned unmodified browser client ignores that message and still cannot clear buffered playback; `docs/compatibility/playback-clear-decision.json` therefore remains unresolved and is not a pass.`

- [ ] **Step 11: Gate.** Run the gate block with `4.1-s11` (verify, e2e-echo, offline-test). `make verify` must print the three observation lines and the recorded-decision line.

- [ ] **Step 12: Commit.**

```bash
git add transport/rtvi/rtvi.go transport/rtvi/bot_interrupted_test.go transport/ws/transport.go transport/ws/output_rig_test.go transport/ws/bot_interrupted_test.go \
  server/flow_e2e_test.go server/barge_in_e2e_test.go server/barge_in_client_e2e_test.go tools/compat/test_rtvi_barge_in.js tools/compat/probe_playback_clear.js \
  docs/compatibility/playback-clear-decision.json docs/pipecat-compatibility.md docs/pipecat-parity-matrix.md docs/output-pacing-and-flush.md
git commit -m "feat(ws): barge-in sends RTVI bot-interrupted after the cut reply's audio

The transport writes bot-interrupted when a barge-in's flush has dropped
the reply's queued audio, as Pipecat's RTVIObserver does. The protobuf
interruption frame is not sent: the pinned transport rejects it. The
playback-clear probe now feeds each signal through the pinned client's
real message listener and must match the recorded decision, which stays
relax-unmodified-client-playback-clear: no wire signal clears its player.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.2: a synchronous LLM error fails over; a refused task loses the turn, not the call (A9)

**Files:**
- Modify: `core/llm_failover.go`, `server/llm_failover.go`, `providers/openai/llm.go`, `providers/xai/llm.go`, `providers/google/llm.go`, `docs/pipecat-parity-matrix.md`
- Create: `server/llm_sync_failover_e2e_test.go`, `providers/openai/refused_task_test.go`, `providers/xai/refused_task_test.go`, `providers/google/refused_task_test.go`
- Test (append): `core/llm_failover_test.go`, `server/llm_failover_test.go`

**Interfaces:**
- Consumes: `failoverGen`, `mayFailOver`, `failoverLocked`, `stopTimerLocked`, `Emitter`, `core.ErrTooManyTasks`, `core.ErrSessionClosed`, the rig `failoverRig`/`newFailoverRig`/`fake`/`errBoom`, `flowRigE2E`, `r.s.registry.factories`, `r.s.handleMetrics`.
- Produces: unexported `pushTracker{Emitter; pushed bool; downstreamErr error}`, `(*LLMFailover).syncFailure(g, err, um, dir, e, pushed) error`; test doubles `syncFailLLM`, `refuseUser`, `syncDownLLM`, `spentSession` (one per provider package).

- [ ] **Step 1: Write the failing core tests.** Append to `core/llm_failover_test.go`:

```go
// syncFailLLM refuses a user message synchronously, as a provider whose request cannot start does;
// push says whether it forwarded the message first (every production LLM does).
type syncFailLLM struct {
	err   error
	push  bool
	calls atomic.Int32
}

func (p *syncFailLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.UserMessageFrame); !ok {
		return e.PushDirection(f, dir)
	}
	p.calls.Add(1)
	if p.push {
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
	}
	return p.err
}

func TestSyncErrorFromThePrimaryFailsOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		push bool
	}{{"after forwarding the turn", true}, {"before forwarding the turn", false}} {
		t.Run(tc.name, func(t *testing.T) {
			primary, backup := &syncFailLLM{err: errBoom, push: tc.push}, fake([]string{"hel", "lo"}, 0, nil)
			r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
				{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}}})
			r.say(t, "hi")
			r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
			users, deltas, finals, errs := r.summary()
			if users != 1 || deltas != "hello" || finals != 1 || errs != 0 {
				t.Fatalf("users=%d deltas=%q finals=%d errs=%d", users, deltas, finals, errs)
			}
			if err := r.sess.Err(); err != nil {
				t.Fatalf("a synchronous provider error failed the session: %v", err)
			}
			if got := r.switched(); len(got) != 1 || got[0] != "a>b:server_error" {
				t.Fatalf("switches %v", got)
			}
		})
	}
}

// The session's own task budget is not a provider outage: no jump, the error stays the session's.
func TestLocalSyncErrorNeverFailsOver(t *testing.T) {
	primary, backup := &syncFailLLM{err: core.ErrTooManyTasks, push: true}, fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates: []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		Classify: func(err error) string {
			if errors.Is(err, core.ErrTooManyTasks) {
				return ""
			}
			return "server_error"
		},
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return r.sess.Err() != nil })
	if !errors.Is(r.sess.Err(), core.ErrTooManyTasks) || backup.Started.Load() != 0 || len(r.switched()) != 0 {
		t.Fatalf("err=%v backupStarted=%d switches=%v", r.sess.Err(), backup.Started.Load(), r.switched())
	}
}

var errRefused = errors.New("a later stage refused the turn")

// refuseUser is a stage after the LLM that refuses every user message.
type refuseUser struct{}

func (refuseUser) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.UserMessageFrame); ok {
		return errRefused
	}
	return e.PushDirection(f, dir)
}

// A later stage refusing the user message is not the candidate's failure (pin: true before and after).
func TestADownstreamRefusalIsNotTheCandidatesFailure(t *testing.T) {
	backup := fake([]string{"x"}, 0, nil)
	w, err := core.NewLLMFailover(core.LLMFailoverConfig{Classify: func(error) string { return "server_error" },
		Candidates: []core.LLMCandidate{{Provider: "a", Proc: fake([]string{"never"}, 0, nil)}, {Provider: "b", Proc: backup}}})
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(w, refuseUser{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(901, pipe, core.DefaultResourceBudget())
	if err != nil || sess.Start() != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if err := sess.Post(&core.UserMessageFrame{Turn: "t1", Content: "hi", Gen: sess.CurrentGeneration(), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(2 * time.Second); sess.Err() == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the refusal did not reach the session")
		}
	}
	if !errors.Is(sess.Err(), errRefused) || backup.Started.Load() != 0 {
		t.Fatalf("err=%v backupStarted=%d", sess.Err(), backup.Started.Load())
	}
}
```

Run: `(cd core && go test -race -count=1 -run 'TestSyncErrorFromThePrimary|TestLocalSyncError|TestADownstreamRefusal' .)`. Expected: `TestSyncErrorFromThePrimaryFailsOver/*` FAIL (`users=1 deltas="" …`, the session failed with `503 upstream`); the other two pass (pins).

- [ ] **Step 2: Classify a synchronous error.** In `core/llm_failover.go` replace the last statement of `begin`, `	return w.cfg.Candidates[g.order[0]].Proc.ProcessFrame(ctx, um, dir, e)`, with:

```go
	pe := &pushTracker{Emitter: e}
	err := w.cfg.Candidates[g.order[0]].Proc.ProcessFrame(ctx, um, dir, pe)
	if err == nil || pe.downstreamErr != nil {
		return err // started, or a later stage refused the user message: not this candidate's failure
	}
	return w.syncFailure(g, err, um, dir, e, pe.pushed)
}

// pushTracker records whether a candidate forwarded the user message and what
// the stages after this one answered.
type pushTracker struct {
	Emitter
	pushed        bool
	downstreamErr error
}

func (p *pushTracker) Push(f Frame) error { return p.note(f, p.Emitter.Push(f)) }

func (p *pushTracker) PushDirection(f Frame, d Direction) error {
	return p.note(f, p.Emitter.PushDirection(f, d))
}

func (p *pushTracker) note(f Frame, err error) error {
	if _, ok := f.(*UserMessageFrame); ok {
		p.pushed, p.downstreamErr = true, err
	}
	return err
}

// syncFailure handles the first candidate refusing the user message
// synchronously (its request could not start). Like an error frame before any
// output, it moves to the next candidate when mayFailOver allows: a provider
// failure does; the session's own limits do not (server llmClassify maps
// ErrTooManyTasks and ErrSessionClosed to ""), and then the error is the
// session's, as before. The user message is forwarded here when the failed
// candidate had not forwarded it.
func (w *LLMFailover) syncFailure(g *failoverGen, err error, um *UserMessageFrame, dir Direction, e Emitter, pushed bool) error {
	w.mu.Lock()
	if w.cur != g || g.done || g.pos != 0 || !w.mayFailOver(g, err) {
		if w.cur == g && !g.done {
			g.done = true
			stopTimerLocked(g)
		}
		w.mu.Unlock()
		return err
	}
	if ferr := w.failoverLocked(g, g.order[0], err, func() error { return err }); ferr != nil {
		return ferr // every candidate failed synchronously: the session's error, as before
	}
	if !pushed {
		return e.PushDirection(um, dir)
	}
	return nil
}
```

(The `}` that closed `begin` is now the one after `return w.syncFailure(…)`; delete the old closing brace so the file compiles.) Run: `(cd core && gofmt -l . && go test -race -count=20 -run 'Failover|SyncError|Downstream' .)`. Expected: `ok`. Mutation checks: drop `pe.downstreamErr != nil` → `TestADownstreamRefusal…` fails (`backupStarted=1`); drop the `if !pushed` push → `before forwarding the turn` fails (`users=0`).

- [ ] **Step 3: Write the failing server classification rows.** In `server/llm_failover_test.go`, inside `TestLLMClassify`'s table after `		{errors.New("503"), flow.ClassServer},` add:

```go
		{fmt.Errorf("openai llm: %w", core.ErrTooManyTasks), ""}, // the session's own task budget
		{core.ErrSessionClosed, ""},                               // teardown
```

Run: `(cd server && go test -count=1 -run TestLLMClassify .)`. Expected: FAIL `llmClassify(openai llm: core: async task budget exceeded) = "server_error", want ""`.

- [ ] **Step 4: Local errors never fail over.** In `server/llm_failover.go` replace the body of `llmClassify` with:

```go
	var ce interface{ ClientError() bool }
	switch {
	case errors.Is(err, core.ErrTooManyTasks), errors.Is(err, core.ErrSessionClosed):
		return "" // the session's own limits or teardown: another model would not help
	case errors.Is(err, core.ErrFirstTokenTimeout):
		return flow.ClassTimeout
	}
	return flow.ClassOf(err, resilience.Verdict{ClientError: errors.As(err, &ce) && ce.ClientError()})
```

Run: `(cd server && go test -count=1 -run 'TestLLMClassify|TestBuildLLM|TestCallPipelineFailover' .)`. Expected: `ok`.

- [ ] **Step 5: Write the failing provider tests.** Create `providers/openai/refused_task_test.go`:

```go
package openai

import (
	"context"
	"errors"
	"testing"

	"govoice/core"
)

// spentSession is a session whose task budget is used up: every Go is refused.
type spentSession struct{ *fakeSession }

func (spentSession) Go(func(context.Context)) error { return core.ErrTooManyTasks }

// A refused generation task loses that turn (one non-fatal error, re-entered so a failover stage sees it),
// not the call.
func TestRefusedTaskLosesTheTurnNotTheCall(t *testing.T) {
	p, err := NewProcessor(Config{BaseURL: "http://127.0.0.1:1", AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	s := spentSession{newSession()}
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"}, core.Downstream, sink{}); err != nil {
		t.Fatalf("a spent task budget failed the call: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 1 {
		t.Fatalf("re-entered %d frames, want one error", len(s.got))
	}
	ef, ok := s.got[0].(*core.ErrorFrame)
	if !ok || ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || ef.Turn != "t1" || ef.Gen != 1 {
		t.Fatalf("frame %+v", s.got[0])
	}
}
```

Create `providers/xai/refused_task_test.go`:

```go
package xai

import (
	"context"
	"errors"
	"testing"

	"govoice/core"
)

// spentSession is a session whose task budget is used up: every Go is refused.
type spentSession struct{ *mockSessionController }

func (spentSession) Go(func(context.Context)) error { return core.ErrTooManyTasks }

func TestRefusedTaskLosesTheTurnNotTheCall(t *testing.T) {
	p, err := NewProcessor(Config{Endpoint: "http://127.0.0.1:1/v1/responses", APIKey: "xai-test-key-1234567890"})
	if err != nil {
		t.Fatal(err)
	}
	s := spentSession{newMockSessionController(1, 1)}
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatalf("a spent task budget failed the call: %v", err)
	}
	s.reenteredMu.Lock()
	defer s.reenteredMu.Unlock()
	if len(s.reentered) != 1 {
		t.Fatalf("re-entered %d frames, want one error", len(s.reentered))
	}
	ef, ok := s.reentered[0].(*core.ErrorFrame)
	if !ok || ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || ef.Turn != "t1" || ef.Gen != 1 {
		t.Fatalf("frame %+v", s.reentered[0])
	}
}
```

Create `providers/google/refused_task_test.go`:

```go
package google

import (
	"context"
	"errors"
	"sync"
	"testing"

	"govoice/core"
)

// spentSession is a session whose task budget is used up: every Go is refused.
type spentSession struct {
	mu  sync.Mutex
	got []core.Frame
}

func (*spentSession) ID() uint64                     { return 1 }
func (*spentSession) Context() context.Context       { return context.Background() }
func (*spentSession) CurrentGeneration() uint64      { return 1 }
func (*spentSession) IncrementGeneration() uint64    { return 2 }
func (*spentSession) Go(func(context.Context)) error { return core.ErrTooManyTasks }
func (s *spentSession) Reenter(f core.Frame, _ core.Direction, _ int, _ uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, f)
	return nil
}

func TestRefusedTaskLosesTheTurnNotTheCall(t *testing.T) {
	cfg := DefaultLLMConfig()
	cfg.Client = &mockLLMClient{}
	p, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &spentSession{}
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"}, core.Downstream, probeEmitter{}); err != nil {
		t.Fatalf("a spent task budget failed the call: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 1 {
		t.Fatalf("re-entered %d frames, want one error", len(s.got))
	}
	ef, ok := s.got[0].(*core.ErrorFrame)
	if !ok || ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || ef.Turn != "t1" || ef.Gen != 1 {
		t.Fatalf("frame %+v", s.got[0])
	}
}
```

Run: `for m in openai xai google; do (cd providers/$m && go test -count=1 -run TestRefusedTaskLosesTheTurnNotTheCall .); done`. Expected: each FAIL `a spent task budget failed the call: core: async task budget exceeded`.

- [ ] **Step 6: Absorb a refused task in the providers.** `providers/openai/llm.go`: replace `	return s.Go(func(sessionCtx context.Context) {` (in `handleUserTurn`) with `	err := s.Go(func(sessionCtx context.Context) {`, and replace the end of `handleUserTurn`

```go
				Requests: core.Int64Ptr(1),
			}, core.Downstream, p.index, gen)
		}
	})
}
```

with

```go
				Requests: core.Int64Ptr(1),
			}, core.Downstream, p.index, gen)
		}
	})
	if errors.Is(err, core.ErrTooManyTasks) { // the session's task budget: lose this turn, not the call
		cancel()
		p.Errors.Add(1)
		// Re-entered, not pushed: under LLMFailover a fallback's emitter swallows,
		// and the wrapper must see the error to end the generation.
		_ = s.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index, gen)
		return nil
	}
	return err
}
```

`providers/xai/llm.go`: replace `	return s.Go(func(sessionCtx context.Context) {` with `	err = s.Go(func(sessionCtx context.Context) {` (`err` is already declared by `providerTools, err := MapToolDefinitions(…)`), and replace

```go
				_ = s.Reenter(usageFrame, core.Downstream, p.index, gen)
			}
		}
	})
}
```

with

```go
				_ = s.Reenter(usageFrame, core.Downstream, p.index, gen)
			}
		}
	})
	if errors.Is(err, core.ErrTooManyTasks) { // the session's task budget: lose this turn, not the call
		cancel()
		p.Errors.Add(1)
		// Re-entered, not pushed: under LLMFailover a fallback's emitter swallows.
		_ = s.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index, gen)
		return nil
	}
	return err
}
```

`providers/google/llm.go`: replace `	return p.session.Go(func(taskCtx context.Context) {` with `	err := p.session.Go(func(taskCtx context.Context) {` and replace

```go
			_ = p.session.Reenter(usageFrame, core.Downstream, p.index+1, gen)
		}
	})
}
```

with

```go
			_ = p.session.Reenter(usageFrame, core.Downstream, p.index+1, gen)
		}
	})
	if errors.Is(err, core.ErrTooManyTasks) { // the session's task budget: lose this turn, not the call
		cancel()
		p.Errors.Add(1)
		// Re-entered, not pushed: under LLMFailover a fallback's emitter swallows.
		_ = p.session.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index+1, gen)
		return nil
	}
	return err
}
```

If `go vet` reports `err` redeclared in Google's `handleUserTurn` (an earlier `err :=` in the same scope), use `err = ` there instead. Run: `for m in openai xai google echo; do (cd providers/$m && gofmt -l . && go vet ./... && go test -race -count=1 ./...) || break; done`. Expected: `ok` for every package.

- [ ] **Step 7: Write the server e2e.** Create `server/llm_sync_failover_e2e_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"govoice/core"
	"govoice/providers/google"
	"govoice/server/flow"
)

// syncDownLLM is a primary whose request cannot start: it forwards the user turn, then reports a provider
// outage synchronously, as a processor whose client refuses the request does.
type syncDownLLM struct{ calls atomic.Int32 }

func (p *syncDownLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.UserMessageFrame); !ok {
		return e.PushDirection(f, dir)
	}
	p.calls.Add(1)
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	return &google.ProviderError{Capability: "llm", BaseErr: google.ErrProviderUnavailable, SafeMessage: "injected outage",
		StatusCode: http.StatusServiceUnavailable, Retryable: true}
}

// A primary LLM failing synchronously on every turn: the fallback answers each turn and the call lives on.
func TestCallBehaviorASynchronousLLMFailureFailsOverAndTheCallSurvives(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	down := &syncDownLLM{}
	healthy := r.s.registry.factories["echo.llm"]
	r.s.registry.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if spec.Model == "downmodel" {
			return down, nil
		}
		return healthy(cfg, spec)
	})
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Failover", Config: json.RawMessage(
		`{"stt":{"provider":"echo","model":"s"},` +
			`"llm":{"provider":"echo","model":"downmodel","system_prompt":"Failover","fallbacks":[{"provider":"echo","model":"m"}]},` +
			`"tts":{"provider":"echo","voice":"failvoice"}}`)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("first question")
	c.waitReplies("first question", 1)
	c.say("second question")
	c.waitReplies("second question", 1)
	if a, b := c.replies("first question"), c.replies("second question"); a != 1 || b != 1 {
		t.Fatalf("answers: first %d, second %d, want each exactly once", a, b)
	}
	if c.ended() {
		t.Fatal("a synchronous LLM failure ended the call")
	}
	if n := down.calls.Load(); n != 2 {
		t.Fatalf("the primary was tried %d times, want once per turn", n)
	}
	rec := httptest.NewRecorder()
	r.s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := `voice_llm_failover_total{from="echo",to="echo",class="server_error"} 2`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("metrics lack %s", want)
	}
}
```

Run: `(cd server && go test -race -count=3 -tags xaispeech -run TestCallBehaviorASynchronousLLMFailure .)`. Expected: `ok`. Mutation check: revert Step 2 → `timed out waiting for 1 x "first question"` (the session failed and the call ended).

- [ ] **Step 8: Docs.** `docs/pipecat-parity-matrix.md` row `| Failover |`: in the notes cell after `LLM switches only before the first token and never after a tool ran;` insert ` a primary that refuses the request synchronously (a provider error before any output) switches the same way, while the session's own task budget never does (that turn ends with a non-fatal error and the call continues);` and add `` `server/llm_sync_failover_e2e_test.go` `` to the evidence cell.

- [ ] **Step 9: Gate.** Run the gate block with `4.2-s9` (verify, e2e-echo, offline-test, provider-matrix-dry).

- [ ] **Step 10: Commit.**

```bash
git add core/llm_failover.go core/llm_failover_test.go server/llm_failover.go server/llm_failover_test.go server/llm_sync_failover_e2e_test.go \
  providers/openai/llm.go providers/openai/refused_task_test.go providers/xai/llm.go providers/xai/refused_task_test.go \
  providers/google/llm.go providers/google/refused_task_test.go docs/pipecat-parity-matrix.md
git commit -m "fix(core): a synchronous LLM error fails over; a refused task loses the turn

LLMFailover classifies an error the first candidate returns from
ProcessFrame like an error frame before any output, so a provider that
cannot start a request falls back instead of failing the call. The LLM
providers turn a refused session task into a non-fatal error for that
turn, and llmClassify never fails over on the session's own limits.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.3: output audio is resampled with the ratio of its own rate (A10)

**Files:**
- Modify: `transport/ws/transport.go`, `server/tts_failover.go` (comment), `core/tts_failover.go` (comment)
- Create: `transport/ws/output_resample_test.go`

**Interfaces:**
- Consumes: `outputRig`, `constPCM` (Task 4.1), `resample.NewResampler`, `Connection.resamplersMu`, `Connection.outResampler`.
- Produces: `Connection.outRate int` (the rate `outResampler` was built for).

- [ ] **Step 1: Write the failing wire test.** Create `transport/ws/output_resample_test.go`:

```go
package ws_test

import (
	"testing"
	"time"

	"govoice/core"
)

// Output frames at 16 kHz, then 48 kHz (a TTS fallback or a tone at another rate), then 16 kHz again: each is
// resampled to the 24 kHz player rate with the ratio of its own rate, so 140 ms of audio is 3360 samples on the
// wire (a resampler may hold back a sample or two of its stream; this does not test filter continuity). Today the
// 48 kHz frames go through the 16 kHz resampler and come out three times too long.
func TestOutputResamplerFollowsEachFramesRate(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	segments := []struct{ rate, frames int }{{16000, 3}, {48000, 3}, {16000, 1}}
	total := 0
	for _, s := range segments {
		total += s.frames
	}
	seq := 0
	for _, s := range segments {
		for i := 0; i < s.frames; i++ {
			seq++
			last := seq == total
			if err := r.sess.Post(&core.OutputAudioFrame{Samples: constPCM(s.rate, 20*time.Millisecond, 1000), Encoding: core.EncodingPCM16LE,
				SampleRate: s.rate, Channels: 1, Duration: 20 * time.Millisecond, Gen: gen, Turn: "t", UnitID: 1, Seq: uint64(seq),
				IsFinal: last, TurnFinal: last}); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.waitFor(t, "the reply to drain", func(log []wireEvent) bool {
		for _, ev := range log {
			if ev.msg == "bot-stopped-speaking" {
				return true
			}
		}
		return false
	})
	samples := 0
	for _, ev := range r.events() {
		if ev.msg != "" {
			continue
		}
		if ev.rate != 24000 {
			t.Fatalf("a frame left at %d Hz", ev.rate)
		}
		samples += ev.bytes / 2
	}
	if want := 3360; samples < want-4 || samples > want+4 {
		t.Fatalf("%d samples on the wire, want %d±4 (140 ms at 24 kHz)", samples, want)
	}
}
```

Run: `go test -race -count=1 -run TestOutputResamplerFollowsEachFramesRate ./transport/ws/`. Expected: FAIL `6239 samples on the wire, want 3360±4` (±2; measured in a scratch copy).

- [ ] **Step 2: Rebuild the output resampler on a rate change.** In `transport/ws/transport.go`:
(a) in `type Connection struct` replace `	outResampler *resample.Resampler         // providerRate -> 24000` with

```go
	outResampler *resample.Resampler         // providerRate -> FixedPlayerRate
	outRate      int                         // the rate outResampler was built for
```

(b) in `ProcessFrame`, replace the `case core.FrameOutputAudio:` body up to (not including) `		return c.enqueueOutput(ctx, oaf)` with:

```go
	case core.FrameOutputAudio:
		oaf := f.(*core.OutputAudioFrame)
		// Convert provider rate to fixed player rate if necessary. A frame at
		// another rate than the last (a TTS fallback, a tone) gets a resampler
		// built for its own rate.
		if oaf.SampleRate != c.cfg.FixedPlayerRate {
			c.resamplersMu.Lock()
			if c.outResampler == nil || c.outRate != oaf.SampleRate {
				r, err := resample.NewResampler(oaf.SampleRate, c.cfg.FixedPlayerRate)
				if err != nil {
					c.resamplersMu.Unlock()
					return err
				}
				c.outResampler, c.outRate = r, oaf.SampleRate
			}
			resampled, err := c.outResampler.ResamplePCM16(oaf.Samples)
			c.resamplersMu.Unlock()
			if err != nil {
				return err
			}
			oaf.Samples = resampled
			oaf.SampleRate = c.cfg.FixedPlayerRate
		}

```

(c) `server/tts_failover.go`: replace the `ttsRate` comment's sentence `The transport builds its resampler from the first frame and cannot follow a rate change, so every candidate is configured to this rate (the factories read it from the same setting) and core.NewTTSFailover rejects a mismatch.` with `TTSFailover closes a stalled unit with a frame at the candidate's rate, so every candidate is configured to this rate (the factories read it from the same setting) and core.NewTTSFailover rejects a mismatch.`
(d) `core/tts_failover.go`, `TTSCandidate.Rate`'s comment: replace `The transport builds its resampler from the first frame and` / `	// cannot follow a mid-session change, so known rates must all be equal.` with `expire closes a stalled unit with a frame at this rate, so` / `	// known rates must all be equal.`

Run: `gofmt -l transport server core && go vet ./transport/... && go test -race -count=20 -run 'TestOutputResampler|TestBargeIn' ./transport/ws/ && go test -race -count=1 ./transport/...`. Expected: `ok` (verified in a scratch copy). Mutation check: change the condition back to `if c.outResampler == nil {` → `6239 samples on the wire` (verified).

- [ ] **Step 3: Gate.** Run the gate block with `4.3-s3` (verify, e2e-echo, offline-test).

- [ ] **Step 4: Commit.**

```bash
git add transport/ws/transport.go transport/ws/output_resample_test.go server/tts_failover.go core/tts_failover.go
git commit -m "fix(ws): resample output audio with the ratio of its own rate

The output side kept one resampler built from the first frame's rate and
used it for every later rate. It is now rebuilt when the rate changes.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.4: a crash-looping VAD worker hands its sessions to a live one and restarts off its run loop (A11)

**Files:**
- Modify: `vad/worker_executor.go`, `vad/worker_capacity_test.go` (it calls the removed `respawnWithBackoff`), `docs/operations_runbook.md`
- Create: `server/vad_worker_e2e_test.go`
- Test (append): `vad/resilience_test.go`

**Interfaces:**
- Consumes: `workerProcessBackend`, `workerProcess`, `Job`, `ErrWorker`, `closeMu`, `availCount`, `statCounters`, `Executor.HardKill` (test seam), `WorkerBinary()`, server `callerSpeech`/`sendAudio` (`server/offline_e2e_test.go`, `xaispeech` tag), `wireLog`/`countOf` (Task 4.1).
- Produces: `workerProcess.up atomic.Bool`, `.respawning bool`, `.quit chan struct{}`; unexported `workerProc{cmd, stdin, stdout}` with `.reap()`, `(*workerProcess).start() (workerProc, error)`, `.installLocked(workerProc)`, `.handshake(io.Writer, io.Reader) error`, `.markDown()`, `.respawnLoop(time.Duration)`, `.noteCrashLocked() time.Duration`, `.handOff([]*Job, error)`, `(*workerProcessBackend).liveQueue(int) chan *Job`; test helpers `crashableWorker(t, flag)` (vad), `crashableVADWorker(t, flag)`, `vadWorkerPIDs(t)` (server). Removes `respawnWithBackoff` and `startAndHandshake`.

- [ ] **Step 1: Write the failing worker test.** Append to `vad/resilience_test.go` (add `"errors"` and `"os"` and `"path/filepath"` to its imports if missing):

```go
// crashableWorker is a vadworker launcher that exits at once while flag exists, so a killed worker's
// restarts keep failing (a crash loop) until the test removes the flag.
func crashableWorker(t *testing.T, flag string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vadworker.sh")
	script := "#!/bin/sh\n[ -e '" + flag + "' ] && exit 1\nexec '" + WorkerBinary() + "' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A worker that crash-loops must not leave the sessions pinned to it VAD-blind: from the first chunk after the
// crash they are served by the live worker, while the dead one keeps retrying off its run loop and comes back
// once it can start again.
func TestWorkerProcess_CrashLoopingWorkerHandsItsSessionsToALiveOne(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "crash")
	exec, err := New(Config{Mode: "process", Workers: 2, Batch: 4, QueueCap: 256, IntraOp: 1, PerSessionLimit: 1,
		ParentDeadline: 200 * time.Millisecond, ParentGrace: 200 * time.Millisecond, WorkerBinary: crashableWorker(t, flag)})
	if err != nil {
		t.Fatalf("new worker process executor: %v", err)
	}
	defer exec.Close()
	st := exec.Register(0) // session 0 -> worker 0
	defer st.Close()
	run := func(seq uint64) error {
		done := make(chan error, 1)
		if err := st.Submit(&Job{Seq: seq, Done: func(j *Job) { done <- j.Err }}); err != nil {
			return err
		}
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			return errors.New("the chunk never finished")
		}
	}
	waitUntil := func(what string, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	if err := run(1); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.HardKill(0); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(2); seq <= 40; seq++ {
		if err := run(seq); err != nil {
			t.Fatalf("chunk %d after worker 0 crashed: %v", seq, err)
		}
	}
	waitUntil("worker 0 to crash-loop", func() bool { return exec.Stats().Restarts >= 3 })
	if avail, total := exec.Capacity(); avail != 1 || total != 2 {
		t.Fatalf("capacity (%d,%d) while worker 0 crash-loops, want (1,2)", avail, total)
	}
	if err := run(100); err != nil {
		t.Fatalf("a chunk during the crash loop: %v", err)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	waitUntil("worker 0 to come back", func() bool { a, _ := exec.Capacity(); return a == 2 })
	if err := run(101); err != nil {
		t.Fatalf("a chunk after recovery: %v", err)
	}
}
```

Run: `go test -race -count=1 -run 'TestWorkerProcess_CrashLooping' ./vad/`. Expected: FAIL `chunk 2 after worker 0 crashed: vad: worker …` (the run loop sleeps, fails the respawn, fails the job).

- [ ] **Step 2: Hand off, restart off the loop.** In `vad/worker_executor.go`:

(a) In `type workerProcess struct` replace `	down       bool // counted out of backend.availCount` with:

```go
	down       bool          // counted out of backend.availCount
	respawning bool          // a respawnLoop is running
	up         atomic.Bool   // healthy and serving: handOff only sends to an up worker
	quit       chan struct{} // closed by close: ends respawnLoop's wait
```

(b) In `NewWorkerProcessExecutor`'s `wp := &workerProcess{` literal add `			quit:     make(chan struct{}),`, and replace

```go
		if err := wp.startAndHandshake(); err != nil {
			_ = b.close()
			return nil, fmt.Errorf("vad worker %d startup handshake failed: %w", i, err)
		}
		b.availCount.Add(1)
```

with

```go
		p, err := wp.start()
		if err != nil {
			_ = b.close()
			return nil, fmt.Errorf("vad worker %d startup handshake failed: %w", i, err)
		}
		wp.mu.Lock()
		wp.installLocked(p)
		wp.mu.Unlock()
		b.availCount.Add(1)
		wp.up.Store(true)
```

(c) Replace the whole `func (wp *workerProcess) startAndHandshake() error {…}` with:

```go
// workerProc is a started worker process that passed its handshake.
type workerProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (p workerProc) reap() {
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

// start starts a worker process and checks its model without taking wp.mu
// (the handshake can take 3 s and the run loop must not wait on it). The
// caller installs the result with installLocked.
func (wp *workerProcess) start() (workerProc, error) {
	cmd := exec.Command(wp.backend.cfg.WorkerBinary, fmt.Sprint(wp.backend.cfg.Batch))
	cmd.Env = WorkerEnv()
	cmd.Stderr = wp.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return workerProc{}, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return workerProc{}, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return workerProc{}, fmt.Errorf("start worker: %w", err)
	}
	p := workerProc{cmd: cmd, stdin: stdin, stdout: stdout}
	if err := wp.handshake(stdin, stdout); err != nil {
		p.reap()
		return workerProc{}, err
	}
	return p, nil
}

// installLocked makes p the worker's process. wp.mu held.
func (wp *workerProcess) installLocked(p workerProc) {
	wp.cmd, wp.stdin, wp.stdout, wp.healthy = p.cmd, p.stdin, p.stdout, true
}

// handshake sends the expected model hash and checks the worker's answer within 3 s.
func (wp *workerProcess) handshake(stdin io.Writer, stdout io.Reader) error {
	req := HandshakeReq{ExpectedModelSHA256: wp.backend.modelSHA, MaxBatch: uint32(wp.backend.cfg.Batch)}
	if err := WriteFrame(stdin, EncodeHandshakeReq(req)); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	type hsResult struct {
		resp HandshakeResp
		err  error
	}
	hsCh := make(chan hsResult, 1)
	go func() { // ends with the reply, or when reap closes stdout
		payload, err := ReadFrame(stdout, nil)
		if err != nil {
			hsCh <- hsResult{err: err}
			return
		}
		resp, err := DecodeHandshakeResp(payload)
		hsCh <- hsResult{resp: resp, err: err}
	}()
	t := time.NewTimer(3 * time.Second)
	defer t.Stop()
	select {
	case res := <-hsCh:
		switch {
		case res.err != nil:
			return fmt.Errorf("handshake read/decode: %w", res.err)
		case res.resp.Status != StatusOK:
			return fmt.Errorf("%w: worker returned status %d", ErrWorkerHandshake, res.resp.Status)
		case !bytes.Equal(res.resp.ActualModelSHA256[:], wp.backend.modelSHA[:]):
			return fmt.Errorf("%w: worker model SHA mismatch", ErrModelHashMismatch)
		}
		return nil
	case <-t.C:
		return errors.New("worker handshake timed out")
	}
}
```

(If `ReadFrame`/`WriteFrame` take `io.ReadCloser`/`io.WriteCloser`, keep the parameter types they take.)

(d) In `func (wp *workerProcess) close()` add, after `	wp.mu.Unlock()`, the line `	close(wp.quit) // backend.close calls close once per worker`.

(e) In `executeBatch` replace

```go
	if !healthy {
		if !wp.respawnWithBackoff() {
			for _, j := range jobs {
				j.Err = ErrWorker
				wp.backend.statCounters.workerErrors.Add(1)
				wp.backend.finish(j)
			}
			return
		}
	}
```

with

```go
	if !healthy {
		// Down (crashed, or killed between batches): restart it off this loop
		// and let a live worker serve these chunks instead of failing them.
		wp.markDown()
		wp.handOff(jobs, ErrWorker)
		return
	}
```

and in the hang branch replace `		_ = wp.respawnWithBackoff()` with `		wp.markDown()` (the hung batch still fails with `ErrWorkerHung`: its deadline has passed).

(f) Replace the whole `func (wp *workerProcess) handleWorkerFailure(jobs []*Job, err error) {…}` with:

```go
// handleWorkerFailure kills a worker whose pipe failed and hands the batch to
// a live worker: the jobs' input and state are unchanged by the failed attempt.
func (wp *workerProcess) handleWorkerFailure(jobs []*Job, err error) {
	wp.mu.Lock()
	wp.killAndReapLocked()
	wp.mu.Unlock()

	wp.inFlightMu.Lock()
	for _, j := range jobs {
		delete(wp.inFlight, j.RequestID)
	}
	wp.inFlightMu.Unlock()

	wp.markDown()
	wp.handOff(jobs, err)
}
```

(g) Replace the whole `func (wp *workerProcess) respawnWithBackoff() bool {…}` with:

```go
// markDown takes the worker out of capacity and of handOff targets and starts
// one respawnLoop, unless the backend is closing. Called on the run loop.
func (wp *workerProcess) markDown() {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.up.Store(false)
	if !wp.down {
		wp.down = true
		wp.backend.availCount.Add(-1)
	}
	if wp.closed || wp.respawning {
		return
	}
	wp.respawning = true
	wait := wp.noteCrashLocked()
	wp.wg.Add(1) // the run loop holds a wg count here, so this never races backend.close's Wait at zero
	go wp.respawnLoop(wait)
}

// respawnLoop restarts the worker off its run loop, backing off while the
// restart keeps failing, until it succeeds or the backend closes.
func (wp *workerProcess) respawnLoop(wait time.Duration) {
	defer wp.wg.Done()
	for {
		t := time.NewTimer(wait)
		select {
		case <-wp.quit:
			t.Stop()
			return
		case <-t.C:
		}
		if p, err := wp.start(); err == nil {
			// One critical section: a crash noticed after this sees a worker that
			// is up and healthy, marks it down and starts a new respawnLoop.
			wp.mu.Lock()
			if wp.closed {
				wp.mu.Unlock()
				p.reap()
				return
			}
			wp.installLocked(p)
			wp.respawning, wp.down = false, false
			wp.backend.availCount.Add(1)
			wp.up.Store(true)
			wp.mu.Unlock()
			return
		}
		wp.mu.Lock()
		wait = wp.noteCrashLocked()
		wp.mu.Unlock()
	}
}

// noteCrashLocked records a failure (one restart) and returns the wait before
// the next start: 20 ms doubling to 1 s, and 1 s while crash-looping. wp.mu held.
func (wp *workerProcess) noteCrashLocked() time.Duration {
	now := time.Now()
	cutoff := now.Add(-CrashLoopWindow)
	pruned := wp.crashes[:0]
	for _, t := range wp.crashes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	wp.crashes = append(pruned, now)
	wp.backend.statCounters.restarts.Add(1)
	n := len(wp.crashes)
	if n >= CrashLoopThreshold {
		return time.Second
	}
	return min(10*time.Millisecond*time.Duration(1<<min(n, 6)), time.Second)
}

// handOff gives the jobs of a down worker to a live one: a Silero job carries
// its detector state (Job.State) and a session has one outstanding job, so any
// worker can serve it in order. A job no live worker can take now fails with
// failErr, as before. Called on the run loop.
func (wp *workerProcess) handOff(jobs []*Job, failErr error) {
	b := wp.backend
	failed := jobs[:0]
	b.closeMu.RLock() // Submit's guard: no queue is closed while it is held
	for _, j := range jobs {
		if q := b.liveQueue(wp.id); q != nil && !b.closed.Load() {
			select {
			case q <- j:
				continue
			default:
			}
		}
		failed = append(failed, j)
	}
	b.closeMu.RUnlock()
	for _, j := range failed {
		j.Err = failErr
		b.statCounters.workerErrors.Add(1)
		b.finish(j)
	}
}

// liveQueue is the queue of the next worker after from that is up, or nil.
func (b *workerProcessBackend) liveQueue(from int) chan *Job {
	for i := 1; i < len(b.workers); i++ {
		if w := b.workers[(from+i)%len(b.workers)]; w.up.Load() {
			return w.q
		}
	}
	return nil
}
```

(h) `vad/worker_capacity_test.go`: replace the body of `TestWorkerRespawnFailuresDoNotDrainCapacity` with:

```go
	b := &workerProcessBackend{cfg: Config{WorkerBinary: "/nonexistent/vadworker"}}
	for i := 0; i < 2; i++ {
		b.workers = append(b.workers, &workerProcess{id: i, backend: b, stderr: newBoundedBuffer(64), quit: make(chan struct{})})
	}
	b.availCount.Store(2)
	defer func() { b.workers[0].close(); b.workers[0].wg.Wait() }() // ends the respawnLoop
	for i := 0; i < 3; i++ {
		b.workers[0].markDown() // the respawn of the missing binary keeps failing meanwhile
	}
	if avail, _ := b.capacity(); avail != 1 {
		t.Fatalf("available = %d after repeated failed respawns of one worker, want 1", avail)
	}
```

Run (with the ONNX Runtime from `make ort`): `gofmt -l vad && go vet ./vad/ && go test -race -count=20 -run 'TestWorkerProcess|TestWorkerRespawn|TestDetector' ./vad/ && go test -race -count=1 ./vad/...`. Expected: `ok` (verified in a scratch copy: `./vad/` green, `-count=20` of the worker tests green). Mutation checks: (1) make `executeBatch`'s down branch fail the jobs instead of `wp.handOff(jobs, ErrWorker)` → `chunk 2 after worker 0 crashed`; (2) call `wp.respawnLoop(wait)` synchronously in `markDown` → the test times out on `chunk 2`. A missing `wp.up.Store(true)` after a respawn is not observable here (capacity counts `availCount`, and worker 0 only receives handed-off chunks once worker 1 is down); the review brief covers it.

- [ ] **Step 3: Write the server e2e (a real worker killed mid-call).** Create `server/vad_worker_e2e_test.go`:

```go
//go:build xaispeech

package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"govoice/vad"
)

// crashableVADWorker is a vadworker launcher that exits at once while flag exists, so a killed worker's
// restarts keep failing (a crash loop) until the flag is removed.
func crashableVADWorker(t *testing.T, flag string) string {
	t.Helper()
	real := vad.WorkerBinary()
	if _, err := os.Stat(real); err != nil {
		t.Fatalf("no vadworker binary (run `make build`): %v", err)
	}
	path := filepath.Join(t.TempDir(), "vadworker.sh")
	script := "#!/bin/sh\n[ -e '" + flag + "' ] && exit 1\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// vadWorkerPIDs lists this test process's live vadworker children.
func vadWorkerPIDs(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid()), "vadworker").Output()
	if err != nil {
		t.Fatalf("pgrep vadworker children: %v", err)
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// Two calls on the real process VAD executor (one per worker). One worker process is SIGKILLed mid-call and
// cannot restart (crash loop): both callers' speech is still detected, the dead worker stays out of capacity
// and the restarts metric counts its attempts.
func TestCallBehaviorVADKeepsHearingCallersWhileAWorkerCrashLoops(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	flag := filepath.Join(t.TempDir(), "crash")
	cfg := vad.DefaultConfig()
	cfg.Workers = 2
	cfg.WorkerBinary = crashableVADWorker(t, flag)
	vad.ConfigureArtifacts("", "")
	vexec, err := vad.New(cfg)
	if err != nil {
		t.Fatalf("process VAD executor (run `make ort` and `make build`): %v", err)
	}
	t.Cleanup(func() { _ = vexec.Close() })
	r.s.SetVADExecutor(vexec)
	flowID := behaviorFlow(t, r, "Listener", "listenvoice", ``)
	calls := []*e2eCall{r.dial(flowID), r.dial(flowID)} // consecutive session ids: one per worker
	speech := callerSpeech(t)
	heard := func(c *e2eCall) int { return countOf(c.wireLog(), "msg:user-started-speaking") }
	for i, c := range calls {
		c.sendAudio(speech)
		eventually(t, fmt.Sprintf("call %d's speech before the crash", i), func() bool { return heard(c) >= 1 })
	}
	pids := vadWorkerPIDs(t)
	if len(pids) != 2 {
		t.Fatalf("vadworker children %v, want 2", pids)
	}
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for i, c := range calls {
		before := heard(c)
		c.sendAudio(speech)
		eventually(t, fmt.Sprintf("call %d's speech after a worker crashed", i), func() bool { return heard(c) > before })
	}
	if avail, total := vexec.Capacity(); avail != 1 || total != 2 {
		t.Fatalf("capacity (%d,%d) while a worker crash-loops, want (1,2)", avail, total)
	}
	r.s.sampleVADMetrics()
	if n := r.s.metrics.VADWorkerRestartsTotal.Load(); n < 1 {
		t.Fatalf("voice_vad_worker_restarts_total = %d", n)
	}
	for i, c := range calls {
		if c.ended() {
			t.Fatalf("call %d ended", i)
		}
	}
}
```

Run: `(cd server && go test -race -count=1 -tags xaispeech -run TestCallBehaviorVADKeepsHearing .)`. Expected: `ok` (verified in a scratch copy, 2/2, about 6 s each; `pgrep -P` finds the `vadworker` the wrapper `exec`s). Run it once against the Step 2 changes stashed: it fails at `call N's speech after a worker crashed` for the call pinned to the killed worker.

- [ ] **Step 4: Runbook.** `docs/operations_runbook.md`, under `### Incident 1: Native VAD Worker Crash`, append to the `- **Underlying Mechanism**` bullet: ` While a worker restarts (or crash-loops), the chunks of the calls pinned to it are handed to a live worker (each chunk carries its detector state), so those callers keep their VAD; the worker is out of `vad_unavailable` capacity until it starts again, and restarts back off from 20 ms to 1 s off the worker's run loop. A batch whose worker died under it (a pipe failure, e.g. a SIGKILL) is re-run on a live worker; a batch that hung past its deadline fails, and chunks are lost while every worker is down. The child's stderr tail is kept in memory (64 KB) but not logged.`

- [ ] **Step 5: Gate.** Run the gate block with `4.4-s5` (verify, e2e-echo, offline-test). Also: `go test -race -count=20 ./vad/ 2>&1 | tee "$L/4.4-s5-vad-count20.log"`.

- [ ] **Step 6: Commit.**

```bash
git add vad/worker_executor.go vad/resilience_test.go vad/worker_capacity_test.go server/vad_worker_e2e_test.go docs/operations_runbook.md
git commit -m "fix(vad): a down worker hands its chunks to a live one and restarts off its loop

A crashed or crash-looping VAD worker slept its backoff on its own run
loop and failed every chunk of the calls pinned to it. Its loop now hands
those chunks to a live worker (each carries its detector state) while a
respawn goroutine restarts it with the same backoff, and the handshake no
longer holds the worker lock.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.5: the TTS failover's stall watchdog runs on the session run loop (A12)

**Files:**
- Modify: `core/tts_failover.go`, `server/tts_failover.go`, `server/registry.go`
- Create: `core/tts_failover_loop_test.go`, `server/tts_stall_e2e_test.go`

**Interfaces:**
- Consumes: `TTSFailover.expire`, `run`, `ttsUnit`, `FrameInternal`, `ErrQueueFull`, `ErrSessionClosed`, the test stage `gateFrame`/`gateStage{hold func()}` (`core/unit_failure_test.go:120-133`), `coretest.PassthroughProcessor`, the flow rig (`newFlowRigE2E`, `r.spokenBy`, `eventually`).
- Produces: unexported `ttsExpire{u *ttsUnit; attempt int}` (`FrameInternal`), `(*TTSFailover).postExpire(*ttsExpire)`; `Registry.ttsUnitTimeout time.Duration` (zero in production: core's 30 s default); test doubles `loopTTS`, `newLoopFailover`, `audioFor`, `stalledTTS`. Nothing in `core/ttsunit`, `core/serial_queue.go` or the Google/xAI TTS changes.

- [ ] **Step 1: Write the failing core tests.** Create `core/tts_failover_loop_test.go`:

```go
package core_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// loopTTS is a TTS candidate that counts the calls it gets while the session's run loop is parked (held):
// such a call can only come from another goroutine, which is what the "callers serialize" contract of
// ttsunit.Runner and core.SerialQueue forbids. stall makes it swallow the next unit, as a wedged provider
// does; any other unit is answered at once from a session task, as a provider's synthesis task does.
type loopTTS struct {
	session core.SessionController
	index   int
	held    *atomic.Bool
	offLoop atomic.Int32
	units   atomic.Int32 // synthesis units received
	stall   atomic.Bool
}

func (p *loopTTS) Attach(s core.SessionController, index int) error {
	p.session, p.index = s, index
	return nil
}

func (p *loopTTS) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if p.held.Load() {
		p.offLoop.Add(1)
	}
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	p.units.Add(1)
	if p.stall.CompareAndSwap(true, false) {
		return nil // wedged: this unit is never answered
	}
	return p.session.Go(func(context.Context) {
		_ = p.session.Reenter(&core.TTSStartedFrame{Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID}, core.Downstream, p.index+1, su.Gen)
		_ = p.session.Reenter(&core.OutputAudioFrame{Samples: make([]byte, 960), Encoding: core.EncodingPCM16LE, SampleRate: 24000,
			Channels: 1, Duration: 20 * time.Millisecond, Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID, Seq: 1, IsFinal: true,
			TurnFinal: su.IsFinal}, core.Downstream, p.index+1, su.Gen)
	})
}

// newLoopFailover is TTSFailover(primary, fallback) -> gateStage -> recorder on a started session. The gate
// parks the run loop (held = true) on a gateFrame until release is closed.
func newLoopFailover(t *testing.T) (sess *core.Session, primary, fallback *loopTTS, rec *coretest.PassthroughProcessor, entered, release chan struct{}) {
	t.Helper()
	var held atomic.Bool
	primary, fallback = &loopTTS{held: &held}, &loopTTS{held: &held}
	w, err := core.NewTTSFailover(core.TTSFailoverConfig{UnitTimeout: 30 * time.Millisecond,
		Candidates: []core.TTSCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: fallback}}})
	if err != nil {
		t.Fatal(err)
	}
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate := &gateStage{hold: func() {
		held.Store(true)
		once.Do(func() { close(entered) })
		<-release
		held.Store(false)
	}}
	rec = &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(w, gate, rec)
	if err != nil {
		t.Fatal(err)
	}
	sess, err = core.NewSession(950, pipe, core.DefaultResourceBudget())
	if err != nil || sess.Start() != nil {
		t.Fatal(err)
	}
	return sess, primary, fallback, rec, entered, release
}

func audioFor(rec *coretest.PassthroughProcessor, unit uint64) bool {
	for _, f := range rec.GetFrames() {
		if a, ok := f.(*core.OutputAudioFrame); ok && a.UnitID == unit {
			return true
		}
	}
	return false
}

// A unit's watchdog expires while the run loop is busy elsewhere (here: parked in the gate). The expiry
// must wait for the run loop instead of cancelling the wedged candidate and starting the fallback from the
// timer goroutine; afterwards the unit is spoken by the fallback, and so is the next one.
func TestTTSWatchdogExpiryDrivesCandidatesOnlyFromTheRunLoop(t *testing.T) {
	sess, primary, fallback, rec, entered, release := newLoopFailover(t)
	defer func() {
		_ = sess.Close()
		if n := sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	}()
	gen := sess.CurrentGeneration()
	primary.stall.Store(true)
	if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: gen, UnitID: 1, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Post(&gateFrame{}); err != nil { // queued behind the unit: the loop parks after starting it
		t.Fatal(err)
	}
	<-entered
	// The watchdog (30 ms + 20 ms per character) fires several times over while the loop is parked.
	time.Sleep(300 * time.Millisecond) // a bounded wait for something that must not happen
	if n := primary.offLoop.Load() + fallback.offLoop.Load(); n != 0 {
		close(release)
		t.Fatalf("%d candidate calls came from outside the run loop (the watchdog's timer goroutine)", n)
	}
	close(release)
	if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: gen, UnitID: 2, Text: "b", IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(2 * time.Second); !(audioFor(rec, 1) && audioFor(rec, 2)); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("units after the stall were not spoken; frames=%d", rec.Count())
		}
	}
}

// A watchdog that fires after the session closed finds no run loop: the expiry is dropped, nothing leaks.
func TestTTSWatchdogExpiryAfterCloseIsDropped(t *testing.T) {
	sess, primary, _, _, _, release := newLoopFailover(t)
	close(release)
	primary.stall.Store(true)
	if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: sess.CurrentGeneration(), UnitID: 1, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(time.Second); primary.units.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the unit never reached the primary")
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // past the 50 ms watchdog: its expiry meets a closed session
	if n := sess.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
}
```

Run: `(cd core && go test -race -count=50 -run 'TestTTSWatchdogExpiry' .)`. Expected: `TestTTSWatchdogExpiryDrivesCandidatesOnlyFromTheRunLoop` FAIL 50/50 (`2 candidate calls came from outside the run loop (the watchdog's timer goroutine)`: the expiry cancelled the primary and started the fallback while the loop was parked); `TestTTSWatchdogExpiryAfterCloseIsDropped` passes (a pin: today's timer calls a closed candidate harmlessly). Both measured in a scratch copy of 3a243fd.

- [ ] **Step 2: Post the expiry to the run loop.** In `core/tts_failover.go`:
(a) in `run`, replace `				u.watchdog = time.AfterFunc(w.unitTimeout(u), func() { w.expire(u, attempt) })` with

```go
				u.watchdog = time.AfterFunc(w.unitTimeout(u), func() { w.postExpire(&ttsExpire{u: u, attempt: attempt}) })
```

(b) in `ProcessFrame`, directly before `	case *InterruptionFrame, *CancelFrame:` add

```go
	case *ttsExpire:
		w.expire(fr.u, fr.attempt) // a watchdog fired: handled here, on the run loop
		return nil
```

(c) directly above the comment `// expire abandons a candidate that went quiet on u` add

```go
// ttsExpire carries a unit watchdog's expiry onto the session run loop, so a
// candidate is never driven from the timer goroutine while the run loop drives
// it too (ttsunit.Runner and core.SerialQueue need their callers serialized).
type ttsExpire struct {
	u       *ttsUnit
	attempt int
}

func (*ttsExpire) Type() FrameType { return FrameInternal }

// postExpire queues e for the run loop behind the frames already admitted. A
// full inbox refuses internal frames, so it retries until the session takes it;
// a closed session (ErrSessionClosed) ends it.
func (w *TTSFailover) postExpire(e *ttsExpire) {
	if errors.Is(w.session.Reenter(e, Downstream, w.index, 0), ErrQueueFull) {
		time.AfterFunc(5*time.Millisecond, func() { w.postExpire(e) })
	}
}
```

`expire` itself is unchanged: it re-checks `w.cur == u && u.attempt == attempt` under `w.mu`, so an expiry that arrives after the unit finished, after an interruption or twice is a no-op. Generation 0 keeps the session fence from dropping it; a stale unit is no longer `w.cur` once the loop has seen the next unit or the interruption.

Run: `(cd core && gofmt -l . && go vet . && go test -race -count=50 -run 'TestTTSWatchdogExpiry' . && go test -race -count=3 -run 'TTS|Failover|UnitFailure' . && go test -race -count=1 ./...)`. Expected: `ok` (50/50 GREEN in a scratch copy). Mutation check: restore `func() { w.expire(u, attempt) }` → 50/50 FAIL again.

- [ ] **Step 3: Write the server e2e (a stalled TTS unit).** In `server/registry.go`, in `type Registry struct` after `	onTTSFallbackSkipped func()                                           // set by BindMetrics` add

```go
	ttsUnitTimeout       time.Duration                                    // TTSFailover's stall watchdog; 0 = core's default (30 s); e2e tests shorten it
```

and in `server/tts_failover.go` `buildTTS`, in the `core.NewTTSFailover(core.TTSFailoverConfig{` literal after `		Candidates: cands, Classify: ttsClassify, Allow: call.Failover.Allows, OnFailover: r.onTTSFailover,` add `		UnitTimeout: r.ttsUnitTimeout,` (gofmt). Create `server/tts_stall_e2e_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/flow"
)

// stalledTTS is a TTS provider whose synthesis never answers (a wedged socket): it takes units and emits nothing.
type stalledTTS struct{ units atomic.Int32 }

func (p *stalledTTS) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.SynthesisUnitFrame); ok {
		p.units.Add(1)
		return nil
	}
	return e.PushDirection(f, dir)
}

// The primary TTS stalls on the reply's first sentence: its watchdog expires (handled on the run loop), the
// sentence moves to the fallback voice, and every later sentence is spoken by the fallback too.
func TestCallBehaviorAStalledTTSUnitFailsOverAndLaterUnitsAreSpoken(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	r.s.registry.ttsUnitTimeout = 200 * time.Millisecond // core's default is 30 s
	stalled := &stalledTTS{}
	healthy := r.s.registry.factories["echo.tts"]
	r.s.registry.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if spec.Voice == "stallvoice" {
			return stalled, nil
		}
		return healthy(cfg, spec)
	})
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Stall", Config: json.RawMessage(
		`{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"Stall"},` +
			`"tts":{"provider":"echo","voice":"stallvoice","fallbacks":[{"provider":"echo","voice":"okvoice"}]}}`)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("First sentence here. Second sentence here. Third sentence here.")
	for _, s := range []string{"First sentence", "Second sentence", "Third sentence"} {
		eventually(t, s+" spoken by the fallback", func() bool { return r.spokenBy("okvoice", s) })
	}
	if n := stalled.units.Load(); n != 1 {
		t.Fatalf("the stalled primary got %d units, want 1 (demoted for the rest of the turn)", n)
	}
	if c.ended() {
		t.Fatal("a stalled TTS unit ended the call")
	}
}
```

Run: `(cd server && go vet -tags xaispeech . && go test -race -count=3 -tags xaispeech -run TestCallBehaviorAStalledTTSUnit .)`. Expected: `ok` (3/3 in a scratch copy, about 1.6 s each). This e2e pins the stall path end to end (it also passes with the timer-goroutine expiry: the RED for the concurrency is Step 1).

- [ ] **Step 4: Gate.** Run the gate block with `4.5-s4` (verify, e2e-echo, offline-test). Also `(cd core && go test -race -count=50 -run 'TestTTSWatchdogExpiry' . && go test -race -count=20 ./ttsunit/) 2>&1 | tee "$L/4.5-s4-stress.log"`.

- [ ] **Step 5: Commit.**

```bash
git add core/tts_failover.go core/tts_failover_loop_test.go server/tts_failover.go server/registry.go server/tts_stall_e2e_test.go
git commit -m "fix(core): the TTS failover's stall watchdog runs on the run loop

A unit's watchdog called the wedged candidate and started the next one
from its timer goroutine, while the run loop drives the same candidates;
the unit queues behind them assume serialized callers and could deadlock.
The watchdog now re-enters an internal frame, and the expiry runs on the
run loop.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.6a: tighter websocket read limit; one disconnect count per connection

**Files:**
- Modify: `transport/ws/wire.go`, `transport/ws/transport.go`, `transport/ws/read_limit_test.go`, `transport/ws/ws_test.go` (comment), `docs/observability.md`
- Create: `transport/ws/wire_limits_test.go`

**Interfaces:**
- Consumes: `pingEnv`, `newPingEnv`, `sendRaw`, `performHandshake`, `disconnectCategory`.
- Produces: `(*Connection).observe(event string, n uint64)`; test helper `disconnects(e *pingEnv) uint64`. Removes `observeDisconnect` (and the stale `// Close gracefully terminates the connection.` comment with it).

- [ ] **Step 1: Write the failing tests.** Create `transport/ws/wire_limits_test.go`:

```go
package ws_test

import (
	"strings"
	"testing"
	"time"

	"govoice/transport/ws"
)

// The largest valid frame (a full 16 KB RTVI message) fits the advertised max with headroom, and the max is
// not much larger: it sizes the read buffer one socket can make the server hold.
func TestLargestValidFrameFitsTheAdvertisedMax(t *testing.T) {
	frame, err := ws.EncodeMessageFrame(strings.Repeat("x", ws.MaxMessageDataBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.DecodeBinaryFrame(frame, 1, time.Now()); err != nil {
		t.Fatalf("the largest valid message is rejected: %v", err)
	}
	if len(frame) > ws.MaxEncodedFrameSize || ws.MaxEncodedFrameSize > 24*1024 {
		t.Fatalf("largest valid frame %d B, advertised max %d B", len(frame), ws.MaxEncodedFrameSize)
	}
}
```

In `transport/ws/read_limit_test.go`:
- add after `sendRaw`:

```go
// disconnects sums the disconnect_* counters: every closed connection adds exactly one.
func disconnects(e *pingEnv) uint64 {
	return e.count("disconnect_normal") + e.count("disconnect_client") + e.count("disconnect_server") + e.count("disconnect_timeout")
}
```

- replace `TestMessageUpToAdvertisedMaxReachesTheFrameDecoder` with:

```go
// A message up to the advertised max reaches the frame decoder (here: malformed protobuf), which closes with
// 1007: a server-side disconnect, counted once.
func TestMessageUpToAdvertisedMaxReachesTheFrameDecoder(t *testing.T) {
	e, got := sendRaw(t, 18*1024)
	if got != 18*1024 || e.count("protocol_error") != 1 {
		t.Fatalf("decoder saw %d bytes, protocol_error=%d", got, e.count("protocol_error"))
	}
	if e.count("disconnect_server") != 1 || disconnects(e) != e.count("client_disconnected") {
		t.Fatalf("disconnect_server=%d, disconnects=%d, client_disconnected=%d", e.count("disconnect_server"), disconnects(e), e.count("client_disconnected"))
	}
}
```

- in `TestMessageOverAdvertisedMaxIsAProtocolError` add before its closing brace:

```go
	if e.count("disconnect_server") != 1 || disconnects(e) != e.count("client_disconnected") {
		t.Fatalf("disconnect_server=%d, disconnects=%d, client_disconnected=%d", e.count("disconnect_server"), disconnects(e), e.count("client_disconnected"))
	}
```

- in `TestMessageBeyondReadLimitEndsAsProtocolError` replace `	_ = c.Write(ctx, websocket.MessageBinary, make([]byte, 80*1024))` with

```go
	// Below the library's 32 KB default: only the explicit read limit refuses it.
	_ = c.Write(ctx, websocket.MessageBinary, make([]byte, 24*1024))
```

- append:

```go
// The client's own disconnect-bot is a normal disconnect, counted once.
func TestDisconnectBotCountsOneNormalDisconnect(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	msg, _ := ws.EncodeMessageFrame(`{"id":"2","label":"rtvi-ai","type":"disconnect-bot"}`)
	if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	// readLoop's Close counts client_disconnected before its deferred disconnect count: wait for the latter.
	if !waitFor(t, 5*time.Second, func() bool { return disconnects(e) == 1 }) {
		t.Fatal("no disconnect was counted")
	}
	if e.count("disconnect_normal") != 1 || disconnects(e) != 1 {
		t.Fatalf("disconnect_normal=%d, disconnects=%d", e.count("disconnect_normal"), disconnects(e))
	}
}

// The server ending a call from outside the read loop (the end of the call, a fatal error, an idle hang-up,
// a slow client) cancels the connection: a server-side disconnect, not the client's.
func TestServerSideCloseCountsOneServerDisconnect(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	if !waitFor(t, 5*time.Second, func() bool { return e.conn.Load() != nil }) {
		t.Fatal("no session")
	}
	e.conn.Load().Close()
	if !waitFor(t, 5*time.Second, func() bool { return disconnects(e) == 1 }) {
		t.Fatal("no disconnect was counted")
	}
	if e.count("disconnect_server") != 1 || e.count("disconnect_client") != 0 {
		t.Fatalf("disconnect_server=%d disconnect_client=%d", e.count("disconnect_server"), e.count("disconnect_client"))
	}
}
```

In `transport/ws/ws_test.go` replace the comment `	// 2b. Oversized message (> 64 KB)` with `	// 2b. Oversized message (> MaxEncodedFrameSize)`.

Run: `go test -race -count=1 -run 'TestLargestValidFrame|TestMessage|TestDisconnectBot' ./transport/ws/`. Expected: `TestLargestValidFrameFitsTheAdvertisedMax` FAIL (`advertised max 65536 B`), `TestMessageUpToAdvertisedMax…` and `…OverAdvertisedMax…` FAIL (`disconnect_server=0`), `TestMessageBeyondReadLimit…` FAIL (`client saw close status 1007 … want 1009`), `TestDisconnectBot…` FAIL (`disconnect_normal=0`), `TestServerSideClose…` FAIL (`disconnect_server=0 disconnect_client=1`).

- [ ] **Step 2: Tighten the limit; count every disconnect once.** `transport/ws/wire.go`: replace `	MaxEncodedFrameSize   = 64 * 1024 // 64 KB maximum protobuf binary frame` with `	MaxEncodedFrameSize   = 20 * 1024 // the largest valid frame is a 16 KB message plus a few bytes (audio is at most 4 KB)`.

`transport/ws/transport.go`:
(a) replace the comment above `	wsConn.SetReadLimit(MaxEncodedFrameSize + readLimitHeadroom)` with:

```go
	// The read limit is MaxEncodedFrameSize plus 1 KB of headroom, below the
	// library's 32 KB default: it bounds what one socket can make the server
	// buffer. A message just over the max reaches DecodeBinaryFrame and ends as a
	// protocol error (1007, "protocol_error"); anything larger is closed by the
	// library (1009), which readLoop records as protocol_error + disconnect_server.
```

(b) at the top of `func (c *Connection) readLoop() {` add:

```go
	// Every exit counts one disconnect_<cause>, so they sum to client_disconnected:
	// a failed Read classifies itself; the client's disconnect-bot is normal;
	// every other return is the server closing the socket (1007, 1008, 1011,
	// 1013, going away).
	cause := "server"
	defer func() { c.observe("disconnect_"+cause, 1) }()
```

(c) replace

```go
		if err != nil {
			c.observeDisconnect(err)
			c.traceEvent("transport.disconnect", map[string]any{"reason": disconnectCategory(err)})
			return
		}
```

with

```go
		if err != nil {
			cause = disconnectCategory(err)
			if cause == "client" && c.ctx.Err() != nil && c.CloseCause() != "peer_unresponsive" {
				// The server ended the call from outside this loop (Close, hang-up,
				// ready timeout); a client that stopped answering pings stays "client".
				cause = "server"
			}
			if errors.Is(err, websocket.ErrMessageTooBig) {
				c.observe("protocol_error", 1) // past the read limit: the library closed with 1009
			}
			c.traceEvent("transport.disconnect", map[string]any{"reason": cause})
			return
		}
```

(d) replace

```go
				if _, ok := respFrame.(*core.EndFrame); ok {
					c.traceEvent("transport.rtvi_disconnect", nil)
```

with

```go
				if _, ok := respFrame.(*core.EndFrame); ok {
					cause = "normal" // the client's own disconnect-bot
					c.traceEvent("transport.rtvi_disconnect", nil)
```

(e) delete the whole `// Close gracefully terminates the connection.` + `func (c *Connection) observeDisconnect(err error) {…}` and add in its place:

```go
func (c *Connection) observe(event string, n uint64) {
	if c.cfg.Observe != nil {
		c.cfg.Observe(event, n)
	}
}
```

Run: `gofmt -l transport && go vet ./transport/... && go test -race -count=20 -run 'TestServerSideClose|TestDisconnectBot|TestMessage|TestLargest|Ping' ./transport/ws/ && go test -race -count=1 ./transport/...`. Expected: `ok` (verified in a scratch copy, `-count=20`). Mutation checks: delete the deferred observe → `disconnect_server=0`; drop the `peer_unresponsive` exception → the existing `TestPing_StuckClientSessionTornDown` fails (`disconnect_client=0`); drop the `c.ctx.Err()` override → `TestServerSideClose…` fails.

- [ ] **Step 3: Docs.** `docs/observability.md`: next to the `voice_ws_disconnects_total` (or the `disconnect_*`) entry add: `Every closed connection counts exactly one disconnect by cause (normal, client, server, timeout), so the causes sum to the connections closed; a frame the server rejects (1007/1008/1009), an admission or setup refusal (1013/1011), a drain and any call the server ends itself (end of call, fatal error, idle hang-up, slow client, ready timeout) are `server`; the client's `disconnect-bot` is `normal`; a client that stops answering pings is `client`. Messages larger than 21 KB (the largest valid frame is a 16 KB RTVI message) are refused with 1009.` (Locate the metric with `grep -n "disconnect" docs/observability.md`.)

- [ ] **Step 4: Gate.** Run the gate block with `4.6a-s4` (verify, e2e-echo, offline-test).

- [ ] **Step 5: Commit.**

```bash
git add transport/ws/wire.go transport/ws/transport.go transport/ws/read_limit_test.go transport/ws/wire_limits_test.go transport/ws/ws_test.go docs/observability.md
git commit -m "fix(ws): 20 KB read limit; every closed connection counts one disconnect

No valid frame exceeds 16.5 KB, so the 64 KB advertised max only widened
the per-socket read buffer. Server-initiated closes and the client's
disconnect-bot left the readLoop without any disconnect_* count; one
deferred observe now records exactly one cause per connection.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.6b: issued voice tokens live `token.max_lifetime` (default 1 minute)

**Files:**
- Modify: `server/api_routes.go`, `server/config_validate.go`, `server/config_defaults.go`, `server/config_defaults_test.go`, `config/development.yaml`, `config/production.yaml`, `config/cloudrun.yaml`, `config/perf.yaml`, `deploy/rehearsal/server.yaml`, `server/testdata/effective_development.json`, `server/testdata/effective_production.json`, `docs/api/v1.md`
- Test (append): `server/api_boundary_test.go`

**Interfaces:**
- Consumes: `apiRequest`, `readyTestFlow`, `testProviderEnv`, `s.validator.ValidateAndConsume`.
- Produces: `effectiveVoiceTokenLifetime()` without the hidden 60 s cap; `maxVoiceTokenLifetime` (5 m) checked by `ValidateConfig`.

- [ ] **Step 1: Write the failing test.** Append to `server/api_boundary_test.go`:

```go
// token.max_lifetime is the lifetime of the tokens the server issues (and the validator's ceiling), not only
// the ceiling with a hidden one-minute cap on issuance.
func TestVoiceTokenLifetimeFollowsTheConfiguredMaximum(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Token.MaxLifetime = 3 * time.Minute
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	flowID := readyTestFlow(t, s)
	w := apiRequest(s, "POST", "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","flow_id":"`+flowID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	claims, err := s.validator.ValidateAndConsume(resp.Data.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time); got != 3*time.Minute {
		t.Fatalf("issued lifetime %s, want the configured 3m", got)
	}
	var sc struct {
		Data struct {
			Lifetime int `json:"voice_token_max_lifetime_seconds"`
		} `json:"data"`
	}
	_ = json.Unmarshal(apiRequest(s, "GET", "/api/v1/server-config", "management-test-key-32-bytes-long-and-separate", "").Body.Bytes(), &sc)
	if sc.Data.Lifetime != 180 {
		t.Fatalf("server-config reports %d s", sc.Data.Lifetime)
	}
}
```

Also append:

```go
// The voice token travels in the /ws URL query: its configured lifetime is capped at 5 minutes.
func TestConfigRejectsAVoiceTokenLifetimeAboveFiveMinutes(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.MaxLifetime = 5 * time.Minute
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("5m rejected: %v", err)
	}
	cfg.Token.MaxLifetime = 5*time.Minute + time.Second
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "at most 5m0s") {
		t.Fatalf("5m1s accepted or wrong error: %v", err)
	}
}
```

Run: `(cd server && go test -count=1 -run 'TestVoiceTokenLifetime|TestAPITokenFractional|TestConfigRejects' .)`. Expected: FAIL `issued lifetime 1m0s, want the configured 3m` and `5m1s accepted or wrong error: <nil>`.

- [ ] **Step 2: Honor the knob; default 1 minute.** `server/api_routes.go`: replace `effectiveVoiceTokenLifetime` with:

```go
// effectiveVoiceTokenLifetime is how long an issued voice token is valid:
// token.max_lifetime (default 1 m), in whole seconds so exp-iat stays within
// the validator's ceiling, which is the same setting.
func (s *Server) effectiveVoiceTokenLifetime() time.Duration {
	lifetime := s.cfg.Token.MaxLifetime
	if lifetime <= 0 {
		lifetime = time.Minute
	}
	return lifetime.Truncate(time.Second)
}
```

`server/config_validate.go`: directly above `func ValidateConfig(` add

```go
// maxVoiceTokenLifetime caps token.max_lifetime, the lifetime of issued voice tokens.
const maxVoiceTokenLifetime = 5 * time.Minute

```

and after the block that returns `errors.New("token.max_lifetime must be at least 2 seconds when configured")` add

```go
	if cfg.Token.MaxLifetime > maxVoiceTokenLifetime {
		// The voice token travels in the /ws URL query (proxy and access logs): keep its life short.
		return fmt.Errorf("token.max_lifetime must be at most %v", maxVoiceTokenLifetime)
	}
```

(both verified in a scratch copy). `server/config_defaults.go`: `	cfg.Token.MaxLifetime = 5 * time.Minute` → `	cfg.Token.MaxLifetime = time.Minute`. `server/config_defaults_test.go`: `"token lifetime 5m":     c.Token.MaxLifetime == 5*time.Minute,` → `"token lifetime 1m":     c.Token.MaxLifetime == time.Minute,`. In `config/development.yaml`, `config/production.yaml`, `config/cloudrun.yaml`, `config/perf.yaml`, `deploy/rehearsal/server.yaml` replace `max_lifetime: 5m` with `max_lifetime: 1m`. Regenerate the effective-config goldens and check that only `MaxLifetime` changed:

```bash
(cd server && UPDATE_GOLDEN=1 go test -count=1 -run TestConfig_ShippedYAMLEffectiveGolden . && git diff --stat testdata/ && git diff testdata/ | grep '^[-+] ')
```

Expected: two lines per file, `-  "MaxLifetime": 300000000000,` / `+  "MaxLifetime": 60000000000,`.

`docs/api/v1.md`: replace `a lifetime capped at 60 seconds and the configured maximum` with `a lifetime of `token.max_lifetime` (default 1 minute, at most 5 minutes; the validator rejects longer tokens)`.

Run: `(cd server && gofmt -l . && go test -race -count=1 -run 'Token|Config|APIToken|ServerConfig' .)`. Expected: `ok`.

- [ ] **Step 3: Gate.** Run the gate block with `4.6b-s3` (verify, e2e-echo, offline-test).

- [ ] **Step 4: Commit.**

```bash
git add server/api_routes.go server/config_validate.go server/config_defaults.go server/config_defaults_test.go server/api_boundary_test.go server/testdata/effective_development.json server/testdata/effective_production.json \
  config/development.yaml config/production.yaml config/cloudrun.yaml config/perf.yaml deploy/rehearsal/server.yaml docs/api/v1.md
git commit -m "fix(server): issued voice tokens live token.max_lifetime (default 1m)

Issuance silently capped tokens at one minute while every shipped config
set max_lifetime: 5m, which only widened the validator's ceiling. The
setting is now the issued lifetime, at most 5m (the token travels in the
URL query); the default and the shipped configs are 1m, so tokens still
live a minute and the ceiling tightens.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.6c: a refused management start leaves nothing pointing at the closed store

**Files:**
- Modify: `server/management.go`
- Test (append): `server/management_test.go`

**Interfaces:**
- Consumes: `apiTestServer`, `readyTestFlow`, `factoryKey`, `apiRequest`, `record.NewLocalSink`, `flow.NewService`.
- Produces: `unavailableFlowProviders(ctx context.Context, flows *flow.Service) []string` (new parameter).

- [ ] **Step 1: Write the failing test.** Append to `server/management_test.go` (add `"time"` if a later edit needs it; this test does not):

```go
// A start EnableManagement refuses (a production store with flows this build cannot run, an unusable
// recording directory) closes the store: nothing may still point at it and the APIs stay unavailable.
func TestRefusedManagementStartLeavesNoClosedStoreBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(t *testing.T, s *Server)
	}{
		{"production flow this build cannot run", func(t *testing.T, s *Server) {
			s.cfg.Profile = ProfileProduction
			delete(s.registry.factories, factoryKey("google", "tts"))
		}},
		{"recording directory is a file", func(t *testing.T, s *Server) {
			file := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			s.cfg.Recording.Enabled, s.cfg.Recording.Dir = true, file
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
			seed := apiTestServer(t)
			seed.cfg.Storage.Dialect = "sqlite"
			if err := seed.EnableManagement(t.Context()); err != nil {
				t.Fatal(err)
			}
			readyTestFlow(t, seed)
			seed.closeManagement()

			s := apiTestServer(t)
			s.cfg.Storage.Dialect = "sqlite"
			tc.refuse(t, s)
			if err := s.EnableManagement(t.Context()); err == nil {
				s.closeManagement()
				t.Fatal("the start was not refused")
			}
			if s.store != nil || s.flows != nil || s.mcpService != nil || s.mgmt != nil || s.recSink != nil {
				t.Fatalf("a refused start left store=%v flows=%v mcp=%v mgmt=%v recSink=%v",
					s.store != nil, s.flows != nil, s.mcpService != nil, s.mgmt != nil, s.recSink != nil)
			}
			if code := apiRequest(s, http.MethodGet, "/api/v1/flows", "management-test-key-32-bytes-long-and-separate", "").Code; code != http.StatusServiceUnavailable {
				t.Fatalf("/api/v1/flows = %d after a refused start, want 503", code)
			}
		})
	}
}
```

Run: `(cd server && go test -count=1 -run 'TestRefusedManagementStart|TestUnavailableFlowProviders|TestEnableManagement' .)`. Expected: both subtests FAIL (`a refused start left store=true flows=true mcp=true …`); `TestUnavailableFlowProvidersReported` still passes.

- [ ] **Step 2: Check before installing.** In `server/management.go`:
(a) change `func (s *Server) unavailableFlowProviders(ctx context.Context) []string {` to `func (s *Server) unavailableFlowProviders(ctx context.Context, flows *flow.Service) []string {` and inside it `page, err := s.flows.List(ctx, 100, offset)` to `page, err := flows.List(ctx, 100, offset)`;
(b) replace the block from `	s.SetStore(store)` through the end of the `if bad := s.unavailableFlowProviders(ctx); len(bad) > 0 {…}` block with:

```go
	// The production provider check and the recording sink can still fail:
	// both run before anything points at the store fail() closes.
	flows := flow.NewService(store, s.availability)
	if bad := s.unavailableFlowProviders(ctx, flows); len(bad) > 0 {
		for _, b := range bad {
			s.logger.Error("flow references a provider this build cannot run", slog.String("problem", b))
		}
		if s.cfg.Profile == ProfileProduction {
			return fail(fmt.Errorf("%d stored flow stage(s) use providers missing from this build (xAI STT/TTS need -tags xaispeech): %s", len(bad), strings.Join(bad, "; ")))
		}
	}
	var sink *record.LocalSink
	if rc := s.cfg.Recording; rc.Enabled {
		if sink, err = record.NewLocalSink(rc.Dir); err != nil {
			return fail(fmt.Errorf("recording sink: %w", err))
		}
	}

	s.SetStore(store)
	s.endOrphanedSessions(ctx, store)
	s.mcpService = mcpService
	s.SetProviderManagementAPI(s.providersRoutes())
	s.flows = flows
	s.SetFlowManagementAPI(api.NewFlowsHandler(s.flows))
	s.SetMCPManagementAPI(mcp.Handler(mcpService))
	s.SetSessionManagementAPI(api.NewTracesHandler(store.Sessions(), store.Traces()))

	if policy != nil {
		m.retention = retention.NewJob(store.Retention(), *policy, retention.WithInterval(s.cfg.Retention.Interval), retention.WithLogger(s.logger))
		m.retention.Start(context.Background())
	} else {
		s.logger.Warn("retention disabled: stored transcripts and traces are kept indefinitely")
	}
	if sink != nil {
		s.recSink = sink
		jctx, stop := context.WithCancel(context.Background())
		m.recStop = stop
		go sink.Janitor(jctx, time.Duration(s.cfg.Recording.RetentionDays)*24*time.Hour, time.Hour, s.logger)
	}
```

(`err` is in scope from `store, err := storage.Open(…)`.)
(c) `server/management_test.go` `TestUnavailableFlowProvidersReported`: replace both `s.unavailableFlowProviders(t.Context())` with `s.unavailableFlowProviders(t.Context(), s.flows)`.

Run: `(cd server && gofmt -l . && go vet ./... && go test -race -count=1 -run 'Management|UnavailableFlow|DevStore|Recording|Retention' .)`. Expected: `ok`.

- [ ] **Step 3: Gate.** Run the gate block with `4.6c-s3` (verify, e2e-echo, offline-test).

- [ ] **Step 4: Commit.**

```bash
git add server/management.go server/management_test.go
git commit -m "fix(server): a refused management start leaves no closed store behind

The production provider check and the recording sink ran after the
store, flow service and routes were installed, so their failure closed a
store the server still pointed at. Both now run first.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.6d: the demo install outlives its first caller, and Shutdown still cancels and waits for it

**Files:**
- Modify: `server/demo_api.go`, `server/server.go`
- Test (append): `server/demo_api_test.go`

**Interfaces:**
- Consumes: `racyServers`, `racyFlows`, `singleflight.Group.DoChan`, `s.mu`, `s.shuttingDown`, `s.demoWG`, `s.stopDemo`, `waitDemo`.
- Produces: `demoInstallTimeout` (30 s), `errDemoShuttingDown`, `Server.stopFlight context.CancelFunc` (guarded by `s.mu`), `(*Server).stopDemoLocked()`; test double `gatedServers` with `newGatedServers(ignoreCancel bool)`.

Why not `context.WithoutCancel(ctx)`: `autoInstallDemo` exists so Shutdown can cancel the install (`stopDemo`) and then wait for it (`waitDemo` before `closeManagement`, `server/server.go:1146`, "the install must not run against a closed store"). A flight detached from every context would make the auto-install's caller return at once on cancel, `demoWG` would reach zero, and `closeManagement` would close the store under a flight that keeps running up to 30 s. The flight therefore runs on a server-owned context Shutdown cancels, and registers on `demoWG` itself.

- [ ] **Step 1: Write the failing tests.** Append to `server/demo_api_test.go`:

```go
// gatedServers hands the context an install runs on to the test, then waits for release. With
// ignoreCancel it keeps waiting after that context is cancelled (an install that has not reached a
// cancellation point yet).
type gatedServers struct {
	*racyServers
	got          chan context.Context
	release      chan struct{}
	ignoreCancel bool
}

func newGatedServers(ignoreCancel bool) *gatedServers {
	return &gatedServers{racyServers: &racyServers{}, got: make(chan context.Context, 1), release: make(chan struct{}), ignoreCancel: ignoreCancel}
}

func (g *gatedServers) List(ctx context.Context) ([]storage.MCPServer, error) {
	select {
	case g.got <- ctx:
	default:
	}
	if g.ignoreCancel {
		<-g.release
	} else {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.racyServers.List(ctx)
}

// The install is shared by every caller (singleflight): the caller that started it hanging up must not
// cancel it, or every joiner fails with context.Canceled too.
func TestDentalInstallOutlivesTheCallerThatStartedIt(t *testing.T) {
	s, fl, srv := &Server{}, &racyFlows{}, newGatedServers(false)
	caller, hangUp := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := s.installDental(caller, srv, fl, "https://d/mcp"); first <- err }()
	flight := <-srv.got
	hangUp()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that hung up got %v, want context.Canceled", err)
	}
	if err := flight.Err(); err != nil {
		t.Fatalf("the shared install was cancelled with its first caller (%v): every joiner would fail", err)
	}
	close(srv.release)
	if _, err := s.installDental(context.Background(), srv, fl, "https://d/mcp"); err != nil {
		t.Fatal(err)
	}
	if srv.creates != 1 || fl.creates != 1 {
		t.Fatalf("server creates %d, flow creates %d, want one install", srv.creates, fl.creates)
	}
}

// Shutdown cancels a running install and waits for it (waitDemo) before it closes the store; once shutting
// down, no install starts.
func TestShutdownCancelsTheDemoInstallAndWaitsForIt(t *testing.T) {
	s, fl, srv := &Server{}, &racyFlows{}, newGatedServers(true)
	done := make(chan error, 1)
	go func() { _, err := s.installDental(context.Background(), srv, fl, "https://d/mcp"); done <- err }()
	flight := <-srv.got
	s.mu.Lock() // what Shutdown does first
	s.shuttingDown.Store(true)
	s.stopDemoLocked()
	s.mu.Unlock()
	if !errors.Is(flight.Err(), context.Canceled) {
		t.Fatalf("Shutdown did not cancel the install: %v", flight.Err())
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.waitDemo(short) {
		t.Fatal("waitDemo returned while the install still ran: the store would close under it")
	}
	close(srv.release)
	if !s.waitDemo(context.Background()) {
		t.Fatal("waitDemo did not return after the install ended")
	}
	<-done
	if _, err := s.installDental(context.Background(), srv, fl, "https://d/mcp"); !errors.Is(err, errDemoShuttingDown) {
		t.Fatalf("an install after Shutdown began got %v, want errDemoShuttingDown", err)
	}
}
```

Run: `(cd server && go test -race -count=1 -run 'TestDentalInstallOutlives|TestShutdownCancelsTheDemo|TestInstallDentalIsSerialised' .)`. Expected: build failure (`s.stopDemoLocked undefined`, `undefined: errDemoShuttingDown`); with those two stubbed on today's code (verified in a scratch copy), `TestDentalInstallOutlives…` fails with `the shared install was cancelled with its first caller (context canceled)` and `TestShutdownCancelsTheDemo…` with `Shutdown did not cancel the install: <nil>`.

- [ ] **Step 2: Run the flight on a server-owned context.** In `server/server.go`, in `type Server struct` after `	stopDemo      context.CancelFunc // cancels the startup demo install; guarded by mu` add `	stopFlight    context.CancelFunc // cancels the running demo install flight; guarded by mu`; in `Shutdown` replace

```go
	s.shuttingDown.Store(true)
	if s.stopDemo != nil {
		s.stopDemo()
	}
	s.mu.Unlock()
```

with

```go
	s.shuttingDown.Store(true)
	s.stopDemoLocked()
	s.mu.Unlock()
```

In `server/demo_api.go` add `"errors"` to the imports and replace `installDental` (and its comment) with:

```go
// demoInstallTimeout bounds one shared install (discovery plus DB writes).
const demoInstallTimeout = 30 * time.Second

// errDemoShuttingDown refuses an install once Shutdown began: it must not run against a closing store.
var errDemoShuttingDown = errors.New("demo install refused: the server is shutting down")

// installDental collapses overlapping installs (startup auto-install vs the API)
// into one run, so the list-then-create inside cannot run twice; no lock is held
// during discovery or DB work. The run is shared by every caller, so it does
// not use any caller's context: it runs on its own, bounded, which Shutdown
// cancels (stopDemoLocked) and then waits for (waitDemo, through demoWG) before
// it closes the store. Each caller waits for the result or for its own ctx.
func (s *Server) installDental(ctx context.Context, servers demo.Servers, flows demo.Flows, endpoint string) (demo.Result, error) {
	ch := s.demoSF.DoChan("dental", func() (any, error) {
		fctx, cancel := context.WithTimeout(context.Background(), demoInstallTimeout)
		defer cancel()
		s.mu.Lock()
		if s.shuttingDown.Load() {
			s.mu.Unlock()
			return demo.Result{}, errDemoShuttingDown
		}
		s.demoWG.Add(1) // under mu with the check, so Shutdown's waitDemo cannot see zero first
		s.stopFlight = cancel
		s.mu.Unlock()
		defer s.demoWG.Done()
		defer func() {
			s.mu.Lock()
			s.stopFlight = nil
			s.mu.Unlock()
		}()
		return demo.InstallDental(fctx, servers, flows, endpoint)
	})
	select {
	case r := <-ch:
		res, _ := r.Val.(demo.Result)
		return res, r.Err
	case <-ctx.Done():
		return demo.Result{}, ctx.Err()
	}
}

// stopDemoLocked cancels the startup auto-install and the running install
// flight (Shutdown, with s.mu held).
func (s *Server) stopDemoLocked() {
	if s.stopDemo != nil {
		s.stopDemo()
	}
	if s.stopFlight != nil {
		s.stopFlight()
	}
}
```

Run: `(cd server && gofmt -l . && go vet . && go test -race -count=20 -run 'TestDentalInstall|TestShutdownCancelsTheDemo|TestInstallDentalIsSerialised|TestWaitDemo|TestAutoInstall' . && go test -race -count=1 -tags xaispeech -run 'Demo|Dental' .)`. Expected: `ok` (verified in a scratch copy). Mutation checks: build the flight context with `context.WithoutCancel(ctx)` (the first draft) → `Shutdown did not cancel the install`; move `s.demoWG.Add(1)` out of the flight → `waitDemo returned while the install still ran`.

- [ ] **Step 3: Gate.** Run the gate block with `4.6d-s3` (verify, e2e-echo, offline-test).

- [ ] **Step 4: Commit.**

```bash
git add server/demo_api.go server/server.go server/demo_api_test.go
git commit -m "fix(server): the shared demo install outlives its first caller

The singleflight ran on the first caller's context, so a click that went
away cancelled the install for everyone waiting on it. The flight now
runs on its own bounded context, which Shutdown cancels, and registers on
the demo wait group so Shutdown still waits for it before closing the
store.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.6e: MCP disable/enable distinguish not found, store failure and pending deletion

**Files:**
- Modify: `server/mcp/http.go`
- Test (append): `server/mcp/http_test.go`

**Interfaces:**
- Consumes: `writeLookupError`, `service.Get`, `TestMCPRoutesDistinguishNotFoundFromStoreFailure`'s helpers.
- Produces: no new names.

- [ ] **Step 1: Write the failing rows.** In `server/mcp/http_test.go` `TestMCPRoutesDistinguishNotFoundFromStoreFailure`, replace `	routes := [][3]string{{"DELETE", "", ""}, {"GET", "", ""}, {"POST", "/detach-delete", `{"confirm":true}`}}` with:

```go
	routes := [][3]string{{"DELETE", "", ""}, {"GET", "", ""}, {"POST", "/detach-delete", `{"confirm":true}`},
		{"POST", "/disable", ""}, {"POST", "/enable", ""}}
```

Run: `(cd server && go test -count=1 -run 'TestMCPRoutesDistinguish|TestMCPDisableDelete' ./mcp/)`. Expected: FAIL `POST/enable missing server: 409, want 404`, `POST/disable closed store: 404, want 500`, `POST/enable closed store: 409, want 500`; `TestMCPDisableDeleteConflictAndConfirmedDetachRoutes` (pending deletion → 409) passes.

- [ ] **Step 2: Map the errors.** In `server/mcp/http.go`, in the disable route replace

```go
		if err := service.Disable(r.Context(), serverID); err != nil {
			api.WriteError(w, 404, "not_found", "MCP server not found", id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			api.WriteError(w, 404, "not_found", "MCP server not found", id)
			return
		}
```

with

```go
		if err := service.Disable(r.Context(), serverID); err != nil {
			writeLookupError(w, err, id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			writeLookupError(w, err, id)
			return
		}
```

and in the enable route replace

```go
		if err := service.Enable(r.Context(), serverID); err != nil {
			api.WriteError(w, 409, "mcp_server_unavailable", "MCP server cannot be enabled while deletion is pending", id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			api.WriteError(w, 404, "not_found", "MCP server not found", id)
			return
		}
```

with

```go
		if err := service.Enable(r.Context(), serverID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				writeLookupError(w, err, id) // the store failed: 500
				return
			}
			// No row updated: the server is missing (404) or its deletion is pending (409).
			if _, gerr := service.Get(r.Context(), serverID); gerr != nil {
				writeLookupError(w, gerr, id)
				return
			}
			api.WriteError(w, 409, "mcp_server_unavailable", "MCP server cannot be enabled while deletion is pending", id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			writeLookupError(w, err, id)
			return
		}
```

Run: `(cd server && gofmt -l ./mcp && go test -race -count=1 ./mcp/)`. Expected: `ok`. Also `grep -rn "mcp-servers/.*/\(enable\|disable\)" ui/src e2e --include='*.ts' --include='*.tsx'` and check every caller handles 404/500 the way it handled 409/404 (the UI shows the error text); report anything that relied on the old codes.

- [ ] **Step 3: Gate.** Run the gate block with `4.6e-s3` (verify, e2e-echo, offline-test).

- [ ] **Step 4: Commit.**

```bash
git add server/mcp/http.go server/mcp/http_test.go
git commit -m "fix(mcp): disable/enable report not found, store failure and pending deletion apart

Disable answered 404 and enable 409 for every error, a broken store
included. They now use the shared lookup mapping (404/500), and enable
answers 409 only when the server exists and its deletion is pending.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.6f: `turn.analyzer.stop_secs` is the field `Hold` (lint ST1011)

**Files:**
- Modify: `server/config.go`, `server/config_defaults.go`, `server/config_defaults_test.go`, `server/config_validate.go`, `server/registry.go`, `server/turn_stop_test.go`, `server/testdata/effective_development.json`, `server/testdata/effective_production.json`
- Test (append): `server/config_test.go`

**Interfaces:**
- Produces: `Config.Turn.Analyzer.Hold time.Duration` (YAML `stop_secs`), replacing `StopSecs`.

- [ ] **Step 1: Write the YAML-compat test.** Append to `server/config_test.go` (add imports `os`, `path/filepath`, `time` if missing):

```go
// The Go field is a duration named for what it does; the YAML key stays Pipecat's stop_secs.
func TestTurnAnalyzerStopSecsKeepsItsYAMLKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("profile: development\nturn:\n  analyzer:\n    stop_secs: 2s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Turn.Analyzer.Hold != 2*time.Second {
		t.Fatalf("stop_secs loaded as %v", cfg.Turn.Analyzer.Hold)
	}
}
```

Run: `(cd server && go test -count=1 -run TestTurnAnalyzerStopSecs .)`. Expected: build failure `cfg.Turn.Analyzer.Hold undefined`.

- [ ] **Step 2: Rename.** `server/config.go`: `			StopSecs  time.Duration `yaml:"stop_secs"`` → `			Hold      time.Duration `yaml:"stop_secs"` // Pipecat stop_secs`, and in the comment above `"incomplete" holds the turn up to StopSecs of` → `"incomplete" holds the turn up to Hold (stop_secs) of`. Replace `Turn.Analyzer.StopSecs` with `Turn.Analyzer.Hold` in `server/config_defaults.go`, `server/config_defaults_test.go`, `server/config_validate.go` (`a.StopSecs` → `a.Hold`), `server/registry.go` (two uses), `server/turn_stop_test.go` (`cfg.Turn.Analyzer.StopSecs = stopSecs` → `cfg.Turn.Analyzer.Hold = hold`, and the constant `stopSecs` → `hold` in `const speechTimeout, stopSecs = …` and its uses). Regenerate the goldens (only the key changes):

```bash
(cd server && UPDATE_GOLDEN=1 go test -count=1 -run TestConfig_ShippedYAMLEffectiveGolden . && git diff testdata/ | grep '^[-+] ')
```

Expected: `-   "StopSecs": 3000000000,` / `+   "Hold": 3000000000,` in each file. Run: `grep -rn "StopSecs" --include='*.go' . ; (cd server && gofmt -l . && go vet ./... && go test -race -count=1 -run 'Turn|Config' .)`. Expected: the grep prints nothing; `ok`. Optional lint (needs the module download): `(cd server && go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 -checks ST1011 ./...)` prints nothing about `StopSecs`.

- [ ] **Step 3: Gate.** Run the gate block with `4.6f-s3` (`make verify` only: no behaviour change).

- [ ] **Step 4: Commit.**

```bash
git add server/config.go server/config_defaults.go server/config_defaults_test.go server/config_validate.go server/registry.go server/turn_stop_test.go server/config_test.go \
  server/testdata/effective_development.json server/testdata/effective_production.json
git commit -m "refactor(server): turn.analyzer.stop_secs is the duration field Hold

Lint ST1011: a time.Duration named with a unit suffix. The YAML key
stays stop_secs.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4.7: phase close-out (flake sweep, full gate, docs check, review)

**Files:** none planned. Reviewer-driven fixes only, each with its own failing test and commit.

**Interfaces:** Consumes everything above. Produces the phase verdict.

- [ ] **Step 1: Flake sweep** (all logs under the flake directory).

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
(cd core && go test -race -count=20 ./ttsunit/ && go test -race -count=50 -run 'TestTTSWatchdogExpiry' . \
  && go test -race -count=20 -run 'Failover|SyncError|Downstream|UnitFailure' .) 2>&1 | tee "$L/4.7-s1-core.log"
go test -race -count=20 ./vad/ 2>&1 | tee "$L/4.7-s1-vad.log"
go test -race -count=20 -run 'TestBargeIn|TestOutputResampler|TestMessage|TestDisconnectBot|TestServerSideClose|TestLargestValidFrame|Ping' ./transport/ws/ 2>&1 | tee "$L/4.7-s1-transport.log"
for m in providers/openai providers/xai providers/google providers/echo; do (cd $m && go test -race -count=5 ./... && go test -race -count=3 -tags xaispeech ./...) || exit 1; done 2>&1 | tee "$L/4.7-s1-providers.log"
(cd server && go test -race -count=3 -tags xaispeech -run 'TestCallBehaviorBargeIn|TestCallBehaviorPipecatClient|TestCallBehaviorASynchronous|TestCallBehaviorVADKeepsHearing|TestCallBehaviorAStalledTTSUnit|TestVoiceTokenLifetime|TestConfigRejects|TestRefusedManagementStart|TestDentalInstall|TestShutdownCancelsTheDemo|TestLLMClassify' . ./mcp/) 2>&1 | tee "$L/4.7-s1-server.log"
node tools/compat/probe_playback_clear.js 2>&1 | tee "$L/4.7-s1-probe.log"
```

Expected: everything `ok`; the probe exits 0 with the recorded-decision line. Any failure: name the test, keep the log, fix with a failing test first.

- [ ] **Step 2: Full gate, sequentially, on an idle machine.** Run the gate block with `4.7-s2` (verify, e2e-echo, offline-test, provider-matrix-dry), then `make test-postgres 2>&1 | tee "$L/4.7-s2-postgres.log"` (Docker, or `TEST_POSTGRES_DSN`). Expected: all green.

- [ ] **Step 3: Dead-code and bloat check.**

```bash
grep -rn "respawnWithBackoff\|startAndHandshake\|observeDisconnect\|StopSecs\|w.expire(u, attempt)" --include='*.go' core providers transport vad server
grep -rn "bot-interrupted" docs/pipecat-compatibility.md docs/compatibility/playback-clear-decision.json docs/pipecat-parity-matrix.md
git diff --stat main...HEAD
```

Expected: the first grep prints nothing; the second finds the three docs.

- [ ] **Step 4: Adversarial review.** Dispatch an independent reviewer agent (AGENTS.md "Mandatory Code Review Workflow") on `git diff main...HEAD` with this brief:

> Review Phase 4 (failover and transport correctness) against docs/superpowers/remediation/plans/2026-10-18-phase-4-failover-transport.md. Hunt for: (1) A7 × pacing: any path where audio of a cut reply (other than the one audio frame already admitted to the socket) reaches the wire after bot-interrupted, where the next reply's audio precedes it, where it is sent twice for one barge-in or while the assistant is idle (typed turns, idle bot-stuck interruptions, route flips; the known case after an LLM error leaves activeTurn set is deferred), and where the synchronous WriteWireMessage on the run loop can stall barge-in latency badly on a slow client; (2) the probe: can probe_playback_clear.js pass while the pinned client's behaviour changed (timing of the async listener, key order), and is the decision file still accurate; (3) A9: every synchronous error path through begin → syncFailure → failoverLocked (lock handoff, the first-token watchdog firing between begin and syncFailure, the user message forwarded zero or two times, a fallback refused synchronously, side-effect and budget checks), and whether the providers' re-entered ErrorFrame for ErrTooManyTasks can block the run loop or double-count Errors; (4) A10: the rebuilt resampler on a rate change, the shared resamplersMu between the read goroutine and the run loop, any per-frame allocation added; (5) A11: respawnLoop termination (quit, success), wg.Add from the run loop vs backend.close's Wait, markDown/respawnLoop on down/up/availCount/healthy (one critical section?), a job handed between two flapping workers (bounded by its deadline?), closeMu.RLock vs close's Lock, leaked children when close races a handshake, the handshake goroutine on timeout, Restarts accounting, a missing up.Store after a respawn; (6) A12: every remaining path where a TTS candidate's ProcessFrame runs off the run loop (fromCandidate → run on a candidate's task goroutine: is it exclusive with the loop's run through cur, and is a unit fenced meanwhile dropped before it takes a slot?), the ttsExpire retry loop after close, an expiry for a unit that is no longer cur, expire's Reenter calls from the loop on a full queue; (7) 4.6: one disconnect_* per connection on every path and the ctx-cancel → server rule (ping failure stays client), the 20 KB limit vs any legitimate client message, the token lifetime change and its 5 m cap, management ordering (endOrphanedSessions, janitor start), the demo flight vs Shutdown (stopDemoLocked, demoWG.Add inside the flight under s.mu, an install after shutdown began), MCP enable's extra Get; (8) YAGNI: anything added that no server path consumes (Registry.ttsUnitTimeout is a test seam on the registry), helpers that duplicate existing ones, docs that overclaim (playback clear, VAD resilience); (9) e2e determinism: every assertion that could pass with a late or duplicate event, any sleep used for synchronisation (the two bounded negative waits in core/tts_failover_loop_test.go are intentional), the pgrep-based kill. Report each finding with file:line and a failing-test idea.

Resolve every finding with a failing test, then the fix, then `make verify`, then a `fix(<area>): …` commit.

- [ ] **Step 5: Deferred-minor sweep.** Record these in the PR description as deferred (not done here), each with an owner phase or "backlog": a ping-failure close is counted `disconnect_client` (arguably `timeout`; transport, backlog); a VAD worker crash is noticed on its next batch only (no process-exit watcher; that batch is re-run on a live worker, backlog) and the child's stderr tail is collected but never logged (vad has no logger, backlog); no Pipecat golden for `bot-interrupted` (needs `make parity-regen` with the pinned venv; the unit test pins `models.py:637-646`, master Phase 8); after an LLM error the assistant's `activeTurn` is never reset, so the next turn sends a `bot-interrupted` with nothing cut off (pre-existing, backlog); the server still pins every TTS candidate to one rate (`ttsRate`) because `TTSFailover.expire` closes a unit at the candidate's rate, and a rate switch restarts the resampler's filter state (backlog); `fromCandidate` still starts the next TTS unit from a candidate's task goroutine (exclusive with the run loop through `cur`; moving it to the loop would put unit starts behind paced audio, backlog); an `LLMFailover` whose every candidate fails synchronously still fails the session (R4); no breaker learns of a synchronous LLM failure, so the primary's circuit never opens and every turn retries it first (backlog); a first-token watchdog firing between `begin` and `syncFailure` hits `g.pos != 0` and fails the session (the timeout is seconds away; accepted); `server/probe.go` pre-session close (R10, no change).

- [ ] **Step 6: Report.** In the PR description: the gate logs' paths and verdicts, the reviewer's findings and their resolutions, the deferred list, the rulings below, and, as requirement notes for the human (not decided silently): (a) the playback-clear answer (R1) with the probe's three observation lines, and that the V2 output-flush gate remains unresolved; (b) the token-lifetime behaviour change for deployments whose own YAML sets `max_lifetime` above 1 m, and that above 5 m the server now refuses to start (R9, Q2); (c) whether `make offline-test` and `make test-postgres` ran and passed; (d) that no Playwright test was added (Q4) and that the real-client Node test ran. Phase 4 is done when Steps 1–4 are green and the reviewer has no open findings.

---

## Rulings (with evidence; binding for this phase)

1. **R1 (A7 key question): sending the protobuf `interruption` frame does not make the pinned client clear its playback, and no other wire signal does.** Evidence: the pinned transport's `Frame` type and `deserialize` know only `audio`/`message` (`websocket-transport/dist/index.js:2762-2896`), so field 5 is rejected and logged (`:3028-3037`); nothing but `disconnect()` calls the player's `interrupt()` (`:1570`, `:1573-1575`, `:1787`); `handleMessage` ignores `bot-interrupted` (`client-js/dist/index.js:2496`) and only fires the app callback for `user-started-speaking` (`:2371`); the scratch experiment and the rewritten probe observe exactly this. Minimal correct emission: RTVI `bot-interrupted` (Pipecat `observer.py:480-483`), not field 5. The decision stays `relax-unmodified-client-playback-clear`, now with per-signal observations the probe enforces.
2. **R2 (A7 order point): emit on `FrameFlush` after `BeginFlush`, synchronously.** Evidence: `FlushFrame` is produced only by `handleInterruption` (`assistant_context.go:694`); `BeginFlush` drops queued interruptible audio and fences stale frames (`pacing.go:367`, `:500-545`); a write already admitted is not cancelled (`docs/output-pacing-and-flush.md:20`), hence "at most the one audio frame already admitted to the socket" (one provider chunk, which can be longer than 20 ms; this bound is argued, not tested: moving the write before `BeginFlush` passes the tests too, because `beginWrite`'s stale check drops queued frames either way). Sending from the flush acknowledgement callback instead would avoid that frame but could let the next reply's first frame precede the message (the ack is asynchronous and the pacer may already write new-generation audio), which is worse for a client that clears on it.
3. **R3 (A9 classes): table in Decision 2.** Evidence: `server/stt_failover.go:34-37` (Phase 2 ruled `ErrTooManyTasks` local); a fallback's synchronous failure already counts as failed (`llm_failover.go:284-308`); TTS absorbs a refused task the same way (`ttsunit.go:228-243`).
4. **R4 (A9 exhaustion): when every candidate fails synchronously the session still fails.** Evidence: after the providers absorb `ErrTooManyTasks`, the remaining synchronous errors are attachment or configuration bugs (`processor not attached to session`, `MapToolDefinitions`), which today fail loudly with or without fallbacks.
5. **R5 (A10): rebuild the transport's output resampler on a rate change; keep the server's single TTS rate.** Evidence: no server path emits two rates (`ttsRate`, `server/tts_failover.go:91-99`; `core.NewTTSFailover` rejects mismatched rates, `:114`; DTMF tones use the same rate, `server/dtmf.go:43-47`), so a per-rate map with a bound would be machinery nothing uses; `core/tts_failover.go:440-449` closes a stalled unit at the candidate's rate. The e2e is at the transport boundary for the same reason, and asserts the ratio, not filter continuity.
6. **R6 (A11): hand off at the down worker's run loop, not in `Submit`, including the batch whose pipe failed.** Evidence: `vadworker` is stateless and `Job.State` goes in and comes back (`vad/processor.go:304,379`); `PerSessionLimit` is fixed at 1 and the processor has one job (`p.busy`); a pipe failure leaves the job's input and state unchanged; one place, zero cost on the healthy path; `Executor.HardKill`'s existing test expects the next job to succeed (`vad/resilience_test.go:118-207`). The design review ran the SIGKILL server e2e 3/3 on macOS (and it fails on today's executor).
7. **R7 (A12): move the watchdog expiry onto the run loop; keep `abandon`, `SerialQueue` and the Google/xAI TTS as they are.** Evidence: the root cause is `expire` driving candidates from a `time.AfterFunc` goroutine (`core/tts_failover.go:258,404-452`) against `SerialQueue`'s documented contract (`core/serial_queue.go:19`); the first draft's `ttsunit.Order` plus a locked `SerialQueue` still deadlocked 3/3 in the review's stress run (slot n+1's job queued ahead of slot n's); `TestTTSWatchdogExpiryDrivesCandidatesOnlyFromTheRunLoop` fails 50/50 on today's code and passes 50/50 after (scratch copy, `-race -count=50`); with serialized calls the refused slot is always the newest, so `abandon`'s `nextSeq--` is right.
8. **R8 (4.6a): 20 KB.** Evidence: `MaxMessageDataBytes` 16 KB and `MaxAudioBytes` 4 KB (`wire.go:16-17`); a full 16 KB message frame is a few bytes over 16 KB; 20 KB keeps ~20% headroom; the read limit (21 KB) stays below the library's 32 KB default.
9. **R9 (4.6d of master: token lifetime): honor the knob, default 1 m.** Evidence: only the server mints voice tokens (no `SignedString` outside tests), so the validator ceiling of 5 m protected nothing; the console and integrations dial right after issuance; `docs/api/v1.md:73` documented the hidden cap. Issued lifetime for the shipped configs is unchanged (60 s).
10. **R10 (probe partial-failure close): no change.** Evidence: before `core.NewSession` only `core.NewPipeline` (nil processor) or `core.NewSession` (empty pipeline, invalid budget; `core/session.go:150-156`) can fail, neither reachable with a built processor and `DefaultResourceBudget()`; after it, `sess.Close` closes the processors (`server/probe.go:86-87`). A defensive close would be untestable dead code.
11. **R11 (demo install): a server-owned flight context that Shutdown cancels, registered on `demoWG` inside the flight.** Evidence: `server/server.go:1018-1023` cancels `stopDemo`, `:1146` waits `waitDemo` before `closeManagement` ("the install must not run against a closed store"); `context.WithoutCancel` (the first draft) would have broken that guarantee; `TestShutdownCancelsTheDemoInstallAndWaitsForIt` pins it.
12. **R12 (disconnect accounting): one deferred count in `readLoop`; a server-cancelled connection is `server`.** Evidence: `readLoop` is the single goroutine every connection runs until it ends (`transport.go:595-596`); `Close` counts `client_disconnected` once (`:923-925`); `disconnectCategory` already classifies a failed `Read`, but a `Read` failing because the server cancelled `c.ctx` (`Close`, the ready watchdog `:560-564`) has status -1 and was classified `client`; the ping path (`:650-660`) cancels too and is the client's fault, so `peer_unresponsive` keeps `client` (the existing `TestPing_StuckClientSessionTornDown` pins it).

## Rulings on the open questions (controller, 2026-10-18; binding)

- **Q1. The protobuf `interruption` frame is not sent.** Pipecat's own server makes websocket-transport 1.7.2 log `Failed to deserialize incoming message` on every barge-in; that is not copied.
- **Q2. `token.max_lifetime` is honoured, and capped at 5 m by `ValidateConfig`** (the voice token travels in the `/ws` URL query, so its lifetime stays bounded); release note for deployments whose own YAML sets `5m` (now 5-minute tokens) or more (now a startup error). Pinned by `TestVoiceTokenLifetimeFollowsTheConfiguredMaximum`, `TestConfigRejectsAVoiceTokenLifetimeAboveFiveMinutes`.
- **Q3. No `voice_vad_workers_available` gauge.** Restarts and `vad_unavailable` rejects cover the runbook.
- **Q4. No Playwright test this phase** (nothing is UI-visible), provided the real-client Node test (`TestCallBehaviorPipecatClientTakesABargeInWithoutErrors`) actually runs in `make test-e2e-go`: it does, on Google-like echo timing (4.1 Step 7; verified in a scratch copy, about 7 s).
- **Q5. MCP enable on a missing server answers 404** (was 409). The only caller is `ui/src/api/client.ts:668/676`, which shows the error text.

## Self-review

- **Spec coverage.** A7: Task 4.1 (emission, transport and server e2e on Google-like echo timing, real client, evidence-based probe and decision, docs, parity row); key question answered in R1 with the probe's observations. A9: Task 4.2 (core sync path, classification, providers, server e2e with factory override and exactly-once answers, matrix row). A10: Task 4.3 (rebuild on rate change, transport wire e2e, stale comments in server and core). A11: Task 4.4 (hand-off of down and pipe-failed batches, respawn off the loop with one install critical section, no lock across the handshake, `worker_capacity_test.go` rewritten, real-child test, server e2e with a killed `vadworker`, runbook). A12: Task 4.5 (watchdog expiry on the run loop, core RED/GREEN test, close test, server stall e2e). Deferred cleanups: (a)(b)(c) 4.6a (with the server-cancel cause fix), (d) 4.6b (with the 5 m cap), (e) 4.6c, (f) R10, (g) 4.6f, (h) 4.6d (server-owned flight Shutdown cancels and waits for), MCP routes 4.6e. Master Phase 4 gate (`-race -count=20` on `core/ttsunit` and `vad`, reviewer): 4.7 Steps 1 and 4. Layers: L1 every task; L2 transport 4.1/4.3/4.6a, server 4.1/4.2/4.4/4.5, VAD children 4.4; L3 4.1; L4 ruled out (Q4); stress 4.4/4.5/4.7.
- **Proven in a scratch copy (git archive of 3a243fd + this plan's code, `go test -race`).** 4.1: both barge-in server tests and the real-client test pass 3/3 (about 7 s each); removing the emission fails both, emitting twice fails both with `2 times, want 1`; the rewritten probe prints the recorded observations and exits 0 with the new decision file, 2 with the old one. 4.3: wire test fails at 6239 samples on today's code, passes `-count=20` after. 4.4: `./vad/` green, worker tests `-count=20` green, the SIGKILL server e2e 2/2. 4.5: `TestTTSWatchdogExpiryDrivesCandidatesOnlyFromTheRunLoop` 50/50 FAIL on today's `expire`, 50/50 PASS after; core `./...` green; the stall e2e 3/3. 4.6a: the read-limit and disconnect tests `-count=20` green (the `peer_unresponsive` exception was found here: the existing ping test failed without it). 4.6b: the 5 m cap test green. 4.6d: both demo tests fail on today's code (stubs only), pass `-count=20` after. 4.2: the e2e with exactly-once answers 3/3.
- **Placeholder scan.** Every code step holds the code or an exact text-anchored replacement; `:NNN` numbers refer to 3a243fd and are hints. Two steps edit closure ends by quoting their unique last lines (4.2 Step 6). One edit is a mechanical rename across named files with a grep that must come back empty (4.6f Step 2). Docs edits quote the text they replace except `docs/observability.md` (4.6a Step 3), which names the grep to find the entry.
- **Type consistency.** `FormatBotInterrupted() (string, error)` is used by `transport.go` and its unit test. `outputRig`, `wireEvent{msg, bytes, rate, first}`, `constPCM(int, time.Duration, int16)`, `countAudio([]wireEvent, int16)` are defined once (4.1) and used by 4.1 and 4.3. `e2eCall.wire`, `wireLog()`, `countOf`, `indexFrom`, `longReply`, `bargeInRig`, `within` are defined in `server/barge_in_e2e_test.go` (4.1) and used by 4.1 and 4.4 (`server/vad_worker_e2e_test.go` is `xaispeech`-tagged like `callerSpeech`/`sendAudio`). `pushTracker` and `syncFailure(g *failoverGen, err error, um *UserMessageFrame, dir Direction, e Emitter, pushed bool) error` match their single call site. Each provider's `spentSession` implements `core.SessionController`. `Connection.outRate` is set wherever `outResampler` is built. `workerProc`, `start() (workerProc, error)`, `installLocked(workerProc)`, `handshake(io.Writer, io.Reader) error`, `markDown()`, `respawnLoop(time.Duration)`, `noteCrashLocked() time.Duration`, `handOff([]*Job, error)`, `liveQueue(int) chan *Job` are each defined once; `handOff` is called with `ErrWorker` (down worker) and the pipe error (`handleWorkerFailure`). `ttsExpire{u *ttsUnit; attempt int}`, `postExpire(*ttsExpire)` and the `*ttsExpire` case in `ProcessFrame` match; `Registry.ttsUnitTimeout` is read by `buildTTS` and set only by `TestCallBehaviorAStalledTTSUnitFailsOverAndLaterUnitsAreSpoken`. `gateFrame`/`gateStage` are reused from `core/unit_failure_test.go`. `Server.stopFlight`, `stopDemoLocked()`, `errDemoShuttingDown`, `demoInstallTimeout`, `gatedServers`/`newGatedServers(bool)` match between 4.6d's code and tests. `maxVoiceTokenLifetime` is used by `ValidateConfig` and its test's error text (`at most 5m0s`). `unavailableFlowProviders(ctx, *flow.Service)` matches both callers. `Hold` replaces `StopSecs` everywhere the grep finds it.
- **Design-review edits (second pass, 2026-10-18; all applied).** (1) 4.1 e2e tests run on Google-like echo timing with ten-sentence `longReply` and 20 s `within` waits (they could never pass on "instant"). (2) The tail bound is "at most the one audio frame already admitted to the socket (one provider chunk)", argued not tested; the untestable mutation check was removed; "never for a turn with nothing playing" became "not while the assistant is idle" with the LLM-error caveat; Pipecat sends the message on every interruption, and its Python consumers (not the local JS clients) act on it. (3) The probe records every media manager call (Proxy) and every `console.error`/`console.warn` line. (4) A12 redesigned: the watchdog expiry runs on the run loop; `ttsunit.Order`, the Google/xAI edits and the `SerialQueue` change are gone (they did not prevent the deadlock). (5) 4.4: `worker_capacity_test.go` rewritten; one critical section installs a respawned worker (the first draft's self-review wrongly called that window closed); pipe-failed batches are handed off; runbook and deferred notes corrected; stderr logging stated out of scope. (6) 4.6d: a server-owned flight context Shutdown cancels and waits for. (7) 4.6a: the disconnect-bot test waits for the disconnect sum; a server-cancelled connection counts `server` (ping failures stay `client`). (8) The offline gate probes `/healthz`. (9) 4.2's e2e asserts each answer exactly once; three gaps added to the deferred list. (10) 4.3 takes the smaller design (rebuild on rate change) and no longer claims filter continuity. (11) The open questions are ruled (Q1–Q5), with the 5 m `token.max_lifetime` cap added to 4.6b.
- **Fixes made during self-review (first pass, kept).** The emission moved from `FrameInterruption` to `FrameFlush` (a typed turn while idle would otherwise send it); the barge-in e2e waits for the barrier reply's own playback end instead of a third turn (which could legitimately interrupt again); the real-client script asserts counts and errors and leaves wire order to the Go side (the client deserializes asynchronously); `Restarts` is counted synchronously in `markDown`, so `TestWorkerProcess_HardKillAndRespawn`'s counter check is not racy; the demo tests check the flight's own context, which is deterministic.
