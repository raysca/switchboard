# M1 audit: core pipeline

Scope: `core/` (session, frames, assistant context, TTS slack merging, sentence splitting, heard text, context and summarization), `core/turn`, `core/resilience`, `core/{llm,stt,tts}_failover.go`, `core/ttsunit`, `smartturn`, `vad`. This was a read-only audit of `main` at `40dcf5f`, run on 2026-10-09. No source was changed.

**Probes.** The probes are throwaway tests in a scratch module that imports the real packages. They are not committed. Each one fails on the current code and passes once its bug is fixed.

**Baseline.** `go vet ./...` is clean on core. These all pass with no flakes or races:

- `go test -race -count=3` on `core/...`, `vad`, `smartturn`.
- `go test -race -count=6` on `core`, `core/turn`, `core/ttsunit`, `core/resilience`.

## Findings

| ID | Sev | Title | Confidence | Google / xAI / Nvidia launch |
|---|---|---|---|---|
| M1-01 | high | A VAD result can be dropped (generation fence or full inbox), and VAD then stays busy for the rest of the call | confirmed (probe) | all three (VAD is always on) |
| M1-02 | high | Idle "say" speech uses unit ids from 1<<40, so the ws pacer refuses the next reply and the call fails | confirmed (probe and code path) | all three, when `idle.prompt_mode: say` or `idle.bot_fallback` is set |
| M1-03 | high (Nvidia), medium (Google, xAI) | Every queued TTS unit holds a session task slot (budget 16): long replies lose sentences | confirmed (probe) | all three; Nvidia most (no slack merging) |
| M1-04 | medium | A TTS unit that fails with no audio leaves the reply "active" forever and uncommitted | confirmed (probe) | all three |
| M1-05 | medium | Context budget overflow inside the active turn returns an error from the assistant commit, which fails the session | confirmed (probe) | all three (tool-using agents) |
| M1-06 | low | Turn spans with no terminal event pile up; after 257 the session's tracing stops | confirmed (probe) | all three, OTel on only |
| M1-07 | low | Idle "say" speech cannot be barged in | likely | all three, with the same options as M1-02 |
| M1-08 | low | AsyncTrigger idle check and turn start race (TOCTOU): a reply can be reset without a commit or barrier | speculative | all three, async tools only |
| M1-09 | low | First-token failover sends its cancel to the old candidate late; it can hit that candidate's next generation | speculative | Google and xAI LLM with fallbacks |
| M1-10 | low | The breaker's HALF_OPEN transition is never reported to the observer | confirmed (code) | all three (metrics and /ready only) |

### M1-01: a dropped VAD result leaves VAD busy for the rest of the call (high, confirmed)

- **Where:** `vad/processor.go:130` (the result re-enters stamped with `s.CurrentGeneration()`), `vad/processor.go:290` and `:310` (`busy` is cleared only when a result is processed), `core/session.go:724` (stale-generation discard), `core/session.go:499-501` (`FrameInternal` gets `ErrQueueFull` when the inbox is full).
- **Evidence:**
  - `PerSessionLimit` is 1, so `busy` gates every later submission.
  - The `vadResultFrame` is an ordinary data envelope with `Gen` set to the generation at completion. `FrameInternal` is not fence-surviving.
  - If the generation advances while the result is still queued, `processEnvelope` discards it as stale. The same happens if the inbox is full: `enqueueData` returns `ErrQueueFull` for internal frames. The `Done` callback only ends the trace span.
  - After that, `busy` stays true and `trySubmit` never submits again.
  - Probe `TestVADResultDroppedByGenerationFenceWedgesVAD`: the result is queued, `IncrementGeneration` runs, then 10 more audio frames arrive. Output: `submissions=1 stale=1`, and no job was ever submitted again.
  - For comparison, `core/turn/processor.go:200-212` handles exactly this case for its tick frame. VAD does not.
- **Failure scenario:**
  - Any generation bump that is not made inline by a VAD result can trigger it: a DTMF key press during bot speech (`core/audio/dtmf/processor.go:141`), the idle bot-stuck interruption (control queue, processed ahead of data), a committed transcript arriving while a reply is active, or the turn manager's `TurnEventStarted`. A burst that fills the 64-slot inbox can also trigger it.
  - Results arrive every 32 ms, so a bump has a real chance of landing while one is queued.
  - From then on: no `UserStartedSpeaking`/`UserStoppedSpeaking`, so no barge-in. STT utterance boundaries and turn ends driven by the VAD stop never happen, and the call is effectively deaf until it times out.
