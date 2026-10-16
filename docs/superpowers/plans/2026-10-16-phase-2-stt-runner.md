# Phase 2: Shared STT Runner, Off-Loop Dial, Drop Recovery (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. Each task is one independently reviewable deliverable with its own tests and its own commit. Tasks run in order: 2.1 → 2.2 → 2.3 (pilot) → 2.4 → 2.5 → 2.6 → 2.7 → 2.8. Task 2.7 depends only on 2.1 and may run in parallel with 2.3–2.6.

**Goal:** No STT provider opens its stream on the session run loop (A1). A provider stream that ends while its turn still owes a transcript invalidates the turn so `core.STTFailover` replays it (A8). The four websocket and gRPC adapters (Deepgram, Cartesia, ElevenLabs, Riva) share one runner instead of four ~330-line copies (D1). Deepgram exports `STTCounts` (B7), and xAI no longer holds `sendMu` across network writes (A15, second half).

**Architecture:** A new package `core/sttunit` mirrors `core/ttsunit`. It holds a `Runner` that owns `ProcessFrame`, turn binding, the per-stream session task (dial, ordered pre-roll flush, receive loop), finish and drain, drop recovery, the trace span and the error counter. A provider supplies `Dial(ctx) (Conn, error)` and a `Conn` (`Send`, `Finish`, `Recv`, `Close`) that carries the wire format and the transcript assembly. Google and xAI keep their own processors (rotation; handshake and admission rules) and get local fixes. The full design and its evidence are in [Architecture](#architecture) below.

**Tech Stack:** Go 1.27 workspace (`core`, `providers/*`, `server`, root module), `github.com/coder/websocket`, gRPC (Riva, Google Speech v2), `core.Session` supervisor (`Go`, `Reenter`, watchdog).

**Spec:** `docs/superpowers/specs/2026-10-15-deep-review.md` items **A1**, **A8**, **D1**, **A15** (second half: xai `sendMu`) and **B7**. Master plan: `docs/superpowers/plans/2026-10-15-review-remediation.md` Phase 2. Mapping: master 2.1 (design) is this document; master 2.2 is Task 2.1 here; master 2.3 is Task 2.2; master 2.4 is Tasks 2.3–2.6; master 2.5 is Task 2.7; the phase gate is Task 2.8.

## Global Constraints

Copied from `AGENTS.md` and the master plan. Every task inherits them.

- `core/` never imports `server/`. Everything added to `core/` is consumed by `server/` on a call path: `sttunit.Runner` is reached through `server/registry.go` (`deepgram.stt`, `cartesia.stt`, `elevenlabs.stt`) and `server/nvidia.go` (`nvidia.stt`). `sttunit.DefaultMaxPreRollBytes` is also read by `providers/google` (echo and google STT).
- Every queue, channel and slice is bounded: the pre-roll by the runner's `maxPreRoll` (default `DefaultMaxPreRollBytes`), and the runner's stream set by the session's `MaxAsyncTasks` because each stream holds one session task. Every goroutine has a termination path and is spawned with `session.Go`.
- No `context.Context` in new struct fields. The runner stores a `context.CancelFunc`, never a ctx. `defer cancel()` comes immediately after each derived context.
- Never hold a lock across I/O. No `Send`, `Finish`, `Recv`, `Dial` or `Reenter` runs under `Runner.mu` or `stream.mu`. Lock order is `Runner.mu` before `stream.mu`, and never the reverse.
- Use monotonic time for durations (`time.AfterFunc`, `context.WithTimeout`). Wall clock is used only for candidate timestamps (`Config.Now`), as today.
- Wrap errors with `%w` and inspect them with `errors.Is` or `errors.As`. A provider panic in the stream task is recovered by `Session.Go`, which fails that call and not the daemon.
- All code passes `go test -race` in every module (`make test`) and leaves `gofmt -l .` empty.
- Use conventional commits, one logical change each. Commit trailer: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Provider secrets come from the environment only. Never write or store keys. Every test uses fakes and placeholder keys.
- The RTVI 2.1.0 wire protocol is unchanged (nothing in this phase touches `transport/`).
- **E2E regression gate (user requirement):** after every task, run `make verify`. After every provider migration (Tasks 2.3–2.7) and at the end (Task 2.8), also run `make e2e-echo`. When `docker ps --format '{{.Names}}' | grep -E 'offline-(adapter|speaches)-1'` shows the offline stack (adapter on 127.0.0.1:8099), also run `make offline-test`, which drives the real xAI STT processor, so Task 2.7 must pass it. Run these on an otherwise idle machine, one at a time. Never run paid or live targets (`live-*`, `agent-matrix`, `provider-matrix`, `e2e-xai`, `integration`-tagged tests).
- **Per-phase gate:** an independent adversarial reviewer agent audits the phase diff (Task 2.8) before the phase is done.

## Review Focus

These are the failure modes this phase must not ship. Each one is pinned by named tests.

1. **A hung dial with a fallback configured (master Review Focus 1).** A failover candidate's dial hangs. Two things must hold. First, the call survives the 2 s `MaxFrameProcessing` watchdog (`Session.Err() == nil`). Second, the failover happens before the turn is dropped. The turn processor ends an untranscribed turn `turn.DefaultNoTranscriptTimeout` (5 s) after silence onset (`core/turn/stop.go:50,83`). The old `failoverPolicy("stt")` budget was 2 × 5 s, so a hung dial reported only after the turn was gone. Task 2.2 cuts it to 2 × 1.5 s, which is ≈ 3.1 s worst case including backoff. The fallback must then deliver the transcript of the whole utterance. Pinned by `TestRunner_DialDoesNotBlockProcessFrame` (2.1), `TestSTTFailoverPrimaryDialHangsFallbackTranscribes` (2.2: a 3 s dial, i.e. the new worst case), `TestSTTFailoverPolicyReportsAHungDialBeforeTheTurnIsDropped` (2.2, server), `TestGoogleSTTOpenRunsOffTheRunLoop` and `TestXAISTTOpenRunsOffTheRunLoop` (2.7).
2. **Pre-roll while dialing (master Review Focus 2).** No audio is lost or reordered, the buffer is bounded (oldest dropped), and a `Finish` owed by a turn that ended mid-dial goes after the held audio, never before it. This also holds while a flush pass is in flight, and `Send`/`Finish` never overlap (the test `fakeConn` fails the test on any overlap). Pinned by `TestRunner_PreRollFlushedInOrder`, `TestRunner_PreRollBounded`, `TestRunner_FinishWhileDialingGoesAfterPreRoll`, `TestRunner_LiveAudioDuringFlushKeepsOrder`, `TestRunner_FinishDuringFlushGoesLast` and `TestRunner_CloseDuringFlushIsQuiet` (2.1). On xAI the same ordering now holds without `sendMu`: `TestXAISTTDoneFollowsBufferedAudioWhenStopBeatsHandshake` and `TestXAISTTKeepsReplayBurstBeforeReady` must still pass (2.7).
3. **Goroutine termination and dial storms.** Each stream holds exactly one session task. That task ends on a dial error, on cancel (barge-in, turn interrupted, drain timeout, `Close`) or when `Recv` fails or ends. `Close` during a dial leaks nothing. After a failed dial nothing redials until the next turn or barge-in, so an outage costs one `ErrorFrame` per turn, not one per 20 ms frame. A session at its `MaxAsyncTasks` limit loses that turn's transcript (a non-fatal `ErrorFrame`), not the call. Pinned by `TestRunner_CloseCancelsDialAndLeaksNothing`, `TestRunner_DialFailureReportsErrorFrame`, `TestRunner_TaskLimitLosesTheTurnNotTheCall` and `TestRunner_FlushSendFailure` (2.1). Every runner test's cleanup also asserts `Session.LeakedTasks() == 0`. Google and xAI get `...OpenFailureReportsOncePerTurn` (2.7).
4. **Generation fencing on late finals.** A result from a stream bound to generation N that arrives after the session moved to N+1 must not reach the transcript stage, and a same-turn rebinding at a newer generation must keep the stream. Pinned by `TestRunner_LateResultOfOldGenerationIsFenced` (2.1) and the unchanged provider tests `TestSTT_SameTurnNewerGenerationAdoptsStream` (Deepgram, Cartesia), `TestSTT_SameTurnNewerGenerationRebindsStream` (ElevenLabs), `TestSTT_TeardownAndRebind` (Riva) and `TestSTT_InterruptionReopensAtNewGeneration`.
5. **Drop recovery precision (A8).** The runner invalidates only when a transcript is owed. That means the stream is bound and either `Finish` was never sent, or the last candidate it emitted was not `Final` (words heard but not committed). A `Final` candidate commits everything heard, even without `EndOfUtterance`. Riva, for example, may answer `CloseSend` with no further result. Otherwise a provider end is quiet. A clean end never reports to the breaker. The `TranscriptInvalidationFrame` comes before its `ErrorFrame`. A bound stream whose pre-roll flush fails is invalidated too. Pinned by `TestRunner_MidUtteranceDrop_InvalidatesTurn`, `TestRunner_ProviderEndIsQuietWhenNothingIsOwed`, `TestRunner_FinalBeforeFinishThenEndIsQuiet`, `TestRunner_EndAfterFinishWithWordsButNoFinalInvalidates` and `TestRunner_FlushSendFailureLosesBoundTurn` (2.2), each adapter's `TestSTT_CleanCloseMidUtteranceInvalidatesTurn` (2.3–2.6), Riva's `TestSTT_FinalBeforeCloseSendThenEndIsQuiet` (2.6), and `TestGoogleSTTProviderEndMidUtteranceInvalidatesTurn` (2.7).

---

## Architecture

### What the code does today (verified 2026-10-16 on `main` @ 2e55347)

- `providers/deepgram/stt.go` `ProcessFrame` (:190) calls `ensureStream` (:318) from the `UserStartedSpeakingFrame` case (:194) and from `handleAudio` (:262). `ensureStream` runs `resilience.Execute(ctx, breaker, policy, classify, nil, websocket.Dial…)` (:349) on the run loop. The same shape is in cartesia (`ensureStream` :344, `OpenBounded` :378), elevenlabs (:361, `Execute` :394), nvidia (:377, `OpenBounded` :407), google (`openPrimaryStream` :602, `OpenStream` :623) and xai (`ensureStream` :515, `OpenStream` :545).
- `core/budget.go:25` sets `MaxFrameProcessing: 2 * time.Second`. `core/session.go:309-332` (`watchdog`) calls `s.fail(ErrProcessorWatchdog)` when one envelope runs longer. Retry budgets: deepgram 3 × 5 s (`DefaultSTTConfig` :44); cartesia, elevenlabs, nvidia, google and xai 2 × 5 s. `server/tts_failover.go:69 failoverPolicy` caps failover candidates at 2 × 5 s. A hanging dial therefore kills the call at 2 s instead of failing over. Even off the loop, 2 × 5 s is longer than `turn.DefaultNoTranscriptTimeout` (5 s from silence onset, `core/turn/stop.go:50,83`), after which the turn processor drops an untranscribed turn. Task 2.2 tightens it.
- Receiver exits with no frame: deepgram :407-415 (NormalClosure/GoingAway/NET-0001), cartesia :458, elevenlabs :467 and :485-488, nvidia :463-465 (`streamEnded` = `io.EOF`), google :820 (`isSTTIdleOrClosed`). `core/stt_failover.go:475-483` fails over only on an `ErrorFrame` or a `TranscriptInvalidationFrame`. `providers/xai/stt.go:640-662` (`failBoundStream` :897) is the working template: it emits `TranscriptInvalidationFrame` then `ErrorFrame{Err: %w ErrTranscriptInvalidated}`.
- The four ws/gRPC adapters are byte-similar outside `wsURL`/`initRequest`, dial options, message decode and transcript assembly. The shared parts are `stream`/`sttStream` (owner, event, endTrace), `STTProcessor{mu, active, all, closed, turn, gen}`, `Attach`, `Close`, `detachAllLocked`, the `ProcessFrame` switch, `beginTurn`, the `handleAudio` shell, `ioParent`, `ensureStream` (span, fail, race re-check, `session.Go(receive)`), the `receive` defer, `reportError`, `frameGen`, `finishActive`, `finish` (closing flag, `AfterFunc(DrainTimeout)`), `stopTurn`, `release` and `stop`.
- `server/stt_failover.go:82 buildSTT` builds each candidate through `build("stt", spec)` (the registry factories) and wraps two or more candidates in `core.NewSTTFailover`. `server/server.go:1218,1243` read `core.STTCounters` for `voice_provider_errors_total` and `STTErrorsTotal`. Deepgram does not implement it (B7).
- `core/ttsunit/ttsunit.go` is the template: `ttsunit.New(Config{…, Attempt})` returns `*Runner`, which providers embed (`cartesia/tts.go:52`, `google/tts_streaming.go:19`). The Runner implements `Attach`, `Close`, `ProcessFrame` and the counters.

### Decision 1: package `core/sttunit`, type `Runner`, embedded like `ttsunit`

```go
// Provider side (Deepgram, abridged; full file in Task 2.3)
type STTProcessor struct {
	*sttunit.Runner
	cfg STTConfig
}
p.Runner = sttunit.New(sttunit.Config{Provider: "deepgram", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify, DrainTimeout: cfg.DrainTimeout, Now: cfg.Now})
```

The exact API (defined in Task 2.1, never changed after):

```go
const DefaultMaxPreRollBytes = 60 * 16000 * 2
var ErrStreamEnded = errors.New("sttunit: stream ended by the provider")

type Conn interface {
	Send(ctx context.Context, pcm []byte) error
	Finish(ctx context.Context) error
	Recv(ctx context.Context) (Result, error)
	Close()
}
type Result struct {
	Text                  string
	Final, EndOfUtterance bool
	Stability             float64
	HasStability          bool
}
type Config struct {
	Provider        string
	Dial            func(ctx context.Context) (Conn, error)
	Breaker         *resilience.Breaker
	Classify        resilience.Classify
	DrainTimeout    time.Duration // default 2 s
	SendTimeout     time.Duration // default 500 ms; must stay well under the 2 s frame watchdog
	Now             func() time.Time
}
// The pre-roll limit is the unexported Runner field maxPreRoll (default
// DefaultMaxPreRollBytes); only in-package tests change it.
func New(cfg Config) *Runner
func (r *Runner) Attach(s core.SessionController, index int) error
func (r *Runner) Close() error
func (r *Runner) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error
func (r *Runner) STTCounts() (rot, rotFail, dedup, errs uint64) // core.STTCounters
func (r *Runner) Streams() int
```

Why these hooks and no others. In ttsunit the provider hook is `Attempt` (one try). Here a stream is long-lived and full-duplex, so the hook is a connection: `Dial` opens it, and the `Conn` it returns is the only per-provider state. Each provider needs:

| Hook | Deepgram | Cartesia | ElevenLabs | Riva (nvidia) |
|---|---|---|---|---|
| `Dial` | `wsURL` + `Authorization: Token` + `resilience.Execute` (breaker inside) | `wsURL` + `X-API-Key`/`Cartesia-Version` + `WouldRefuse` gate + `OpenBounded` + manual `Report`/`RecordSuccess` + `SetReadLimit(1<<20)` | `wsURL` + `xi-api-key` + `Execute` + `SetReadLimit(1<<20)` | `WouldRefuse` gate + `OpenBounded(openOnce)`; success recorded on first answer |
| `Send` | binary PCM | binary PCM | JSON `input_audio_chunk` (base64), marks `sent` | `rpc.Send(AudioContent)`; a send still blocked when ctx ends cancels the RPC (`SendTimeout` 1 s, down from 2 s: run-loop sends must stay under the 2 s watchdog) |
| `Finish` | text `Finalize`, `CloseStream` | text `finalize`, `close` | commit chunk; `errNothingToCommit` when no audio went out | `CloseSend`, marks `closing` |
| `Recv` decode | `Results` → committed + segment; `Error` → error wrapping the local `errInBand` (classified `NoBreaker`, so it stays out of the shared breaker as today); normal/going-away/NET-0001 → `ErrStreamEnded` | `transcript` delta / `flush_done` EOU / `done` → `ErrStreamEnded` / `error` | partial/committed with two-form dedup; committed after commit → EOU, then `ErrStreamEnded`; time-limit/insufficient-audio → `ErrStreamEnded` | multi-result interim join with lowest stability; `io.EOF` → `ErrStreamEnded`; `RecordSuccess` on first answer |

Retries, the dial's breaker accounting and the dial policy stay provider-owned. They differ today (Execute vs OpenBounded, success on open vs on first answer), and tests pin each of them. The runner reports mid-stream failures to `Config.Breaker`, which matches what every adapter's `receive` does today.

**`Streams()`** is a deliberate test seam. Provider tests use it (`activeCount`) to observe teardown without reaching into removed internals. It is documented as test-only, marked `// ponytail:` in the code, and kept by ruling (see Rulings).

### Decision 2: lifecycle and goroutine model

```
run loop (ProcessFrame)                               stream task (session.Go)
──────────────────────                               ────────────────────────
UserStarted/TurnEventStarted → beginTurn (adopt or finish old)
UserStarted / first audio → ensure: span, stream{cancel}, r.active=st ─Go─▶ run(ctx, st):
InputAudio → hold(st, pcm):                                                  conn, err := Dial(ctx)
   st.conn == nil → append to st.preRoll (bounded)                          err: ctx done → quiet
   else → conn.Send(ctx+SendTimeout) on the run loop (as today)                  else dialFailed: ErrorFrame, r.noDial=true
UserStopped/TurnCompleted → finishActive → finish:                          flush: loop { take preRoll under st.mu;
   st.closing = true; conn != nil → sendFinish                                   empty → st.conn = conn; owed Finish → sendFinish; break
   (conn == nil → the task sends it after the pre-roll)                          else Send each (no lock) }
Interruption/Cancel/TurnInterrupted/Close → stop: cancel(), conn.Close()    receive: Recv loop → emit candidates
                                                                             ErrStreamEnded → quiet, or A8 invalidate (Task 2.2)
                                                                             other error → breaker, ErrorFrame
                                                                             defer: conn.Close, release, stop(traceErr)
```

- **One task per stream.** The dial runs at the head of the stream's task, which then becomes the receiver. Unlike today, a dialing stream holds a session task for its whole dial budget. That is up to ~16 s for a Deepgram primary (3 × 5 s plus backoff) and ≈ 3 s for a failover candidate. Before, the run loop itself was blocked for that time. A turn that ends while its stream dials, followed by the next turn's dial, can hold 2–3 of the 16 `MaxAsyncTasks` at once. When `session.Go` refuses with `ErrTooManyTasks`, `ensure` treats it like a failed dial: `noDial` is set, a non-fatal `ErrorFrame` goes out, and the frame passes through. The turn goes untranscribed, but the call is not failed.
- **Writes stay on the run loop once the stream is open**, as today. That keeps synchronous send errors flowing to `STTFailover.syncFailure` unchanged. Only the dial and the pre-roll flush leave the loop. Ordering is guaranteed without a lock across I/O: the run loop sends directly only after `st.conn` is set. `st.conn` is set only when a flush pass finds the pre-roll empty, under `st.mu`. Audio arriving during a pass joins the pre-roll and is sent by the next pass. `Finish` is decided under the same lock (`closing` and `conn` read together), so exactly one side sends it, and it comes after the held audio. `Send` and `Finish` never overlap because of the invariant **closing ⇒ not active**. `finishActive` and `beginTurn` take a stream out of `r.active` before calling `finish`, so the run loop never sends live audio to a stream that is finishing.
- **Termination paths of the task:** `Dial` returns (error or conn), `ctx` cancelled by `stop` (from `Close`, interruption, `TurnEventInterrupted`, drain timeout or a send failure), or `Recv` returns an error or `ErrStreamEnded`. `stop` calls `cancel()` and `conn.Close()` once (`sync.Once`). Each `Conn.Recv` honours ctx (coder/websocket `Read(ctx)`, and the gRPC stream lives on a child of ctx).
- **No ctx in structs:** `stream.cancel` is a `CancelFunc`. The task gets its ctx through the `session.Go` closure. Run-loop sends derive from the frame's ctx.
- **Dial failure:** `ErrorFrame{Err, Turn, Gen}` for the stream's turn (never `Fatal`), counted unless it is `ErrCircuitOpen` (ElevenLabs, Cartesia and Riva excluded it before), with `r.noDial = true` while that stream was active. A failed dial of a stream that is unbound *and* no longer active sends no frame. This is `STTFailover`'s "recovered" re-evaluation abandoning an idle stream (`core/stt_failover.go:357-362`), where an `ErrorFrame` with `Turn ""` would only become a spurious RTVI `provider_error`. A pre-roll flush whose `Send` fails is handled like a failed dial (Task 2.2: invalidation when bound). `beginTurn` and `InterruptionFrame` clear `noDial`. Without failover the call continues and that turn goes untranscribed, where today the session failed. With failover, `STTFailover.fromCandidate` classifies the error and `jump`s with replay.
- **Generation fencing:** unchanged rules. A stream carries the `(turn, gen)` it was bound with. Candidates are re-entered at that gen (`frameGen` substitutes the current gen only for gen 0), so the session fence (`core/session.go:732`) drops a stale one. An interruption advances `r.gen` to `CurrentGeneration()` and stops every stream, so the next audio opens at the new generation. Same-turn rebinding at a newer gen is kept (`beginTurn`).
- **Trace parity:** span `stt.stream` with `{"provider": …}` started on the run loop with the frame ctx (parentage as today), events `stt.start` (dial done), `stt.first_final`, `stt.complete`/`stt.failure`, and `End(provider_failure)` on failures. New: one `stt.preroll_dropped` event per stream when the bound drops audio.

### Decision 3: pre-roll vs `STTFailover`'s journal (who owns what)

- `STTFailover` (`core/stt_failover.go`) owns **cross-candidate replay**: a 10 s journal (configurable up to 60 s, `server/stt_failover.go:25 maxSTTReplay`) of frames, plus the utterance mark, and it replays into the *next* candidate.
- `sttunit` owns **in-candidate ordering while one stream dials**: the same `[]byte` slices, held only until `st.conn` is set.
- **No double buffering of bytes.** Frames are immutable once posted (`core/stt_failover.go:224` relies on this). The pre-roll keeps the slice and does not copy it. With failover in front, the bytes are already retained by the journal, and the pre-roll adds only 24 bytes of slice header per 20 ms frame. Without failover, the pre-roll holds at most what arrives during the dial, which is bounded by the provider's retry budget (see the memory math below).
- **Bound = 60 s of 16 kHz PCM16 (1,920,000 B).** This deviates from the master plan's "5 s", for this reason: a failover `jump` replays up to `maxSTTReplay` (60 s) into the replacement in one burst, and the replacement's dial may still be in flight. xAI keeps the same 60 s bound (`providers/xai/stt.go:471-473`, 3000 chunks) for that reason. Beyond the bound the oldest audio is dropped and the span gets `stt.preroll_dropped` once. It is not a Prometheus counter (ruling).
- **Memory math.** The pre-roll holds slices and copies nothing. In practice it is bounded by the dial budget, not by the cap:

  | Case | Held audio |
  |---|---|
  | Deepgram primary (3 × 5 s + backoff ≈ 15–16 s) | ≈ 0.5 MB |
  | Failover candidate (2 × 1.5 s after Task 2.2, 2 × 5 s before) | ≈ 0.1 MB (≤ 0.33 MB before) |
  | Theoretical cap per dialing active stream | 1.92 MB |
  | Cap × 300 calls | ≈ 576 MB |
  | Realistic provider-wide hang (every call holding ~16 s) | ≈ 150 MB |

  `preRoll = preRoll[1:]` keeps the dropped prefix's backing array alive until the next `append` reallocates. That is bounded at 2× the slice capacity.

### Decision 4: drop recovery (A8) rule

When `Conn.Recv` returns `ErrStreamEnded` and the task was not stopped by us, the stream counts as lost when:

```
bound (st.turn != "") && (!st.closing || (st.lastText != "" && !st.settled))
```

Here `settled` means the last emitted candidate was `Final`, so all heard text is committed. It does not require `EndOfUtterance`, because a provider's closing response need not carry an EOU final. Riva may answer `CloseSend` with no further result after an earlier final, and that must not be called a lost turn. A lost stream gets `TranscriptInvalidationFrame{Turn, Gen, Err}` and then `ErrorFrame{Err: fmt.Errorf("%w: <provider> stream ended mid-utterance: %w", core.ErrTranscriptInvalidated, cause)}`. Both wraps use `%w`, so `errors.Is(err, sttunit.ErrStreamEnded)` holds. The runner counts the error and does not report it to the breaker (a clean end is not an outage). This is the xAI M3-09 rule (`providers/xai/stt.go:643`) generalized. Our own cuts (drain timeout, interruption, `Close`) cancel the ctx first and are always quiet. A drain timeout stays quiet as today, and the turn processor's `NoTranscriptTimeout` covers it.

**Behaviour change, deliberate.** A provider idle-close *while a turn is bound and not yet finished* used to be quiet and is now a lost turn. These are Deepgram NET-0001, Cartesia and ElevenLabs "idle" closes, and ElevenLabs `insufficient_audio_activity`. Audio flows continuously during a turn, so this only happens when the provider dropped us mid-utterance, which is exactly A8. Idle closes of **unbound** streams (between turns, the realistic case) stay quiet. The four existing "idle close is not an outage" tests therefore post idle audio instead of a turn (listed in each task). This change is acceptable only together with the `settled = Final` rule above.

**Known gaps (predate this plan, not fixed here).** These two cases still end quietly, or with a plain `ErrorFrame`, and never invalidate the turn:

1. A `Finish` write that fails after words were heard (the runner closes the stream quietly, as every adapter does today).
2. A mid-stream `Recv` failure that is not `ErrStreamEnded` (a plain `ErrorFrame`, as today; `STTFailover` still fails over on its class).

Two further gaps found in review (accepted, not fixed):

3. Adoption race: a provider idle-close that arrives just after `beginTurn` adopts an unbound stream invalidates the new turn (the stream was healthy when adopted and lost the turn's first words). Failover replays the turn, so the cost is one replay.
4. A real write failure that lands just before `ErrStreamEnded` is treated as quiet (`handleAudio` sees `st.ended` set, or the stream stopped), so it is neither counted nor reported to the breaker. The next audio opens a fresh stream.

5. Drain-timeout loss: if a stream has heard interim words, was asked to finish, and the drain timer fires before any final arrives, the stream is closed quietly (our own cut). The turn gets no transcript and no error; only the turn processor's `NoTranscriptTimeout` covers it.
6. Riva live-Send stall without failover: a Send that outlives `SendTimeout` returns a retryable timeout error from `ProcessFrame` (a failover needs that synchronous error to jump). Without an STT failover the error fails the session, as before this phase. The concurrent `Recv` reports the same stall as a timeout too (not a client-class `Canceled`), so which side is heard first no longer changes the failover class or the breaker charge.

Routing items 1 and 2 through `invalidate` would force failover class `server_error` even for client errors (`core/stt_failover.go:478-483`), changing the class-based failover policy. That needs its own decision.

### Decision 5: Google and xAI do not migrate; they get local fixes (Task 2.7)

Master plan rule: "only move them if the runner expresses their needs without new hooks".

- **Google (`providers/google/stt.go`, also the `echo` provider).** It rotates a stream mid-utterance at 240 s with 500 ms overlap (`rotateStream` :667). Its assembly is offset-based across streams (`assembleResultLocked` :954, `googleAssembly` per turn and gen). It reports `RotationsCount`/`DeduplicatedResults` through `STTCounts`, and it re-admits detached finals at the current gen (:889-893). Expressing rotation needs a new hook (the runner has one stream per utterance), so Google stays. Local fixes: (a) the open moves to a session task with a bounded `pending` hold flushed in order and a `closeOwed` half-close, plus a one-open-per-turn gate after a failure. (b) A8: on `isSTTIdleOrClosed` (`io.EOF`, idle timeout) the turn is invalidated using the same rule as Decision 4, with `retired` excluding streams replaced by rotation. Rotation's own open stays synchronous. It fires only for a single utterance longer than 240 s, because streams are per utterance. That residual risk is recorded in Task 2.7. Evidence that the change is safe: the `echo` provider (`server/registry_echo.go:17-21`) is `google.NewSTTProcessor`, so `make e2e-echo` and `make loadtest-smoke` exercise it.
- **xAI (`providers/xai/stt.go`, build tag `xaispeech`).** It has a per-stream handshake (`transcript.created` gates audio, :469-479, :676-725), detached `speech_final` admitted at the current generation (:779-786), a bounded draining set of 8 (:990) and the M3-02/M3-09 ordering rules pinned by `stt_ordering_test.go`. The runner would need a handshake hook and an admission-gen rule. xAI already has a 60 s pre-ready buffer and correct A8, so it stays. Local fixes: (a) the open moves to a session task, reusing the existing `audioBuf`/`pendingDone` machinery, with the same one-open-per-turn gate. (b) A15: `sendMu` is deleted. The `transcript.created` flush becomes the same "drain until empty, then mark ready under `st.mu`" loop. coder/websocket writes are concurrency-safe, and after the flush no two writers exist. `make offline-test` drives this processor (`server/registry_offline_speech.go:27`).

### Test strategy

- **Runner unit tests** (`core/sttunit/runner_test.go`, package `sttunit`) use a fake `Conn` (records `send:<first byte>`/`finish`, and `Recv` is fed from a channel), a real `core.Session` and `coretest.PassthroughProcessor`. Each test's cleanup asserts `LeakedTasks() == 0`. Run with `-race -count=1`, and in 2.8 with `-count=20`.
- **Failover integration** (`core/stt_failover_test.go`, package `core_test`): `sttunit.Runner` as the primary with a 3 s dial that fails, and `coretest` STT as the fallback, on the existing `sttRig`.
- **Per-adapter regression:** each existing `stt_test.go` keeps passing. Two shims in the test file (`ensureStream` → `p.dial` then close, `activeCount` → `p.Streams()`) keep the dial, breaker and teardown tests unchanged. Tests that read removed internals (`p.mu`, `p.active`, `committed`, `owner()`, `beginTurn`, `finishActive`, `Errors`) are rewritten to observe the wire and the frames. Each task lists them. Each adapter gains `TestSTT_CleanCloseMidUtteranceInvalidatesTurn`, and Deepgram also gains `TestSTT_CountsErrorsForMetrics` (B7).
- **Load:** `make loadtest-smoke` before Task 2.1 (baseline) and in Task 2.8. The echo path is Google STT, so this measures the Google local fix and the runtime overhead. Accept when the after `stt_final` p95 is at most the before p95 × 1.10 + 20 ms (noise band) and `voice_to_voice` p95 stays under the target's own `-max-p95-v2v 1500`.

---

### Task 2.1: `core/sttunit` runner with off-loop dial and bounded pre-roll (A1)

**Files:**
- Create: `core/sttunit/runner.go`, `core/sttunit/runner_test.go`
- Modify: none (`core/session.go` needs no helper)

**Interfaces:**
- Consumes: `core.SessionController` (`Go`, `Reenter`, `Context`, `ID`, `CurrentGeneration`), `core.StartContextSpan`, `core.NewTraceEvent`, `core.TraceContextFromContext`, `resilience.Breaker.Report`, `resilience.Classify`, `resilience.ErrCircuitOpen`, frame types `UserStartedSpeakingFrame`, `UserStoppedSpeakingFrame`, `TurnEventFrame`, `InputAudioFrame`, `InterruptionFrame`, `CancelFrame`, `TranscriptCandidateFrame`, `ErrorFrame`.
- Produces: `sttunit.{New, Config, Runner, Conn, Result, ErrStreamEnded, DefaultMaxPreRollBytes}`, `(*Runner).{Attach, Close, ProcessFrame, STTCounts, Streams}`.

- [ ] **Step 0: Record the load baseline (before any code).** On a clean `feat/phase-2-stt-runner` at its branch point:

```bash
mkdir -p "${TMPDIR:-/tmp}/phase2" && make loadtest-smoke 2>&1 | tee "${TMPDIR:-/tmp}/phase2/loadtest-before.txt"
```

Expected: the run passes, or prints `loadtest-smoke: SKIPPED: third_party/silero_vad.onnx missing`. In the skip case, note it in Task 2.8's report. Do not commit the file.

- [ ] **Step 1: Write the failing tests.** Create `core/sttunit/runner_test.go`:

```go
package sttunit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
)

// fakeConn is a scripted provider stream: it records sends and finishes in
// order, and Recv returns what the test feeds into results. Send and Finish
// must never overlap (the Conn contract): any overlap fails the test.
type fakeConn struct {
	t        *testing.T
	inFlight atomic.Int32 // Send/Finish calls running now
	// gate, when set, holds the first Send until it is closed (or its ctx
	// ends); started is closed when that Send begins.
	gate    chan struct{}
	started chan struct{}
	gated   atomic.Bool

	mu      sync.Mutex
	ops     []string // "send:<first byte>" or "finish"
	closed  bool
	sendErr error
	recvs   atomic.Int32 // Recv calls so far (a call after a result means it was processed)
	results chan recvResult
}

type recvResult struct {
	res Result
	err error
}

func newFakeConn(t *testing.T) *fakeConn {
	return &fakeConn{t: t, results: make(chan recvResult, 16)}
}

// newGatedConn returns a fakeConn whose first Send blocks until gate closes.
func newGatedConn(t *testing.T) (c *fakeConn, gate chan struct{}) {
	c = newFakeConn(t)
	c.gate, c.started = make(chan struct{}), make(chan struct{})
	return c, c.gate
}

func (c *fakeConn) enter(op string) {
	if c.inFlight.Add(1) != 1 {
		c.t.Errorf("%s overlapped another Send/Finish", op)
	}
}

func (c *fakeConn) Send(ctx context.Context, pcm []byte) error {
	c.enter("Send")
	defer c.inFlight.Add(-1)
	if c.gate != nil && c.gated.CompareAndSwap(false, true) {
		close(c.started)
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.ops = append(c.ops, "send:"+strconv.Itoa(int(pcm[0])))
	return nil
}

func (c *fakeConn) Finish(context.Context) error {
	c.enter("Finish")
	defer c.inFlight.Add(-1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, "finish")
	return nil
}

func (c *fakeConn) Recv(ctx context.Context) (Result, error) {
	c.recvs.Add(1)
	select {
	case r := <-c.results:
		return r.res, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (c *fakeConn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *fakeConn) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ops)
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeConn) setSendErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendErr = err
}

// gatedDial blocks until release is closed, then returns conn.
func gatedDial(release <-chan struct{}, conn Conn) func(context.Context) (Conn, error) {
	return func(ctx context.Context) (Conn, error) {
		select {
		case <-release:
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func dialConn(conn Conn) func(context.Context) (Conn, error) {
	return func(context.Context) (Conn, error) { return conn, nil }
}

type harness struct {
	t     *testing.T
	r     *Runner
	sess  *core.Session
	sink  *coretest.PassthroughProcessor
	dials atomic.Int32
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	return newHarnessWithBudget(t, cfg, core.DefaultResourceBudget())
}

func newHarnessWithBudget(t *testing.T, cfg Config, budget core.ResourceBudget) *harness {
	t.Helper()
	h := &harness{t: t, sink: &coretest.PassthroughProcessor{}}
	dial := cfg.Dial
	cfg.Dial = func(ctx context.Context) (Conn, error) {
		h.dials.Add(1)
		return dial(ctx)
	}
	if cfg.Provider == "" {
		cfg.Provider = "fake"
	}
	h.r = New(cfg)
	pipe, err := core.NewPipeline(h.r, h.sink)
	if err != nil {
		t.Fatal(err)
	}
	h.sess, err = core.NewSession(7, pipe, budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = h.sess.Close()
		if n := h.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d session tasks", n)
		}
	})
	return h
}

func (h *harness) post(f core.Frame) {
	h.t.Helper()
	if err := h.sess.Post(f); err != nil {
		h.t.Fatal(err)
	}
}

// audio posts one 2-byte frame whose first byte names it.
func (h *harness) audio(b byte) {
	h.t.Helper()
	h.post(&core.InputAudioFrame{Samples: []byte{b, 0}, SampleRate: 16000, Channels: 1})
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func framesOf[T core.Frame](h *harness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func (h *harness) candidates() []*core.TranscriptCandidateFrame {
	return framesOf[*core.TranscriptCandidateFrame](h)
}
func (h *harness) errorFrames() []*core.ErrorFrame { return framesOf[*core.ErrorFrame](h) }
func (h *harness) audioPassed() int                { return len(framesOf[*core.InputAudioFrame](h)) }

type discard struct{}

func (discard) Push(core.Frame) error                          { return nil }
func (discard) PushDirection(core.Frame, core.Direction) error { return nil }

// A1: ProcessFrame never waits for the dial, so a dial longer than the 2 s
// frame watchdog does not end the call, and the audio sent meanwhile reaches
// the stream once it opens.
func TestRunner_DialDoesNotBlockProcessFrame(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: func(ctx context.Context) (Conn, error) {
		select {
		case <-time.After(3 * time.Second):
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	// Through the run loop, so the real 2 s frame watchdog is armed.
	start := time.Now()
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.waitFor("frames through the runner", func() bool { return h.audioPassed() == 2 })
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("frames took %v to pass: ProcessFrame waited for the dial", d)
	}
	time.Sleep(2500 * time.Millisecond) // past the watchdog, the dial still in flight
	if err := h.sess.Err(); err != nil {
		t.Fatalf("session failed while the dial hung: %v", err)
	}
	h.waitFor("pre-roll flushed", func() bool { return len(conn.snapshot()) == 2 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2"}) {
		t.Fatalf("sent %v", got)
	}
}

func TestRunner_PreRollFlushedInOrder(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for b := byte(1); b <= 5; b++ {
		h.audio(b)
	}
	h.waitFor("audio passed downstream while dialing", func() bool { return h.audioPassed() == 5 })
	close(release)
	h.audio(6)
	h.waitFor("all audio sent", func() bool { return len(conn.snapshot()) == 6 })
	want := []string{"send:1", "send:2", "send:3", "send:4", "send:5", "send:6"}
	if got := conn.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

func TestRunner_PreRollBounded(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.r.maxPreRoll = 3 * 2 // three 2-byte frames; set before any frame is posted
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for b := byte(1); b <= 5; b++ {
		h.audio(b)
	}
	h.waitFor("audio passed downstream while dialing", func() bool { return h.audioPassed() == 5 })
	close(release)
	h.waitFor("held audio sent", func() bool { return len(conn.snapshot()) == 3 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:3", "send:4", "send:5"}) {
		t.Fatalf("sent %v: the oldest audio must be dropped first", got)
	}
}

func TestRunner_FinishWhileDialingGoesAfterPreRoll(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("stop passed downstream", func() bool { return len(framesOf[*core.UserStoppedSpeakingFrame](h)) == 1 })
	close(release)
	h.waitFor("finish", func() bool { return len(conn.snapshot()) == 3 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2", "finish"}) {
		t.Fatalf("ops %v: Finish must follow the held audio", got)
	}
}

func TestRunner_DialFailureReportsErrorFrame(t *testing.T) {
	boom := errors.New("dial: connection refused")
	h := newHarness(t, Config{Dial: func(context.Context) (Conn, error) { return nil, boom }})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	ef := h.errorFrames()[0]
	if !errors.Is(ef.Err, boom) || ef.Fatal || ef.Turn != "turn-1-1" || ef.Gen != 1 {
		t.Fatalf("error frame %+v", ef)
	}
	if _, _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d, want 1", n)
	}
	// No redial for the rest of the turn: the audio passes through untranscribed.
	h.audio(2)
	h.audio(3)
	h.waitFor("audio passed", func() bool { return h.audioPassed() == 3 })
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dials = %d, want one per turn", n)
	}
	// The next turn dials again.
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	h.waitFor("second dial", func() bool { return h.dials.Load() == 2 })
	if err := h.sess.Err(); err != nil {
		t.Fatalf("a failed dial ended the call: %v", err)
	}
}

func TestRunner_CircuitOpenDialIsReportedNotCounted(t *testing.T) {
	h := newHarness(t, Config{Dial: func(context.Context) (Conn, error) {
		return nil, fmt.Errorf("fake: %w", resilience.ErrCircuitOpen)
	}})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	if !errors.Is(h.errorFrames()[0].Err, resilience.ErrCircuitOpen) {
		t.Fatalf("err = %v", h.errorFrames()[0].Err)
	}
	if _, _, _, n := h.r.STTCounts(); n != 0 {
		t.Fatalf("an open breaker counted as a provider error: %d", n)
	}
}

func TestRunner_CloseCancelsDialAndLeaksNothing(t *testing.T) {
	dialing := make(chan struct{})
	h := newHarness(t, Config{Dial: func(ctx context.Context) (Conn, error) {
		close(dialing)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	<-dialing
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	if n := h.sess.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
	if n := h.r.Streams(); n != 0 {
		t.Fatalf("streams after Close = %d", n)
	}
	if len(h.errorFrames()) != 0 {
		t.Fatal("a cancelled dial was reported as a failure")
	}
}

func TestRunner_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.audio(1) // idle audio opens an unbound stream
	conn.results <- recvResult{res: Result{Text: "hi", Final: true}}
	h.waitFor("result read", func() bool { return conn.recvs.Load() >= 2 })
	if len(h.candidates()) != 0 {
		t.Fatal("an unbound stream emitted a candidate")
	}
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(2)
	conn.results <- recvResult{res: Result{Text: "hi there"}}
	h.waitFor("adopted candidate", func() bool { return len(h.candidates()) == 1 })
	c := h.candidates()[0]
	if c.Text != "hi there" || c.Turn != "turn-1-1" || c.Gen != 1 || c.Provider != "fake" || h.dials.Load() != 1 {
		t.Fatalf("candidate %+v dials=%d", c, h.dials.Load())
	}
}

func TestRunner_LateResultOfOldGenerationIsFenced(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
	h.sess.IncrementGeneration() // a barge-in moved on; its InterruptionFrame is not here yet
	conn.results <- recvResult{res: Result{Text: "stale", Final: true, EndOfUtterance: true}}
	h.waitFor("result read", func() bool { return conn.recvs.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if c := h.candidates(); len(c) != 0 {
		t.Fatalf("a generation-1 final reached the transcript stage: %+v", c)
	}
}

func TestRunner_SendFailureFailsTheFrameAndReleases(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
	broken := errors.New("broken pipe")
	conn.setSendErr(broken)
	err := h.r.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: []byte{1, 0}}, core.Downstream, discard{})
	if !errors.Is(err, broken) {
		t.Fatalf("ProcessFrame err = %v, want the send error (STTFailover.syncFailure needs it)", err)
	}
	if _, _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d", n)
	}
	h.waitFor("released and closed", func() bool { return h.r.Streams() == 0 && conn.isClosed() })
}

// Live audio arriving while a flush pass is sending joins the pre-roll and
// goes in the next pass: order 1-4, never a live Send overlapping the flush.
func TestRunner_LiveAudioDuringFlushKeepsOrder(t *testing.T) {
	release := make(chan struct{})
	conn, gate := newGatedConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn), SendTimeout: 5 * time.Second})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.waitFor("held", func() bool { return h.audioPassed() == 2 })
	close(release)
	<-conn.started // the flush is inside Send(1)
	h.audio(3)
	h.audio(4)
	h.waitFor("live audio passed", func() bool { return h.audioPassed() == 4 })
	close(gate)
	h.waitFor("all sent", func() bool { return len(conn.snapshot()) == 4 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2", "send:3", "send:4"}) {
		t.Fatalf("sent %v", got)
	}
}

func TestRunner_FinishDuringFlushGoesLast(t *testing.T) {
	release := make(chan struct{})
	conn, gate := newGatedConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn), SendTimeout: 5 * time.Second})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.waitFor("held", func() bool { return h.audioPassed() == 2 })
	close(release)
	<-conn.started
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("stop passed", func() bool { return len(framesOf[*core.UserStoppedSpeakingFrame](h)) == 1 })
	close(gate)
	h.waitFor("finish", func() bool { return len(conn.snapshot()) == 3 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2", "finish"}) {
		t.Fatalf("ops %v", got)
	}
}

func TestRunner_CloseDuringFlushIsQuiet(t *testing.T) {
	release := make(chan struct{})
	conn, _ := newGatedConn(t) // the gate never opens: Close must unblock the Send
	h := newHarness(t, Config{Dial: gatedDial(release, conn), SendTimeout: 5 * time.Second})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("held", func() bool { return h.audioPassed() == 1 })
	close(release)
	<-conn.started
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	if n := h.sess.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
	if len(h.errorFrames()) != 0 {
		t.Fatal("Close during a flush was reported as a failure")
	}
}

// A flush Send that fails is a failed open: one error, no redial this turn.
// (Task 2.2 adds the bound-turn case, which invalidates.)
func TestRunner_FlushSendFailure(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	broken := errors.New("broken pipe")
	conn.setSendErr(broken)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.audio(1) // idle audio: an unbound stream, still active
	h.waitFor("held", func() bool { return h.audioPassed() == 1 })
	close(release)
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	if !errors.Is(h.errorFrames()[0].Err, broken) {
		t.Fatalf("err = %v", h.errorFrames()[0].Err)
	}
	h.audio(2)
	h.waitFor("audio passed", func() bool { return h.audioPassed() == 2 })
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dials = %d: a failed flush must not redial before the next turn", n)
	}
}

// ErrTooManyTasks loses the turn's transcript, not the call: a non-fatal
// error, the frame passes, the session lives.
func TestRunner_TaskLimitLosesTheTurnNotTheCall(t *testing.T) {
	budget := core.DefaultResourceBudget()
	budget.MaxAsyncTasks = 1
	release := make(chan struct{})
	defer close(release)
	h := newHarnessWithBudget(t, Config{Dial: gatedDial(release, newFakeConn(t))}, budget)
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1}) // its dial holds the only task
	h.post(&core.UserStoppedSpeakingFrame{})               // the dialing stream now finishes
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2}) // no task left for this turn's stream
	h.audio(1)
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	ef := h.errorFrames()[0]
	if !errors.Is(ef.Err, core.ErrTooManyTasks) || ef.Fatal || ef.Turn != "turn-1-2" {
		t.Fatalf("error frame %+v", ef)
	}
	h.waitFor("frames passed", func() bool {
		return len(framesOf[*core.UserStartedSpeakingFrame](h)) == 2 && h.audioPassed() == 1
	})
	if err := h.sess.Err(); err != nil {
		t.Fatalf("the task limit ended the call: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail.**

```bash
cd core && go test -race -count=1 ./sttunit/...
```

Expected: FAIL. The build fails with `undefined: Result`, `undefined: Conn`, `undefined: Runner`, `undefined: New` and `undefined: Config`.

- [ ] **Step 3: Implement.** Create `core/sttunit/runner.go`:

```go
// Package sttunit is the stream machinery shared by the streaming STT
// providers (Deepgram, Cartesia, ElevenLabs, Riva): one provider stream per
// utterance, opened off the session run loop with the audio that arrives
// meanwhile held, bounded, in a pre-roll; turn binding (an unbound stream is
// adopted by the next turn); the receive loop; finish and drain; the trace
// span and the error counter. A provider supplies only Dial and the Conn it
// returns (wire format and transcript assembly).
package sttunit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// DefaultMaxPreRollBytes bounds the audio a stream holds while it dials: 60 s
// of 16 kHz PCM16 mono, the server's largest STT failover replay (server
// maxSTTReplay), which lands in one burst while a replacement dials. Live
// audio fills it only for as long as the dial takes.
const DefaultMaxPreRollBytes = 60 * 16000 * 2

// ErrStreamEnded is what Conn.Recv returns when the provider ended the stream
// without a failure: a clean close, its idle timeout, or the end of results
// after Finish.
var ErrStreamEnded = errors.New("sttunit: stream ended by the provider")

var (
	errProviderFailure = errors.New("provider_failure")
	errCanceled        = errors.New("canceled")
)

// Conn is one open provider stream. The Runner calls Send and Finish from one
// goroutine at a time (never concurrently with each other), Recv from the
// stream's own task, and Close from any goroutine, possibly more than once.
type Conn interface {
	// Send writes one chunk of caller audio (16-bit mono PCM).
	Send(ctx context.Context, pcm []byte) error
	// Finish asks for the final results of the audio sent. An error closes
	// the stream without reporting it.
	Finish(ctx context.Context) error
	// Recv blocks for the next provider event and returns the stream's
	// cumulative transcript after it. It returns ErrStreamEnded when the
	// provider ended the stream cleanly; any other error is a failure.
	Recv(ctx context.Context) (Result, error)
	Close()
}

// Result is a stream's cumulative transcript after one provider event. An
// empty Text emits nothing; the Conn keeps its own state, so a turn that
// adopts an unbound stream gets the text heard before it.
type Result struct {
	Text                  string
	Final, EndOfUtterance bool
	Stability             float64
	HasStability          bool
}

// Config wires a Runner to a provider.
type Config struct {
	Provider string // label on candidates and the trace span
	// Dial opens one stream on ctx (the stream lives on it). Retries and the
	// breaker's accounting of the open stay with the provider.
	Dial func(ctx context.Context) (Conn, error)
	// Breaker, with Classify, gets one report per mid-stream failure. May be nil.
	Breaker  *resilience.Breaker
	Classify resilience.Classify
	// DrainTimeout bounds the wait for the final results after Finish (default 2 s).
	DrainTimeout time.Duration
	// SendTimeout bounds one Send or Finish (default 500 ms). Live sends run on
	// the session run loop, so it must stay well under the 2 s frame watchdog.
	SendTimeout time.Duration
	// Now stamps candidates (default time.Now).
	Now func() time.Time
}

type stream struct {
	cancel context.CancelFunc // ends the dial, the reads and the drain

	mu       sync.Mutex
	conn     Conn     // nil while dialing and flushing the pre-roll
	preRoll  [][]byte // audio held while dialing, oldest first
	preBytes int
	turn     string // "" until a turn adopts the stream
	gen      uint64
	closing  bool // Finish is owed (still dialing) or sent
	stopped  bool
	draining *time.Timer

	stopOnce, traceFinal, traceEnd, overflow sync.Once
	span                                     core.Span
	policy                                   core.TracePolicy
	traceSession                             core.SessionID
}

func (st *stream) owner() (string, uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.turn, st.gen
}

func (st *stream) event(name string) {
	if st.span != nil {
		turn, gen := st.owner()
		st.span.AddEvent(core.NewTraceEvent(name, time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.policy, nil))
	}
}

func (st *stream) endTrace(err error) {
	st.traceEnd.Do(func() {
		if st.span == nil {
			return
		}
		if err != nil {
			st.event("stt.failure")
		} else {
			st.event("stt.complete")
		}
		st.span.End(err)
	})
}

// Runner is the STT processor core: embed it in a provider's STTProcessor.
type Runner struct {
	cfg     Config
	session core.SessionController
	index   int
	// maxPreRoll bounds the audio held while a stream dials; beyond it the
	// oldest audio is dropped. DefaultMaxPreRollBytes; only tests change it.
	maxPreRoll int

	mu     sync.Mutex
	active *stream
	// all is the active stream plus finishing ones. Each holds one session
	// task, so the session's MaxAsyncTasks bounds it.
	all    map[*stream]struct{}
	closed bool
	turn   string
	gen    uint64
	noDial bool // a dial failed: no redial until the next turn or a barge-in

	errs atomic.Uint64
}

var _ core.STTCounters = (*Runner)(nil)

// New returns a Runner for cfg; cfg.Dial is required.
func New(cfg Config) *Runner {
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 2 * time.Second
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = 500 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{cfg: cfg, maxPreRoll: DefaultMaxPreRollBytes, all: map[*stream]struct{}{}}
}

// STTCounts implements core.STTCounters (no rotation or dedup here).
func (r *Runner) STTCounts() (rot, rotFail, dedup, errs uint64) {
	return 0, 0, 0, r.errs.Load()
}

// Streams is the number of open provider streams (dialing, active or
// finishing). Test-only accessor: provider tests use it to observe teardown.
// ponytail: no server reader; a gauge can read it if operators ever need one.
func (r *Runner) Streams() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.all)
}

