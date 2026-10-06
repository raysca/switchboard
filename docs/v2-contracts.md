# Voice Platform V2 Contracts

**Status:** Phase 0 contract freeze for V2 planning
**Authority:** `docs/v2-spec.md` and `docs/v2-plan.md`
**Scope:** Stable cross-workstream boundaries; implementation may refine internal details without changing these observable contracts.

## 1. Architecture and ownership

```text
Framework (`core`, providers, transports)
        ↓ public APIs
Server (`server`, persistence, management APIs)
        ↓ versioned HTTP management APIs
Admin UI (`ui`)
```

The Framework owns frames, processors, pipelines, sessions, turn identity and lifecycle, supervision, interruption, audio processing, VAD, turn detection, context, tool execution primitives, tracing and observer interfaces, provider integration interfaces, and transport abstractions. The Framework has no dependency on Server, persistence, UI, product authentication, or MCP.

The Server owns persistent agents and immutable versions, provider credentials and capabilities, MCP configuration and discovery, agent configuration, storage, trace/session history, management APIs and authentication, onboarding, provider discovery, agent readiness, and deployment/runtime configuration presentation. Server runtime construction uses the same public Framework APIs available to a standalone Go application.

The UI owns presentation and browser interaction. It calls authenticated, versioned management APIs and uses the normal RTVI connection flow for the embedded voice test. It does not call database repositories or construct a private test-only runtime.

## 2. Framework contracts

### 2.1 Session, turn, and generation identity

`core.SessionID` and `core.TurnID` are distinct opaque defined string types. They are created once at their respective lifecycle boundaries, propagated through frames, trace events, observer events, tool calls, and provider usage events, and are never parsed to infer another value. A `TurnID` identifies one conversational turn; session-local turn ordering is separate metadata. Server-issued UUIDv7 strings may be used as the underlying session and turn values.

`core.Generation` is a monotonically increasing unsigned cancellation fence scoped to a session. A generation may mark work stale and prevent stale frames from being delivered after interruption or turn replacement. It is not an identifier for a session, user turn, assistant turn, trace, or persistent record. Turn identity remains stable when generation changes.

Task 1 publishes these as distinct defined types in `core/identity.go`. Existing V1 session and frame methods still use their historical `uint64`/`string` fields; the P0 turn and frame integration will migrate those runtime paths.

Task 2 adds `TurnLifecycle` as the one-shot start/complete/interruption state primitive. It accepts an opaque `TurnID` and a session-scoped `Generation`; terminal decisions require the expected `TurnID`, so a late completion or interruption cannot end the next turn. Duplicate starts, completions, interruptions, and idle false starts are ignored. Existing V1 frame fields remain in use at the adapter boundary pending the broader frame migration. The generation fence advances once when a flush is committed and stale queued or late assistant text, synthesis units, TTS frames, and output audio are discarded before processor delivery or at the session edge. A partial assistant context event for audio already delivered remains visible, as does explicitly uninterruptible output. Generation never becomes a turn identifier.

`InputMuteFrame` and `InputUnmuteFrame` are session controls. Post, reentry, and processor-emitted mute controls all enter the same ordered control queue. Control admission and audio tagging are serialized: audio admitted while muted is marked for discard before any processor (including VAD/STT). `InputMuted()` reports applied state, updated in control-queue order with `InputMutedStateFrame`; a queued control can therefore change admission policy before its state event is delivered. Unmute keeps the same session and generation. A posted or reentered `FlushFrame` must carry a nonzero target generation; a reentry generation argument must match the frame target. Invalid or conflicting targets are rejected before advancing the fence, and an admitted flush advances it exactly once. Flush and lifecycle controls remain in the V1 control queue; duplicate or stale interruption/flush controls do not advance the current turn, while a committed barrier can propagate its target-generation control. Stale write-completion acknowledgements are discarded because they must not commit old media. The P0 flush barrier here covers core queues and the fake transport buffer; real WebSocket pacing/disposition integration remains Task 8, and this contract makes no claim about audio already buffered by a client.

**Implementation Note (Task 30 — Urgent Control Lane)**

`Session` provides a dedicated priority control lane for urgent lifecycle and barrier controls (`CancelFrame`, `InterruptionFrame`, `FlushFrame`, `EndFrame`, and critical `ErrorFrame`), classified via `core.IsUrgentControl(f)`.
- **Media Congestion Bypass**: Urgent control envelopes jump ahead of queued data/audio frames, delivering immediately to pipeline processors even when the data buffer is saturated.
- **Strict FIFO Control Order**: Multiple urgent controls maintain exact enqueue order among themselves.
- **Starvation Resistance**: The scheduler fair-checks both control and data queues, preventing media starvation while guaranteeing zero-delay control responsiveness.

**Acceptance criteria**

- Compile-time-distinct `SessionID` and `TurnID` values cannot be assigned to one another without an explicit conversion.
- A turn keeps the same `TurnID` across its speech, transcript, model, tool, and output events, including interruption; cancellation advances the generation without synthesizing a new identity.
- Session and turn IDs remain opaque at framework boundaries; no consumer derives identity from generation, sequence number, or a database row number.
- A restart or concurrent session cannot reuse a server durable ID.

### 2.2 Trace and span API

The provider-neutral public API is in `core`:

```go
type Tracer interface {
	StartSpan(ctx context.Context, event TraceEvent) Span
}

type Span interface {
	SetAttribute(key string, value any)
	AddEvent(event TraceEvent)
	End(err error)
}
```

`TraceEvent` carries a stable event name, event time, optional `SessionID`, optional `TurnID`, optional cancellation `Generation`, and structured attributes. The returned span is a no-op span when tracing is disabled; callers need not branch on tracer presence. Span parentage follows the context passed to `StartSpan`. The API does not expose persistence or exporter types.

The initial surface provides `core.NoopTracer`, `core.ContextWithSpan` / `core.SpanFromContext`, and `core.NewTraceEvent`. The constructor copies and filters attributes; `Attributes()` returns a new map. `TraceEvent.ToolCallID` is a scalar correlation field so concurrent calls in one turn remain distinct without admitting payload text. For example, `child := tracer.StartSpan(core.ContextWithSpan(ctx, parent), event)` carries parentage through the context. Task 13 instruments session, turn, frame/processor, tool-loop, interruption, flush, and terminal runtime paths with this vocabulary. Exporter callbacks are isolated behind a bounded asynchronous dispatcher: regular work can be dropped under pressure, but an accepted span retains exactly one terminal operation. Terminal capacity is reserved at admission, so saturation declines new starts rather than retaining unbounded work, and exporter callbacks run outside dispatcher locks. The dispatcher never exposes raw runtime errors; terminals receive only a canonical safe category.

`core.ContextWithTraceContext` carries the session tracer and content policy into asynchronous processor work. `core.StartContextSpan` starts a provider child under the span already in that context. `Session.RootTraceContext()` supplies a stable session parent to external components after session start; the WebSocket/RTVI connection uses it for a session-correlated transport span. A resolved async span remains available as a parent even after its terminal callback, and accepted attributes/events preceding `End` are delivered before the terminal callback. Provider operations emit one first-final or first-audio event per stream/request, then one completion or failure event; VAD jobs emit bounded latency and speech decisions. A `ToolLoop` derived with `WithTraceContext` uses the session dispatcher and parentage without creating another worker.

The standard event vocabulary includes session start/end/failure; turn start/end; speech start/stop and VAD latency; STT start, partial, final, and finalization; context commit; LLM request, first output, completion, and failure; tool requested/start/complete/failure; TTS request, first audio, completion, and failure; interruption and flush; provider readiness/usage/error; transport disconnect; and session failure. Custom processors may add namespaced events and structured metadata.

**Acceptance criteria**