- **Fix direction:** re-enter the result with generation 0 (or make `vadResultFrame` fence-surviving). On a `Reenter` error, clear `busy` through a path that cannot be dropped, or retry.
- **Known?** No (`docs/benchmarks/capacity.md` covers stale chunks, not this).

### M1-02: idle "say" unit ids break the next reply (high, confirmed)

- **Where:** `core/idle.go:29` (`idleUnitBase = 1 << 40`), `core/idle.go:482`; `transport/ws/pacing.go:250-258` (`acceptUnitIdentityLocked` requires unit ids that never decrease within the session and that are above the retired id); `transport/ws/transport.go:890` and `:1016-1026` (`enqueueOutput` returns errors other than slow-client, so `ProcessFrame` fails and the session fails).
- **Evidence:**
  - `say()` sends a `SynthesisUnitFrame` with `UnitID` 2^40+n. The assistant's own unit counter starts at 1.
  - Probe `TestIdleSayUnitIDBreaksLaterReplies`: unit 2^40+1 is enqueued on a real `ws.PacedWriter`, then assistant unit 7. Result: `ws: finality unit ID must be nonzero and session-monotonic`.
  - The server e2e `TestCallPipelineIdleStuckModelRecovers` passes only because it uses `FakeTransportOutput`, not the ws pacer.
- **Failure scenario:** with `idle.bot_fallback` set, a stuck turn speaks the fallback phrase, and the user's next turn fails the session on its first audio frame. The same happens with `idle.prompt_mode: say` after the first "Are you still there?" when the user answers. The goodbye path is harmless, because the call is ending anyway.
- **Fix direction:** allocate idle units from the same session-wide counter as the assistant (share one allocator), or have the pacer exempt or reset for out-of-band units.
- **Known?** No.

### M1-03: the TTS unit backlog uses up the session task budget (high for Nvidia, medium for Google and xAI; confirmed)

- **Where:**
  - `core/ttsunit/ttsunit.go:145` and `:184` (one `session.Go` per unit, which waits on `cond` until earlier units finish), with the same pattern in `providers/google/tts.go:317` and `providers/xai/tts.go:302`.
  - `core/budget.go:24` (`MaxAsyncTasks: 16`).
  - `core/ttsunit/ttsunit.go:233-238` (a refused unit is lost, and an `ErrorFrame` is sent).
  - `providers/google/llm.go:393`, `providers/xai/llm.go:340` and `providers/openai/llm.go:382` (`return session.Go(...)`, so `ErrTooManyTasks` fails the session).
- **Evidence:**
  - A unit's task holds its slot for the whole wait. With a single TTS provider there is no `TTSFailover`, which would feed units one at a time, so every unit spawns its task at once.
  - Probe `TestUnitsBeyondTaskBudgetAreDropped` (a pipeline with no other tasks, 20 units): `units dropped with ErrTooManyTasks: 4 of 20`.
  - In production, STT stream tasks, the LLM generation, transcript, DTMF and idle drivers, and flush-barrier timers already hold several slots, so the headroom is about 10 to 12 units.
- **Failure scenario:**
  - The LLM streams faster than TTS, so a reply of more than about 10 sentences (lists, directions) silently skips sentences in the middle.
  - Each skipped unit also triggers M1-04, so the reply is never committed.
  - Nvidia (no slack merging) hits this soonest. With Google and xAI, merging cuts the unit count.
  - Likely second-order effect: a `UserMessageFrame` processed while the slots are full (a typed turn, or a committed final in the same envelope as its interruption barrier, before the cancelled TTS tasks exit) makes the LLM's `session.Go` fail, and that ends the call.
- **Fix direction:** run one long-lived synthesis worker per TTS processor that drains a unit queue (no task per waiting unit), or raise and exempt the budget for waiting units. The LLM should not turn `ErrTooManyTasks` into a session failure.
- **Known?** No.

### M1-04: a failed unit with no audio leaves the reply active forever (medium, confirmed)

- **Where:** `core/assistant_context.go:239-251` (only a final `WriteCompletion` marks a unit delivered), `:305-315` (`allPendingDelivered`), `:416-418`. The TTS error path is `core/ttsunit/ttsunit.go:210-222` (an `ErrorFrame` only, with no final frame when nothing was committed) and `core/tts_failover.go:277-284` (chain exhausted).
- **Evidence:**
  - Nothing in the assistant context or the transport treats a unit's `ErrorFrame` as closing that unit.
  - Probe `TestFailedUnitLeavesReplyActive`: unit 1 is written, then unit 2 (the final unit) fails. Result: `active=true assistantMessages=0`.