func (r *Runner) Attach(s core.SessionController, index int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session, r.index = s, index
	return nil
}

// Close tears down every stream, dials included; it is idempotent.
func (r *Runner) Close() error {
	r.mu.Lock()
	r.closed = true
	streams := r.detachAllLocked()
	r.mu.Unlock()
	for _, st := range streams {
		r.stop(st, nil)
	}
	return nil
}

func (r *Runner) detachAllLocked() []*stream {
	out := make([]*stream, 0, len(r.all))
	for st := range r.all {
		out = append(out, st)
	}
	r.all, r.active = map[*stream]struct{}{}, nil
	return out
}

func (r *Runner) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.UserStartedSpeakingFrame:
		r.beginTurn(ctx, fmt.Sprintf("turn-%d-%d", fr.Gen, fr.Seq), fr.Gen)
		r.ensure(ctx)
		return e.Push(f)
	case *core.TurnEventFrame:
		switch fr.Kind {
		case core.TurnEventStarted:
			r.beginTurn(ctx, string(fr.Turn), uint64(fr.Gen))
		case core.TurnEventCompleted:
			r.finishActive(ctx, string(fr.Turn), uint64(fr.Gen))
		case core.TurnEventInterrupted:
			r.stopTurn(string(fr.Turn), uint64(fr.Gen))
		}
		return e.PushDirection(f, dir)
	case *core.InputAudioFrame:
		return r.handleAudio(ctx, fr, e)
	case *core.UserStoppedSpeakingFrame:
		r.finishActive(ctx, "", 0)
		return e.PushDirection(f, dir)
	case *core.InterruptionFrame, *core.CancelFrame:
		r.mu.Lock()
		if _, ok := f.(*core.InterruptionFrame); ok && r.session != nil && r.turn != "" {
			// Barge-in advanced the generation: the rest of this utterance opens
			// a new stream that must carry it, or the session fence drops it.
			r.gen = max(r.gen, r.session.CurrentGeneration())
		}
		r.noDial = false
		streams := r.detachAllLocked()
		r.mu.Unlock()
		for _, st := range streams {
			r.stop(st, nil)
		}
		return e.PushDirection(f, dir)
	default:
		return e.PushDirection(f, dir)
	}
}