- A disabled tracer has a no-op path and creates no exporter, database, or server dependency.
- Spans correlate by typed session and turn IDs and preserve parent/child timing.
- The standard vocabulary can represent the complete user-speech → STT → context → LLM → tool loop → TTS → output path and interruption ordering.
- Tracing errors cannot fail or delay a voice session.

### 2.3 Trace content policy

`TracePolicy.Content` has exactly `none`, `metadata`, or `text` modes. Production defaults to `metadata`. Raw audio bytes are never included in traces. Metadata mode records event names, timing, identifiers, counts, sample formats, provider/model names, status, and safe error categories, but not transcript text, prompts, tool arguments, tool results, keys, or audio. Text mode permits selected text fields; tool arguments/results still pass through an explicit redaction policy. No mode permits credentials or raw audio.

The Task 1 constructor defaults an unset mode to `metadata`, admits a fixed set of scalar metadata keys, and drops unknown keys and mutable values. An unknown content mode drops all attributes. Namespaced `custom.*` attributes may hold nested `map[string]any` and `[]any` values containing only finite numbers and booleans; they are deeply copied on input and output, limited to eight levels and 64 entries per collection. A shared 1,024-node work budget applies across custom attributes on each copy. Values that fail validation or exhaust the budget are dropped; more than 1,024 custom keys drops all custom metadata. Keys are processed in lexical order for deterministic truncation. This permits structured counters, thresholds, and scores without a text-content path. Text mode admits `transcript`, `prompt`, and `text`; `tool.arguments` and `tool.result` require `RedactToolContent`. Callers supply safe provider/model/status/category labels rather than raw provider errors. The core ToolLoop records redacted arguments only after call validation and intentionally omits tool result payloads from runtime traces until a future explicitly authorized post-normalization design exists.

**Acceptance criteria**

- Tests with unique sentinel secrets/text prove that metadata mode, logs, errors, and event attributes do not contain them.
- Text inclusion requires explicit policy configuration and does not enable raw-audio capture.
- Server retention controls decide whether selected content is persisted; the core policy only controls what the tracer is allowed to receive.

### 2.4 Turn-scoped conversation context

`ConversationContext` is the ordered provider-neutral history boundary. It
stores system, user, assistant, tool-call, and tool-result messages. Inputs and
returned JSON-safe metadata are recursively copied within fixed depth/node
bounds; invalid raw JSON, non-finite numbers, and unsupported mutable values
fail closed before mutation. Tool calls and results retain
call, turn, generation, and result-status correlation; identical replay is
idempotent, conflicting reuse or an unmatched result fails closed. Direct
tool-kind `AddMessage` calls are rejected so this validation cannot be bypassed.
Replay identities survive message-window eviction in a bounded ledger; once the
ledger is full, new identities fail closed rather than weakening the fence.
Only completed call/result identities remain replay-authoritative after history
eviction. A pending call whose message was evicted cannot authorize its first
result, and a result insertion that would evict its own exchange is rolled back
with all eviction accounting unchanged.
Generationless V1 user frames are normalized to the effective session
generation before assistant cancellation matching.
The `core/tools` values are aliases of the
same core contracts carried by `ToolCallFrame` and `ToolResultFrame`, so the
context and later ToolLoop do not require parallel adapter types.

Committed user text enters through `UserContextProcessor`. Assistant text is
committed exactly once only after every synthesis unit in the whole turn has a
successful final write acknowledgement. Cancellation drops undelivered text.
Interruption commits only the contiguous, fully delivered prefix as partial;
late or stale generation work cannot extend it. Tool-call assistant messages do
not by themselves mark a conversational turn complete for eviction.

Budget enforcement preserves system instructions and the active user turn,
evicts only the oldest complete turn, and rolls back additions that cannot fit.
Rollback includes eviction counters and history, so failed mutations leave no
phantom accounting. A context-budget failure during interrupted partial commit
is returned and no assistant commit frame is emitted.

**Implementation Note (Task 28 — Context Compaction Hook)**

`core.ConversationContext` provides an application-extensible, provider-neutral compaction hook boundary via `CompactionHook` / `CompactionHookFunc` and `cc.SetCompactionHook(hook)`.
- **Atomic Execution & Failure Isolation**: `cc.Compact(ctx)` captures an immutable snapshot of messages and invokes the hook. If the hook returns `applied=false` (declining compaction) or returns an error, the context remains completely untouched and immediately usable for subsequent turns.
- **System Instruction Preservation**: Any compaction result must preserve the original system instruction at index 0. Results omitting or mutating system prompt fail with `ErrInvalidCompaction` and roll back.
- **Tool Ledger Retention**: Tool replay ledgers and idempotency tracking are maintained through compaction, ensuring tool call identification and conflict detection remain valid across summarized histories.
- **Budget Bound Enforcement**: The compacted message list is strictly validated against `MaxMessages` and `MaxTokens`. Results exceeding limits fail closed without partial application.

### 2.5 Observer and semantic debug events

`core.Observer` is a non-mutating observation boundary. Its immutable event values describe frame kind, direction, processor identity/index, session/turn/generation correlation, and monotonic timing/lifecycle. Observer APIs do not grant a mutable frame pointer or a callback that can replace, drop, reorder, or inject frames. Callbacks run in event order and must return promptly; observer failures are isolated from session execution and counted/reported through a safe diagnostic hook.

The framework emits protocol-neutral `DebugEvent` values for session/pipeline lifecycle, frame flow, speech/transcription, LLM/TTS lifecycle, interruption/flush, tool call/result, provider readiness/error, and teardown. An adapter maps these semantic values to Whisker-compatible messages. A `Serializer` owns conversion between semantic events/frames and a wire representation; it is not a pipeline processor and does not choose product persistence. RTVI serialization remains its separate client-facing compatibility surface.

Task 1 defines `Observer`, `ObserverEvent`, and `DebugEvent` value surfaces. Their constructors filter and copy attributes, including approved structured numeric/boolean metadata, and their accessors return deep copies. Observer values contain a frame kind rather than a frame pointer. `DebugEvent` has explicit scalar `PipelineID`, `Processor`, `ProcessorIndex`, `FrameKind`, `Direction`, `Sequence`, and `ToolCallID` fields alongside session/turn/generation correlation, so a later adapter can distinguish frame flow and concurrent tool calls without parsing attributes. Delivery, panic isolation, protocol adapters, and general `Serializer` behavior remain assigned to later tasks.

**Acceptance criteria**

- Observer delivery cannot mutate frame flow and an observer panic/error does not fail the session.
- Event order, direction, correlation IDs, processor position, and lifecycle boundaries are testable.
- A semantic event can be serialized for a selected debug protocol without adding Whisker-specific types to core processors.
- RTVI and Whisker conformance results are recorded independently.

### 2.6 Runtime settings updates

Runtime changes enter a session through a typed `RuntimeSettingsUpdate` control message. The update includes a monotonic revision and a typed set of supported values, is validated as a complete unit, and takes effect at a documented control boundary. An invalid update is rejected without partially changing processor state. Arbitrary mutation of processor internals is not public API. Initial updateable values are input mute, interruption enabled, silence/turn timeouts, language, and selected runtime thresholds.

**Acceptance criteria**

- A valid update is observed as one revision and applies atomically.
- An invalid or out-of-order revision leaves the prior settings active and produces a structured error/event.
- Updates take effect without rebuilding a session for the supported fields.

**Implementation Note (Task 27)**

`core.RuntimeSettingsUpdate` implements `FrameSettingsControl`. Upon arrival in the session control queue, it is validated via `update.Validate()`. If invalid, `RuntimeSettingsErrorEvent` with `ErrInvalidRuntimeSettings` is emitted without changing session state. If the revision is less than or equal to current applied revision, `RuntimeSettingsErrorEvent` with `ErrStaleSettingsRevision` is emitted. Valid updates are applied atomically under `s.settingsMu` and `s.RuntimeSettings()` snapshot is updated, followed by emission of `RuntimeSettingsAppliedEvent`. Input mute changes update admission gating immediately without rebuilding the pipeline or session.