- **Failure scenario:** any unit that fails after retries (or failover), or is dropped by M1-03, leaves the reply uncommitted.
  - `Active()` stays true, so `IdleController` never prompts or hangs up (`Answering`) and `AsyncTrigger` defers results up to its 60 s `MaxWait`.
  - The history lacks the reply until the user speaks again. At that point a spurious barge-in runs (generation bump, flush) and records the reply as partial.
- **Fix direction:** on an `ErrorFrame` carrying a `UnitID` of the active turn, drop that unit from `pendingUnits` (or mark it delivered and empty) and re-check the commit condition.
- **Known?** No.

### M1-05: context overflow in the active turn fails the session (medium, confirmed)

- **Where:** `core/context.go:555-575` and `:592-606` (the active turn is never evicted, which returns `ErrContextLimitExceeded`); `core/assistant_context.go:484-486` (`commitTurn`) and `:555-557` (`handleInterruption`) return the error from `ProcessFrame`, so `s.fail`.
- **Evidence:**
  - Defaults are `max_tokens` 4000 (`server/agent/config.go`) and tool results up to 64 KB (`core/tools/loop.go:67`). A tool result that fits but nearly fills the budget is accepted (only one that does not fit becomes `result_too_large`).
  - The assistant reply is then the message that overflows, and it belongs to the active turn.
  - Probe `TestOverBudgetActiveTurnCommitErrors`: a 15 KB tool result, then a roughly 1,400-character reply. Commit returns `core: conversation context exceeds budget limits even after turn eviction`.
- **Failure scenario:** a tool-using agent (search, lookup) returns a large result. The bot speaks the whole answer, then the call drops at the commit. A barge-in in the same turn fails at the partial commit.
- **Fix direction:** in assistant commits, treat `ErrContextLimitExceeded` as non-fatal (truncate or skip the history write, log or count it). Size tool results against the context budget, not only `MaxResultBytes`.
- **Known?** No.

### M1-06: open turn spans stop tracing after 257 (low, confirmed)

- **Where:** `core/session.go:756-777` and `:1160-1168` (a turn span starts per turn id); `core/session.go:1031-1039` (it ends only on `TurnEvent` Completed/Interrupted/NoSpeechTimeout or an `InterruptionFrame` with that turn); `core/trace_async.go` `enqueueStart` (`active >= capacity` refuses new spans; capacity is 257).
- **Evidence:**
  - With the turn manager off (the production default), no `TurnEventCompleted` is ever emitted, so every turn that is not interrupted keeps its span open until the session closes.
  - Probe `TestTurnSpansAccumulateUntilTracingStops`: `open spans=257 started=763 (+0 for one more frame)`.
- **Failure scenario:** in a long call with OTel on, all spans after about 257 user turns are dropped (`DroppedOperations`). There is a small memory cost as well (`turnSpans` keeps an entry per turn).
- **Fix direction:** end the turn span on the completed `AssistantMessageFrame`, or when the next user turn starts.
- **Known?** No.

### M1-07: idle "say" speech cannot be barged in (low, likely)

- **Where:** `core/idle.go:477-483` (the unit has generation 0 and a turn of `idle-say-n`), `core/assistant_context.go:259-262` (barge-in only when `activeTurn != ""`).
- **Evidence:** the say unit never makes the assistant active, so `UserStartedSpeaking` runs no barrier, and generation 0 output is not fenced anyway.
- **Failure scenario:** a fallback phrase or a say-mode prompt plays over a user who has started talking.
- **Fix direction:** have barge-in flush idle speech too (track it as a reply, or send `Interruption`/`Flush` when idle speech is playing).

### M1-08: AsyncTrigger fire races with a turn starting (low, speculative)

- **Where:** `core/async_trigger.go:140-145` and `:205-206`; `core/assistant_context.go:137-161`.
- **Evidence:** `idle()` is checked on the trigger goroutine. The `UserMessageFrame` re-enters at the LLM index, past the user-context interruption barrier.
- **Failure scenario:** a reply that becomes active between the check and the run loop handling the message is reset with no commit and no interruption. Its already-queued TTS units (same generation) keep playing next to the new reply.
- **Fix direction:** re-check busy state on the run loop when the synthetic turn is processed, or push it through the same barrier.

### M1-09: the first-token failover cancel can land on the old candidate's next generation (low, speculative)