// beginTurn makes (turn, gen) current. An unbound stream (opened between
// turns) is adopted: it already carries the speech onset VAD reported late.
// The same turn at a newer generation (a turn manager's barge-in) keeps its
// stream, rebound. A stream owned by another turn is finished and drains alone.
func (r *Runner) beginTurn(ctx context.Context, turn string, gen uint64) {
	r.mu.Lock()
	r.turn, r.gen, r.noDial = turn, gen, false
	var old *stream
	if a := r.active; a != nil {
		a.mu.Lock()
		if a.turn == "" || (a.turn == turn && a.gen < gen) {
			a.turn, a.gen = turn, gen
		} else if a.turn != turn || a.gen != gen {
			old, r.active = a, nil
		}
		a.mu.Unlock()
	}
	r.mu.Unlock()
	if old != nil {
		r.finish(ctx, old)
	}
}

// ensure returns the stream audio goes to, starting one when there is none.
// The dial runs on the stream's session task, never here (A1). It returns nil
// when closed, after this turn's dial failed, or when the session has no task
// left (ErrTooManyTasks: reported like a failed dial, never returned, since a
// ProcessFrame error would fail the whole call).
func (r *Runner) ensure(ctx context.Context) *stream {
	r.mu.Lock()
	if r.active != nil || r.closed || r.noDial || r.session == nil {
		st := r.active
		r.mu.Unlock()
		return st
	}
	s, turn, gen := r.session, r.turn, r.gen
	r.mu.Unlock()

	traceSession := core.SessionID(strconv.FormatUint(s.ID(), 10))
	sctx, cancel := context.WithCancel(s.Context())
	st := &stream{
		cancel: cancel, turn: turn, gen: gen, traceSession: traceSession,
		span:   core.StartContextSpan(ctx, "stt.stream", traceSession, core.TurnID(turn), core.Generation(gen), map[string]any{"provider": r.cfg.Provider}),
		policy: core.TraceContextFromContext(ctx).Policy,
	}
	r.mu.Lock()
	if r.closed || r.active != nil { // Close ran meanwhile, or another caller won
		cur := r.active
		r.mu.Unlock()
		cancel()
		st.endTrace(errCanceled)
		return cur
	}
	r.active = st
	r.all[st] = struct{}{}
	r.mu.Unlock()
	if err := s.Go(func(context.Context) { r.run(sctx, st) }); err != nil {
		if errors.Is(err, core.ErrSessionClosed) {
			r.release(st)
			r.stop(st, errCanceled)
			return nil
		}
		r.dialFailed(st, err) // ErrTooManyTasks: lose this turn's transcript, not the call
		r.release(st)
		r.stop(st, errProviderFailure)
		return nil
	}
	return st
}

func (r *Runner) handleAudio(ctx context.Context, f *core.InputAudioFrame, e core.Emitter) error {
	st := r.ensure(ctx)
	if st == nil { // closed, no task, or this turn's dial failed (already reported)
		return e.Push(f)
	}
	conn := r.hold(st, f.Samples)
	if conn == nil { // held in the pre-roll until the dial completes
		return e.Push(f)
	}
	sctx, cancel := context.WithTimeout(ctx, r.cfg.SendTimeout)
	defer cancel()
	if err := conn.Send(sctx, f.Samples); err != nil {
		if r.isStopped(st) { // torn down concurrently: not a provider failure
			return e.Push(f)
		}
		r.errs.Add(1)
		r.release(st)
		r.stop(st, errProviderFailure)
		r.report(err)
		return err
	}
	return e.Push(f)
}

// hold returns st's conn, or keeps pcm in the pre-roll while st dials and
// returns nil. Past maxPreRoll the oldest audio is dropped; the span
// says so once.
func (r *Runner) hold(st *stream, pcm []byte) Conn {
	st.mu.Lock()
	if st.conn != nil {
		conn := st.conn
		st.mu.Unlock()
		return conn
	}
	// ponytail: frames are immutable once posted (core/stt_failover.go journals them the same way), so the pre-roll keeps the slice, not a copy.
	st.preRoll = append(st.preRoll, pcm)
	st.preBytes += len(pcm)
	dropped := false
	// ponytail: preRoll[1:] keeps the dropped prefix's array alive until the next append grows it (at most 2x capacity).
	for st.preBytes > r.maxPreRoll && len(st.preRoll) > 1 {
		st.preBytes -= len(st.preRoll[0])
		st.preRoll[0] = nil
		st.preRoll = st.preRoll[1:]
		dropped = true
	}
	st.mu.Unlock()
	if dropped {
		st.overflow.Do(func() { st.event("stt.preroll_dropped") })
	}
	return nil
}

// run is a stream's session task: dial, flush the pre-roll in order, then read
// results until the stream ends or is stopped.
func (r *Runner) run(ctx context.Context, st *stream) {
	var traceErr error
	defer func() {
		r.release(st)
		r.stop(st, traceErr)
	}()
	conn, err := r.cfg.Dial(ctx)
	if err != nil {
		if ctx.Err() == nil { // not our own teardown
			traceErr = errProviderFailure
			r.dialFailed(st, err)
		}
		return
	}
	defer conn.Close()
	st.event("stt.start")
	if err := r.flush(ctx, st, conn); err != nil {
		if ctx.Err() == nil && !r.isStopped(st) {
			// The stream died taking its held audio: a failed open, with the breaker told.
			traceErr = errProviderFailure
			r.report(err)
			r.dialFailed(st, err)
		}
		return
	}
	traceErr = r.receive(ctx, st, conn)
}

// dialFailed reports a failed open: an ErrorFrame for the stream's turn (what
// an STT failover moves on), counted unless the breaker refused the dial.
// Nothing redials until the next turn or a barge-in, so an outage costs one
// error per turn, not one per audio frame. An unbound stream that is no longer
// active (STTFailover's "recovered" re-evaluation abandoned it) owes nobody a
// transcript: no frame, which would only reach the client as a spurious
// RTVI provider_error with no turn.
func (r *Runner) dialFailed(st *stream, err error) {
	r.mu.Lock()
	active := r.active == st
	if active {
		r.noDial = true
	}
	r.mu.Unlock()
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		r.errs.Add(1)
	}
	if turn, _ := st.owner(); turn == "" && !active {
		return
	}
	r.reportError(st, err)
}

// flush sends the pre-roll in order, then attaches conn so the run loop sends
// live audio straight to it. Audio arriving during a pass joins the pre-roll
// and goes in the next one, so no lock is held across a Send. A Finish owed by
// a turn that ended while dialing goes after the held audio.
func (r *Runner) flush(ctx context.Context, st *stream, conn Conn) error {
	for {
		st.mu.Lock()
		if st.stopped {
			st.mu.Unlock()
			return errCanceled
		}
		batch := st.preRoll
		st.preRoll, st.preBytes = nil, 0
		if len(batch) == 0 {
			st.conn = conn
			owed := st.closing
			st.mu.Unlock()
			if owed {
				r.sendFinish(ctx, st, conn)
			}
			return nil
		}
		st.mu.Unlock()
		for _, pcm := range batch {
			sctx, cancel := context.WithTimeout(ctx, r.cfg.SendTimeout)
			err := conn.Send(sctx, pcm)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

// finish asks for st's final results. While st dials, the Finish is owed and
// its task sends it after the pre-roll. Safe to call more than once.
//
// Invariant closing => !active: finishActive and beginTurn take st out of
// r.active before calling finish, so the run loop never sends live audio to a
// finishing stream and Send and Finish never overlap on one Conn.
func (r *Runner) finish(ctx context.Context, st *stream) {
	st.mu.Lock()
	if st.closing {
		st.mu.Unlock()
		return
	}
	st.closing = true
	conn := st.conn
	st.mu.Unlock()
	if conn != nil {
		r.sendFinish(ctx, st, conn)
	}
}

// sendFinish sends Finish and force-closes st after DrainTimeout.
func (r *Runner) sendFinish(ctx context.Context, st *stream, conn Conn) {
	fctx, cancel := context.WithTimeout(ctx, r.cfg.SendTimeout)
	defer cancel()
	if err := conn.Finish(fctx); err != nil {
		r.release(st)
		r.stop(st, nil)
		return
	}
	st.mu.Lock()
	if !st.stopped {
		st.draining = time.AfterFunc(r.cfg.DrainTimeout, func() { r.release(st); r.stop(st, nil) })
	}
	st.mu.Unlock()
}

// finishActive finishes the active stream (turn "" = whichever is active, i.e.
// the user stopped speaking). The turn is released so audio after it opens an
// unbound stream for the next utterance.
func (r *Runner) finishActive(ctx context.Context, turn string, gen uint64) {
	r.mu.Lock()
	st := r.active
	if st == nil {
		r.mu.Unlock()
		return
	}
	t, g := st.owner()
	if turn != "" && (t != turn || g != gen) {
		r.mu.Unlock()
		return
	}
	r.active = nil // late audio opens a new stream rather than a finishing one
	if r.turn == t && r.gen == g {
		r.turn, r.gen = "", 0
	}
	r.mu.Unlock()
	r.finish(ctx, st)
}

func (r *Runner) stopTurn(turn string, gen uint64) {
	r.mu.Lock()
	var victims []*stream
	for st := range r.all {
		if t, g := st.owner(); t == turn && g == gen {
			victims = append(victims, st)
			delete(r.all, st)
			if r.active == st {
				r.active = nil
			}
		}
	}
	r.mu.Unlock()
	for _, st := range victims {
		r.stop(st, nil)
	}
}

func (r *Runner) release(st *stream) {
	r.mu.Lock()
	delete(r.all, st)
	if r.active == st {
		r.active = nil
	}
	r.mu.Unlock()
}

// stop cancels st (its dial, reads and drain) and closes its conn exactly
// once, then ends the trace with err.
func (r *Runner) stop(st *stream, err error) {
	st.mu.Lock()
	st.stopped = true
	if st.draining != nil {
		st.draining.Stop()
	}
	conn := st.conn
	st.mu.Unlock()
	st.stopOnce.Do(func() {
		st.cancel()
		if conn != nil {
			conn.Close()
		}
	})
	st.endTrace(err)
}

func (r *Runner) isStopped(st *stream) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.stopped
}

// receive reads results until the stream ends, and returns the trace error.
func (r *Runner) receive(ctx context.Context, st *stream, conn Conn) error {
	for {
		res, err := conn.Recv(ctx)
		switch {
		case err == nil:
			r.emit(st, res)
		case ctx.Err() != nil:
			return nil // stopped: barge-in, turn interrupted, drain timeout or Close
		case errors.Is(err, ErrStreamEnded):
			return nil
		default:
			r.errs.Add(1)
			r.report(err)
			r.reportError(st, err)
			return errProviderFailure
		}
	}
}

// emit turns a result into a candidate for the stream's turn.
func (r *Runner) emit(st *stream, res Result) {
	if res.Text == "" {
		return
	}
	turn, gen := st.owner()
	if turn == "" { // unbound (between turns): the Conn keeps the text for the adopting turn
		return
	}
	gen = r.frameGen(gen)
	frame := &core.TranscriptCandidateFrame{
		Text: res.Text, Turn: core.TurnID(turn), Gen: core.Generation(gen), Timestamp: r.cfg.Now(),
		Final: res.Final, EndOfUtterance: res.EndOfUtterance,
		Stability: res.Stability, HasStability: res.HasStability, Provider: r.cfg.Provider,
	}
	if r.session.Reenter(frame, core.Downstream, r.index+1, gen) == nil && res.Final {
		st.traceFinal.Do(func() { st.event("stt.first_final") })
	}
}

func (r *Runner) reportError(st *stream, err error) {
	turn, gen := st.owner()
	gen = r.frameGen(gen)
	_ = r.session.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, r.index+1, gen)
}

func (r *Runner) frameGen(gen uint64) uint64 {
	if gen == 0 {
		return r.session.CurrentGeneration()
	}
	return gen
}

func (r *Runner) report(err error) {
	if r.cfg.Breaker != nil && r.cfg.Classify != nil {
		r.cfg.Breaker.Report(err, r.cfg.Classify)
	}
}
```

- [ ] **Step 4: Run the tests to see them pass.**

```bash
cd core && gofmt -l ./sttunit && go vet ./sttunit/... && go test -race -count=1 ./sttunit/... && go test -race -count=5 -run 'TestRunner_' ./sttunit/
```

Expected: no gofmt output, `ok govoice/core/sttunit` twice (the second run takes about 15 s, since `TestRunner_DialDoesNotBlockProcessFrame` sleeps 3 s per run).

- [ ] **Step 5: Gate.** Run `make verify`. Expected: `=== All verify checks passed ===`. Nothing consumes `sttunit` yet, so e2e is not required for this task. Task 2.3 makes the first consumer.

- [ ] **Step 6: Commit.**

```bash
git add core/sttunit/runner.go core/sttunit/runner_test.go
git commit -m "feat(core): sttunit runner dials off the run loop

ProcessFrame never opens a provider stream: the dial runs on the stream's
session task and audio arriving meanwhile is held in a bounded pre-roll,
flushed in order (a Finish owed meanwhile goes last). A failed dial is an
ErrorFrame for the turn (STTFailover moves on it) and is not retried until
the next turn. Fixes the dial half of A1 for the adapters that adopt it.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2.2: drop recovery (A8) and the failover proof

**Files:**
- Modify: `core/sttunit/runner.go` (stream fields, `emit`, `receive`, the `run` flush-error branch, plus new `lost`, `dropTurn` and `invalidate`), `server/tts_failover.go` (`failoverPolicy("stt")` budget, Steps 7–11)
- Test: `core/sttunit/runner_test.go` (append), `core/stt_failover_test.go` (append, plus import `govoice/core/sttunit`), `server/failover_timing_test.go` (two table rows), new `server/stt_failover_budget_test.go`

**Interfaces:**
- Consumes: `core.TranscriptInvalidationFrame`, `core.ErrTranscriptInvalidated`, `core.STTFailover` (`fromCandidate` treats an invalidation as class `server_error`, `core/stt_failover.go:482`), the existing `sttRig`, `fakeSTT`, `cands` and `allWords` in `core/stt_failover_test.go`, `coreturn.DefaultNoTranscriptTimeout` (`core/turn/stop.go:50`), `resilience.OpenBounded`/`Execute`.
- Produces: the invalidate-on-loss behaviour of `sttunit.Runner`, and `failoverPolicy("stt")` = at most 2 × 1.5 s (unexported const `sttFailoverAttempt`). No new exported names.

- [ ] **Step 1: Write the failing tests.** Append to `core/sttunit/runner_test.go`:

```go
func (h *harness) invalidations() []*core.TranscriptInvalidationFrame {
	return framesOf[*core.TranscriptInvalidationFrame](h)
}

// A8: the provider ends the stream while the turn still owes its transcript
// (no Finish sent yet): the turn is invalidated, then an error raised, so an
// STT failover replays it. A clean end is not an outage: the breaker is not told.
func TestRunner_MidUtteranceDrop_InvalidatesTurn(t *testing.T) {
	conn := newFakeConn(t)
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := newHarness(t, Config{Dial: dialConn(conn), Breaker: br, Classify: func(error) resilience.Verdict { return resilience.Verdict{Retryable: true} }})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	conn.results <- recvResult{res: Result{Text: "hel"}}
	h.waitFor("interim", func() bool { return len(h.candidates()) == 1 })
	conn.results <- recvResult{err: fmt.Errorf("close 1000: %w", ErrStreamEnded)}
	h.waitFor("invalidation and error", func() bool { return len(h.invalidations()) == 1 && len(h.errorFrames()) == 1 })

	inv, ef := h.invalidations()[0], h.errorFrames()[0]
	if inv.Turn != "turn-1-1" || inv.Gen != 1 || ef.Turn != "turn-1-1" || ef.Gen != 1 {
		t.Fatalf("invalidation %+v error %+v", inv, ef)
	}
	if !errors.Is(ef.Err, core.ErrTranscriptInvalidated) || !errors.Is(ef.Err, ErrStreamEnded) {
		t.Fatalf("error %v must wrap ErrTranscriptInvalidated and the cause", ef.Err)
	}
	frames := h.sink.GetFrames()
	invAt := slices.IndexFunc(frames, func(f core.Frame) bool { _, ok := f.(*core.TranscriptInvalidationFrame); return ok })
	errAt := slices.IndexFunc(frames, func(f core.Frame) bool { _, ok := f.(*core.ErrorFrame); return ok })
	if invAt > errAt {
		t.Fatal("the error overtook the invalidation")
	}
	if br.WouldRefuse() {
		t.Fatal("a clean provider end was reported to the breaker")
	}
	if _, _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d", n)
	}
}

func TestRunner_ProviderEndIsQuietWhenNothingIsOwed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script func(h *harness, conn *fakeConn)
	}{
		{"unbound", func(h *harness, conn *fakeConn) { h.audio(1) }},
		{"after the utterance final", func(h *harness, conn *fakeConn) {
			h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
			h.audio(1)
			h.post(&core.UserStoppedSpeakingFrame{})
			conn.results <- recvResult{res: Result{Text: "done", Final: true, EndOfUtterance: true}}
			h.waitFor("final", func() bool { return len(h.candidates()) == 1 })
		}},
		{"after finish with nothing heard", func(h *harness, conn *fakeConn) {
			h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
			h.audio(1)
			h.post(&core.UserStoppedSpeakingFrame{})
			h.waitFor("finish sent", func() bool { return slices.Contains(conn.snapshot(), "finish") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newFakeConn(t)
			h := newHarness(t, Config{Dial: dialConn(conn)})
			tc.script(h, conn)
			h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
			conn.results <- recvResult{err: ErrStreamEnded}
			h.waitFor("released", func() bool { return h.r.Streams() == 0 })
			time.Sleep(20 * time.Millisecond)
			if n := len(h.errorFrames()) + len(h.invalidations()); n != 0 {
				t.Fatalf("a clean end with nothing owed raised %d frames", n)
			}
		})
	}
}

// After Finish the provider may still drop before the final: words were heard
// and no utterance-final candidate came, so the turn is lost (xai M3-09 rule).
func TestRunner_EndAfterFinishWithWordsButNoFinalInvalidates(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	conn.results <- recvResult{res: Result{Text: "hel"}}
	h.waitFor("interim", func() bool { return len(h.candidates()) == 1 })
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("finish sent", func() bool { return slices.Contains(conn.snapshot(), "finish") })
	conn.results <- recvResult{err: ErrStreamEnded}
	h.waitFor("invalidation and error", func() bool { return len(h.invalidations()) == 1 && len(h.errorFrames()) == 1 })
}

// A Final candidate commits all heard text even without EndOfUtterance (Riva
// answers CloseSend with no further result after an earlier final): the
// provider's end after Finish is then quiet, not a lost turn.
func TestRunner_FinalBeforeFinishThenEndIsQuiet(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	conn.results <- recvResult{res: Result{Text: "hello", Final: true}}
	h.waitFor("final", func() bool { return len(h.candidates()) == 1 })
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("finish sent", func() bool { return slices.Contains(conn.snapshot(), "finish") })
	conn.results <- recvResult{err: ErrStreamEnded}
	h.waitFor("released", func() bool { return h.r.Streams() == 0 })
	time.Sleep(20 * time.Millisecond)
	if n := len(h.errorFrames()) + len(h.invalidations()); n != 0 {
		t.Fatalf("a committed turn was called lost: %d frames", n)
	}
}

// A bound stream whose pre-roll flush fails lost the turn's held audio: the
// turn is invalidated (so failover replays it) and nothing redials this turn.
func TestRunner_FlushSendFailureLosesBoundTurn(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	conn.setSendErr(errors.New("broken pipe"))
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("held", func() bool { return h.audioPassed() == 1 })
	close(release)
	h.waitFor("invalidation and error", func() bool { return len(h.invalidations()) == 1 && len(h.errorFrames()) == 1 })
	if ef := h.errorFrames()[0]; !errors.Is(ef.Err, core.ErrTranscriptInvalidated) || ef.Turn != "turn-1-1" {
		t.Fatalf("error frame %+v", ef)
	}
	h.audio(2)
	h.waitFor("audio passed", func() bool { return h.audioPassed() == 2 })
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dials = %d: no redial for a lost turn", n)
	}
}
```

Append to `core/stt_failover_test.go` (add `"govoice/core/sttunit"` to its import block):

```go
// Review Focus 1 (A1): the primary's dial hangs past the 2 s frame watchdog
// for 3 s, the worst case of failoverPolicy("stt") after Steps 7-11. The call
// survives (the dial runs off the run loop), the failed open fails over, and
// the fallback transcribes the whole utterance from the replay.
func TestSTTFailoverPrimaryDialHangsFallbackTranscribes(t *testing.T) {
	primary := sttunit.New(sttunit.Config{Provider: "a", Dial: func(ctx context.Context) (sttunit.Conn, error) {
		select {
		case <-time.After(3 * time.Second):
			return nil, errors.New("dial: no answer")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	fallback := fakeSTT("b", coretest.STTConfig{})
	r := newSTTRig(t, core.STTFailoverConfig{Candidates: cands(core.STTCandidate{Provider: "a", Proc: primary}, core.STTCandidate{Provider: "b", Proc: fallback})}, false)
	r.audio(0) // pre-speech padding: VAD reports the start late
	r.start()
	for _, w := range []int{1, 2, 3} {
		r.audio(w)
	}
	for deadline := time.Now().Add(6 * time.Second); r.w.Active() != "b"; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no failover; session err = %v", r.sess.Err())
		}
	}
	r.audio(4)
	r.stop()
	r.waitFinal(1)
	if err := r.sess.Err(); err != nil {
		t.Fatalf("the hung dial ended the call: %v", err)
	}
	if got := fallback.Received(); !bytes.Equal(got, r.sent) {
		t.Fatalf("fallback audio is not the utterance once: got %d bytes, want %d", len(got), len(r.sent))
	}
	if f := r.finals(); len(f) != 1 || f[0].Text != allWords || f[0].Provider != "b" {
		t.Fatalf("finals = %+v", f)
	}
	if r.errors() != 0 {
		t.Fatal("the failed primary's error leaked downstream")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail.**

```bash
cd core && go test -race -count=1 -run 'TestRunner_MidUtteranceDrop|TestRunner_ProviderEndIsQuiet|TestRunner_EndAfterFinish|TestRunner_FinalBeforeFinish|TestRunner_FlushSendFailureLosesBoundTurn' ./sttunit/ ; go test -race -count=1 -run TestSTTFailoverPrimaryDialHangsFallbackTranscribes .
```

Expected: `TestRunner_MidUtteranceDrop_InvalidatesTurn`, `TestRunner_EndAfterFinishWithWordsButNoFinalInvalidates` and `TestRunner_FlushSendFailureLosesBoundTurn` FAIL with `timed out waiting for invalidation and error`. `TestRunner_ProviderEndIsQuietWhenNothingIsOwed` and `TestRunner_FinalBeforeFinishThenEndIsQuiet` PASS (they pin the quiet side). `TestSTTFailoverPrimaryDialHangsFallbackTranscribes` already PASSES with Task 2.1. Keep it as the Review Focus 1 regression proof.

- [ ] **Step 3: Implement.** In `core/sttunit/runner.go`:

(a) Add two fields to `stream` after `draining *time.Timer`:

```go
	lastText string // last candidate emitted for the bound turn
	settled  bool   // that candidate was Final: all heard text is committed
```

(b) Replace `emit`'s first lines (from `turn, gen := st.owner()` through the unbound `return`) with:

```go
	st.mu.Lock()
	turn, gen := st.turn, st.gen
	if turn != "" {
		// Final, not Final && EndOfUtterance: a closing response need not carry
		// an EOU final (Riva may answer CloseSend with nothing more).
		st.lastText, st.settled = res.Text, res.Final
	}
	st.mu.Unlock()
	if turn == "" { // unbound (between turns): the Conn keeps the text for the adopting turn
		return
	}
```

(c) In `receive`, replace

```go
		case errors.Is(err, ErrStreamEnded):
			return nil
```

with

```go
		case errors.Is(err, ErrStreamEnded):
			if !r.lost(st) {
				return nil
			}
			r.dropTurn(st, err)
			return errProviderFailure
```

(c2) In `run`, replace the flush-error branch's body

```go
			// The stream died taking its held audio: a failed open, with the breaker told.
			traceErr = errProviderFailure
			r.report(err)
			r.dialFailed(st, err)
```

with

```go
			// The stream died taking its held audio: a failed open, with the breaker
			// told. A bound turn's held audio is gone, so the turn is lost (A8).
			traceErr = errProviderFailure
			r.report(err)
			if turn, _ := st.owner(); turn != "" {
				r.dropTurn(st, err)
			} else {
				r.dialFailed(st, err)
			}
```

(d) Add after `receive`:

```go
// lost reports whether the provider ended st while its bound turn still owed
// a transcript: before Finish, or after it with words heard but not committed
// by a Final candidate.
func (r *Runner) lost(st *stream) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.turn != "" && (!st.closing || (st.lastText != "" && !st.settled))
}