### 2.7 Audio filter

`core.AudioFilter` is a provider-neutral streaming abstraction with explicit input/output audio format, bounded per-stream state, cancellation-aware processing, reset, and close. The first implementation must include one real noise-suppression implementation; a noise gate alone does not satisfy this contract. Filter latency is observable. Resampling and channel/format conversion remain composable core audio utilities rather than provider-specific copies.

**Acceptance criteria**

- A filter declares encoding, sample rate, and channels and rejects unsupported input formats explicitly.
- Repeated chunk processing is bounded and session-safe; reset clears all prior-session signal state.
- Cancellation and close release processing resources, and measured filter latency is available to observers/traces.

Task 6 implements this boundary with a stream-owned SpeexDSP 1.2.1
preprocessor for mono PCM16LE at 8, 16, 24, or 48 kHz. It buffers less than
one 10 ms frame, reports 10 ms algorithmic/block latency, and rebuilds native
state on reset. The composable audio utilities use the same vendored,
BSD-licensed SpeexDSP release for quality-5 band-limited resampling and local
deterministic PCM16/float conversion, mono/stereo normalization, pre-roll,
rechunking, and opt-in turn buffering. The resampler accepts only 8, 16, 24,
or 48 kHz, at most 48,000 input samples per call, and caps any per-call output
allocation at 288,064 samples before narrowing lengths for C. Turn buffers use
`core.TurnID`; all buffers are byte-bounded and require an explicit
drop-oldest or reject-newest policy, with reject-newest reported to the caller
without changing retained bytes. Every mutable native/DSP object is per stream
and serialized; no process-global DSP state or model weights are used. Native
and production builds require CGO and a C compiler; the release container
already builds with `CGO_ENABLED=1`. Gate B remains open until the rest of the
Phase 2 audio path and its independent gate pass.

The deterministic denoiser regression uses a stationary-noise learning prefix
followed by voiced, pitch-varying harmonic bursts and pauses. It projects the
output onto the clean wanted reference, divides residual power by the fitted
gain squared, and requires at least 3 dB SNR improvement plus at least 0.5
wanted-signal gain (about -6 dB). Gain normalization makes uniform attenuation
unable to masquerade as suppression; explicit bypass, fixed-attenuation,
silence, and threshold-gate controls must fail the same predicate. This is a
repeatable DSP regression, not a perceptual quality or real-speech corpus
evaluation.

### 2.8 Turn detector and manager

`core/turn` provides the provider-neutral `TurnDetector` interface, `SilenceDetector`, `HybridDetector`, and a session-owned `Manager`. The manager receives caller-supplied `core.TurnID`, `core.Generation`, VAD/activity observations, and explicit timestamps through `Observe` and `Advance`; it does not read wall time, create timers/goroutines, mint IDs, or depend on providers, Server, storage, STT, or transports. A detector is synchronous, deterministic, fast, and side-effect free. It is called outside the manager state lock with an immutable snapshot (including copied, numerically valid semantic evidence); at most one callback is in flight. Concurrent/reentrant evaluations are coalesced into one complete newest snapshot, including its input and epoch. Both callback preflight and terminal commit compare that snapshot with actual current detector state and any newer queued input. Identical inputs reuse either a positive or negative result; otherwise queued snapshots run. No-op mute controls retain eligible detection even when their timestamp is later; rejected observations leave timestamps and detection unchanged. Genuine mute and speech-resume transitions fence the epoch, and unmute queues fresh silence state when a callback owner is still in flight. Each outer evaluation permits at most two plugin callbacks, after which the newest snapshot uses deterministic `SilenceDetector` fallback. Semantic state and epoch fencing prevent stale terminal commits. Pre-callback frames are discarded after an epoch change but retained across a time-only same-epoch advance. Panics and invalid suggestions are ignored in favor of silence fallback. Only silence and semantic completion reasons are accepted; confidence must always be finite, supplied confidence must be in `[0,1]`, semantic completion requires confidence, and absent confidence is normalized to zero for JSON-safe events.

The manager distinguishes initial no-speech timeout, accepted speech start/stop, silence-based user-turn completion, and active-turn maximum duration. Start and stop debounce plus minimum speech duration suppress short false starts; repeated idle snapshots preserve the first stable stop/silence origin and do not re-emit `Stopped`. `SetMuted` pauses turn and timeout advancement; unmute starts fresh silence timing without replaying muted observations. Timestamp regression is rejected. `Interrupt` commits only for the expected active typed turn and generation. A higher observed generation fences a pending candidate or active turn before old turn-ID filtering, including on an idle snapshot. Completed and interrupted turn IDs are retired within their generation: positive `Config.MaxRetiredTurns` (default 128) bounds this ledger, and when full the manager rejects a new same-generation speaking turn with `ErrRetiredTurnLimit` rather than evicting IDs and risking a stale restart. A generation advance or `Reset` clears the ledger; generation is not used as a substitute for turn identity. Terminal decisions are one-shot through `core.TurnLifecycle` and are returned as ordinary `core.TurnEventFrame` values for the caller to route. Semantic evidence is a provider-neutral confidence value; it can complete early only for the matching turn/generation, a timestamp within the current turn and configured freshness window, confidence above threshold, and an elapsed silence guard. Missing, invalid, stale, or low-confidence evidence falls back to silence.

This state machine does not commit transcripts or context, advance the session cancellation fence, flush output, apply runtime policy updates, or terminate a session. Those actions remain with their owning tasks and integration. The manager owns only bounded per-session state, and its frames have no implicit process or session side effect.

**Acceptance criteria**

- Timestamped start, completion, false-start recovery, no-speech/active-duration timeout, mute, and interruption transitions are deterministic and include typed identity and a reason where applicable.
- A completion or interruption is one-shot; old-turn and old-generation observations cannot alter the current turn.
- Semantic evidence can only shorten the silence path under its identity, timestamp, confidence, freshness, and guard checks; every rejected signal preserves deterministic silence fallback.
- STT/context agreement, output cancellation, and generation-fence integration remain Gate B acceptance items and are not claimed by the state-machine package alone.

### 2.9 Output pacing, speaking state, and flush

`transport/ws.PacedWriter` is the single pacing engine. Enqueue owns a copy of
each frame and its samples, records one normalized media duration for admission
and removal, and rejects frame/byte/duration overflow without changing retained
accounting or consuming a pending final marker. The default residual server
queue remains 100 ms / 4,800 bytes / 64 frames for 24-kHz PCM16 mono;
non-positive budgets receive bounded defaults. Pacing, encoding, writes, and
callbacks occur without the queue lock. Close and cancellation wake pacing and
idle waits, clear accounting, and cancel a write only for terminal close or an
explicitly abortable writer.

`core.SpeakingTracker` and `SpeakingStateFrame` provide typed provider-neutral
started/stopped transitions. A successful non-empty transport write starts
speaking. A true whole-turn paced drain, flush, fatal write failure, or close
stops it once. `TTSStarted`, enqueue, empty/error/stale frames, and synthesis
unit boundaries do not start or flicker speaking. RTVI maps these same typed
transitions to `bot-started-speaking` and `bot-stopped-speaking`; neither event
acknowledges client playback.