- **Where:** `core/llm_failover.go:320-323`.
- **Evidence:**
  - The `InterruptionFrame` to the abandoned candidate is sent from the timer goroutine after `next.ProcessFrame`.
  - The providers' interruption handler cancels whatever `activeCancel` holds (for example `providers/google/llm.go:309-315`), with no generation check.
- **Failure scenario:** if a new user turn starts on that same (primary) candidate inside the window, its fresh generation is cancelled. The window is a few microseconds to milliseconds, so this is unlikely.
- **Fix direction:** send the cancel before starting the next candidate (under the generation check), or pass `Gen` so providers ignore an older cancel.

### M1-10: HALF_OPEN is never reported (low, confirmed by code)

- **Where:** `core/resilience/resilience.go:81`.
- **Evidence:** `allow()` moves Open to HalfOpen without calling `notifyLocked`.
- **Failure scenario:** observers (metrics, `/ready`) show OPEN during the probe.
- **Fix direction:** notify on the HalfOpen transition (outside the lock).

## Known-open items, re-checked in code

| Item (from `docs/pipecat-parity-matrix.md`, "Known gaps and risks") | Status in code |
|---|---|
| A TTS failover cascade deeper than 3 finishing units loses the oldest unit's tail frames | Still open: `core/tts_failover.go:378-383` keeps 4 `done` units. |
| A primary LLM that has started streaming is bounded only by the provider request timeout; the last candidate has no watchdog | Still open: `armLocked` (`core/llm_failover.go:295-304`) arms only before the first token, and only when a next candidate exists. |
| The breaker can open after 5 watchdog cancels | Still open: `OnFirstTokenTimeout` reports to the breaker; the threshold is 5 (`core/resilience/resilience.go:37`). |
| Summaries: no cooldown after a discarded summary, no output cap, token usage not attributed | Still open: the discarded branch (`core/context_summary.go:126-130`) does not set `retryAt`. |
| Eager stop has no speculation gate or retraction | Still open (`core/turn/stop.go:105-125`). |
| Turn manager is opt-in; Smart Turn is not recommended | Unchanged: `turn:` is absent from `config/production.yaml`. |
| Library-only code (`AnalyzerStop`, `SpeakingTracker.IsSpeaking`, `ValidateToolPairing`) | Unchanged. |

## Areas checked and found clean

- **Session** (`core/session.go`):
  - Admission vs Close (`admissionMu`).
  - `Go` registration vs `Wait`.
  - Control-queue priority.
  - `outSlots` accounting, including stale and fenced output and the error path.
  - The watchdog lives outside the run loop.
  - The teardown deadline covers the loop, the closers and the tasks.
  - `FlushFrame` commits once.
  - Panic containment.
- **Resilience:**
  - `Execute`: probe token release on cancel or client error; timeout vs caller deadline; `RetryAfter` capping.
  - `OpenBounded`: closes a stream that opened just as the deadline hit.
  - No public `Allow()` caller that could leak a probe.
- **LLM failover:**
  - Proxy re-entry gating (abandoned, done, other generation).
  - The `committed` rule ignores usage frames.
  - Side-effect and budget gates.
  - The replay swallows the duplicate user message.
- **TTS failover:**
  - Ownership by (candidate, unit).
  - A fenced current unit is cleared when the next unit arrives.
  - The watchdog is fenced by `attempt`.
  - Error-before-final ordering with `ttsunit`.
  - Lock is never held across candidate calls.
- **STT failover:** lock order (`sendMu` before `mu`), journal eviction keeping the open utterance's controls, asynchronous replay on a failure from the stream path, and carry/`mergeOverlap`.
- **ttsunit:** in-order release, `abandon` on a refused task, retry reset before commit, `Finish` idempotent. **ReplySlot:** close off-lock.
- **Assistant context:** slack merger maths (first unit immediate, cap, final flush at reply end); sentence splitting (abbreviations, numbers, fences, CJK); heard-text prefix (bounds, word boundaries); partial commit on barge-in (contiguous delivered units plus the heard prefix).
- **Conversation context:** whole-turn eviction keeps tool call and result pairs together; tool rounds queue plain messages; `Messages()` hides unresolved calls mid-round. **Summarizer:** span pull-back for unpaired calls; `replaceSpan` compare-and-swap.
- **Idle controller:** single timer loop, `wake` coalescing, end-after-goodbye.
- **`core/turn` processor:** tick fencing and replacement; at most one tick queued.
- **VAD worker executor:** hang, kill and respawn; every job finishes exactly once; `close` vs `Submit`.
- **`smartturn` analyzer:** asynchronous verdict with cache (light review only).