// dropTurn ends a bound stream that lost its turn's audio. Nothing redials
// for the rest of the turn (the turn is invalidated and, under failover,
// replayed elsewhere); then the turn is invalidated.
func (r *Runner) dropTurn(st *stream, cause error) {
	r.mu.Lock()
	if r.active == st {
		r.noDial = true
	}
	r.mu.Unlock()
	r.errs.Add(1)
	r.invalidate(st, cause)
}

// invalidate retires the bound turn's partial transcript, then raises the
// error an STT failover replays the turn on (the xai M3-09 shape). A clean
// end is not an outage, so the breaker is not told.
func (r *Runner) invalidate(st *stream, cause error) {
	turn, gen := st.owner()
	gen = r.frameGen(gen)
	if err := r.session.Reenter(&core.TranscriptInvalidationFrame{Turn: core.TurnID(turn), Gen: core.Generation(gen), Err: cause}, core.Downstream, r.index+1, gen); err != nil {
		return // the session is closing
	}
	_ = r.session.Reenter(&core.ErrorFrame{
		Err:  fmt.Errorf("%w: %s stream ended mid-utterance: %w", core.ErrTranscriptInvalidated, r.cfg.Provider, cause),
		Turn: core.TurnID(turn), Gen: core.Generation(gen),
	}, core.Downstream, r.index+1, gen)
}
```

- [ ] **Step 4: Run the tests to see them pass.**

```bash
cd core && gofmt -l ./sttunit && go vet ./... && go test -race -count=1 ./sttunit/... && go test -race -count=1 -run 'TestSTTFailover' . && go test -race -count=20 -run 'TestRunner_(MidUtterance|ProviderEnd|FinalBeforeFinish|EndAfterFinish|FlushSendFailure|PreRoll|FinishWhileDialing|FinishDuringFlush|LiveAudioDuringFlush|UnboundStream|LateResult)' ./sttunit/
```

Expected: no gofmt output, `ok` on every package, and the 20-count run passes.

- [ ] **Step 5: Gate.** Run `make verify`. Expected: `=== All verify checks passed ===`.

- [ ] **Step 6: Commit.**

```bash
git add core/sttunit/runner.go core/sttunit/runner_test.go core/stt_failover_test.go
git commit -m "fix(core): STT drop mid-utterance invalidates the turn so failover replays

A stream the provider ends while its bound turn still owes a transcript
(before Finish, or after it with heard words no Final committed), or whose
pre-roll flush fails, emits a TranscriptInvalidationFrame then an
ErrTranscriptInvalidated error (A8). Adds the Review Focus 1 proof: a
primary whose dial hangs 3 s fails over and the call survives the 2 s
watchdog.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 7: Write the failing server test (ruling: failover STT dial budget).** `failoverPolicy("stt")` (`server/tts_failover.go:69`) allows 2 × 5 s. A hung dial on a failover candidate then reports about 10 s after it began. `turn.SpeechTimeout` drops an untranscribed turn `DefaultNoTranscriptTimeout` (5 s) after silence onset (`core/turn/stop.go:50,83`), so the utterance is lost even though the call survives. The ruling is to cap failover candidates at 2 × 1.5 s. Primaries without a fallback keep their own policies, because `failoverPolicy` is only applied when `spec.Failover` is set.

Create `server/stt_failover_budget_test.go`:

```go
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"govoice/core/resilience"
	coreturn "govoice/core/turn"
	"govoice/providers/cartesia"
	"govoice/providers/deepgram"
	"govoice/providers/elevenlabs"
	"govoice/providers/google"
	"govoice/providers/nvidia"
)

// Review Focus 1: a failover candidate whose STT dial hangs must give up (so
// the ErrorFrame reaches STTFailover) before the turn processor drops the
// untranscribed turn. NoTranscriptTimeout counts from silence onset and the
// dial starts at speech onset, so finishing within it is a conservative bound.
// Both retry helpers the adapters use (OpenBounded, Execute) are timed with
// the real policy against a dial that hangs until its attempt is cut.
func TestSTTFailoverPolicyReportsAHungDialBeforeTheTurnIsDropped(t *testing.T) {
	retry := func(error) resilience.Verdict { return resilience.Verdict{Retryable: true} }
	for _, tc := range []struct {
		name string
		base resilience.Policy
	}{
		{"deepgram", deepgram.DefaultSTTConfig().RetryPolicy},
		{"cartesia", cartesia.DefaultSTTConfig().RetryPolicy},
		{"elevenlabs", elevenlabs.DefaultSTTConfig().RetryPolicy},
		{"nvidia", nvidia.DefaultSTTConfig().RetryPolicy},
		{"google", google.DefaultSTTOpenPolicy()}, // xai's DefaultSTTOpenPolicy is the same 2 x 5 s (build-tagged)
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pol := failoverPolicy("stt", tc.base)
			start := time.Now()
			_, err := resilience.OpenBounded(context.Background(), pol, retry, func(ctx context.Context) (struct{}, error) {
				<-ctx.Done()
				return struct{}{}, ctx.Err()
			})
			if took := time.Since(start); !errors.Is(err, resilience.ErrTimeout) || took >= coreturn.DefaultNoTranscriptTimeout {
				t.Fatalf("OpenBounded: err=%v after %v; must give up before %v", err, took, coreturn.DefaultNoTranscriptTimeout)
			}
			start = time.Now()
			err = resilience.Execute(context.Background(), nil, pol, retry, nil, func(ctx context.Context, _ int) error {
				<-ctx.Done()
				return ctx.Err()
			})
			if took := time.Since(start); !errors.Is(err, resilience.ErrTimeout) || took >= coreturn.DefaultNoTranscriptTimeout {
				t.Fatalf("Execute: err=%v after %v; must give up before %v", err, took, coreturn.DefaultNoTranscriptTimeout)
			}
		})
	}
}
```

In `server/failover_timing_test.go` `TestFailoverPolicyOnlyTightens`, update the two `stt` rows. The tightened cap is 1.5 s, and the "never loosened" row needs a base below it:

```go
		{"stt", resilience.Policy{MaxAttempts: 3, AttemptTimeout: 5 * time.Second}, 2, 1500 * time.Millisecond, 3 * time.Second},
		{"stt", resilience.Policy{MaxAttempts: 1, AttemptTimeout: time.Second}, 1, time.Second, time.Second}, // never loosened
```

- [ ] **Step 8: Run it to see it fail.**

```bash
cd server && go test -race -count=1 -run 'TestSTTFailoverPolicyReportsAHungDialBeforeTheTurnIsDropped|TestFailoverPolicyOnlyTightens' .
```

Expected: FAIL. Each subtest reports `must give up before 5s` after ~10 s, and `TestFailoverPolicyOnlyTightens` reports the 5 s row.

- [ ] **Step 9: Implement.** In `server/tts_failover.go`, replace the `failoverPolicy` doc comment's first sentence and the function with:

```go
// failoverPolicy tightens a provider's retry policy for a stage candidate that
// has a fallback behind it: 2 tries instead of 3, and for stt opens at most
// sttFailoverAttempt each, so a hung STT dial reports (and the fallback
// starts) after about 3 s, before the turn processor drops the untranscribed
// turn (turn.DefaultNoTranscriptTimeout, 5 s after silence onset); for tts
// after 2 x the provider's request timeout instead of up to 45 s. It only
// ever shortens base (a zero base field counts as unset). The last candidate
// keeps its full policy. LLMs are instead bounded by the first-token deadline
// (core.LLMFailover).
//
// ponytail: tts keeps its attempt timeout because the unary tts providers apply it
// to the whole unit (dial through the last audio byte; the Google and xAI streaming paths use it
// as an idle limit instead), so shortening it would
// cut long sentences mid-word; a first-audio deadline in ttsunit is the upgrade
// if 2 x 15 s proves too slow.
func failoverPolicy(stage string, base resilience.Policy) resilience.Policy {
	if base.MaxAttempts <= 0 || base.MaxAttempts > 2 {
		base.MaxAttempts = 2
	}
	if stage == "stt" && (base.AttemptTimeout <= 0 || base.AttemptTimeout > sttFailoverAttempt) {
		base.AttemptTimeout = sttFailoverAttempt
	}
	return base
}

// sttFailoverAttempt bounds one STT open of a candidate with a fallback: a
// websocket/gRPC handshake on a healthy provider takes well under it.
// ponytail: a cold Google channel (TLS + token fetch) can need longer; Warm
// pre-dials, and a slow open only costs a needless failover, not the turn.
const sttFailoverAttempt = 1500 * time.Millisecond
```

- [ ] **Step 10: Run it to see it pass.**

```bash
cd server && gofmt -l . && go vet ./... && go test -race -count=1 -run 'TestSTTFailoverPolicyReportsAHungDialBeforeTheTurnIsDropped|TestFailoverPolicyOnlyTightens|STT|Failover' .
```

Expected: `ok`, with each budget subtest taking about 3.1 s (run in parallel). Then run `make verify`, which must print `=== All verify checks passed ===`, and `make e2e-echo`, which must pass: the STT fallback flows in the e2e suites now use the shorter budget.

- [ ] **Step 11: Commit.**

```bash
git add server/tts_failover.go server/failover_timing_test.go server/stt_failover_budget_test.go
git commit -m "fix(server): failover STT dial gives up within the no-transcript window

failoverPolicy(stt) allowed 2 x 5 s, so a hung dial on a candidate with a
fallback reported after the turn processor had already dropped the
untranscribed turn (NoTranscriptTimeout, 5 s). Opens now get 2 x 1.5 s
(about 3 s worst case); primaries without a fallback keep their policies.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2.3: migrate Deepgram (pilot) and add `STTCounts` (B7)

**Files:**
- Modify: `providers/deepgram/stt.go` (rewritten in full below, 577 → ~235 lines), `providers/deepgram/errors.go` (delete `ErrProviderUnavailable`; `classify` keeps in-band errors out of the breaker)
- Test: `providers/deepgram/stt_test.go`. `warm_test.go` and `integration_test.go` are unchanged: they call `ensureStream`/`activeCount`, which the shim keeps.

**Interfaces:**
- Consumes: `sttunit.{New, Config, Conn, Result, ErrStreamEnded}`, `(*Runner).Streams`.
- Produces: `deepgram.STTProcessor` = `{*sttunit.Runner; cfg}` implementing `core.Processor`, `core.Attacher`, `io.Closer`, `core.STTCounters` (B7) and `netpool.Warmer`; unexported `(*STTProcessor).dial(ctx) (sttunit.Conn, error)`.
- Deleted from `stt.go`: `type stream` with `owner`, `event` and `endTrace`; the fields `session`, `index`, `mu`, `active`, `all`, `closed`, `turn` and `gen`; and the functions `Attach`, `Close`, `detachAllLocked`, `ProcessFrame`, `beginTurn`, `handleAudio`, `ioParent`, `ensureStream`, `receive`, `reportError`, `frameGen`, `finishActive`, `finish`, `stopTurn`, `release` and `stop`. `results` becomes a method of `dgConn`.

- [ ] **Step 1: Write the failing test changes** in `providers/deepgram/stt_test.go`:

(a) Replace the `activeCount` method (stt_test.go:383-387) with these shims. The call sites in `warm_test.go`, `integration_test.go` and this file keep compiling unchanged.

```go
// ensureStream dials one stream the way the runner's stream task does, then
// closes it (dial, retry and breaker tests).
func (p *STTProcessor) ensureStream(ctx context.Context) error {
	c, err := p.dial(ctx)
	if err == nil {
		c.Close()
	}
	return err
}