`OutputAudioFrame.IsFinal` remains synthesis-unit finality. `TurnFinal` is
whole-turn finality. Providers and the deterministic fake propagate known
`SynthesisUnitFrame.IsFinal`; an ordered `OutputTurnFinalFrame` covers a last
unit that was streamed before LLM completion. xAI preserves this boundary with
an empty final frame when a final unit has no audio. The pacer reconciles a
marker before admission, while queued, selected, or writing, and immediately
after successful completion. Late reconciliation schedules an abortable drain
at the recorded media end and does not block the transport processor. Empty
unit-finals enter the same completed lifecycle. It bounds pending/completed
marker state to 64 and consumes a pending marker only after successful
admission. Finality-producing adapters must use nonzero session-monotonic unit
IDs; core supplies them globally across turns. A monotonic terminal high-water
mark makes replayed IDs permanent no-ops without unbounded retained state, and
nonmonotonic final identities fail closed. A bounded active-unit ledger permits
later chunks of an already admitted uninterruptible unit even after a future
marker advances retirement; interruptible units remain fenced. Retained
uninterruptible identities are reconciled before stale-generation rejection,
and an older completed marker never replaces a newer drain. FakeTTS serializes
synthesis units in admission order and cancels both active and queued work.

Flush drops queued and selected/pacing interruptible output through the target
generation. Final validation and transport-write registration are atomic with
the flush fence. A transport-bound real WebSocket writer wrapper is not canceled
because canceling an actual in-progress coder/websocket write closes the
connection; flush waits for the wrapper's return before acknowledging. The
integration holds that wrapper only after coder `Write` returns, proving barrier
waiting and same-connection reuse rather than in-progress coder cancellation.
Explicitly recoverable writers may opt into cancellation.
Repeated/overlapping calls keep generation-specific callbacks; late write
returns cannot emit completion or revive speaking, and stale/no-op flushes do
not reset the retained media clock. All waiters observe the same stored terminal
failure when one timeout closes the pacer, so none can acknowledge success. Explicitly
uninterruptible queued, in-flight, or final-drain media survives in relative order even when
older than the active fence, and new-generation output resumes normally. The
separately tested bounded slow-writer path closes the pacer before returning its
terminal error instead of falsely acknowledging. Already written client audio remains outside
this guarantee.

Detailed concurrency and ordering rules are in
`docs/output-pacing-and-flush.md`.

### 2.10 Transcript stabilization and commit

Providers emit `core.TranscriptCandidateFrame`; they never emit a committed
`FinalTranscriptionFrame`. `core/transcript.Finalizer` owns interim filtering,
duplicate suppression, grace/timeout selection, interruption and generation
fencing, and the one committed result for each `(TurnID, Generation)`. A
committed final is explicitly marked, and the V2 `UserContextProcessor` path
accepts only marked finals. Direct unmarked finals remain an opt-in-compatible
V1 boundary and retain the historical generated-turn/timestamp fallback.

Policy ordering and deadlines use an injected monotonic receipt clock owned by
the transcript processor; provider/media timestamps remain separate output and
diagnostic metadata and may arrive out of order. The finalizer itself does not
read wall time, start a timer or goroutine, or invoke application/provider
callbacks. One session-owned deadline driver wakes for the earliest pending
terminal deadline and re-enters the serialized pipeline; it is not a
per-candidate goroutine. Text must be
non-empty valid UTF-8, is copied after trimming, and is limited to 32 KiB by
default. Stability must be finite and in `[0,1]`. Active plus retired identities
are bounded (128 by default); capacity fails closed rather than evicting a
retired identity that could replay. A newer generation clears bounded state and
permanently fences older-generation input.

Google `IsFinal` is retained as a provider-final hypothesis for one audio
portion; Google `Stability` is admitted only on non-final hypotheses, matching
the generated Speech V2 contract. Every result in a response is consumed.
The finalizer retains provider-final text as a settled prefix while a later
whole-turn interim may extend or correct only the tail after that prefix.
Utterance-final text still outranks any subsequent interim.
Result-end offsets order sequential final segments, and text/offset overlap
reconciles audio replay at stream rotation; nonoverlapping segments are appended
faithfully even when words repeat, while text overlap is applied only to proven
overlapping media intervals, including interim results after rotation. Rotation media origins and overlap are per turn,
not session-global. Assembly identities and segments are bounded; a newer
generation reclaims fenced state and active-capacity exhaustion fails explicitly
instead of returning an incomplete transcript. A provider that exhausts these
bounds emits a correlated `TranscriptInvalidationFrame` for the affected
`(TurnID, Generation)` before its observable error. The transcript processor
retires that identity without committing its partial prefix; unrelated turns
continue. Terminal invalidation uses the ordered data lane and waits for bounded
admission; if the queue cannot drain, the session fails closed. The separate
observable `ErrorFrame` retains `ErrTranscriptInvalidated` and the correlated
`Turn`/`Gen` fields in the same lane and uses the same bounded admission rule. An uncorrelated sentinel `ErrorFrame`
remains a fail-all compatibility fallback for legacy providers. Each Google response
produces one complete raw snapshot after all consecutive result portions are
assembled. Interim output includes the committed segment prefix. Rotation
replays only audio sent before the triggering chunk, so that chunk is not
replayed and sent twice. xAI `is_final` is provider-final but does not close an
utterance; `speech_final` and `transcript.done` supply the stronger utterance
boundary, with nonempty done text taking precedence over the prior partial.
Streams retain the turn/generation bound when opened, so a draining old stream
cannot be relabeled by a later speech start. The interruption control boundary
updates the pending generation for future xAI streams. xAI serializes provider
writes separately from lifecycle state, so cancel can unblock context-aware I/O,
and closes a receiver's socket before removing its owner. The common policy, not either
adapter, decides commitment. Production server and demo assembly keep nonzero
grace and timeout defaults because the lifecycle driver supplies due advances.
Typed xAI completion sends the matching stream's audio-done signal and starts a
bounded drain; typed interruption cancels only that immutable owner. Starting a
new V1 or typed turn moves the prior immutable owner to a bounded draining set,
allowing its late final until completion, interruption, deadline, session close,
or bounded eviction. A failed supervised receiver start is returned to the
pipeline. An upstream barge-in `InterruptionFrame` reconciles the transcript
processor's pending generation while preserving the active speech-owned turn,
so the matching generation's stop can complete the candidate; late candidates
still cannot steal that identity.

**Acceptance criteria**

- Stable interims release once; unstable and duplicate interims remain raw.
- A better candidate arriving before the grace deadline wins, while timeout can
  commit the best bounded interim when no provider final arrives.
- Completion, interruption, stale generations, duplicate finals, and
  concurrent replay cannot produce more than one committed final/context row.
- Same-generation turns remain distinct even when their text is identical.
- Replaying one marked commit cannot append context twice; the identity ledger
  is bounded and fails closed at capacity.

### 2.11 Provider readiness and usage

Providers report typed readiness transitions and optional provider-neutral usage events. Readiness includes provider/component, state, timestamp, and safe reason category. Usage includes provider/model, session/turn IDs when available, unit name, quantity, and timestamp (for example LLM tokens, STT audio duration, TTS characters/audio duration, tool calls, or request count). These are observability values, not billing or admission decisions.

**Acceptance criteria**

- Missing provider metrics are represented as absent, never fabricated as zero usage.
- Readiness and usage can reach trace/observer consumers without a database dependency.
- Provider errors and secret-bearing response text are redacted before event emission.

### 2.12 Provider-neutral tools and ToolLoop

Core defines `ToolDefinition` (stable name, description, JSON input schema, and policy metadata), `ToolCall` (call ID, name, raw JSON arguments, session/turn/generation correlation), and `ToolResult` (call ID, structured result or safe error, status, and timing). A provider adapter translates provider-native requests/results to these types. `ToolExecutor` invokes registered handlers using a `context.Context`; an MCP wrapper is one Server-supplied handler and is invisible to core.

The Task 1 signatures live in `govoice/core/tools`: `ToolExecutor.Execute(context.Context, ToolCall) (ToolResult, error)` and the matching `ToolExecutorFunc` adapter. JSON schema, arguments, and results use `json.RawMessage`; callers own those byte slices. `ToolResult.ErrorCategory` is a safe category, not provider error text. Applications supply tool definitions, an executor, and a bounded `ToolLoopConfig`; core does not register or construct application handlers.

`ToolLoop` owns sequential model/tool rounds, multiple calls returned in one round, maximum rounds, maximum total calls and payload sizes, per-call and per-round context deadlines, context cancellation, result normalization, generation-fence callbacks, exact call replay, and tool-call trace correlation through the existing trace values. It commits ordered call/results to `ConversationContext` before asking a provider adapter to continue. Malformed arguments, unknown names, executor errors/panics, timeouts, cancellation, and stale generations produce bounded safe results; malformed or conflicting identities and exhausted policy limits return typed errors. Tool execution is synchronous and handlers must honor the supplied context; core does not spawn detached per-call goroutines to try to kill handlers that ignore cancellation. The bounded replay ledger and exchange insertion/rollback rules belong to `ConversationContext`. `ToolLoop` does not own session lifecycle, pipeline execution, provider SDKs, persistence, MCP, or a provider-specific continuation protocol.

Completed replay is checked against the current loop's result byte/depth bounds. If stricter current bounds reject the cached result, the loop returns `ErrReplayResultOutOfBounds`, does not execute again or invoke continuation, and leaves committed context unchanged. Trace spans end with nil on success and safe normalized categories on failure; raw executor errors and handler-provided categories never reach tracing.

**Acceptance criteria**

- Google, xAI, and server-supplied handlers share the same framework tool types.
- Multiple calls, malformed arguments, unknown names, provider errors, timeout, cancellation, interruption, and iteration exhaustion have deterministic results.
- Tool call/result events correlate to the originating turn; content follows trace policy.
- A tool handler cannot keep a session alive past cancellation or its configured timeout.

Generated safe ToolLoop errors obey the configured result-byte cap, which must
be at least 30 bytes. Before provider adapters project calls through
ContextAggregator, they must invoke the ToolLoop atomic claim or explicitly
distinguish a previously recorded call from a new claim.

Google and xAI adapters compose their session-generation fence with any
application `IsCurrent` predicate through an immutable derived ToolLoop. Google
retains complete response-occurrence boundaries and provider-native signed
parts in their original order; xAI retains native `function_call` and
`function_call_output` items across stateless continuation and later context
reconstruction. Provider streaming may assemble native argument fragments, but
argument/reasoning deltas are never assistant text. Both adapters preserve
arbitrary valid JSON results (including integers beyond IEEE-754 exact range),
map failure/cancellation with safe categories, and defer result-size policy to
the configured ToolLoop. Nested JSON-Schema and argument conformance beyond
the documented root/depth/size checks remain application/executor policy.

## 3. Server concepts and ownership

Server owns the following durable domain records. All durable IDs are UUIDv7 strings; wire and repository APIs treat IDs as opaque. Framework `SessionID`/`TurnID` types may carry the corresponding durable value but remain framework types.

| Concept | Owner and frozen meaning |
|---|---|
| `Agent` | Mutable current product resource: identity, name/description, enabled/archive state, and pointer to current version. The API never uses the framework `Pipeline` as its persistence model. |
| `AgentVersion` | Immutable validated configuration snapshot. Stores schema version, normalized config, creation time, and lower-case SHA-256 `config_hash`. Hash excludes mutable timestamps and its own hash field. Existing sessions continue using the snapshot they started with. |
| `ProviderCredential` | Server-only secret material encrypted at rest with a key supplied by deployment configuration. API responses contain ID, provider, masked suffix, validation state/time, and capabilities; never secret bytes. |
| `ProviderCapability` | Registry-derived provider/model/voice/language capability metadata and validation result. Credential strings are not inspected to guess capabilities. |
| `MCPServer` | Mutable Server resource with endpoint/transport/auth reference, enable state, validation state, and latest immutable discovery reference. OAuth/token material remains server-side. |
| `MCPToolDiscovery` | Immutable discovery snapshot. A canonical, name-sorted representation gets a SHA-256 `discovery_hash`; each tool schema gets its own canonical SHA-256 `schema_hash`. A refresh creates a new snapshot and never rewrites history. |
| `AgentMCPTool` | Version-scoped association keyed by agent version, MCP server ID, and remote tool name. Stores selected schema hash and required/optional policy. Schema drift or disappearance marks current configuration as needing review; historical snapshots remain readable. |
| `SessionRecord` | Persistent product session metadata: UUIDv7 ID, agent/version/config hash, start/end, status, provider stack, turn count, and safe summary. Stores the immutable runtime snapshot reference needed to explain which configuration ran. |
| `TurnRecord` | Optional history row keyed by UUIDv7 and Session ID, with timing, final transcript/assistant text only when policy permits, latencies, interruption status, and tools invoked. |
| `Trace`, `Span`, `Event` | Server adapter records framework correlation and nested time relationships. Events carry typed name, safe JSON metadata, and parent/session/turn IDs. Raw audio is excluded. Content persistence is independently governed by retention/privacy settings. |
| `ToolCallRecord` | Server trace/history projection with tool/MCP identity, timing/status, schema hash, and redacted argument/result summaries. It survives live MCP configuration deletion. |

Agent config is canonicalized as a JSON object and hashed with lower-case SHA-256; timestamps and the hash field are not part of the hash input. Version numbers are contiguous per agent and published versions cannot be changed or deleted. Creation plus version 1 publication, and each metadata update plus optional config publication, use one transaction. Updates serialize per agent and allocate the next version under the SQLite immediate transaction or PostgreSQL row lock. Updating only identity/description/enabled state does not create a version; replacing config publishes a new version. Session creation resolves the current enabled, non-archived agent once and persists that version ID and hash. Session lookup resolves by the stored immutable ID and verifies the hash, so changing the current agent pointer cannot alter an existing session. The initial readiness calculation reports bounded safe codes for missing STT/LLM/TTS config, disabled state, or archived state. A `configured` state only describes config presence; the host must inject an external readiness evaluator before the service can report `ready` or block runtime resolution on provider/MCP dependency failure. V1 pipeline import copies `stt`, `llm`, `tts`, `max_messages`, and `max_tokens`; the legacy pipeline key is not included in runtime config or its hash.

`agent.Service` owns product validation and snapshot resolution over `storage.AgentRepository`. `api.NewAgentsHandler` exposes the authenticated management route surface by dependency injection; the management composition layer supplies the persistent Store and mounts it after bearer authentication. The existing V1 server configuration does not implicitly create or open a product database.

Normal MCP deletion is blocked while current agent versions or active captured sessions reference the server. Disable is reversible and removes tools from new session snapshots. Explicit detach-and-delete publishes new current versions without the server's selections, stops new references, lets active sessions drain, then removes the live configuration; historical version, discovery, session, and tool-call evidence is retained. No database cascade removes agent/session/trace history.

Readiness reports `needs_tool_review` for selected schema drift/disappearance, `tool_dependency_unavailable` for a required disabled/unavailable server, and `ready_with_tool_warning` when an optional tool cannot be assigned. `/api/v1/agents/{id}/mcp-tools` accepts a replacement `tools` array; an explicit empty array removes all current selections. `server/runtime.StartSession` resolves readiness and captures wrappers before pinning a session. Framework definitions/execution use only `core.ToolDefinition` and `core.ToolExecutor`; MCP types stay in `server/mcp`. Captured handles pin endpoint, schema, and an in-memory credential lease. Persisted runtime metadata includes no plaintext credential. `session_mcp_tools` records captured wrappers for drain checks. Credential rotation/revocation affects new sessions; end active sessions when immediate revocation is required.

Detaching a referenced MCP server sets a durable review requirement on the replacement agent version. Config-only updates and agent duplication preserve that requirement; duplication does not count as tool review. New sessions remain blocked until an explicit MCP tool selection update, including an explicit empty selection, publishes a reviewed version.