func (p *STTProcessor) activeCount() int { return p.Streams() }
```

(b) In `TestSTT_IdleTimeoutCloseIsNotAnOutage`, replace `h.speak(t)` with idle audio (an unbound stream). A NET-0001 close of a stream bound to an unfinished turn is now a lost turn (A8, Architecture Decision 4):

```go
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1})
```

(c) Replace `TestSTT_UnboundStreamAdoptedByNextTurn` (stt_test.go:437-505; it read `p.mu`, `p.active.committed` and `owner()`) with:

```go
// Audio between turns opens an unbound stream; the next turn adopts it (VAD
// reports speech start after the onset audio was sent) and keeps its text.
// UserStopped finalizes it; later audio opens a fresh unbound stream, which
// the following turn adopts in turn. That an unbound stream emits nothing is
// pinned race-free by sttunit's TestRunner_UnboundStreamAdoptedByNextTurn.
func TestSTT_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	var n atomic.Int32
	ctl := make(chan string, 8)
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageBinary {
				ctl <- string(data)
				continue
			}
			if n.Add(1) == 1 {
				_ = c.Write(ctx, websocket.MessageText, results("hi", true, false, false))
			} else {
				_ = c.Write(ctx, websocket.MessageText, results("there", false, false, false))
			}
		}
	})
	h := start(t, f.cfg(), nil)
	audio := func() {
		_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1})
	}
	last := func() *core.TranscriptCandidateFrame {
		c := h.candidates()
		if len(c) == 0 {
			return nil
		}
		return c[len(c)-1]
	}
	audio()
	waitFor(t, "unbound stream", func() bool { return f.opens.Load() == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	audio()
	waitFor(t, "adopted candidate", func() bool { c := last(); return c != nil && c.Text == "hi there" })
	if c := last(); c.Turn != "turn-1-1" || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	for _, want := range []string{"Finalize", "CloseStream"} {
		select {
		case got := <-ctl:
			if !strings.Contains(got, want) {
				t.Fatalf("control %q, want %s", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %s", want)
		}
	}
	audio()
	waitFor(t, "fresh stream", func() bool { return f.opens.Load() == 2 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	audio()
	waitFor(t, "second turn adopts it", func() bool { c := last(); return c != nil && c.Turn == "turn-1-2" })
	if n := f.opens.Load(); n != 2 {
		t.Fatalf("the second turn opened its own stream: opens=%d", n)
	}
}
```

(d) Append two new tests:

```go
// A8: a clean close while the turn still owes its transcript (no Finalize
// sent) loses the utterance: the turn is invalidated so an STT failover
// replays it, and the breaker is not told.
func TestSTT_CleanCloseMidUtteranceInvalidatesTurn(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := start(t, cfg, nil)
	h.speak(t)
	waitFor(t, "invalidating error", func() bool {
		for _, err := range h.errors() {
			if errors.Is(err, core.ErrTranscriptInvalidated) {
				return true
			}
		}
		return false
	})
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("a clean close is not an outage")
	}
}

// B7: Deepgram stream errors reach voice_provider_errors_total via STTCounts.
func TestSTT_CountsErrorsForMetrics(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusInternalError, "NET-0000")
	})
	h := start(t, f.cfg(), nil)
	var counters core.STTCounters = h.proc
	h.speak(t)
	waitFor(t, "counted error", func() bool { _, _, _, n := counters.STTCounts(); return n == 1 })
}

// Deepgram's in-band "Error" message fails that stream (an ErrorFrame, so a
// failover moves on) but stays out of the shared breaker, as before the runner.
func TestSTT_InBandErrorStaysOutOfTheBreaker(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"Error","err_msg":"bad audio"}`))
		<-ctx.Done()
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := start(t, cfg, nil)
	h.speak(t)
	waitFor(t, "error frame", func() bool { return len(h.errors()) > 0 })
	if !errors.Is(h.errors()[0], errInBand) {
		t.Fatalf("err = %v", h.errors()[0])
	}
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("an in-band stream error opened the shared breaker")
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
cd providers/deepgram && go test -race -count=1 ./...
```

Expected: FAIL to build with `p.dial undefined (type *STTProcessor has no field or method dial)`, `p.Streams undefined`, `undefined: errInBand` and `h.proc (variable of type *STTProcessor) does not implement core.STTCounters`.

- [ ] **Step 3: Implement.** Replace `providers/deepgram/stt.go` with:

```go
package deepgram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/sttunit"
)

// STTConfig configures the Deepgram live-transcription processor.
type STTConfig struct {
	Endpoint       string // default "wss://api.deepgram.com/v1/listen"
	Model          string // default "nova-3"
	Language       string // default "en"
	SampleRate     int    // default 16000; input is 16-bit mono PCM (linear16)
	InterimResults bool   // default true
	APIKey         string
	RetryPolicy    resilience.Policy // applied to opening the stream
	CircuitBreaker *resilience.Breaker
	// DrainTimeout bounds the wait for Deepgram's final results after the
	// turn ends before the socket is torn down.
	DrainTimeout time.Duration
	Now          func() time.Time
	// Pool is the connection pool used to dial the websocket; default is the
	// process-wide "deepgram"/"stt" pool (see core/netpool).
	Pool *netpool.Pool
}

func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: "wss://api.deepgram.com/v1/listen", Model: "nova-3", Language: "en",
		SampleRate: 16000, InterimResults: true, DrainTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 3, AttemptTimeout: 5 * time.Second},
	}
}

// message is the subset of Deepgram's server messages we read.
type message struct {
	Type         string `json:"type"`
	IsFinal      bool   `json:"is_final"`
	SpeechFinal  bool   `json:"speech_final"`
	FromFinalize bool   `json:"from_finalize"`
	Channel      struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
		} `json:"alternatives"`
	} `json:"channel"`
	ErrMsg string `json:"err_msg"`
}

// STTProcessor implements core.Processor for Deepgram speech-to-text: one
// websocket per utterance, dialed off the run loop (turn binding, pre-roll,
// drain and drop recovery live in core/sttunit).
type STTProcessor struct {
	*sttunit.Runner
	cfg STTConfig
}

// B7: the runner's error count reaches voice_provider_errors_total.
var _ core.STTCounters = (*STTProcessor)(nil)

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = def.DrainTimeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = def.RetryPolicy
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Pool == nil {
		cfg.Pool = netpool.Get(netpool.Options{Provider: "deepgram", Stage: "stt", HTTP1Only: true})
	}
	p := &STTProcessor{cfg: cfg}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "deepgram", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm pre-dials TCP+TLS to the endpoint (no websocket session: Deepgram
// closes idle sockets after ~10 s and an unused one would also be closed on
// KeepAlive timeout, so only the transport is warmed). Skipped while the
// breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.cfg.Pool.NoteSkipped()
		return nil
	}
	return p.cfg.Pool.Warm(ctx, p.cfg.Endpoint)
}

func (p *STTProcessor) wsURL() (string, error) {
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse deepgram endpoint: %w", err)
	}
	q := u.Query()
	q.Set("model", p.cfg.Model)
	if p.cfg.Language != "" {
		q.Set("language", p.cfg.Language)
	}
	q.Set("encoding", "linear16")
	q.Set("sample_rate", strconv.Itoa(p.cfg.SampleRate))
	q.Set("channels", "1")
	q.Set("interim_results", strconv.FormatBool(p.cfg.InterimResults))
	q.Set("endpointing", "false") // we own turns
	q.Set("smart_format", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens one stream on ctx. The breaker and retries wrap only the dial:
// nothing is committed until audio flows, so a failed open is always safe to retry.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	target, err := p.wsURL()
	if err != nil {
		return nil, err
	}
	var conn *websocket.Conn
	opts := &websocket.DialOptions{HTTPClient: p.cfg.Pool.Client(0), HTTPHeader: http.Header{"Authorization": {"Token " + p.cfg.APIKey}}}
	err = resilience.Execute(ctx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, classify, nil, func(actx context.Context, _ int) error {
		c, resp, derr := websocket.Dial(actx, target, opts)
		if derr != nil {
			return MapError("stt", derr, resp)
		}
		conn = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &dgConn{conn: conn}, nil
}

// errInBand marks Deepgram's in-band "Error" message: a failure of one
// stream, which says nothing about the provider's health. The breaker is
// shared by every call, so classify keeps it out (as before this runner).
var errInBand = errors.New("deepgram: in-band stream error")

// dgConn is one Deepgram stream and its turn transcript.
type dgConn struct {
	conn      *websocket.Conn
	committed string // finalized segments of this turn, space-joined (Recv's task only)
}

func (c *dgConn) Send(ctx context.Context, pcm []byte) error {
	if err := c.conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
		return MapError("stt", err, nil)
	}
	return nil
}

// Finish asks Deepgram to flush its final results and close.
func (c *dgConn) Finish(ctx context.Context) error {
	for _, msg := range []string{`{"type":"Finalize"}`, `{"type":"CloseStream"}`} {
		if err := c.conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			return err
		}
	}
	return nil
}

func (c *dgConn) Close() { _ = c.conn.CloseNow() }

func (c *dgConn) Recv(ctx context.Context) (sttunit.Result, error) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return sttunit.Result{}, readError(err)
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "Results", "":
			return c.results(&m), nil
		case "Error":
			// errInBand keeps it out of the shared breaker (classify), as before.
			return sttunit.Result{}, MapError("stt", fmt.Errorf("%w: %s", errInBand, m.ErrMsg), nil)
		}
	}
}

// readError maps a failed read: a clean close, or Deepgram's idle timeout
// (NET-0001, no audio for ~10 s: our silence, not an outage), ends the
// stream; anything else is a provider failure.
func readError(err error) error {
	if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway {
		return sttunit.ErrStreamEnded
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) && strings.Contains(strings.ReplaceAll(strings.ToUpper(ce.Reason), "-", ""), "NET0001") {
		return sttunit.ErrStreamEnded
	}
	return MapError("stt", err, nil)
}

// results turns one Results message into the cumulative turn text: the
// finalized segments so far plus this message's text.
func (c *dgConn) results(m *message) sttunit.Result {
	seg := ""
	if len(m.Channel.Alternatives) > 0 {
		seg = strings.TrimSpace(m.Channel.Alternatives[0].Transcript)
	}
	text := join(c.committed, seg)
	if m.IsFinal {
		c.committed = text
	}
	eou := m.SpeechFinal || m.FromFinalize
	if seg == "" && !(m.IsFinal && eou) {
		return sttunit.Result{}
	}
	return sttunit.Result{Text: text, Final: m.IsFinal, EndOfUtterance: m.IsFinal && eou}
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}
```

In `providers/deepgram/errors.go`, delete the line `ErrProviderUnavailable = errors.New("deepgram: provider unavailable")`, which has no reader left. Then make `classify` (errors.go:84) start with the in-band check:

```go
// classify reads an error for the shared breaker and retry loop.
func classify(err error) resilience.Verdict {
	if errors.Is(err, context.Canceled) {
		return resilience.Verdict{ClientError: true}
	}
	if errors.Is(err, errInBand) { // one stream's in-band error: fail over, but not an outage
		return resilience.Verdict{Retryable: true, NoBreaker: true}
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return resilience.Verdict{Retryable: pe.Retryable, ClientError: pe.ClientError, RetryAfter: pe.RetryAfter}
	}
	return resilience.Verdict{Retryable: true}
}
```

`server/stt_failover.go:41 sttClassify` reads Deepgram's `ProviderError` fields, not `classify`, so the failover class is unchanged. Then run `gofmt -w providers/deepgram`.

- [ ] **Step 4: Run them to see them pass.**

```bash
cd providers/deepgram && gofmt -l . && go vet ./... && go test -race -count=1 ./... && go test -race -count=10 -run 'TestSTT_' .
cd ../../server && go test -race -count=1 -run 'STT|Failover|Registry|Warm' .
```

Expected: no gofmt output and `ok` everywhere. The 10-count run is a flake check of the rewritten adoption test.

- [ ] **Step 5: Gate.** Run `make verify`, then `make e2e-echo`, then `make offline-test` if the offline stack is up. Expected: `=== All verify checks passed ===` and both e2e suites green. Record the `git diff --stat` line delta in the commit body.

- [ ] **Step 6: Commit.**

```bash
git add providers/deepgram/stt.go providers/deepgram/errors.go providers/deepgram/stt_test.go
git commit -m "refactor(deepgram): STT on the shared sttunit runner

Deepgram keeps only the dial, the wire messages and transcript assembly; the
stream lifecycle is core/sttunit, so the dial no longer runs on the session
run loop (A1) and a mid-utterance close invalidates the turn (A8). Exports
STTCounts so Deepgram STT errors reach voice_provider_errors_total (B7).

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2.4: migrate Cartesia

**Files:**
- Modify: `providers/cartesia/stt.go` (rewritten in full below, 646 → ~260 lines), `providers/cartesia/errors.go` (delete `ErrProviderUnavailable`)
- Test: `providers/cartesia/stt_test.go`

**Interfaces:**
- Consumes: as Task 2.3.
- Produces: `cartesia.STTProcessor` = `{*sttunit.Runner; cfg}`. `STTCounts` now comes from the runner (the `errs` field and its method are deleted). Unexported: `dial` and `sttConn`.
- Deleted from `stt.go`: `type sttStream` with `owner`, `event` and `endTrace`; the fields `session`, `index`, `mu`, `active`, `all`, `closed`, `turn`, `gen` and `errs`; and the functions `STTCounts`, `Attach`, `Close`, `detachAllLocked`, `ProcessFrame`, `beginTurn`, `handleAudio`, `ioParent`, `ensureStream`, `receive`, `reportError`, `frameGen`, `emit`, `finishActive`, `finish`, `stopTurn`, `release` and `stop`. `transcript` and `flushed` become methods of `sttConn`.

- [ ] **Step 1: Write the failing test changes** in `providers/cartesia/stt_test.go`:

(a) Replace `activeCount` (stt_test.go:118-122) with the same two shims as Task 2.3 (a), receiver `*STTProcessor`:

```go
// ensureStream dials one stream the way the runner's stream task does, then
// closes it (dial, retry and breaker tests).
func (p *STTProcessor) ensureStream(ctx context.Context) error {
	c, err := p.dial(ctx)
	if err == nil {
		c.Close()
	}
	return err
}

func (p *STTProcessor) activeCount() int { return p.Streams() }
```

(b) `TestSTT_BreakerOpensOnOutage`: delete the final check `if _, _, _, errs := h.proc.STTCounts(); errs != 2 { … }` (stt_test.go:298-300). Dial failures are now counted by the runner when it reports them, not by the dial. `sttunit.TestRunner_DialFailureReportsErrorFrame` pins that count, and `TestRunner_CircuitOpenDialIsReportedNotCounted` pins the circuit-open exclusion this test relied on.

(c) `TestSTT_IdleCloseIsNotAnOutage`: replace `h.speak()` with `h.audio(1)`. Idle closes are quiet for unbound streams. A bound, unfinished turn is A8.

(d) Replace `TestSTT_UnboundStreamAdoptedByNextTurn` (stt_test.go:491-530) with:

```go
func TestSTT_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	f := newSFake(t, echo(func(n int) [][]byte {
		if n == 1 {
			return [][]byte{transcript("hi", true)}
		}
		return [][]byte{transcript(" there", false)}
	}, nil))
	h := sstart(t, f.cfg(), nil)
	last := func() *core.TranscriptCandidateFrame {
		c := h.candidates()
		if len(c) == 0 {
			return nil
		}
		return c[len(c)-1]
	}
	h.audio(1)
	swait(t, "unbound stream", func() bool { return f.opens.Load() == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	swait(t, "adopted candidate", func() bool { c := last(); return c != nil && c.Text == "hi there" })
	if c := last(); c.Turn != "turn-1-1" || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	swait(t, "closed", func() bool { return h.proc.activeCount() == 0 })
	h.audio(1)
	swait(t, "fresh stream", func() bool { return f.opens.Load() == 2 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	h.audio(1)
	swait(t, "second turn adopts it", func() bool { c := last(); return c != nil && c.Turn == "turn-1-2" })
	if n := f.opens.Load(); n != 2 {
		t.Fatalf("the second turn opened its own stream: opens=%d", n)
	}
}
```

(e) Append:

```go
// A8: a clean close while the turn still owes its transcript invalidates it.
func TestSTT_CleanCloseMidUtteranceInvalidatesTurn(t *testing.T) {
	f := newSFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	h.speak()
	swait(t, "invalidating error", func() bool {
		for _, err := range h.errors() {
			if errors.Is(err, core.ErrTranscriptInvalidated) {
				return true
			}
		}
		return false
	})
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("a clean close is not an outage")
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
cd providers/cartesia && go test -race -count=1 ./...
```

Expected: FAIL to build with `p.dial undefined` and `p.Streams undefined`.

- [ ] **Step 3: Implement.** Replace `providers/cartesia/stt.go` with:

```go
package cartesia

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/sttunit"
)

// Protocol (docs.cartesia.ai STT websocket reference): binary PCM frames up;
// text "finalize" (flush buffered audio -> server "flush_done") and "close"
// (flush and end -> server "done"). Server JSON: transcript {text, is_final,
// words?}, flush_done, done, error {status_code, title, message}. A transcript's
// text is a delta: finals are concatenated verbatim (no added whitespace), an
// interim replaces the previous interim.

// STTConfig configures the Cartesia (Ink) streaming transcription processor.
type STTConfig struct {
	Endpoint   string // default "wss://api.cartesia.ai/stt/websocket"
	Version    string // Cartesia-Version (header and cartesia_version query), default "2026-08-14"
	Model      string // default "ink-whisper"; docs also list "ink-2", "ink-preview"
	Language   string // default "en"; sent for ink-whisper only (per the docs)
	SampleRate int    // default 16000; input is 16-bit mono PCM (pcm_s16le)
	// MinVolume (0-1) and MaxSilenceSecs tune Cartesia's own silence
	// auto-finalization (ink-whisper only); zero leaves the server default.
	MinVolume      float64
	MaxSilenceSecs float64
	APIKey         string
	// RetryPolicy bounds opening the websocket (default 2 tries x 5 s).
	RetryPolicy    resilience.Policy
	CircuitBreaker *resilience.Breaker
	// DrainTimeout bounds the wait for the final results after the turn ends
	// before the socket is torn down.
	DrainTimeout time.Duration
	Now          func() time.Time
	// Pool dials the websocket; default is the process-wide "cartesia"/"stt" pool.
	Pool *netpool.Pool
}

func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: "wss://api.cartesia.ai/stt/websocket", Version: "2026-08-14", Model: "ink-whisper",
		Language: "en", SampleRate: 16000, DrainTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond},
	}
}

// sttMessage is the subset of Cartesia's STT server messages we read.
type sttMessage struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	IsFinal    bool   `json:"is_final"`
	StatusCode int    `json:"status_code"`
	Title      string `json:"title"`
	Message    string `json:"message"`
}

// STTProcessor implements core.Processor for Cartesia speech-to-text: one
// websocket per utterance, dialed off the run loop (the stream lifecycle lives
// in core/sttunit).
type STTProcessor struct {
	*sttunit.Runner
	cfg STTConfig
}

var _ core.STTCounters = (*STTProcessor)(nil)

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Version == "" {
		cfg.Version = def.Version
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = def.DrainTimeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = def.RetryPolicy
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Pool == nil {
		cfg.Pool = netpool.Get(netpool.Options{Provider: "cartesia", Stage: "stt", HTTP1Only: true})
	}
	p := &STTProcessor{cfg: cfg}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "cartesia", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm pre-dials TCP+TLS only: an unused websocket would just idle out.
// Skipped while the breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.cfg.Pool.NoteSkipped()
		return nil
	}
	return p.cfg.Pool.Warm(ctx, p.cfg.Endpoint)
}

// wsURL carries no credential; the key travels in a header only.
func (p *STTProcessor) wsURL() (string, error) {
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil {
		return "", errors.New("cartesia: invalid stt endpoint")
	}
	q := u.Query()
	q.Set("model", p.cfg.Model)
	q.Set("encoding", "pcm_s16le")
	q.Set("sample_rate", strconv.Itoa(p.cfg.SampleRate))
	q.Set("cartesia_version", p.cfg.Version)
	// language, min_volume and max_silence_duration_secs are documented as
	// ink-whisper only; other models (ink-2, ink-preview) do not get them.
	whisper := strings.HasPrefix(p.cfg.Model, "ink-whisper")
	if whisper && p.cfg.Language != "" {
		q.Set("language", p.cfg.Language)
	}
	if whisper && p.cfg.MinVolume > 0 {
		q.Set("min_volume", strconv.FormatFloat(p.cfg.MinVolume, 'f', -1, 64))
	}
	if whisper && p.cfg.MaxSilenceSecs > 0 {
		q.Set("max_silence_duration_secs", strconv.FormatFloat(p.cfg.MaxSilenceSecs, 'f', -1, 64))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens one stream on ctx. Retries wrap only the dial; the breaker gets
// one report per open.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	target, err := p.wsURL()
	if err != nil {
		return nil, err
	}
	br := p.cfg.CircuitBreaker
	if br != nil && br.WouldRefuse() {
		return nil, ErrCircuitOpen
	}
	opts := &websocket.DialOptions{HTTPClient: p.cfg.Pool.Client(0),
		HTTPHeader: http.Header{"X-API-Key": {p.cfg.APIKey}, "Cartesia-Version": {p.cfg.Version}}}
	conn, err := resilience.OpenBounded(ctx, p.cfg.RetryPolicy, classify, func(actx context.Context) (*websocket.Conn, error) {
		c, resp, derr := websocket.Dial(actx, target, opts)
		if derr != nil {
			return nil, MapError("stt", derr, resp)
		}
		c.SetReadLimit(1 << 20)
		return c, nil
	})
	if err != nil {
		if errors.Is(err, resilience.ErrTimeout) { // a hang is an outage
			err = &ProviderError{Capability: "stt", Code: "TIMEOUT", Message: SanitizeString(err.Error()), Retryable: true, Cause: err}
		}
		if br != nil {
			br.Report(err, classify)
		}
		return nil, err
	}
	if br != nil {
		br.RecordSuccess()
	}
	return &sttConn{conn: conn}, nil
}

// idleClose reports whether err is the server dropping a connection that got
// no audio (our silence, not an outage). The exact idle close code/reason is
// undocumented, so match the reason text.
func idleClose(err error) bool {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return false
	}
	r := strings.ToLower(ce.Reason)
	return strings.Contains(r, "idle") || strings.Contains(r, "timeout") || strings.Contains(r, "timed out")
}

// sttConn is one Cartesia stream and its turn transcript (Recv's task only).
type sttConn struct {
	conn               *websocket.Conn
	committed, interim string // final deltas concatenated verbatim; the current non-final delta
}

func (c *sttConn) Send(ctx context.Context, pcm []byte) error {
	if err := c.conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
		return MapError("stt", err, nil)
	}
	return nil
}

// Finish asks Cartesia to flush (flush_done) and close (done).
func (c *sttConn) Finish(ctx context.Context) error {
	for _, msg := range []string{"finalize", "close"} {
		if err := c.conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			return err
		}
	}
	return nil
}

func (c *sttConn) Close() { _ = c.conn.CloseNow() }

func (c *sttConn) Recv(ctx context.Context) (sttunit.Result, error) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway || idleClose(err) {
				return sttunit.Result{}, sttunit.ErrStreamEnded
			}
			return sttunit.Result{}, MapError("stt", err, nil)
		}
		var m sttMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "transcript":
			return c.transcript(&m), nil
		case "flush_done":
			return c.flushed(), nil
		case "done":
			return sttunit.Result{}, sttunit.ErrStreamEnded
		case "error":
			msg := m.Message
			if msg == "" {
				msg = m.Title
			}
			return sttunit.Result{}, messageError("stt", m.StatusCode, msg)
		}
	}
}

// transcript turns one transcript delta into the cumulative turn text: the
// finalized deltas so far plus the current interim.
func (c *sttConn) transcript(m *sttMessage) sttunit.Result {
	if m.IsFinal {
		c.committed += m.Text
		c.interim = ""
	} else {
		c.interim = m.Text
	}
	return sttunit.Result{Text: strings.TrimSpace(c.committed + c.interim), Final: m.IsFinal}
}

// flushed handles flush_done: everything buffered at finalize is now final, so
// the cumulative text ends the utterance.
func (c *sttConn) flushed() sttunit.Result {
	c.committed += c.interim
	c.interim = ""
	return sttunit.Result{Text: strings.TrimSpace(c.committed), Final: true, EndOfUtterance: true}
}
```

In `providers/cartesia/errors.go` delete the line `ErrProviderUnavailable = errors.New("cartesia: provider unavailable")`. Then run `gofmt -w providers/cartesia`.

- [ ] **Step 4: Run them to see them pass.**

```bash
cd providers/cartesia && gofmt -l . && go vet ./... && go test -race -count=1 ./... && go test -race -count=10 -run 'TestSTT_' .
```

Expected: no gofmt output and `ok`.

- [ ] **Step 5: Gate.** Run `make verify`, `make e2e-echo`, and `make offline-test` if the stack is up. All green.

- [ ] **Step 6: Commit.**

```bash
git add providers/cartesia/stt.go providers/cartesia/errors.go providers/cartesia/stt_test.go
git commit -m "refactor(cartesia): STT on the shared sttunit runner

Keeps the dial, wire messages and delta assembly; the dial leaves the run
loop (A1) and a mid-utterance close invalidates the turn (A8).

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2.5: migrate ElevenLabs

**Files:**
- Modify: `providers/elevenlabs/stt.go` (rewritten in full below, 648 → ~300 lines)
- Test: `providers/elevenlabs/stt_test.go`. `stt_warm_test.go` is unchanged: it calls `ensureStream`, which the shim keeps.

**Interfaces:**
- Consumes: as Task 2.3.
- Produces: `elevenlabs.STTProcessor` = `{*sttunit.Runner; cfg}`. `STTCounts` comes from the runner. Unexported: `dial`, `sttConn` and `errNothingToCommit`.
- Deleted from `stt.go`: `type stream` with `owner`, `event` and `endTrace`; the fields `session`, `index`, `errs`, `mu`, `active`, `all`, `closed`, `turn` and `gen`; and the functions `STTCounts`, `Attach`, `Close`, `detachAllLocked`, `ProcessFrame`, `beginTurn`, `handleAudio`, `report`, `ioParent`, `ensureStream`, `receive`, `reportError`, `frameGen`, `finishActive`, `finish`, `stopTurn`, `release` and `stop`, plus `var ErrProviderUnavailable` (declared in `stt.go`, with no other reader). `transcript` becomes a method of `sttConn`.

- [ ] **Step 1: Write the failing test changes** in `providers/elevenlabs/stt_test.go`:

(a) Replace `activeCount` (stt_test.go:162-166) with the two shims from Task 2.3 (a), verbatim.

(b) `TestSTT_IdleCloseIsNotAnOutage` and `TestSTT_InsufficientAudioActivityIsQuiet`: replace `h.speak()` with `h.audio(1)` in both. Quiet ends apply to unbound streams. For a bound, unfinished turn they are A8.

(c) Replace `TestSTT_UnboundStreamAdoptedByNextTurn` (stt_test.go:604-659) with:

```go
func TestSTT_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	var n atomic.Int32
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if m.Commit {
				continue
			}
			if n.Add(1) == 1 {
				sttMsg(ctx, c, "committed_transcript", "hi")
			} else {
				sttMsg(ctx, c, "partial_transcript", "there")
			}
		}
	})
	h := startSTT(t, f.cfg(), nil)
	last := func() *core.TranscriptCandidateFrame {
		c := h.candidates()
		if len(c) == 0 {
			return nil
		}
		return c[len(c)-1]
	}
	h.audio(1)
	sttWait(t, "unbound stream", func() bool { return f.opens.Load() == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	sttWait(t, "adopted candidate", func() bool { c := last(); return c != nil && c.Text == "hi there" })
	if c := last(); c.Turn != "turn-1-1" || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	h.audio(1)
	sttWait(t, "fresh stream", func() bool { return f.opens.Load() == 2 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	h.audio(1)
	sttWait(t, "second turn adopts it", func() bool { c := last(); return c != nil && c.Turn == "turn-1-2" })
	if n := f.opens.Load(); n != 2 {
		t.Fatalf("the second turn opened its own stream: opens=%d", n)
	}
}
```

(d) Append:

```go
// A8: a clean close while the turn still owes its transcript invalidates it.
func TestSTT_CleanCloseMidUtteranceInvalidatesTurn(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _ = readChunk(ctx, c)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = sttBreaker()
	h := startSTT(t, cfg, nil)
	h.speak()
	sttWait(t, "invalidating error", func() bool {
		for _, err := range h.errors() {
			if errors.Is(err, core.ErrTranscriptInvalidated) {
				return true
			}
		}
		return false
	})
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("a clean close is not an outage")
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
cd providers/elevenlabs && go test -race -count=1 ./...
```

Expected: FAIL to build with `p.dial undefined` and `p.Streams undefined`.

- [ ] **Step 3: Implement.** Replace `providers/elevenlabs/stt.go` with:

```go
package elevenlabs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/sttunit"
)

// sttRates are the documented pcm_<rate> input formats of the realtime STT.
var sttRates = map[int]bool{8000: true, 16000: true, 22050: true, 24000: true, 44100: true, 48000: true}

// STTConfig configures the ElevenLabs realtime (Scribe) STT processor.
type STTConfig struct {
	Endpoint string // full URL, default "wss://api.elevenlabs.io/v1/speech-to-text/realtime"
	Model    string // default "scribe_v2_realtime"
	// Language is an ISO 639 code ("en"); "" lets the model detect it. A regional
	// tag ("en-US") is cut to its primary subtag.
	Language   string
	SampleRate int // default 16000; input is 16-bit mono PCM (pcm_<rate>)
	// IncludeTimestamps asks for word timings (not used by the pipeline yet).
	IncludeTimestamps bool
	APIKey            string
	RetryPolicy       resilience.Policy // applied to opening the stream
	CircuitBreaker    *resilience.Breaker
	// DrainTimeout bounds the wait for the committed transcript after the turn
	// ends before the socket is torn down.
	DrainTimeout time.Duration
	Now          func() time.Time
	// Pool dials the websocket; default is the process-wide "elevenlabs"/"stt" pool.
	Pool *netpool.Pool
}

func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: "wss://api.elevenlabs.io/v1/speech-to-text/realtime", Model: "scribe_v2_realtime",
		SampleRate: 16000, DrainTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second},
	}
}

// sttMessage is the subset of ElevenLabs' realtime STT server messages we read.
type sttMessage struct {
	Type  string `json:"message_type"`
	Text  string `json:"text"`
	Error string `json:"error"`
}

// STTProcessor implements core.Processor for ElevenLabs realtime speech-to-text:
// one websocket per utterance, dialed off the run loop (the stream lifecycle
// lives in core/sttunit). Turn detection is ours: the stream uses the manual
// commit strategy and is committed when the turn ends.
type STTProcessor struct {
	*sttunit.Runner
	cfg STTConfig
}

var _ core.STTCounters = (*STTProcessor)(nil)

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if !sttRates[cfg.SampleRate] {
		return nil, fmt.Errorf("%w: unsupported STT sample rate %d", ErrInvalidConfig, cfg.SampleRate)
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = def.DrainTimeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = def.RetryPolicy
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Pool == nil {
		cfg.Pool = netpool.Get(netpool.Options{Provider: "elevenlabs", Stage: "stt", HTTP1Only: true})
	}
	p := &STTProcessor{cfg: cfg}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "elevenlabs", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm pre-dials TCP+TLS to the endpoint (no websocket session is opened).
// Skipped while the breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.cfg.Pool.NoteSkipped()
		return nil
	}
	return p.cfg.Pool.Warm(ctx, p.cfg.Endpoint)
}

type audioChunk struct {
	MessageType string `json:"message_type"`
	Audio       string `json:"audio_base_64"`
	Commit      bool   `json:"commit"`
	SampleRate  int    `json:"sample_rate"`
}

func (p *STTProcessor) chunk(pcm []byte, commit bool) []byte {
	b, _ := json.Marshal(audioChunk{"input_audio_chunk", base64.StdEncoding.EncodeToString(pcm), commit, p.cfg.SampleRate})
	return b
}

// mapErr classifies err and scrubs the API key from its text.
func (p *STTProcessor) mapErr(err error, resp *http.Response) *ProviderError {
	pe := MapError("stt", err, resp)
	pe.Message = strings.ReplaceAll(pe.Message, p.cfg.APIKey, "[REDACTED]")
	return pe
}

// wsURL carries parameters only; the key travels in the xi-api-key header.
func (p *STTProcessor) wsURL() (string, error) {
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse elevenlabs endpoint: %w", err)
	}
	q := u.Query()
	q.Set("model_id", p.cfg.Model)
	q.Set("audio_format", "pcm_"+strconv.Itoa(p.cfg.SampleRate))
	q.Set("commit_strategy", "manual") // we own turns
	if l := p.cfg.Language; l != "" {
		if i := strings.IndexAny(l, "-_"); i > 0 {
			l = l[:i]
		}
		q.Set("language_code", strings.ToLower(l))
	}
	if p.cfg.IncludeTimestamps {
		q.Set("include_timestamps", "true")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens one stream on ctx. The breaker and retries wrap only the dial (as
// Deepgram): nothing is committed until audio flows, so a failed open is safe
// to retry. Execute releases a half-open probe on client errors/cancel without
// closing the breaker. The dial ctx governs the handshake only.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	target, err := p.wsURL()
	if err != nil {
		return nil, err
	}
	var conn *websocket.Conn
	opts := &websocket.DialOptions{HTTPClient: p.cfg.Pool.Client(0), HTTPHeader: http.Header{"xi-api-key": {p.cfg.APIKey}}}
	err = resilience.Execute(ctx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, classify, nil, func(actx context.Context, _ int) error {
		c, resp, derr := websocket.Dial(actx, target, opts)
		if derr != nil {
			return p.mapErr(derr, resp)
		}
		c.SetReadLimit(1 << 20) // committed_transcript_with_timestamps (per-word objects) can pass the 32 KiB default
		conn = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &sttConn{p: p, conn: conn}, nil
}

// quietClose reports a close that is our own silence or a clean end, not an outage.
func quietClose(err error) bool {
	if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway {
		return true
	}
	// ponytail: the idle-close wording is undocumented; keyword guess.
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		r := strings.ToLower(ce.Reason)
		return strings.Contains(r, "idle") || strings.Contains(r, "inactiv")
	}
	return false
}

// errNothingToCommit: a stream that never got audio is closed, not committed.
var errNothingToCommit = errors.New("elevenlabs: no audio to commit")

// sttConn is one ElevenLabs stream and its turn transcript.
type sttConn struct {
	p    *STTProcessor
	conn *websocket.Conn

	sent       atomic.Bool // audio went out: there is something to commit
	commitSent atomic.Bool // our commit is in flight: the next committed text ends the utterance

	// Recv's task only:
	committed string // committed segments of this turn, space-joined
	lastKind  string // last committed message kind with no partial since (dedup of the two committed forms)
	lastText  string
	done      bool // the committed text answering our commit arrived
}

func (c *sttConn) Send(ctx context.Context, pcm []byte) error {
	if err := c.conn.Write(ctx, websocket.MessageText, c.p.chunk(pcm, false)); err != nil {
		return c.p.mapErr(err, nil)
	}
	c.sent.Store(true)
	return nil
}

// Finish sends the manual commit (an empty audio chunk with commit=true, as
// the official SDKs send it). A stream that never got audio has nothing to commit.
func (c *sttConn) Finish(ctx context.Context) error {
	if !c.sent.Load() {
		return errNothingToCommit
	}
	c.commitSent.Store(true)
	return c.conn.Write(ctx, websocket.MessageText, c.p.chunk(nil, true))
}

func (c *sttConn) Close() { _ = c.conn.CloseNow() }

func (c *sttConn) Recv(ctx context.Context) (sttunit.Result, error) {
	if c.done {
		return sttunit.Result{}, sttunit.ErrStreamEnded
	}
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if quietClose(err) {
				return sttunit.Result{}, sttunit.ErrStreamEnded
			}
			return sttunit.Result{}, c.p.mapErr(err, nil)
		}
		var m sttMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch {
		case m.Type == "partial_transcript" || m.Type == "committed_transcript" || m.Type == "committed_transcript_with_timestamps":
			res, eou := c.transcript(&m)
			c.done = eou // committed after our commit: the turn is done
			return res, nil
		case m.Type == "session_time_limit_exceeded" || m.Type == "insufficient_audio_activity":
			// A very long session, or our own silence: the server closes next.
			return sttunit.Result{}, sttunit.ErrStreamEnded
		case sttErrorKinds[m.Type]:
			return sttunit.Result{}, sttMessageError(m.Type, strings.ReplaceAll(m.Error, c.p.cfg.APIKey, "[REDACTED]"))
		} // session_started, warning, entities, edited_transcript...: nothing to do
	}
}

// transcript turns one transcript message into the cumulative turn text:
// committed segments so far plus this message's text. It reports whether the
// stream is finished (the committed text answering our commit).
func (c *sttConn) transcript(m *sttMessage) (sttunit.Result, bool) {
	seg := strings.TrimSpace(m.Text)
	final := m.Type != "partial_transcript"
	if final && c.lastKind != "" && c.lastKind != m.Type && c.lastText == seg {
		return sttunit.Result{}, false // the same commit sent as plain and as with-timestamps
	}
	text := join(c.committed, seg)
	if final {
		c.committed, c.lastKind, c.lastText = text, m.Type, seg
	} else {
		c.lastKind = ""
	}
	eou := final && c.commitSent.Load()
	if seg == "" && !final {
		return sttunit.Result{}, eou
	}
	return sttunit.Result{Text: text, Final: final, EndOfUtterance: eou}, eou
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}
```

Run `gofmt -w providers/elevenlabs`. Then confirm that `grep -rn ErrProviderUnavailable providers/elevenlabs` returns nothing.

- [ ] **Step 4: Run them to see them pass.**

```bash
cd providers/elevenlabs && gofmt -l . && go vet ./... && go test -race -count=1 ./... && go test -race -count=10 -run 'TestSTT' .
```

Expected: no gofmt output and `ok`. That includes `TestSTT_CloseTearsDownEndsTraceOnceNoLeak` (goroutine count) and `TestSTT_ErrorMessagesBreakerAndRedaction`, whose error count is 1, now counted by the runner.

- [ ] **Step 5: Gate.** Run `make verify`, `make e2e-echo`, and `make offline-test` if the stack is up. All green.

- [ ] **Step 6: Commit.**

```bash
git add providers/elevenlabs/stt.go providers/elevenlabs/stt_test.go
git commit -m "refactor(elevenlabs): STT on the shared sttunit runner

Keeps the dial, the chunk/commit wire format and committed-form dedup; the
dial leaves the run loop (A1) and a mid-utterance close invalidates the turn
(A8).

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2.6: migrate Riva (nvidia)

**Files:**
- Modify: `providers/nvidia/stt.go` (rewritten in full below, 628 → ~290 lines), `providers/nvidia/errors.go` (delete `ErrProviderUnavailable`)
- Test: `providers/nvidia/stt_test.go`

**Interfaces:**
- Consumes: as Task 2.3, plus `context.AfterFunc` (send deadline).
- Produces: `nvidia.STTProcessor` = `{*sttunit.Runner; cfg; conn; client}`. It overrides `Close` so the gRPC channel is closed too. `STTCounts` comes from the runner (the exported `Errors atomic.Uint64` field is deleted; no reader outside the tests). `openOnce` keeps its name and returns `(*sttConn, error)`. Runner `SendTimeout` = `cfg.SendTimeout`, whose default drops from 2 s to 1 s because live sends run on the run loop, which has a 2 s watchdog. No test pins the old value (`grep -rn SendTimeout providers/nvidia server` finds only the config and defaults).
- Deleted from `stt.go`: `type stream` with `owner`, `event`, `endTrace`, `send` and `closeSend`; `errSendClosed`; the fields `session`, `index`, `mu`, `active`, `all`, `closed`, `turn`, `gen` and `Errors`; and the functions `STTCounts`, `Attach`, `detachAllLocked`, `ProcessFrame`, `beginTurn`, `handleAudio`, `ioParent`, `ensureStream`, `receive`, `reportError`, `frameGen`, `finishActive`, `finish`, `stopTurn`, `release` and `stop`. `results` becomes a method of `sttConn`.

- [ ] **Step 1: Write the failing test changes** in `providers/nvidia/stt_test.go`:

(a) After `waitFor` (stt_test.go:144), add the dial shim (there is no `activeCount` in this file):

```go
// ensureStream dials one stream the way the runner's stream task does, then
// closes it (breaker tests).
func (p *STTProcessor) ensureStream(ctx context.Context) error {
	c, err := p.dial(ctx)
	if err == nil {
		c.Close()
	}
	return err
}
```

(b) `TestDial_SSRFGuard` (:304): replace `h.proc.Errors.Load() > 0` with `func() bool { _, _, _, n := h.proc.STTCounts(); return n > 0 }()`. The full line is:

```go
	waitFor(t, "open error", func() bool { _, _, _, n := h.proc.STTCounts(); return n > 0 })
```

(c) `TestSTT_BreakerOpensOnOutageNotOnAuth` (:365): replace `h.proc.finishActive("", 0) // let the next speech open a fresh stream` with:

```go
		_ = h.sess.Post(&core.UserStoppedSpeakingFrame{}) // let the next speech open a fresh stream
```

(d) `TestSTT_TeardownAndRebind` (:396-405): replace `h.proc.beginTurn("turn-1-1", 2) // same turn, newer generation` with:

```go
	_ = h.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-1-1", Gen: 2}) // same turn, newer generation
```

Then delete the owner check `if turn, gen := h.proc.active.owner(); … { t.Fatalf("owner = …") }` (:403-405). The "no reopen" assertion above it stays, and the rebinding itself is pinned by the runner's `beginTurn` together with the Deepgram, Cartesia and ElevenLabs same-turn tests.

(e) `TestSTT_NewTurnFinalizesOldStream` (:430): replace `h.proc.beginTurn("turn-9-9", 9)` with:

```go
	_ = h.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-9-9", Gen: 9})
```

(f) Append:

```go
// A8: the server ends the RPC (OK status, io.EOF) before our CloseSend: the
// turn still owed its transcript, so it is invalidated.
func TestSTT_CleanCloseMidUtteranceInvalidatesTurn(t *testing.T) {
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		_, err := s.Recv() // first audio
		return err
	})
	cfg := f.cfg(cc)
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	h := start(t, cfg)
	h.speak()
	waitFor(t, "invalidating error", func() bool {
		for _, err := range h.errs() {
			if errors.Is(err, core.ErrTranscriptInvalidated) {
				return true
			}
		}
		return false
	})
	if cfg.CircuitBreaker.WouldRefuse() {
		t.Fatal("a clean end is not an outage")
	}
}

// Riva may answer CloseSend with no further result: a final before it already
// committed the text, so the RPC's end is quiet, not a lost turn (settled = Final).
func TestSTT_FinalBeforeCloseSendThenEndIsQuiet(t *testing.T) {
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		if _, err := s.Recv(); err != nil { // first audio
			return err
		}
		_ = s.Send(result("hello", true, 0)) // a final, before any CloseSend: no EndOfUtterance
		for {                                // until the client half-closes; then end with nothing more
			if _, err := s.Recv(); err != nil {
				return nil
			}
		}
	})
	h := start(t, f.cfg(cc))
	h.speak()
	waitFor(t, "final", func() bool { c := h.candidates(); return len(c) == 1 && c[0].Final && !c[0].EndOfUtterance })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the RPC never ended")
	}
	time.Sleep(50 * time.Millisecond)
	for _, err := range h.errs() {
		if errors.Is(err, core.ErrTranscriptInvalidated) {
			t.Fatalf("a committed turn was invalidated: %v", err)
		}
	}
}
```

- [ ] **Step 2: Run them to see them fail.**

```bash
cd providers/nvidia && go test -race -count=1 ./...
```

Expected: FAIL to build with `p.dial undefined`.

- [ ] **Step 3: Implement.** Replace `providers/nvidia/stt.go` with:

```go
package nvidia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/sttunit"
	"govoice/providers/nvidia/rivapb"
)

// STTConfig configures the Riva streaming recognizer
// (nvidia.riva.asr.RivaSpeechRecognition/StreamingRecognize).
type STTConfig struct {
	Endpoint     string // host:port; default DefaultEndpoint (NVCF)
	Insecure     bool   // plaintext gRPC for self-hosted Riva; the API key is then never sent
	APIKey       string
	FunctionID   string // NVCF function-id of the hosted Riva/Parakeet function
	AllowPrivate bool   // permit loopback/private endpoints (self-hosted Riva)
	Language     string // default "en-US"
	Model        string // optional Riva model name; empty = server default
	SampleRate   int    // default 16000; input is 16-bit mono PCM

	RetryPolicy    resilience.Policy // bounds opening a stream: MaxAttempts tries of AttemptTimeout each
	CircuitBreaker *resilience.Breaker
	// DrainTimeout bounds the wait for the final results after the turn ends
	// before the RPC is cancelled.
	DrainTimeout time.Duration
	// SendTimeout bounds one audio Send (HTTP/2 flow control can block it).
	// Live sends run on the session run loop: keep it under the 2 s frame watchdog.
	SendTimeout time.Duration
	Now         func() time.Time
	// Conn is an existing channel (tests); the processor does not close it.
	Conn *grpc.ClientConn
}

// DefaultSTTConfig returns hosted-endpoint defaults. A stream open gets two
// tries of 5 s: it must not hang on a dead channel while a caller waits. A
// send gets 1 s: it runs on the session run loop, whose watchdog is 2 s.
func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: DefaultEndpoint, Language: "en-US", SampleRate: 16000,
		DrainTimeout: 2 * time.Second, SendTimeout: time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond},
	}
}

// STTProcessor implements core.Processor for Riva streaming ASR: one RPC per
// utterance on a per-processor channel, opened off the run loop (the stream
// lifecycle lives in core/sttunit).
type STTProcessor struct {
	*sttunit.Runner
	cfg    STTConfig
	conn   *Conn
	client rivapb.RivaSpeechRecognitionClient
}

var _ core.STTCounters = (*STTProcessor)(nil)

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Language == "" {
		cfg.Language = def.Language
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = def.DrainTimeout
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = def.SendTimeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = def.RetryPolicy
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.DefaultConfig())
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if !cfg.Insecure && cfg.Endpoint == DefaultEndpoint {
		switch {
		case cfg.FunctionID == "":
			return nil, ErrMissingFunctionID
		case cfg.APIKey == "":
			return nil, ErrMissingAPIKey
		}
	}
	conn, err := Dial(ConnConfig{Endpoint: cfg.Endpoint, Insecure: cfg.Insecure, APIKey: cfg.APIKey,
		FunctionID: cfg.FunctionID, AllowPrivate: cfg.AllowPrivate, Conn: cfg.Conn})
	if err != nil {
		return nil, err
	}
	p := &STTProcessor{cfg: cfg, conn: conn, client: rivapb.NewRivaSpeechRecognitionClient(conn.Channel())}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "nvidia", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, SendTimeout: cfg.SendTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm brings the channel to READY before the first utterance (no RPC, so no
// auth and no billing). Skipped while the breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br.WouldRefuse() {
		skipWarm("stt")
		return nil
	}
	return p.conn.Warm(ctx, "stt")
}

// Close tears down every stream and the channel; it is idempotent.
func (p *STTProcessor) Close() error {
	_ = p.Runner.Close()
	return p.conn.Close()
}

func (p *STTProcessor) initRequest() *rivapb.StreamingRecognizeRequest {
	return &rivapb.StreamingRecognizeRequest{StreamingRequest: &rivapb.StreamingRecognizeRequest_StreamingConfig{
		StreamingConfig: &rivapb.StreamingRecognitionConfig{
			Config: &rivapb.RecognitionConfig{
				Encoding:                   rivapb.AudioEncoding_LINEAR_PCM,
				SampleRateHertz:            int32(p.cfg.SampleRate),
				LanguageCode:               p.cfg.Language,
				MaxAlternatives:            1,
				EnableAutomaticPunctuation: true,
				AudioChannelCount:          1,
				Model:                      p.cfg.Model,
			},
			InterimResults: true,
		}}}
}

// openOnce starts the RPC and sends the config message. A failed first Send
// is io.EOF; the real status is on Recv.
func (p *STTProcessor) openOnce(actx context.Context) (*sttConn, error) {
	netpool.CountRequest("nvidia", "stt")
	rpc, err := p.client.StreamingRecognize(p.conn.Context(actx))
	if err != nil {
		return nil, MapError("stt", err)
	}
	if err := rpc.Send(p.initRequest()); err != nil {
		if errors.Is(err, io.EOF) {
			if _, rerr := rpc.Recv(); rerr != nil {
				err = rerr
			}
		}
		return nil, MapError("stt", err)
	}
	return &sttConn{rpc: rpc}, nil
}

// dial opens one RPC on ctx. OpenBounded retries only the open and the breaker
// gets one report per open. Success is recorded when the server first answers
// (sttConn.Recv): a connected channel still rejects with Unavailable per RPC,
// and counting the open as success would reset the breaker on every turn.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	br := p.cfg.CircuitBreaker
	if br.WouldRefuse() {
		return nil, ErrCircuitOpen
	}
	rctx, cancel := context.WithCancel(ctx) // the RPC lives on rctx; Close cancels it
	c, err := resilience.OpenBounded(rctx, p.cfg.RetryPolicy, classify, p.openOnce)
	if err != nil {
		cancel()
		perr := MapError("stt", err)
		br.Report(perr, classify)
		return nil, perr
	}
	c.cancel, c.br = cancel, br
	return c, nil
}

// sttConn is one Riva RPC and its turn transcript.
type sttConn struct {
	rpc     rivapb.RivaSpeechRecognition_StreamingRecognizeClient
	cancel  context.CancelFunc
	br      *resilience.Breaker
	okOnce  sync.Once
	closing atomic.Bool // CloseSend sent: a final now ends the utterance

	committed string // finalized segments of this turn, space-joined (Recv's task only)
}

// Send writes one chunk. A Send still blocked when ctx ends (the runner's
// SendTimeout) cancels the RPC and fails as a timeout: a hung connection is
// an outage.
func (c *sttConn) Send(ctx context.Context, pcm []byte) error {
	var stuck atomic.Bool
	stop := context.AfterFunc(ctx, func() { stuck.Store(true); c.cancel() })
	err := c.rpc.Send(&rivapb.StreamingRecognizeRequest{StreamingRequest: &rivapb.StreamingRecognizeRequest_AudioContent{AudioContent: pcm}})
	stop()
	switch {
	case err != nil && stuck.Load():
		return MapError("stt", fmt.Errorf("%w: audio send blocked past its deadline: %w", resilience.ErrTimeout, err))
	case err == nil, errors.Is(err, io.EOF):
		return nil // io.EOF: the RPC ended; Recv reads the real status and reports it
	}
	return MapError("stt", err)
}

// Finish half-closes the RPC so Riva flushes its final results and ends it.
func (c *sttConn) Finish(context.Context) error {
	c.closing.Store(true)
	return c.rpc.CloseSend()
}

func (c *sttConn) Close() { c.cancel() }

func (c *sttConn) Recv(context.Context) (sttunit.Result, error) {
	for {
		resp, err := c.rpc.Recv()
		if err != nil {
			if streamEnded(err) { // server finished (after our CloseSend, or its own end)
				c.okOnce.Do(c.br.RecordSuccess)
				return sttunit.Result{}, sttunit.ErrStreamEnded
			}
			return sttunit.Result{}, MapError("stt", err)
		}
		c.okOnce.Do(c.br.RecordSuccess)
		if res, ok := c.results(resp); ok {
			return res, nil
		}
	}
}

// results turns one response into the cumulative turn text: the finalized
// segments so far plus this response's interims. Riva interims restate the
// whole current segment, so they replace the previous response's; one response
// may split it into several results (stable head, unstable tail), which are
// joined in order as riva/client/asr.py does. Stability is the tail's lowest.
func (c *sttConn) results(resp *rivapb.StreamingRecognizeResponse) (sttunit.Result, bool) {
	var interim string
	var final, any bool
	var stability float32
	for _, r := range resp.GetResults() {
		alts := r.GetAlternatives()
		if len(alts) == 0 {
			continue
		}
		seg := strings.TrimSpace(alts[0].GetTranscript())
		if seg == "" {
			continue
		}
		any, final = true, r.GetIsFinal()
		if final {
			c.committed, interim, stability = join(c.committed, seg), "", 0
		} else {
			if s := r.GetStability(); interim == "" || s < stability {
				stability = s
			}
			interim = join(interim, seg)
		}
	}
	if !any {
		return sttunit.Result{}, false
	}
	return sttunit.Result{
		Text: join(c.committed, interim), Final: final, EndOfUtterance: final && c.closing.Load(),
		Stability: float64(stability), HasStability: !final && stability > 0,
	}, true
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}
```

In `providers/nvidia/errors.go` delete the line `ErrProviderUnavailable = errors.New("nvidia: provider unavailable")`. Then run `gofmt -w providers/nvidia`.

- [ ] **Step 4: Run them to see them pass.**

```bash
cd providers/nvidia && gofmt -l . && go vet ./... && go test -race -count=1 ./... && go test -race -count=10 -run 'TestSTT_|TestDial_' .
cd ../../server && go test -race -count=1 -run 'Nvidia|STT|Failover' .
```

Expected: no gofmt output and `ok`. `server/nvidia_test.go:51` still calls `(*nvidia.STTProcessor).Close()`, which the override keeps.