### Storage boundary

The Server uses `database/sql`, `modernc.org/sqlite` for the CGO-free single-node SQLite driver, and `github.com/jackc/pgx/v5/stdlib` for PostgreSQL. Dialect-specific migrations are embedded in the Server and versioned. Thin repositories own SQL and transaction boundaries; product services own validation and domain rules. SQLite and PostgreSQL migrations implement the same repository contract. The Framework has no SQL driver or repository package.

Task 3's storage package uses Goose Provider v3.28.0 for ordered embedded SQL migrations and version bookkeeping. PostgreSQL uses Goose's session advisory lock; SQLite uses a sibling file lock during migration and one connection with `foreign_keys=ON`, WAL, and a busy timeout. Repositories persist opaque UUIDv7 IDs and UTC timestamps. Agent version config and MCP tool schemas use sorted canonical JSON and lower-case SHA-256; discovery hashes the name-sorted `(remote_name, schema_hash)` pairs. The immutable snapshot rows reject updates at the database layer. Historical MCP discovery and associations retain the original server ID as an opaque value after explicit detach-and-delete. Storage operations, backup expectations, and the optional PostgreSQL test DSN are in `docs/operations/storage.md`.

After Task 3 review, SQLite connection settings are applied through the driver DSN on every physical connection. Repository-issued durable IDs are validated as UUIDv7, and timestamps use UTC microsecond precision. PostgreSQL preserves canonical JSON bytes in `TEXT` columns rather than rewriting them through JSONB. Agent versions and selected MCP tools publish atomically; a selected tool must match the live server's latest discovery and schema hash. Database triggers enforce current-version/latest-discovery pointer ownership and session agent/version/hash provenance. Published selections cannot be appended later.

Storage rejects caller SQLite DSN aliases and `_pragma` that could override its mandatory connection policy. File URI migration locks use decoded, normalized paths; in-memory databases do not create lock files. UUIDv7 validation also requires the RFC 4122 variant bits.

### Management API and authentication boundary

Management routes begin at `/api/v1`; success and error bodies use versioned JSON DTOs independent of database structs and framework frames. Management UI/API authentication is distinct from public voice-client admission. Voice clients receive short-lived, scoped, single-use connection tokens and never receive management credentials or provider secrets. The Server configuration endpoint is read-only. Provider secret create/update requests accept a secret once; reads return only masked metadata.

Task 4 freezes the initial wire boundary in `docs/api/v1.md`: the admin-only management API uses a deployment environment key, hashed at startup and compared in constant time; without it, `/api/v1` is unavailable. A separate POST issues at most 60-second `voice:connect` JWTs under the existing voice signing configuration. The WebSocket validator rejects foreign explicit scopes and consumes `jti` once, but accepts historical unscoped V1 admission JWTs during migration. This compatibility exception is limited to public voice admission and does not grant management access. Read-only server configuration is a sanitized projection, never the raw deployment config. The cost of the bootstrap bearer key is deployment-side key distribution/rotation; adding user sessions or RBAC later changes management authentication while keeping the `/api/v1` resource DTO boundary. Retiring unscoped voice JWTs later requires client/token-issuer coordination.

Task 4 review round 1: scoped tokens now carry a validator-generated random issuance epoch, so a replacement validator rejects prior scoped tokens despite clock skew and an empty replay cache. Issuance and admission must be pinned to one replica until a shared replay/issuance service exists. The effective production `kid` is shared between signer and validator; durations use whole-second JWT precision with a configured minimum of two seconds. API body limits count all bytes and noncanonical version paths return the JSON error envelope. The cost is loss of cross-replica portability for API-issued scoped tokens; historical unscoped V1 tokens retain their earlier compatibility behavior.

### UI information architecture

The separate `ui/` product uses React, TypeScript, and Vite. Its primary sections are Dashboard, Agents, Providers, MCP Servers, Sessions, and read-only Server Configuration. Agent detail combines configuration, readiness, selected MCP tools, and the official Pipecat client/UI test panel on the production RTVI path. Session detail displays persisted server traces. A generated or strongly typed API client consumes only `/api/v1` DTOs.

**Acceptance criteria**

- A changed agent creates a new immutable version; an already-running session still reports the original config hash.
- A discovered tool schema change creates a new immutable discovery hash and does not silently alter old agent/session snapshots.
- Database and API deletion paths cannot cascade away historical session, tool-call, or trace evidence.
- Provider and MCP secrets never appear in browser reads, logs, traces, or error messages.
- UI functionality is reachable through the management API and no UI imports Server repositories or core implementation internals.
- SQLite and PostgreSQL pass the same repository contract suite and migrations can be applied on an empty database and upgraded from the prior schema.

## 4. Dependency gates A–G

| Gate | Required evidence to pass |
|---|---|
| A — Core Contracts Stable | Typed session/turn identity, trace model, provider-neutral tool model, and observer/debug model are stable; public contract tests pass. Required before deep provider/MCP/tool work. |
| B — P0 Audio Path Stable | Turn management, STT commit, output pacing, deterministic flush/barge-in, silence/no-speech, and audio normalization pass focused and integrated tests. Required before parity results are treated as meaningful. |
| C — Tool Loop Stable | Provider-neutral multi-step loop, Google adapter, and xAI adapter pass the same call/result, limit, timeout, cancellation, and error contracts. Required before MCP end-to-end integration. |
| D — Persistent Agent Runtime | Immutable agent snapshot, credential resolution, provider selection, and MCP tool wiring work at session construction. Required before the UI test surface is authoritative. |
| E — Trace End-to-End | Framework span → Server persistence → management API → UI timeline is correlated for a real session and remains useful under metadata-only privacy mode. |
| F — Compatibility | RTVI conformance, Whisker semantic conformance, and P0 Pipecat behavioral parity each pass their separate suites; intentional differences are documented. |
| G — Product Release | Onboarding, persistent agents, Pipecat UI test, MCP tool flow, session traces, security, and production packaging pass the seven vertical scenarios and release checklist. |

## 5. Rulings and assumptions

- Ruling: Keep `SessionID`, `TurnID`, and `Generation` as distinct framework types; use UUIDv7 strings for server durable IDs — opaque IDs simplify correlation while generation remains only a cancellation fence — cost if wrong: changing identity plumbing after tracing and provider work lands will require broad API and stored-data migrations.
- Ruling: Hash normalized immutable `AgentVersion` JSON and per-tool canonical schema JSON with SHA-256; keep the hash outside its own input — stable hashes make session provenance and MCP schema drift comparable — cost if wrong: a different canonicalization contract changes all snapshot hashes and needs explicit schema-version migration.
- Ruling: Use `database/sql`, `modernc.org/sqlite`, `pgx/v5/stdlib`, embedded dialect migrations, and thin repositories — this supports CGO-free single-node deployment and PostgreSQL without putting SQL in core — cost if wrong: changing driver or migration structure later touches every repository and deployment build.
- Ruling: Default trace content to metadata-only; raw audio is never traced and text/tool payloads require explicit policy — voice data is sensitive and timing remains useful without content — cost if wrong: stricter defaults may frustrate debugging, while looser defaults increase retention, consent, and breach exposure.
- Ruling: Use the official `github.com/modelcontextprotocol/go-sdk/mcp` package at `v1.7.0` behind a Server-owned `MCPClient` adapter; prefer Streamable HTTP for remote servers and require explicit admin configuration for local/process transport — the official SDK documents client and transport APIs and the version compatibility matrix — cost if wrong: SDK churn or remote-server dialect differences may require an adapter replacement, while a narrower initial transport set delays integrations. ([official Go SDK](https://github.com/modelcontextprotocol/go-sdk))
- Ruling: Identify an MCP tool by `(MCPServerID, remote tool name)` and bind every selected version to an immutable `schema_hash` — remote names are scoped by server and schema changes require review instead of silent identity replacement — cost if wrong: servers that reuse names for distinct resources require adding a remote stable tool ID and migrating associations.
- Ruling: Encrypt provider and MCP secrets with AES-256-GCM using a deployment-managed 32-byte key identifier kept outside the database; mask values in every API response — this supports authenticated encryption and explicit key rotation without persisting the encryption key with the ciphertext — cost if wrong: adopting a KMS envelope or hardware-backed key later requires key-version migration and a credential re-encryption pass.
- Ruling: Separate management authentication from voice admission; issue short-lived, scoped, single-use voice tokens and never expose management/provider secrets to the browser — public RTVI clients and administrators have different trust boundaries — cost if wrong: a different deployment identity model may require replacing the management session mechanism and its UI integration.
- Ruling: Use React, TypeScript, Vite, and a typed `/api/v1` client for the management UI — this keeps UI concepts outside Go core and matches the V2 admin product scope — cost if wrong: choosing another frontend toolchain changes UI build/hosting integration but not the HTTP contract.
- Ruling: Keep Whisker compatibility semantic through a Framework observer/debug adapter until an official package and protocol version are pinned — no exact Whisker wire target is specified in the approved inputs — cost if wrong: byte-level interoperability will require a versioned wire serializer and additional fixture/version maintenance.
- Ruling: A database trace/history failure is observable but cannot abort an active voice session; failure to resolve an agent or credential prevents new session creation — an existing session must outlive optional observability writes while new sessions require valid startup configuration — cost if wrong: operators may temporarily miss history during storage outages or reject a session that a degraded provider fallback could have served.
- Ruling: Processor failures adhere to explicit `FailurePolicy` declarations (`PolicyFailSession`, `PolicyDisable`, `PolicyDegrade`, `PolicySafeRetry`) — defaulting to session failure, while non-fatal policies emit observable `ErrorFrame` events with side-effect-aware retry protection (aborting retries if output was already emitted).

### 2.10 Processor failure and unusable policies

Processors in the session pipeline can optionally implement `core.FailurePolicyProvider`:
```go
type FailurePolicyProvider interface {
    FailurePolicy() FailurePolicy
}
```
Available policies:
1. `PolicyFailSession` (default): Aborts and tears down the session with the processor error.
2. `PolicyDisable`: Permanently disables the processor for subsequent frames in that session. Emits a non-fatal `*core.ErrorFrame{Fatal: false}` and passes the frame to the next processor. Subsequent frames completely bypass the disabled processor.
3. `PolicyDegrade`: Temporarily bypasses the failing processor for the current frame. Emits a non-fatal `*core.ErrorFrame{Fatal: false}` and passes the frame to the next processor. The processor remains active for future frames.
4. `PolicySafeRetry`: Retries processing the frame once before failing the session. If the processor already emitted one or more output frames during the current attempt before failing, retrying is considered unsafe and the session aborts immediately with `ErrRetryUnsafe` to prevent duplicate external side effects.

### 2.11 Provider usage metrics

`core.UsageMetricsFrame` carries optional typed token, audio duration, character, request, or tool usage events emitted by providers:
```go
type UsageMetricsFrame struct {
    SessionID        SessionID
    Turn             string
    Gen              uint64
    Provider         string
    Model            string
    Service          string // "llm", "stt", "tts", "tool"
    Timestamp        time.Time
    PromptTokens     *int64
    CompletionTokens *int64
    TotalTokens      *int64
    Characters       *int64
    AudioDurationMs  *int64
    Requests         *int64
}
```
- **Correlation**: Frames correlate to the active `SessionID`, `TurnID`, and `Generation`.
- **Absence Integrity**: Unavailable units remain `nil` rather than zero-inflated (e.g. TTS omits token fields; LLM omits character/audio fields).
- **Billing Boundary**: No billing or charging decisions are made inside core; usage is an observability/telemetry event mapped to trace attribute `usage.metrics`.

### 2.12 Bot output and playback-state observer

`core.PlaybackStateFrame` and `core.PlaybackTracker` provide fine-grained, ordered bot output and playback lifecycle observations:
```go
type PlaybackState string

const (
    PlaybackGenerating  PlaybackState = "generating"  // LLM / TTS generating speech content
    PlaybackBuffered    PlaybackState = "buffered"    // Pacer has received/buffered audio frames
    PlaybackPlaying     PlaybackState = "playing"     // First audio chunk dispatched to transport
    PlaybackDrained     PlaybackState = "drained"     // Pacer queue completely drained to client
    PlaybackInterrupted PlaybackState = "interrupted" // Output was cancelled or interrupted mid-playback
    PlaybackCompleted   PlaybackState = "completed"   // Turn output cleanly finished
)
```
- **Ordered Lifecycle**: `generating` → `buffered` → `playing` → (`drained` | `interrupted`) → `completed`.
- **Fault & Cancellation Isolation**: Interruptions fence subsequent buffered/playing transitions for the interrupted turn while allowing new generations to restart cleanly.
- **Empty Output**: Silent turns or pure tool-execution turns advance directly from `generating` to `completed` without spurious buffering or playing states.

### 2.13 Serializer abstraction

`core.Serializer` provides the general encoding/decoding abstraction between semantic events/frames and wire formats:
```go
type Serializer interface {
    Format() string
    Encode(v any) ([]byte, error)
    Decode(data []byte) (any, error)
}
```
- **Semantic Separation**: Decouples semantic events (such as `DebugEvent` and `ObserverEvent`) from specific transports or wire formats. RTVI remains a separate adapter without injecting wire types into processors.
- **Validation & Bounds**: Implementations (e.g. `JSONEventSerializer`) enforce payload size limits, reject malformed payloads with `ErrMalformedPayload`, and reject unknown fields when configured with `DisallowUnknownFields`.

### 2.14 Frame and pipeline observer API

`core.Session` exposes `AddObserver(o Observer)` allowing registration of non-mutating observers:
```go
type Observer interface {
    Observe(ObserverEvent)
}
```
- **Mutation Impossibility**: `ObserverEvent` contains value semantics and cloned attributes. Observers receive no mutable frame references or delivery mutation hooks.
- **Ordered Delivery**: `ObserverEvent` values are emitted in exact pipeline dispatch sequence with processor position and timing.
- **Fault & Panic Isolation**: Any panics thrown within observer callbacks are recovered and isolated from the session run-loop, ensuring zero degradation of active calls.

### 2.15 DTMF and transport-neutral tone frames

`core/audio.DTMFFrame` and `core/audio.ToneFrame` provide transport-neutral telephony and audible signaling representations:
```go
type DTMFFrame struct {
    Digit    rune          // '0'-'9', '*', '#', 'A'-'D'
    Duration time.Duration // bounded positive duration (<= 5s)
    Turn     string
    Gen      uint64
}

type ToneFrame struct {
    Frequencies []float64     // 20 Hz to 20,000 Hz
    Duration    time.Duration // bounded positive duration (<= 5s)
    Volume      float64       // 0.0 to 1.0
    Turn        string
    Gen         uint64
}
```
- **Validation**: Enforces standard DTMF dual-tone frequencies (`Frequencies()`) and audible frequency boundaries (`Validate()`).
- **Transport Neutrality**: Conveys telephone keypad interaction and signaling tones across the pipeline without implying specific SIP/PSTN transport coupling.

### 2.16 Bounded audio mixer

`core/audio.AudioMixer` provides multi-source audio stream mixing with deterministic sample bounds and clipping control:
```go
type AudioMixer struct { ... }
type MixerConfig struct {
    SampleRate int
    Channels   int
    MaxSources int // default 8
}
```
- **Stream Bounds**: Enforces `MaxSources` limit (`ErrMaxSourcesExceeded`) to prevent unbounded source registration.
- **Format Integrity**: Enforces uniform sample rate and channel layout across sources (`ErrMixerFormatMismatch`).
- **Clipping Protection**: Sums scaled 16-bit linear PCM samples and strictly saturates outputs within `[-32768, 32767]`, eliminating integer wraparound distortion.
- **Generation Fencing**: Discards obsolete source frames across turn transitions via `SetGeneration`.

### 2.17 Background audio and earcons

`core/audio.BackgroundAndEarconProcessor` orchestrates non-blocking ambient tracks and acoustic prompts (earcons):
```go
type BackgroundControlFrame struct {
    Action   BackgroundAction // start, stop, pause, resume
    TrackID  string
    Volume   float64
    Loop     bool
    PCM16LE  []byte
}

type EarconControlFrame struct {
    EarconID    string
    Volume      float64
    PCM16LE     []byte
    Disposition core.OutputDisposition // Interruptible vs Uninterruptible
}
```
- **Paced Output & Volume Ducking**: Paces PCM background audio chunk-by-chunk and attenuates gain automatically (`DuckOnSpeech`) when assistant speech begins (`TTSStartedFrame`), restoring volume on `TTSStoppedFrame`.
- **Classification & Disposition**: Earcons support explicit `OutputDisposition` (`OutputInterruptible` vs `OutputUninterruptible`).
- **Interruption Behavior**: Interruption frames cancel active interruptible earcons and reset ducking states while preserving uninterruptible cues.

### 2.18 Generic audio source

`core/audio.GenericAudioSource` injects externally provided `io.Reader` audio streams into the pipeline:
```go
type AudioSourceConfig struct {
    SampleRate  int
    Channels    int
    ChunkDur    time.Duration
    Disposition core.OutputDisposition
    Direction   core.Direction // Downstream (output) or Upstream (injected input)
}
```
- **Pacing & Streaming**: Streams audio continuously across timed intervals matching `ChunkDur`, avoiding bursts and honoring pipeline rate requirements.
- **Interruption Fencing**: Downstream interruptible sources immediately halt reading and chunk emission upon receiving an `InterruptionFrame`.
- **Directional Flexibility**: Supports both upstream injected user speech (e.g. testing, dial-in gateways) and downstream assistant audio playback.

### 2.19 Advanced audio filter chain

`core/audio.FilterChain` composes sequential audio processing nodes with dynamic bypass and format enforcement:
```go
type FilterChain struct { ... }
func (fc *FilterChain) AddFilter(id string, filter core.AudioFilter) error
func (fc *FilterChain) SetBypass(id string, bypass bool) error
func (fc *FilterChain) TotalLatency() time.Duration
func (fc *FilterChain) Process(ctx context.Context, in []byte) ([]byte, error)
```
- **Sequential Invariants**: Validates that output format of filter `N` strictly matches input format of filter `N+1` upon insertion (`ErrFilterIncompatibleFormat`).
- **Dynamic Bypass**: Allows toggling filters on/off dynamically at runtime without pipeline disruption, provided formats match.
- **Latency Accounting**: Computes cumulative pipeline DSP latency across all active (non-bypassed) filters for end-to-end timing correlation.

### 2.20 Read-only runtime pipeline settings introspection

`core.Session` exposes `Snapshot() SessionSnapshot` providing a point-in-time, read-only diagnostic inspection:
```go
type SessionSnapshot struct {
    SessionID       uint64
    State           string
    Generation      uint64
    InputMuted      bool
    CreatedAt       time.Time
    Uptime          time.Duration
    FramesIn        uint64
    FramesOut       uint64
    StaleDiscarded  uint64
    AudioDropped    uint64
    ActiveTasks     int
    LeakedTasks     int
    Settings        RuntimeSettings
    Processors      []ProcessorSnapshot
    ObserverCount   int
}
```
- **Thread Safety & Immutability**: All counters and settings are atomically or lock-protected and copied by value; mutations cannot affect the active session run loop.
- **Diagnostic Transparency**: Exposes exact processor pipeline topologies, indices, descriptions, and active observer counts.

### 2.21 Processor capability metadata

`core.CapableProcessor` allows pipeline processors to declare functional capabilities and contract guarantees:
```go
type ProcessorCapabilities struct {
    Name         string
    Version      string
    Description  string
    Capabilities []ProcessorCapability // audio_input, audio_output, speech_to_text, llm, vad, etc.
    InputFrames  []FrameType
    OutputFrames []FrameType
    SideEffects  bool                  // true if processor executes external RPCs/network calls
}

type CapableProcessor interface {
    Processor
    Capabilities() ProcessorCapabilities
}
```
- **Introspection Helpers**: `GetProcessorCapabilities(p)`, `HasCapability(p, cap)`, and `Pipeline.FindProcessorsWithCapability(cap)` allow pipeline introspection without tight coupling to concrete types.
- **Side Effect Tracking**: `SideEffects` flag explicitly identifies processors with side-effecting operations (e.g. LLM/TTS/network calls) to guide retry policies and state rollback.

### 2.22 Provider and processor readiness events

`core.ReadinessFrame` conveys availability and initialization status across pipeline components:
```go
type ReadinessStatus string
const (
    ReadinessReady    ReadinessStatus = "ready"
    ReadinessDegraded ReadinessStatus = "degraded"
    ReadinessNotReady ReadinessStatus = "not_ready"
)

type ReadinessFrame struct {
    Component string          `json:"component"`
    Status    ReadinessStatus `json:"status"`
    Reason    string          `json:"reason,omitempty"`
    Timestamp time.Time       `json:"timestamp"`
    Latency   time.Duration   `json:"latency,omitempty"`
    Gen       uint64          `json:"gen,omitempty"`
}
```
- **Validation**: Enforces non-empty component identification and canonical status enum states (`Validate()`).
- **Health Observability**: Emits non-blocking status transitions (e.g. provider network degradations or cold-start completions) across the pipeline for telemetry and diagnostics.

### 2.23 Turn recording and replay hooks

`core/turn.TurnRecorder` and `core/turn.TurnReplayer` provide deterministic capture and replay of conversation turns:
```go
type TurnRecording struct {
    TurnID    string
    Gen       uint64
    StartTime time.Time
    Duration  time.Duration
    Entries   []RecordedTurnEntry
}

func (r *TurnRecorder) Record(f core.Frame, dir core.Direction)
func (r *TurnRecorder) Stop() *TurnRecording
func (p *TurnReplayer) Replay(ctx context.Context, onEntry func(entry RecordedTurnEntry) error) error
```
- **Serialization Fidelity**: Full JSON round-trip serialization (`SerializeJSON` / `DeserializeJSON`) preserves relative event offsets and directionality.
- **Configurable Replay Speed**: Supports real-time pacing (`speed = 1.0`) or accelerated headless simulation (`speed = 0.0`) for fast continuous integration regression test suites.

### 2.24 Extended behavioral parity scenarios

Phase 10 introduces three extended parity scenarios (8, 9, 10) in `tools/eval/scenarios/` complementing the original seven vertical scenarios:
1. **Scenario 8 (`audio_mixing_and_earcons`)**: Bounded multi-source audio mixing, sample saturation clipping control, uninterruptible earcon delivery through barge-in, and automatic background audio ducking.
2. **Scenario 9 (`runtime_introspection_and_readiness`)**: Dynamic processor readiness notifications (`ReadinessFrame`), capability flag queries (`CapableProcessor`), and point-in-time runtime snapshot diagnostics (`Session.Snapshot`).
3. **Scenario 10 (`deterministic_turn_replay`)**: End-to-end conversation turn recording (`TurnRecorder`), JSON serialization fidelity, and headless regression replay (`TurnReplayer`).