- [ ] **Step 5: Gate.** Run `make verify`, `make e2e-echo`, and `make offline-test` if the stack is up. All green.

- [ ] **Step 6: Commit.**

```bash
git add providers/nvidia/stt.go providers/nvidia/errors.go providers/nvidia/stt_test.go
git commit -m "refactor(nvidia): Riva STT on the shared sttunit runner

Keeps the RPC open, config message, send deadline and multi-result
assembly; the open leaves the run loop (A1) and an RPC the server ends
before our CloseSend invalidates the turn (A8).

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2.7: Google and xAI local fixes (A1, A8 for Google; A1, A15 for xAI)

The decision and evidence are in Architecture Decision 5. Google and xAI keep their processors.

**Files:**
- Modify: `providers/google/stt.go`, `providers/xai/stt.go`, `providers/xai/errors.go` (delete `ErrProviderUnavailable`, which has no reader after this task)
- Create: `providers/google/stt_offloop_test.go`, `providers/xai/stt_offloop_test.go`
- Test: `providers/google/stt_onset_test.go` (one edit), `providers/xai/stt_live_findings_test.go` (shim)

**Interfaces:**
- Consumes: `sttunit.DefaultMaxPreRollBytes` (Google), `core.TranscriptInvalidationFrame`, `core.ErrTranscriptInvalidated`.
- Produces: no new exported names. Google gains `activeStream.{pending, pendingBytes, closeOwed, halfClosed, retired, lastText, lastFinal}`, `STTProcessor.dialFailed` and the functions `openAndReceive`, `flushPending`, `openFailed`, `receive` and `invalidateTurn`. xAI gains `STTProcessor.dialFailed` and the functions `ensure`, `open`, `run`, `openFailed` and `receive`, and loses `ensureStream`, `spawnReceiver`, `ioParent` and `activeSTTStream.sendMu`.

Line numbers in this task (`:NNN`) refer to the unmodified files at 2e55347. They help locate code, but each edit names the text it replaces, and the text is the authority, since earlier edits in the same file shift lines.

#### Google

- [ ] **Step 1: Write the failing tests.** Create `providers/google/stt_offloop_test.go`:

```go
package google

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type slowOpener struct {
	delay time.Duration
	st    *mockSTTStream
}

func (o slowOpener) OpenStream(ctx context.Context, _ STTConfig) (STTStream, error) {
	select {
	case <-time.After(o.delay):
		return o.st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type failingOpener struct{ opens atomic.Int32 }

func (o *failingOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.opens.Add(1)
	return nil, errors.New("dial refused")
}

// endingStream is a mock whose provider ends it (io.EOF) when end closes.
type endingStream struct {
	*mockSTTStream
	end chan struct{}
}

func (s endingStream) Recv() ([]*STTResult, error) {
	select {
	case res := <-s.results:
		return res, nil
	case <-s.end:
		return nil, io.EOF
	case <-s.closed:
		return nil, context.Canceled
	}
}

type endingOpener struct{ st endingStream }

func (o endingOpener) OpenStream(context.Context, STTConfig) (STTStream, error) { return o.st, nil }

func startGoogle(t *testing.T, opener STTStreamOpener) (*core.Session, *coretest.PassthroughProcessor) {
	t.Helper()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(40, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sink
}

func gwait(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func countFrames[T core.Frame](sink *coretest.PassthroughProcessor) (out []T) {
	for _, f := range sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// A1: a stream open longer than the 2 s frame watchdog runs off the run loop;
// the audio sent meanwhile reaches the stream in order once it opens.
func TestGoogleSTTOpenRunsOffTheRunLoop(t *testing.T) {
	st := newMockSTTStream()
	session, _ := startGoogle(t, slowOpener{3 * time.Second, st})
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for b := byte(1); b <= 2; b++ {
		_ = session.Post(&core.InputAudioFrame{Samples: bytes.Repeat([]byte{b}, 640), Gen: 1})
	}
	time.Sleep(2500 * time.Millisecond)
	if err := session.Err(); err != nil {
		t.Fatalf("session failed while the open hung: %v", err)
	}
	gwait(t, "held audio sent", 3*time.Second, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.audioChunks) == 2
	})
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.audioChunks[0][0] != 1 || st.audioChunks[1][0] != 2 {
		t.Fatal("held audio sent out of order")
	}
}

func TestGoogleSTTOpenFailureReportsOncePerTurn(t *testing.T) {
	op := &failingOpener{}
	session, sink := startGoogle(t, op)
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for i := 0; i < 3; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}
	gwait(t, "error frame", time.Second, func() bool { return len(countFrames[*core.ErrorFrame](sink)) == 1 })
	gwait(t, "audio passed", time.Second, func() bool { return len(countFrames[*core.InputAudioFrame](sink)) == 3 })
	if n := op.opens.Load(); n != 1 {
		t.Fatalf("opens = %d, want one per turn", n)
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 2, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	gwait(t, "second turn reopens", time.Second, func() bool { return op.opens.Load() == 2 })
	if err := session.Err(); err != nil {
		t.Fatalf("a failed open ended the call: %v", err)
	}
}

// A8: the provider ends the stream (io.EOF) while the turn is bound, not
// half-closed, with words heard: the turn is invalidated, then an error raised.
func TestGoogleSTTProviderEndMidUtteranceInvalidatesTurn(t *testing.T) {
	st := endingStream{newMockSTTStream(), make(chan struct{})}
	session, sink := startGoogle(t, endingOpener{st})
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	st.PushResult(&STTResult{Transcript: "hello"})
	gwait(t, "interim", time.Second, func() bool { return len(countFrames[*core.TranscriptCandidateFrame](sink)) == 1 })
	close(st.end)
	gwait(t, "invalidation and error", time.Second, func() bool {
		return len(countFrames[*core.TranscriptInvalidationFrame](sink)) == 1 && len(countFrames[*core.ErrorFrame](sink)) == 1
	})
	inv := countFrames[*core.TranscriptInvalidationFrame](sink)[0]
	ef := countFrames[*core.ErrorFrame](sink)[0]
	if inv.Turn != "turn-1-1" || !errors.Is(ef.Err, core.ErrTranscriptInvalidated) {
		t.Fatalf("invalidation %+v error %v", inv, ef.Err)
	}
}
```

In `providers/google/stt_onset_test.go`, `TestGoogleSTTNextUtteranceStreamGetsItsOnsetAudio` must stop assuming open order. The two opens now run on separate tasks, so `opener.streams[n-1]` is not necessarily the second utterance's stream. Replace the body of its polling loop's stream lookup (`if n := len(opener.streams); n > 0 { last := …; … }`) with a search for the stream that received chunk 3:

```go
		var got []byte
		opener.mu.Lock()
		for _, s := range opener.streams {
			var chunks []byte
			s.mu.Lock()
			for _, c := range s.audioChunks {
				chunks = append(chunks, c[0])
			}
			s.mu.Unlock()
			if bytes.HasSuffix(chunks, []byte{3}) {
				got = chunks
			}
		}
		opener.mu.Unlock()
```

Keep the rest of the loop (`if bytes.HasSuffix(got, []byte{3}) { … }` and the deadline) as is. If any other Google test indexes `opener.streams` by position and fails intermittently under `-count=10`, apply the same "find by content" change to it and name it in the commit message.

- [ ] **Step 2: Run them to see them fail.**

```bash
cd providers/google && go test -race -count=1 -run 'TestGoogleSTTOpenRunsOffTheRunLoop|TestGoogleSTTOpenFailureReportsOncePerTurn|TestGoogleSTTProviderEndMidUtteranceInvalidatesTurn' .
```

Expected: `OpenRunsOffTheRunLoop` FAILS with `session failed while the open hung: core: processor exceeded MaxFrameProcessing watchdog`. `OpenFailureReportsOncePerTurn` FAILS: the session errors on `stt: dial refused` and opens is 3. `ProviderEndMidUtterance…` FAILS with `timed out waiting for invalidation and error`.

- [ ] **Step 3: Implement** in `providers/google/stt.go`:

(a) Add `"govoice/core/sttunit"` to the imports. Extend `activeStream` (:240) with these fields after `traceFinal sync.Once`. All of them are guarded by `STTProcessor.mu`:

```go
	// While the stream opens (stream == nil) audio waits in pending, oldest
	// dropped past sttunit.DefaultMaxPreRollBytes; a half-close asked for
	// meanwhile is owed and sent after it.
	pending      [][]byte
	pendingBytes int
	closeOwed    bool
	halfClosed   bool   // CloseSend sent: the end of results is expected
	retired      bool   // replaced by rotation: its turn continues on the replacement
	lastText     string // last candidate text emitted from this stream
	lastFinal    bool   // and whether it was final
```

(b) Add to `STTProcessor` (:311) after `turnAudioBytes int64`:

```go
	dialFailed bool // an open failed: no reopen until the next turn or a barge-in
```

and below the type:

```go
var errNotAttached = errors.New("google: stt is not attached to a session")
```

(c) `Close` (:393): read the stream under the lock and guard nil:

```go
func (p *STTProcessor) Close() error {
	p.mu.Lock()
	p.closed = true
	active := p.active
	p.active = nil
	var stream STTStream
	if active != nil {
		stream = active.stream
	}
	p.mu.Unlock()
	if active != nil {
		active.cancel()
		if stream != nil {
			_ = stream.Close()
		}
	}
	if closer, ok := p.cfg.StreamOpener.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	return nil
}
```

(d) `ProcessFrame` `*core.InterruptionFrame` case (:452-473): replace it with the following. It clears `dialFailed` and reads the stream under the lock.

```go
	case *core.InterruptionFrame:
		p.mu.Lock()
		p.dialFailed = false
		var old *activeStream
		var oldStream STTStream
		generation := uint64(frame.Gen)
		if p.session != nil && p.session.CurrentGeneration() > generation {
			generation = p.session.CurrentGeneration()
		}
		if generation > p.streamGen {
			p.streamGen = generation
			p.turnAudioBytes = 0
			p.audioBuffer = p.audioBuffer[:0]
			if p.active != nil && p.active.gen < generation {
				old, oldStream = p.active, p.active.stream
				p.active = nil
			}
		}
		p.mu.Unlock()
		if old != nil {
			old.cancel()
			if oldStream != nil {
				_ = oldStream.Close()
			}
		}
		return e.PushDirection(f, dir)
```

(e) Replace `halfClose` (:483-497) with:

```go
// halfClose ends st's audio. While st still opens, the CloseSend is owed and
// its opener sends it after the held audio.
func (p *STTProcessor) halfClose(st *activeStream) {
	if st == nil {
		return
	}
	p.mu.Lock()
	stream := st.stream
	if stream == nil {
		st.closeOwed = true
	} else {
		st.halfClosed = true
	}
	turn, gen := st.turnID, st.gen
	p.mu.Unlock()
	if stream == nil {
		return
	}
	if err := stream.CloseSend(); err != nil {
		st.cancel()
		if p.session != nil {
			_ = p.session.Reenter(&core.ErrorFrame{
				Err:  MapError("stt", err),
				Turn: core.TurnID(turn),
				Gen:  core.Generation(gen),
			}, core.Downstream, p.index+1, gen)
		}
	}
}
```

(f) In `handleAudio` (:499-579): change the rotation condition to `p.active != nil && p.active.stream != nil && p.cfg.Now().Sub(p.active.createdAt) >= p.cfg.StreamRotationThreshold`. Then replace everything from the second `p.mu.Lock()` (after `ensureStream`) to the end of the function with:

```go
	p.mu.Lock()
	p.audioBuffer = append(p.audioBuffer, f.Samples...)
	p.turnAudioBytes += int64(len(f.Samples))
	if len(p.audioBuffer) > p.overlapBytes {
		overflow := len(p.audioBuffer) - p.overlapBytes
		p.audioBuffer = p.audioBuffer[overflow:]
	}
	st := p.active
	var stream STTStream
	if st != nil {
		stream = st.stream
		if stream == nil { // still opening: hold the audio in order, oldest dropped past the bound
			// ponytail: frames are immutable once posted, so the slice is kept, not copied.
			st.pending = append(st.pending, f.Samples)
			st.pendingBytes += len(f.Samples)
			for st.pendingBytes > sttunit.DefaultMaxPreRollBytes && len(st.pending) > 1 {
				st.pendingBytes -= len(st.pending[0])
				st.pending[0] = nil
				st.pending = st.pending[1:]
			}
		}
	}
	p.mu.Unlock()
	if stream == nil { // held, or this turn's open failed (already reported)
		return e.Push(f)
	}

	// Send audio chunk
	if err := stream.Send(f.Samples); err != nil {
		if errors.Is(err, io.EOF) {
			// gRPC: the stream already ended (the server closed it, or call
			// teardown cancelled it); its status is the receiver's to report.
			// Not a session failure: drop this frame, the next opens a new stream.
			p.mu.Lock()
			if p.active == st {
				p.active = nil
			}
			p.mu.Unlock()
			return e.Push(f)
		}
		p.mu.Lock()
		turn, turnGen := st.turnID, st.gen
		if p.active == st {
			p.active = nil
		}
		p.mu.Unlock()
		st.endTrace(core.TurnID(turn), core.Generation(turnGen), errors.New("provider_failure"))
		st.cancel()
		_ = stream.Close()
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Report(provErr, classify)
		}
		return provErr
	}

	return e.Push(f)
}
```

The old `if p.session == nil { st.cancel() }` branch inside the EOF case is gone, because a stream now always has a session-task receiver.

(g) `ensureStream` (:581): the guard becomes `if p.active != nil || p.closed || p.dialFailed {`.

(h) `setPendingOwnershipLocked` (:940): add `p.dialFailed = false` as its first statement. It is called on `UserStartedSpeakingFrame` and `TurnEventStarted`.

(i) Replace `openPrimaryStream` (:602-664) with the following four functions:

```go
// openPrimaryStream starts a stream for the current turn. The open runs on a
// session task, never on the run loop (A1: a hung open tripped the 2 s frame
// watchdog and ended the call); audio meanwhile waits in st.pending.
func (p *STTProcessor) openPrimaryStream(ctx context.Context) error {
	if p.cfg.StreamOpener == nil {
		return fmt.Errorf("no STTStreamOpener configured")
	}
	if p.session == nil {
		return errNotAttached
	}
	p.mu.Lock()
	turn, gen := p.turnID, p.streamGen
	p.mu.Unlock()
	span, policy := p.startStreamTrace(ctx, turn, gen)
	streamCtx, cancel := context.WithCancel(p.session.Context())
	p.mu.Lock()
	if p.closed || p.active != nil {
		p.mu.Unlock()
		cancel()
		if span != nil {
			span.End(errors.New("canceled"))
		}
		return nil
	}
	p.streamID++
	st := &activeStream{
		cancel: cancel, ctx: streamCtx, id: p.streamID, turnID: p.turnID, gen: p.streamGen,
		traceSpan: span, tracePolicy: policy, traceSession: p.traceSessionID(),
	}
	p.active = st
	p.mu.Unlock()
	if err := p.session.Go(func(sctx context.Context) { p.openAndReceive(sctx, st) }); err != nil {
		p.mu.Lock()
		if p.active == st {
			p.active = nil
		}
		p.mu.Unlock()
		cancel()
		st.endTrace(core.TurnID(turn), core.Generation(gen), errors.New("canceled"))
		if errors.Is(err, core.ErrSessionClosed) {
			return nil
		}
		return err
	}
	return nil
}

// openAndReceive opens st's stream, sends the audio held while it opened, then
// reads results until the stream ends.
func (p *STTProcessor) openAndReceive(ctx context.Context, st *activeStream) {
	stream, err := func() (STTStream, error) {
		defer func() {
			if v := recover(); v != nil {
				st.cancel()
				p.mu.Lock()
				turn, gen := st.turnID, st.gen
				p.mu.Unlock()
				st.endTrace(core.TurnID(turn), core.Generation(gen), errors.New("provider_failure"))
				panic(v)
			}
		}()
		return p.cfg.StreamOpener.OpenStream(st.ctx, p.cfg)
	}()
	if err != nil {
		p.openFailed(st, err)
		return
	}
	if p.cfg.CircuitBreaker != nil {
		p.cfg.CircuitBreaker.RecordSuccess()
	}
	if err := p.flushPending(st, stream); err != nil {
		_ = stream.Close()
		p.openFailed(st, err)
		return
	}
	p.receive(ctx, st)
}

// flushPending sends the audio held while st opened, in order, then attaches
// stream so live audio goes straight to it. Audio arriving during a pass waits
// for the next one, so p.mu is never held across a Send. A half-close asked
// for meanwhile goes after the held audio.
func (p *STTProcessor) flushPending(st *activeStream, stream STTStream) error {
	for {
		p.mu.Lock()
		batch := st.pending
		st.pending, st.pendingBytes = nil, 0
		if len(batch) == 0 {
			st.stream, st.createdAt = stream, p.cfg.Now()
			owed, turn, gen := st.closeOwed, st.turnID, st.gen
			p.mu.Unlock()
			if st.ctx.Err() != nil { // stopped while opening: whoever stopped it saw no stream to close
				_ = stream.Close()
				return nil
			}
			if st.traceSpan != nil {
				st.traceSpan.AddEvent(core.NewTraceEvent("stt.start", time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.tracePolicy, nil))
			}
			if owed {
				p.halfClose(st)
			}
			return nil
		}
		p.mu.Unlock()
		for _, pcm := range batch {
			if err := stream.Send(pcm); err != nil {
				return err
			}
		}
	}
}

// openFailed reports a stream that could not be opened (or died taking its
// held audio): an ErrorFrame for its turn, which an STT failover moves on.
// Nothing reopens until the next turn or a barge-in, so an outage costs one
// error per turn, not one per audio frame.
func (p *STTProcessor) openFailed(st *activeStream, err error) {
	stopped := st.ctx.Err() != nil // torn down (barge-in, hang-up): not a provider failure
	st.cancel()
	p.mu.Lock()
	turn, gen := st.turnID, st.gen
	if p.active == st {
		p.active, p.dialFailed = nil, !stopped
	}
	p.mu.Unlock()
	if stopped {
		st.endTrace(core.TurnID(turn), core.Generation(gen), errors.New("canceled"))
		return
	}
	st.endTrace(core.TurnID(turn), core.Generation(gen), errors.New("provider_failure"))
	provErr := MapError("stt", err)
	if p.cfg.CircuitBreaker != nil {
		p.cfg.CircuitBreaker.Report(provErr, classify)
	}
	p.Errors.Add(1)
	if gen == 0 {
		gen = p.session.CurrentGeneration()
	}
	_ = p.session.Reenter(&core.ErrorFrame{Err: provErr, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index+1, gen)
}
```

(j) In `rotateStream`, in the `p.mu.Lock()` block that installs `newActive` (:724-744), add `oldActive.retired = true` right before `p.active = newActive`.

(k) Split `func (p *STTProcessor) spawnReceiver(st *activeStream)`. Find it by text: the earlier edits in this task shift its line numbers, and on 2e55347 it is at :774-903. Its call `if err := p.session.Go(func(ctx context.Context) {` becomes `if err := p.session.Go(func(ctx context.Context) { p.receive(ctx, st) }); err != nil {`, and the cleanup block after it is unchanged. The text to move runs from the closure's first statement `var traceFailure error` down to and including the closing `}` of the `for {` loop that starts with `select { case <-st.ctx.Done():`. That closing brace is the line just before the closure's own `}); err != nil {`. Move it verbatim into:

```go
// receive reads st's results until the stream ends.
func (p *STTProcessor) receive(ctx context.Context, st *activeStream) {
	// (former closure body, verbatim, with edits (l) and (m))
}
```

(l) Inside `receive`, replace

```go
				if isSTTIdleOrClosed(err) {
					return
				}
```

with

```go
				if isSTTIdleOrClosed(err) {
					// The provider ended the stream. Still bound, not replaced by a
					// rotation, and either not half-closed or with words heard and no
					// final: the turn's audio is lost, so invalidate the turn and raise
					// the error an STT failover replays it on (A8).
					p.mu.Lock()
					turn := st.turnID
					lost := turn != "" && !st.retired && (!st.halfClosed || (st.lastText != "" && !st.lastFinal))
					p.mu.Unlock()
					if lost && turnGen != 0 {
						p.Errors.Add(1)
						traceFailure = errors.New("provider_failure")
						p.invalidateTurn(turn, turnGen, err)
					}
					return
				}
```

(m) Inside `receive`, immediately before the `p.mu.Unlock()` that directly follows the `for _, res := range results {` loop (the one that calls `p.assembleResultLocked(st, res)`), add:

```go
			if latest != nil && assemblyErr == nil {
				st.lastText, st.lastFinal = text, latest.IsFinal
			}
```

(n) Add after `receive`:

```go
// invalidateTurn retires turn's partial transcript and raises the error an
// STT failover replays the turn on (the stream ended before its final, A8).
func (p *STTProcessor) invalidateTurn(turn string, gen uint64, cause error) {
	if err := p.session.Reenter(&core.TranscriptInvalidationFrame{Turn: core.TurnID(turn), Gen: core.Generation(gen), Err: cause}, core.Downstream, p.index+1, gen); err != nil {
		return // the session is closing
	}
	_ = p.session.Reenter(&core.ErrorFrame{
		Err:  fmt.Errorf("%w: %s stream ended mid-utterance: %w", core.ErrTranscriptInvalidated, p.cfg.Provider, MapError("stt", cause)),
		Turn: core.TurnID(turn), Gen: core.Generation(gen),
	}, core.Downstream, p.index+1, gen)
}
```

- [ ] **Step 4: Run the Google tests.**

```bash
cd providers/google && gofmt -l . && go vet ./... && go test -race -count=1 ./... && go test -race -count=10 -run 'TestGoogleSTT|TestSTT_' .
```

Expected: no gofmt output and `ok`, including `TestSTT_AcceleratedFiveRotationsWithoutDuplicates` (rotation stays synchronous) and `TestGoogleSTTSendEOFIsNotSessionFailure`.

#### xAI

- [ ] **Step 5: Write the failing tests.** Create `providers/xai/stt_offloop_test.go`:

```go
//go:build xaispeech

package xai

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type slowXAIOpener struct {
	delay  time.Duration
	stream *mockSTTStream
}

func (o slowXAIOpener) OpenStream(ctx context.Context, _ STTConfig) (STTStream, error) {
	select {
	case <-time.After(o.delay):
		return o.stream, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func startXAI(t *testing.T, opener STTStreamOpener) (*core.Session, *coretest.PassthroughProcessor) {
	t.Helper()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(41, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sink
}

// A1: an open longer than the 2 s frame watchdog runs off the run loop; the
// audio held meanwhile goes out in order after transcript.created.
func TestXAISTTOpenRunsOffTheRunLoop(t *testing.T) {
	ms := &mockSTTStream{messages: []*STTMessage{{Type: "transcript.created"}}}
	session, _ := startXAI(t, slowXAIOpener{3 * time.Second, ms})
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for b := byte(1); b <= 2; b++ {
		_ = session.Post(&core.InputAudioFrame{Samples: bytes.Repeat([]byte{b}, 640), Gen: 1})
	}
	time.Sleep(2500 * time.Millisecond)
	if err := session.Err(); err != nil {
		t.Fatalf("session failed while the open hung: %v", err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		ms.mu.Lock()
		n := len(ms.audioIn)
		ordered := n >= 2 && ms.audioIn[0][0] == 1 && ms.audioIn[1][0] == 2
		ms.mu.Unlock()
		if n >= 2 {
			if !ordered {
				t.Fatal("held audio sent out of order")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("held audio never sent: %d chunks", n)
		}
	}
}

func TestXAISTTOpenFailureReportsOncePerTurn(t *testing.T) {
	var opens atomic.Int32
	session, sink := startXAI(t, &countOpener{n: &opens, err: errors.New("dial refused")})
	errorsSeen := func() (n int) {
		for _, f := range sink.GetFrames() {
			if _, ok := f.(*core.ErrorFrame); ok {
				n++
			}
		}
		return n
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for i := 0; i < 3; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}
	for deadline := time.Now().Add(time.Second); errorsSeen() != 1; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("error frames = %d, want 1", errorsSeen())
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := opens.Load(); n != 1 || errorsSeen() != 1 {
		t.Fatalf("opens = %d, errors = %d: want one of each per turn", n, errorsSeen())
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	for deadline := time.Now().Add(time.Second); opens.Load() != 2; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the next turn did not reopen")
		}
	}
	if err := session.Err(); err != nil {
		t.Fatalf("a failed open ended the call: %v", err)
	}
}
```

In `providers/xai/stt_live_findings_test.go`, add this shim. `TestXAISTT_BreakerGatesDialAndCancelIsNotAFailure` keeps calling `proc.ensureStream` unchanged:

```go
// ensureStream opens one stream the way the processor's open task does (on the
// session's context), then closes it.
func (p *STTProcessor) ensureStream(context.Context) error {
	s, err := p.open(p.session.Context())
	if err == nil {
		_ = s.Close()
	}
	return err
}
```

- [ ] **Step 6: Run them to see them fail.**

```bash
cd providers/xai && go test -race -count=1 -tags xaispeech -run 'TestXAISTTOpenRunsOffTheRunLoop|TestXAISTTOpenFailureReportsOncePerTurn|TestXAISTT_BreakerGates' .
```

Expected: FAIL to build with `method STTProcessor.ensureStream already declared` (the production method still exists) and `p.open undefined`.

- [ ] **Step 7: Implement** in `providers/xai/stt.go`:

(a) In `activeSTTStream` (:202), delete the field `sendMu sync.Mutex`, and change the comment on `stream` to `stream STTStream // nil while the open runs (set once, under mu)`.

(b) Add to `STTProcessor` (:258) after `streamGen uint64`:

```go
	dialFailed bool // an open failed: no reopen until the next turn or a barge-in
```

and below the type:

```go
var errNotAttached = errors.New("xai: stt is not attached to a session")
```

(c) In `ProcessFrame`: in the `*core.UserStartedSpeakingFrame` case add `p.dialFailed = false` right after `p.mu.Lock()`, and replace

```go
		if err := p.ensureStream(ctx); err != nil {
```

with `if err := p.ensure(ctx); err != nil {`. In the `core.TurnEventStarted` branch add `p.dialFailed = false` right after `p.mu.Lock()`. In the `*core.InterruptionFrame` case add `p.dialFailed = false` right after `p.mu.Lock()`.

(d) In `handleAudio` (:447-503): `ensureStream` becomes `ensure`, and `if st == nil { return ErrProviderUnavailable }` becomes:

```go
	if st == nil { // closed, or this turn's open failed (already reported)
		return e.Push(f)
	}
```

The ready check and send become this (no `sendMu`, and the stream is read under `st.mu`):

```go
	st.mu.Lock()
	if !st.isReady {
		// Buffer audio until transcript.created arrives (and while the open runs).
		// Live audio only fills the handshake time; an STT failover replays up to
		// 60 s at once into a new stream, so the bound (3000 chunks = 60 s of
		// 20 ms audio) covers that burst.
		if len(st.audioBuf) < 3000 {
			st.audioBuf = append(st.audioBuf, append([]byte(nil), f.Samples...))
		}
		st.mu.Unlock()
		p.RequestsTotal.Add(1)
		return e.Push(f)
	}
	s := st.stream
	st.mu.Unlock()

	sendCtx, sendCancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
	defer sendCancel()

	err := s.SendAudio(sendCtx, f.Samples)
	if err != nil {
```

The rest of the error handling is unchanged.

(e) Delete `ioParent` (:505-513) and replace `ensureStream` (:515-605) with:

```go
// ensure starts a stream when none is active. The open runs on a session task,
// never on the run loop (A1); audio meanwhile waits in st.audioBuf exactly as
// it waits for transcript.created. After a failed open nothing reopens until
// the next turn or a barge-in.
func (p *STTProcessor) ensure(ctx context.Context) error {
	p.mu.Lock()
	if p.active != nil || p.closed || p.dialFailed {
		p.mu.Unlock()
		return nil
	}
	if p.session == nil {
		p.mu.Unlock()
		return errNotAttached
	}
	turn, gen := p.turnID, p.streamGen
	p.mu.Unlock()

	traceSession := core.SessionID(strconv.FormatUint(p.session.ID(), 10))
	span := core.StartContextSpan(ctx, "stt.stream", traceSession, core.TurnID(turn), core.Generation(gen), map[string]any{"provider": "xai"})
	streamCtx, cancel := context.WithCancel(p.session.Context())
	p.mu.Lock()
	if p.closed || p.active != nil {
		p.mu.Unlock()
		cancel()
		span.End(errors.New("canceled"))
		return nil
	}
	p.streamID++
	st := &activeSTTStream{
		cancel: cancel, ctx: streamCtx, id: p.streamID, readyCh: make(chan struct{}),
		turnID: p.turnID, gen: p.streamGen, audioBuf: make([][]byte, 0, 16),
		traceSpan: span, tracePolicy: core.TraceContextFromContext(ctx).Policy, traceSession: traceSession,
	}
	p.active = st
	p.mu.Unlock()
	if err := p.session.Go(func(sctx context.Context) { p.run(sctx, st) }); err != nil {
		p.releaseStream(st)
		p.stopStream(st)
		st.endTrace(errors.New("canceled"))
		if errors.Is(err, core.ErrSessionClosed) {
			return nil
		}
		return err
	}
	return nil
}

// open dials one stream on ctx. An open breaker stops the dial; WouldRefuse
// does not take the half-open probe, so a cooled-down breaker lets this dial
// through and its one Report/RecordSuccess settles the state.
func (p *STTProcessor) open(ctx context.Context) (STTStream, error) {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		return nil, ErrCircuitOpen
	}
	s, err := p.cfg.StreamOpener.OpenStream(ctx, p.cfg)
	if err != nil {
		cancelled := ctx.Err() != nil // session torn down / caller gave up: not a provider outage
		if p.cfg.CircuitBreaker != nil && !cancelled {
			p.cfg.CircuitBreaker.Report(err, classify)
		}
		return nil, MapError("stt", err)
	}
	if p.cfg.CircuitBreaker != nil {
		p.cfg.CircuitBreaker.RecordSuccess()
	}
	return s, nil
}

// run is a stream's session task: open it, then read it.
func (p *STTProcessor) run(ctx context.Context, st *activeSTTStream) {
	s, err := func() (STTStream, error) {
		defer func() {
			if v := recover(); v != nil {
				st.endTrace(errors.New("provider_failure"))
				panic(v)
			}
		}()
		return p.open(st.ctx)
	}()
	if err != nil {
		p.openFailed(st, err)
		return
	}
	st.mu.Lock()
	st.stream = s
	turn, gen := st.turnID, st.gen
	st.mu.Unlock()
	if st.ctx.Err() != nil { // stopped while opening: stopStream had no stream to close
		_ = s.Close()
		p.releaseStream(st)
		st.endTrace(errors.New("canceled"))
		return
	}
	st.traceSpan.AddEvent(core.NewTraceEvent("stt.start", time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.tracePolicy, nil))
	p.receive(ctx, st)
}

// openFailed reports a stream that could not be opened: an ErrorFrame for its
// turn (what an STT failover moves on). Nothing reopens until the next turn or
// a barge-in, so an outage costs one error per turn, not one per audio frame.
func (p *STTProcessor) openFailed(st *activeSTTStream, err error) {
	stopped := st.ctx.Err() != nil
	p.mu.Lock()
	if p.active == st {
		p.active, p.dialFailed = nil, !stopped
	}
	p.mu.Unlock()
	p.releaseStream(st)
	p.stopStream(st)
	if stopped {
		st.endTrace(errors.New("canceled"))
		return
	}
	st.endTrace(errors.New("provider_failure"))
	p.ErrorsTotal.Add(1)
	st.mu.Lock()
	turn, gen := st.turnID, st.gen
	st.mu.Unlock()
	if gen == 0 {
		gen = p.session.CurrentGeneration()
	}
	_ = p.session.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index+1, gen)
}
```

(f) Replace `func (p *STTProcessor) spawnReceiver(st *activeSTTStream) error`. Find it by text: edits (d)–(e) shift its line numbers, and on 2e55347 it starts at :607. Its body is `if p.session == nil { return nil }` followed by `return p.session.Go(func(ctx context.Context) { … })`. Replace the whole function with a plain method. The method's body is the closure's body, verbatim: from its first statement `var traceFailure error` down to and including the closing `}` of the `for {` loop that starts with `select { case <-st.ctx.Done():`. That brace is the line just before the closure's own `})`. Apply edit (g) inside it:

```go
// receive reads st until the stream ends.
func (p *STTProcessor) receive(ctx context.Context, st *activeSTTStream) {
	// (former closure body)
}
```

(g) Inside `receive`, replace the whole `case "transcript.created":` block (on 2e55347 :676-725; find it by text: from `case "transcript.created":` up to, but not including, `case "transcript.partial":`) with:

```go
			case "transcript.created":
				// Flush the audio held before the handshake in order, then let live
				// audio through. Audio arriving during a pass joins audioBuf and goes
				// in the next one, so no lock is held across a network write (A15).
				var sendErr error
				for sendErr == nil {
					st.mu.Lock()
					buffered := st.audioBuf
					st.audioBuf = nil
					if len(buffered) == 0 {
						st.isReady = true
						select {
						case <-st.readyCh:
						default:
							close(st.readyCh)
						}
						// The turn ended before the handshake finished: audio.done goes
						// after the buffered audio, never before it (M3-02).
						owed := st.pendingDone
						st.pendingDone = false
						st.mu.Unlock()
						if owed {
							if sendErr = st.stream.CloseSend(st.ctx); sendErr == nil {
								p.armDrainTimer(st)
							}
						}
						break
					}
					st.mu.Unlock()
					for _, chunk := range buffered {
						sendCtx, cancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
						sendErr = st.stream.SendAudio(sendCtx, chunk)
						cancel()
						if sendErr != nil {
							break
						}
					}
				}
				if sendErr != nil {
					if st.ctx.Err() != nil {
						traceFailure = errors.New("canceled")
						return
					}
					p.ErrorsTotal.Add(1)
					traceFailure = errors.New("provider_failure")
					providerErr := MapError("stt", sendErr)
					if p.cfg.CircuitBreaker != nil {
						p.cfg.CircuitBreaker.Report(sendErr, classify)
					}
					p.failBoundStream(st, providerErr, "xai buffered audio send failed")
					return
				}
```

(h) `beginDrain` (:864-893): replace the `st.sendMu.Lock()`, `CloseSend` and `st.sendMu.Unlock()` lines and the preceding `st.mu.Unlock()` with the following, which reads the stream under the lock:

```go
	s := st.stream
	st.mu.Unlock()
	err := s.CloseSend(ctx)
```

(i) Replace `stopStream` (:950-959) with:

```go
func (p *STTProcessor) stopStream(st *activeSTTStream) {
	if st == nil {
		return
	}
	p.stopDrainTimer(st)
	st.stopOnce.Do(func() {
		st.cancel()
		st.mu.Lock()
		s := st.stream
		st.mu.Unlock()
		if s != nil {
			_ = s.Close()
		}
	})
}
```

(j) In `providers/xai/errors.go` delete `ErrProviderUnavailable  = errors.New("xai: provider unavailable")`. Confirm with `grep -rn ErrProviderUnavailable providers/xai` that nothing reads it, then run `gofmt -w providers/xai`.

- [ ] **Step 8: Run the xAI tests.**

```bash
cd providers/xai && gofmt -l . && go vet -tags xaispeech ./... && go vet ./... && go test -race -count=1 -tags xaispeech ./... && go test -race -count=10 -tags xaispeech -run 'TestXAISTT' .
```

Expected: no gofmt output and `ok`, in particular `TestXAISTTDoneFollowsBufferedAudioWhenStopBeatsHandshake`, `TestXAISTTKeepsReplayBurstBeforeReady`, `TestXAISTTAudioBufferingBeforeReady`, the `stt_ordering_test.go` suite and `TestXAISTT_BreakerGatesDialAndCancelIsNotAFailure`. If `grep -n sendMu providers/xai/stt.go` prints anything, the task is not done.

- [ ] **Step 9: Gate.**

```bash
cd server && go test -race -count=1 -tags xaispeech -run 'STT|Failover|Echo|Offline' . && cd ..
make verify && make e2e-echo
docker ps --format '{{.Names}}' | grep -E 'offline-(adapter|speaches)-1' && make offline-test
```

Expected: everything green. `make offline-test` is mandatory for this task when the stack is up, because it is the only end-to-end run of the xAI STT processor.

**Residual risk recorded:** Google's 240 s rotation (`rotateStream`) still opens its replacement on the run loop. It fires only for a single utterance longer than `StreamRotationThreshold` (streams are per utterance). If that ever matters, move it to a session task with the same `pending` hold. Do not do it in this phase.

- [ ] **Step 10: Commit** (two commits).

```bash
git add providers/google/stt.go providers/google/stt_offloop_test.go providers/google/stt_onset_test.go
git commit -m "fix(google): open the STT stream off the run loop; invalidate a dropped turn

The stream open runs on a session task with the audio held, bounded, until it
opens (A1); one open per turn after a failure. A stream the provider ends
while its bound turn owes a transcript invalidates the turn so STT failover
replays it (A8). Rotation stays synchronous (one utterance > 240 s).

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
git add providers/xai/stt.go providers/xai/errors.go providers/xai/stt_offloop_test.go providers/xai/stt_live_findings_test.go
git commit -m "fix(xai): open the STT stream off the run loop; no lock across writes

The open runs on a session task and audio waits in the existing pre-ready
buffer (A1); one open per turn after a failure. sendMu is gone: the
transcript.created flush drains until empty before marking the stream ready,
so order holds without a lock across a network write (A15).

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2.8: phase close-out (full gate and adversarial review)

**Files:** none planned. Only reviewer-driven fixes, each with its own test and commit.

**Interfaces:** Consumes everything above. Produces the phase verdict and the load comparison.

- [ ] **Step 1: Flake sweep.**

```bash
cd core && go test -race -count=20 ./sttunit/ && go test -race -count=10 -run 'TestSTTFailover' . && cd ..
for m in deepgram cartesia elevenlabs nvidia google; do (cd providers/$m && go test -race -count=5 ./...) || exit 1; done
cd providers/xai && go test -race -count=5 -tags xaispeech ./... && cd ../..
```

Expected: all `ok`.

- [ ] **Step 2: Full gate, sequentially, on an idle machine.**

```bash
make verify
make e2e-echo
docker ps --format '{{.Names}}' | grep -E 'offline-(adapter|speaches)-1' && make offline-test
make loadtest-smoke 2>&1 | tee "${TMPDIR:-/tmp}/phase2/loadtest-after.txt"
```

Expected: everything green. Compare `stt_final` and `voice_to_voice` in the two loadtest outputs. Pass when the after `stt_final` p95 is at most the before p95 × 1.10 + 20 ms, and the run's own `-max-p95-v2v 1500` check passes. On a regression, profile `providers/google` (the echo path) before blaming the runner, which echo does not use.

- [ ] **Step 3: Dead-code and bloat check.**

```bash
grep -rn "ensureStream\|ioParent\|detachAllLocked\|sendMu" providers/deepgram providers/cartesia providers/elevenlabs providers/nvidia providers/xai --include='*.go' | grep -v _test.go
git diff --stat main...HEAD -- providers core
```

Expected: the grep prints nothing. The diff stat shows a net removal of more than 700 lines in `providers/{deepgram,cartesia,elevenlabs,nvidia}/stt.go`.

- [ ] **Step 4: Adversarial review.** Dispatch an independent reviewer agent (AGENTS.md "Mandatory Code Review Workflow") on `git diff main...HEAD` with this brief:

> Review Phase 2 (core/sttunit plus the six STT adapters) against docs/superpowers/plans/2026-10-16-phase-2-stt-runner.md. Hunt for: (1) goroutine leaks or tasks that outlive Close, including a dial that ignores ctx, a Recv not unblocked by cancel, and a drain timer that outlives stop; (2) any Send, Finish, Recv, Dial or Reenter under Runner.mu or stream.mu, and any stream.mu → Runner.mu nesting; (3) pre-roll ordering holes between flush's final pass and the run loop's first live Send, and Finish sent twice or before held audio; (4) generation fencing on late finals and adoption at the right gen; (5) A8 false positives (invalidation after a settled final, on our own cancel, or on drain timeout) and false negatives; (6) dial storms after failures; (7) YAGNI: anything in sttunit not used by a provider wired in server/registry.go or server/nvidia.go, any write-only field, and whether Streams() should stay; (8) behaviour changes versus main that tests do not pin. Report each finding with file:line and a failing-test idea.

Resolve every finding with a failing test, then the fix, then `make verify`, then a commit of the form `fix(core): …` or `fix(<provider>): …`.

- [ ] **Step 5: Report.** Post in the PR description the load numbers before and after, the line delta, the reviewer's findings and their resolutions, and whether `offline-test` ran. If it did not run because the stack was down, say so plainly. Phase 2 is done when Steps 1–4 are green and the reviewer has no open findings.

---

## Rulings (controller, 2026-10-16; binding)

1. **Keep `Runner.Streams()`.** It is an exported, documented, test-only accessor that provider tests use (`activeCount`) to observe teardown.
2. **Pre-roll overflow is a trace event only** (`stt.preroll_dropped`, once per stream). There is no Prometheus counter.
3. **Pre-roll default is 60 s** (`DefaultMaxPreRollBytes`, exported for Google). The limit is the unexported runner field `maxPreRoll`, and only in-package tests change it. The memory math is in Decision 3: no copy, bounded in practice by the dial budget, ≈ 576 MB theoretical worst case at 300 calls and ≈ 150 MB for a realistic provider-wide hang.
4. **A bound idle-close becomes a lost turn**, but only together with the `settled = Final` rule (Decision 4), so a turn already committed by a `Final` is never invalidated.
5. **A failed dial without failover keeps the call alive.** The client gets an RTVI `provider_error` (non-fatal `ErrorFrame`) and the next turn redials. The bot-idle hang-up bounds a zombie call with a dead STT. `ErrTooManyTasks` follows the same path and is never returned from `ProcessFrame` (Task 2.1 `ensure`).
6. **Deepgram's in-band `"Error"` stays out of the shared breaker.** It is wrapped in the local `errInBand` and classified `NoBreaker`.
7. **`failoverPolicy("stt")` = at most 2 × 1.5 s** (Task 2.2, Steps 7–11), so a hung failover dial reports before `NoTranscriptTimeout` drops the turn. Primaries without a fallback keep their own policies.
8. **Riva `SendTimeout` defaults to 1 s.** Sends run on the run loop, which has a 2 s watchdog.

Known gaps that are not fixed here are listed under Decision 4: a `Finish` write failure after words were heard, and a mid-stream `Recv` failure that is not `ErrStreamEnded`.

## Self-review

- **Spec coverage.** A1: ws/gRPC adapters via Tasks 2.1 and 2.3–2.6; Google and xAI via Task 2.7, with Google rotation recorded as a residual risk. A8: runner via 2.2; adapters inherit it, pinned by per-adapter clean-close tests; Google via 2.7; xAI already correct. D1: 2.3–2.6, deletions listed per task, verified by the 2.8 Step 3 grep and diff stat. A15 (xAI `sendMu`): 2.7 (g)–(i), with Step 8 checking that `sendMu` is gone. B7: 2.3 (`var _ core.STTCounters`, `TestSTT_CountsErrorsForMetrics`). Master Review Focus 1 and 2: Review Focus 1–2 here. Phase gate (loadtest, reviewer focus on goroutine termination, pre-roll bound and fencing): 2.8.
- **Placeholder scan.** Every code step holds the code to write, or an exact replacement anchored by text. `:NNN` line numbers refer to 2e55347 and are hints only. Two steps move existing closure bodies verbatim instead of re-printing ~120 unchanged lines: Google `receive` (2.7 k) and xAI `receive` (2.7 f). Both name the first statement (`var traceFailure error`) and the closing brace of the `for {` loop that starts with `select { case <-st.ctx.Done():` as the text bounds, and list the edits inside. No "similar to Task N": each adapter's code and test edits are printed in full in its own task.
- **Type consistency.** `sttunit.Conn` methods (`Send(ctx, []byte) error`, `Finish(ctx) error`, `Recv(ctx) (Result, error)`, `Close()`) are implemented identically by `dgConn`, cartesia `sttConn`, elevenlabs `sttConn`, nvidia `sttConn` and the test `fakeConn`. `Config` fields used by providers (`Provider`, `Dial`, `Breaker`, `Classify`, `DrainTimeout`, `SendTimeout`, `Now`) exist in Task 2.1, and `Config` has no `MaxPreRollBytes`. Tests set the unexported `Runner.maxPreRoll`. `ensure` returns `*stream` (no error) in Task 2.1, and every caller (`ProcessFrame`'s `UserStartedSpeakingFrame` case, `handleAudio`) uses that form. `dialFailed`, `dropTurn` (2.2), `lost` (2.2) and `invalidate` (2.2) are each defined once, before use in their task. `errInBand` is declared in Deepgram `stt.go` and read by `classify` in `errors.go` (same package). `newFakeConn(t)`, `newGatedConn(t)` and `newHarnessWithBudget` are defined in Task 2.1 and used with those signatures in 2.1 and 2.2. `sttFailoverAttempt` is defined and used only in `server/tts_failover.go`. `ErrStreamEnded` and `DefaultMaxPreRollBytes` names match across tasks. The test shims `ensureStream` and `activeCount` call `p.dial` and `p.Streams()`, which exist after each migration. Riva's `openOnce` returns `*sttConn`, and `TestDial_SSRFGuard` discards the value. The 2.2 edit to `emit` matches the 2.1 code it replaces (`turn, gen := st.owner()` through the unbound `return`).
- **Fixes made during self-review.** (1) The Google flush originally read `st.turnID` outside `p.mu` for the `stt.start` event; it now captures `turn, gen` under the lock. (2) The Google and xAI interruption and `Close` paths read `stream` outside the lock, which raced with the open task's write; both now read it under the lock and guard nil. (3) The runner's `ensure` re-check now returns the current active stream rather than dropping the frame. (4) Moving the Google onset test's "last stream" lookup to "find by content" removes an ordering flake that the off-loop open introduced. (5) The `lastText`/`settled` fields moved from 2.1 to 2.2 so that 2.1 ships no write-only fields.
- **Design-review edits applied (second pass).** All ten are in:
  1. `settled = res.Final`, with `TestRunner_FinalBeforeFinishThenEndIsQuiet` and Riva's `TestSTT_FinalBeforeCloseSendThenEndIsQuiet`.
  2. `failoverPolicy("stt")` 2 × 1.5 s, with its server test, plus Review Focus 1 and the `MaxAsyncTasks` sentence corrected.
  3. `ErrTooManyTasks` is handled like a failed dial, with `TestRunner_TaskLimitLosesTheTurnNotTheCall`.
  4. A flush `Send` failure is a failed dial in 2.1 and invalidates a bound turn in 2.2 via `dropTurn`, which also sets `noDial`.
  5. `dialFailed` stays silent for an unbound stream that is no longer active.
  6. `fakeConn` overlap detection; the flush handoff tests; the A1 test now runs through the session loop with a 500 ms bound; the closing ⇒ not-active comment.
  7. Deepgram `errInBand` and `NoBreaker`, with `TestSTT_InBandErrorStaysOutOfTheBreaker`.
  8. Riva `SendTimeout` 1 s.
  9. `maxPreRoll` is unexported.
  10. Text anchors for the moved closure bodies.
- **Re-check after the second pass.** `TestRunner_FlushSendFailure` (2.1, unbound: a plain error) and `TestRunner_FlushSendFailureLosesBoundTurn` (2.2, bound: invalidation) do not contradict each other: 2.2's flush branch keeps `dialFailed` for unbound streams. `TestRunner_TaskLimitLosesTheTurnNotTheCall` relies on the first turn's dialing stream holding the only task while the second turn's stream is refused. The gate is closed by `defer` before cleanup, so the cleanup's `LeakedTasks` check stays valid. The server budget test times both retry helpers against the real `failoverPolicy` and fails on today's 5 s cap (≈ 10 s). The existing `TestFailoverPolicyOnlyTightens` rows that pinned 5 s are updated in the same step.
