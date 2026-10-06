# Voice Platform V2 Specification

**Status:** Draft  
**Target:** V2  
**Purpose:** Define the second major release after V1 has proven the complete end-to-end voice system.

V2 is split into two explicit product tracks:

1. **Framework V2** — tighten the reusable Go voice framework, close the major audio/call capability gaps against Pipecat, and add first-class tracing and tool execution primitives.
2. **Server V2** — turn the config-driven V1 server into a persistent, UI-managed production product for creating, testing, observing, and operating voice agents.

The server must remain a consumer of the framework.

The framework must remain independently usable without the server.

---

# 1. V2 Goal

V1 proves:

```text
RTVI Client
→ WebSocket
→ Session Runtime
→ VAD
→ STT
→ Context
→ LLM
→ TTS
→ Client
```

with real providers and a config-driven server.

V2 must prove something broader:

> The framework is mature enough to serve as a reusable production voice runtime, and the server is complete enough that users can create, configure, test, inspect, and operate voice agents without editing code or configuration files.

V2 is therefore about:

- framework maturity,
- production observability,
- call/audio feature completeness,
- robust tool execution,
- persistent agent management,
- provider credential management,
- trace/session inspection,
- onboarding,
- production server UX.

Telephony remains out of scope.

---

# 2. Architectural Separation

The project must maintain a clear boundary between:

```text
Framework
    ↓
Server Product
    ↓
Management UI
```

## Framework

The framework owns:

- frames,
- processors,
- pipelines,
- sessions,
- supervision,
- interruption,
- audio processing,
- VAD,
- turn detection,
- context,
- tool execution primitives,
- tracing interfaces,
- provider integration interfaces,
- transport abstractions,
- reusable processors.

The framework does **not** own:

- persistent agents,
- user accounts,
- databases,
- UI concepts,
- API-key storage,
- product onboarding,
- server configuration editing.

## Server

The server owns:

- persistent agents,
- provider credentials,
- agent configuration,
- database storage,
- trace persistence,
- session history,
- server management APIs,
- onboarding,
- provider capability discovery,
- agent validation,
- management UI,
- deployment/runtime configuration presentation.

The server must construct and run agents using the same framework APIs available to normal Go applications.

---

# 3. Pipecat as Behavioral Reference

V2 continues using the local Pipecat checkout as the behavioral reference.

The objective is not API-level or source-level cloning.

When closing an audio/call behavior gap:

1. identify the corresponding Pipecat feature,
2. inspect implementation and tests,
3. define the behavior being preserved,
4. implement it naturally in Go,
5. add equivalent behavioral tests,
6. document intentional differences.

Maintain:

```text
docs/pipecat-compatibility.md
```

with:

- implemented behavior,
- intentionally different behavior,
- unsupported behavior,
- future behavior.

V2 should materially reduce the unsupported audio/call behavior list.

---

# 4. V2 Repository Shape

The V1 monorepo structure remains appropriate.

Conceptually:

```text
/
├── core/
│
├── providers/
│   ├── google/
│   ├── xai/
│   └── additional providers as needed
│
├── server/
│
├── ui/
│
├── examples/
├── docs/
└── site/
```

The core and provider modules remain independently installable.

Dependency direction remains:

```text
core
 ↑
providers
 ↑
server
 ↑
ui
```

The UI communicates with the server through management/product APIs.

---

# PART I — FRAMEWORK V2

# 5. Framework V2 Objective

Framework V2 should take the V1 runtime from:

> a complete working realtime voice framework

to:

> a mature reusable voice runtime with strong audio/call semantics, observability, tool execution, and a substantially smaller functional gap relative to Pipecat.

V2 framework work should focus on **voice-runtime depth**, not unrelated AI platform features.

---

# 6. AI Tracing

Tracing becomes a first-class framework capability.

The framework must expose structured lifecycle events without depending on a particular persistence or telemetry backend.

## 6.1 Trace Interface

Define a provider-neutral tracing interface.

Conceptually:

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

Exact API is not prescribed.

The requirements are:

- zero/low overhead when disabled,
- no server dependency,
- session/turn correlation,
- nested timing relationships,
- structured metadata,
- support for custom processors,
- safe handling of text/audio metadata.

## 6.2 Trace Hierarchy

A normal voice interaction should be representable approximately as:

```text
Session
└── Turn
    ├── VAD
    ├── STT
    │   ├── connect
    │   ├── first partial
    │   └── final transcript
    ├── Context
    ├── LLM
    │   ├── request
    │   ├── first token
    │   └── complete
    ├── Tool Loop
    │   ├── tool call
    │   ├── tool execution
    │   └── tool result
    └── TTS
        ├── request
        ├── first audio
        └── complete
```

## 6.3 Standard Trace Events

V2 should define standard events for:

- session start/end,
- turn start/end,
- speech start/stop,
- VAD latency,
- STT start/final,
- transcript finalization,
- LLM request,
- LLM first output,
- LLM complete,
- TTS request,
- TTS first audio,
- TTS complete,
- interruption,
- flush,
- tool requested,
- tool started,
- tool completed,
- tool failed,
- provider errors,
- transport disconnect,
- session failure.

## 6.4 Trace Data Safety

Raw audio must not be included in traces.

Conversation text should be controllable through a trace policy:

```text
none
metadata only
text
```

The default for production should avoid storing sensitive content unless explicitly enabled.

---

# 7. Turn Model

V2 should formalize the notion of a conversational turn.

The framework should be able to associate:

- user speech,
- transcription,
- LLM generation,
- tool execution,
- assistant text,
- TTS audio,
- interruption,

with a stable turn identifier.

This should simplify:

- tracing,
- cancellation,
- observability,
- tool loops,
- context commits,
- UI representation.

Turn IDs remain framework/runtime concepts and do not require persistence.

---

# 8. Interruption and Barge-In Hardening

V1 introduces barge-in.

V2 must make it production-grade.

The framework should support and test:

- interruption during LLM generation,
- interruption during TTS generation,
- interruption while TTS audio is buffered,
- interruption during tool execution,
- repeated rapid interruptions,
- false speech-start recovery,
- assistant completion racing with user speech,
- provider cancellation delays,
- output flush ordering.

Required invariant:

> Once interruption is committed, stale assistant audio must not continue playing except for unavoidable transport/audio already delivered to the client.

---

# 9. Audio Pipeline Completeness

V2 should fill in the major audio/call processing gaps needed for parity with mature voice pipelines.

The exact list should be guided by the local Pipecat repository and real product requirements.

At minimum evaluate and implement the following.

---

# 10. Silence Detection

Add reusable silence-related processors.

Potential processors include:

```text
SilenceDetector
SilenceTimeout
SpeechTimeout
NoSpeechDetector
```

Capabilities should include:

- detect prolonged silence,
- emit silence events,
- trigger configurable callbacks/frames,
- distinguish initial silence from mid-call silence,
- reset on user activity,
- support session timeout logic.

Potential frames:

```text
SilenceStartedFrame
SilenceEndedFrame
SilenceTimeoutFrame
```

The implementation should avoid introducing processor-specific session orchestration.

---

# 11. Turn Detection

VAD answers:

> Is the user currently speaking?

Turn detection should answer:

> Is the user likely finished with their conversational turn?

V2 should support a pluggable turn-detection abstraction.

Possible strategies:

- silence-duration based,
- semantic/ML end-of-turn model,
- hybrid approach.

The pipeline should not hardcode one turn detector.

A turn detector should emit a framework-level event/frame representing committed user-turn completion.

---

# 12. Audio Resampling

Add reusable audio resampling support.

Providers and transports should not each implement bespoke sample-rate conversion.

Core should support normalizing between commonly required sample rates such as:

```text
8 kHz
16 kHz
24 kHz
48 kHz
```

Resampling must be:

- streaming,
- bounded,
- session-safe,
- efficient enough for large session counts.

---

# 13. Audio Format Conversion

Provide reusable audio format conversion where required.

Examples:

- PCM16,
- float PCM,
- mono/stereo normalization,
- provider-required encodings where appropriate.

Keep codec-heavy functionality modular.

Do not make every provider responsible for basic PCM transformations.

---

# 14. Audio Buffering

Introduce reusable bounded audio buffer primitives/processors.

Use cases:

- pre-roll before speech start,
- VAD buffering,
- provider chunk sizing,
- TTS output pacing,
- interruption flushes.

Buffers must have explicit bounds and stale-data policies.

---

# 15. Audio Output Pacing

TTS providers may generate audio faster than wall-clock playback.

V2 should support a reusable output pacing mechanism so transport output is correctly timed.

The framework should avoid allowing one fast TTS producer to create unbounded output buffers.

---

# 16. Audio Mixer / Sources

Evaluate support for basic audio-source composition.

Potential future use cases include:

- assistant speech,
- notification tones,
- earcons,
- waiting sounds.

If included in V2, keep the initial mixer deliberately simple.

This is lower priority than barge-in, turn detection, and resampling.

---

# 17. Idle / Session Timeout Processors

Provide reusable processors for:

- no user audio,
- no user speech,
- no turn activity,
- maximum session duration,
- provider inactivity.

Timeout behavior should emit normal control frames rather than directly terminating the process.

---

# 18. Frame Processor Catalog

Framework V2 should define a useful catalog of reusable frame processors instead of forcing application developers to write common voice-pipeline glue repeatedly.

The catalog should be organized into clear families:

```text
Core Processors
Voice Processors
AI Processors
```

The goal is not to maximize the number of processors.

The goal is to cover the common behaviors needed by production voice pipelines while keeping the framework composable and understandable.

## 18.1 Core Processors

Core processors should provide generic pipeline utilities that are not voice-provider specific.

Recommended V2 processors include:

### FrameFilter

Pass or consume frames based on:

- frame type,
- direction,
- predicate,
- metadata.

### FrameMapper

Transform one frame into another.

Useful for:

- metadata normalization,
- simple frame adaptation,
- application-specific mappings.

### ConditionalRouter

Route frames based on:

- type,
- direction,
- runtime condition,
- session state.

V2 should keep routing deliberately constrained and avoid turning the framework into a general workflow/DAG engine.

### BufferProcessor

Bounded buffering for frames where short-term accumulation is required.

Must support:

- explicit capacity,
- overflow policy,
- flush,
- cancellation.

### RateLimiter

Limit the rate of selected frame types or downstream operations.

### Debouncer / Throttler

Useful for noisy/high-frequency signals where only a bounded cadence is useful.

### Pause / Resume Gate

Temporarily hold or reject selected pipeline traffic while preserving control-frame semantics.

### SessionTimeoutProcessor

Emit lifecycle/control frames for:

- maximum session duration,
- inactivity,
- provider inactivity.

### IdleTimeoutProcessor

Track lack of relevant activity and emit timeout events through the normal frame model.

---

## 18.2 Voice Processors

Voice processors should cover reusable audio and conversational behaviors that should not be reimplemented separately by each provider.

Recommended V2 processors include:

### SilenceDetector

Detect prolonged silence.

Potential frames:

```text
SilenceStartedFrame
SilenceEndedFrame
SilenceTimeoutFrame
```

Should support:

- initial silence,
- inter-turn silence,
- configurable thresholds,
- reset on speech/activity.

### NoSpeechDetector

Detect sessions where no meaningful user speech occurs within a configured period.

### TurnDetector

Convert VAD/speech state into committed conversational turn completion.

The turn detector should be pluggable.

Potential strategies:

- silence-duration,
- semantic/ML,
- hybrid.

### SpeechDebounce / False-Start Suppressor

Reduce false speech starts caused by:

- short noise bursts,
- clicks,
- transient activation.

### MinimumSpeechDurationFilter

Avoid committing extremely short speech events as user turns unless explicitly configured.

### AudioResampler

Streaming conversion between common sample rates.

Target rates should include at least:

```text
8 kHz
16 kHz
24 kHz
48 kHz
```

### AudioFormatConverter

Reusable conversion for:

- PCM16,
- float PCM,
- mono/stereo normalization,
- provider-required raw formats.

### ChannelNormalizer

Normalize channel layouts without making every provider/transport handle this independently.

### Gain / Volume Normalizer

Optional processor for safe amplitude normalization where appropriate.

### NoiseGate

Optional simple noise gating where it materially improves voice pipelines.

This should not be confused with full noise suppression.

### AudioPreRollBuffer

Retain a short bounded window before committed speech start so VAD/turn detection does not cut off the beginning of an utterance.

### AudioChunker / Rechunker

Adapt stream chunk sizes between:

- transport,
- VAD,
- STT,
- TTS,
- output pacing.

### OutputAudioPacer

Pace generated audio approximately in real time rather than allowing a fast TTS producer to flood transport buffers.

### OutputAudioBuffer

Bounded buffering for generated speech.

Must integrate with interruption/flush semantics.

### InterruptionFlushProcessor

Provide deterministic removal of stale assistant:

- text,
- TTS requests,
- buffered TTS audio,
- transport-bound audio

after an interruption is committed.

### AudioMixer

Optional lower-priority V2 processor for simple composition of:

- assistant speech,
- tones,
- earcons,
- waiting audio.

Do not allow mixer work to delay higher-priority V2 audio/call completion.

---

## 18.3 STT-Oriented Processors

Recommended reusable STT-side processors include:

### InterimTranscriptFilter

Control whether interim transcripts continue downstream.

### TranscriptStabilizer

Handle provider-specific repeated/interim text behavior and produce cleaner stable transcript progression.

### TranscriptDeduplicator

Prevent duplicate final/interim content from being committed multiple times.

### ConfidenceFilter

Optionally suppress transcripts below a configured confidence threshold where the provider supplies meaningful confidence information.

### LanguageSelector

Apply or switch language metadata/policy where supported.

### TranscriptRedactor

Optional processor for configurable redaction before traces/context persistence.

---

## 18.4 Text and TTS Preparation Processors

Streaming LLM output frequently needs preparation before TTS.

Recommended processors include:

### SentenceAggregator

Accumulate streamed text until a complete speakable sentence is available.

### ClauseAggregator

Allow lower-latency TTS by committing safe clause-sized units before full sentence completion.

### PunctuationAwareChunker

Chunk streamed text while respecting natural speech boundaries.

### TTSNormalizer

Normalize text for speech without changing semantic meaning.

Potential responsibilities:

- whitespace cleanup,
- punctuation normalization,
- provider-safe text normalization.

Avoid provider-specific pronunciation logic in core unless it is genuinely reusable.

### TTSTextBuffer

Bounded buffering between streamed LLM output and TTS synthesis.

### WordTimestampSynchronizer

Where supported, correlate TTS word/timing metadata with emitted audio for:

- UI highlighting,
- interruption,
- debugging.

---

## 18.5 Context Processors

Recommended context processors include:

### UserContextAggregator

Commit final user transcript into provider-neutral conversation context.

### AssistantContextAggregator

Commit completed assistant content.

### InterruptedAssistantReconciler

Define what portion, if any, of interrupted assistant output is committed into conversation history.

### ContextWindowLimiter

Provide hooks/policies for limiting conversation context without hardcoding a provider-specific token strategy.

### SystemMessageInjector

Inject stable or dynamic system/context messages into the framework-neutral context model.

### ToolResultAggregator

Commit tool calls/results consistently before continuing the LLM loop.

---

## 18.6 AI Processors

AI processors include the provider-facing processing stages already central to the framework:

```text
STT Processor
LLM Processor
Tool Loop Processor
TTS Processor
```

These remain provider-neutral at the core abstraction level, with concrete provider implementations living in provider modules.

The framework should avoid creating giant STT/LLM/TTS interfaces when the normal `Processor` model plus small capability interfaces is sufficient.

---

## 18.7 Tool Processors

Recommended reusable tool processors/components include:

### ToolCallDispatcher

Convert provider-neutral tool-call requests into ToolLoop execution.

### ToolApprovalGate

Optional processor/capability for tools that require confirmation before side effects.

### ToolTimeout

Ensure tool execution remains bounded.

### ToolResultNormalizer

Normalize tool output before adding it back into context/LLM requests.

Tool orchestration should still follow the ToolLoop architecture defined elsewhere in this specification.

---

## 18.8 Observability-Adjacent Utilities

Useful utilities include:

```text
FrameTap
FrameCounter
LatencyMarker
SamplingTap
```

However, the framework should not force every observability concern into the `Processor` abstraction.

Where semantics are better represented as:

```text
Observer
Tracer
Serializer
Transport Adapter
```

use separate interfaces.

This distinction is important for keeping processors focused on frame behavior rather than turning every cross-cutting concern into pipeline mutation.

---

## 18.9 Processor Tiers in Documentation

Framework documentation should present processors in three high-level tiers:

```text
Core Processors
- filter
- mapper
- router
- buffer
- timeout
- rate limit

Voice Processors
- VAD
- silence detector
- turn detector
- resampler
- chunker
- pre-roll
- output pacer
- interruption/flush
- transcript stabilizer

AI Processors
- STT
- context aggregation
- LLM
- tool loop
- TTS
```

This should make the framework easier to understand than a flat catalog of processor types.

---

# 19. Metrics / Observer Processors

Formalize observer/tap behavior for users who need pipeline introspection without changing frame flow.

This may integrate with tracing.

The framework should clearly distinguish:

```text
Processor
Observer
Tracer
```

if those concepts are materially different.

---

# 20. Context Aggregation Hardening

Bring conversation aggregation closer to production maturity.

V2 should support:

- system messages,
- user messages,
- assistant messages,
- tool calls/results,
- interrupted assistant turns,
- configurable commit semantics,
- provider-neutral conversion,
- context limits/hooks.

The framework should define what happens to assistant content that was generated but interrupted before completion.

---

# 21. Tool Calling Model

Tool calling becomes a first-class framework capability in V2.

The goal is not to build a workflow engine.

The goal is to support robust LLM tool loops in a provider-neutral way.

---

# 22. Tool Definitions

Core should support provider-neutral tool definitions.

Conceptually:

```go
type Tool struct {
    Name        string
    Description string
    Schema      JSONSchema
    Handler     ToolHandler
}
```

Exact representation is implementation-plan territory.

Tools need:

- stable name,
- description,
- structured argument schema,
- execution handler,
- timeout,
- tracing,
- cancellation.

---

# 23. Tool Frames / Events

The framework should expose tool execution through structured frames or equivalent internal events.

Possible concepts:

```text
ToolCallFrame
ToolStartedFrame
ToolResultFrame
ToolErrorFrame
```

Tool execution must remain observable through normal tracing.

---

# 24. Tool Loop

The framework must support the common agent loop:

```text
LLM
 ↓
tool call(s)
 ↓
execute tools
 ↓
tool results
 ↓
LLM
 ↓
possibly more tools
 ↓
final text
```

Requirements:

- configurable maximum loop iterations,
- support multiple tool calls where provider semantics allow,
- cancellation,
- timeout,
- deterministic context updates,
- traceability,
- provider-neutral behavior,
- protection against infinite loops.

---

# 25. Tool Executor Abstraction

Separate the tool-loop policy from tool execution.

Conceptually:

```text
ToolLoop
   ↓
ToolExecutor
   ↓
registered handlers
```

This allows future execution modes such as:

- in-process Go handlers,
- HTTP tools,
- MCP tools,
- isolated tools.

Only in-process framework tools are required at the framework level in V2.

The Server may add declarative HTTP tooling later if included in the implementation plan.

---

# 26. Go Tool-Loop Library Evaluation

Before implementing the V2 tool loop, the implementation plan should include a short evaluation of current Go libraries/frameworks that support:

- structured tool schemas,
- multi-step tool loops,
- multiple LLM providers,
- cancellation/context,
- streaming,
- low framework coupling.

Adopting a library is optional.

Any external framework must not:

- own the voice session lifecycle,
- replace the FrameProcessor pipeline,
- introduce a conflicting agent runtime,
- force provider-specific types into core.

If no library fits cleanly, implement the small required loop natively.

---

# 27. Provider Tool Support

Google and xAI LLM provider modules should support framework tool definitions.

Provider modules are responsible for:

- translating framework tool schemas,
- interpreting provider tool-call output,
- mapping results back into provider requests.

Tool business logic remains outside provider packages.

---

# 28. Framework Error Hardening

V2 should formalize error categories and retryability.

Errors should retain:

- source processor,
- provider,
- operation,
- category,
- retryability,
- causal error.

Framework behavior for recoverable provider errors should be explicit.

Avoid automatic retries where they may duplicate side effects.

---

# 29. Framework Lifecycle Hardening

Tighten:

- processor startup,
- processor shutdown,
- session startup ordering,
- transport attach/detach,
- provider resource cleanup,
- interrupted task cleanup,
- tool cancellation,
- teardown deadlines.

The framework should have deterministic lifecycle tests.

---

# 30. Framework Testing Parity

Use the Pipecat repository to identify relevant audio/call behavior tests.

Build an explicit parity matrix.

Example:

```text
Behavior                         Go V2
--------------------------------------
bidirectional frames             yes
barge-in                         yes
audio flush                      yes
silence detection                yes
turn detection                   yes
resampling                       yes
context aggregation              yes
tool loop                        yes
...
```

The implementation plan should define the exact V2 parity target.

---

# 30.1 Compatibility and Conformance Suite

Framework V2 must maintain explicit compatibility testing across two independent external surfaces.

## RTVI Compatibility

RTVI tests prove that existing Pipecat-compatible clients can connect to and operate the Go server correctly.

This covers the client-facing protocol surface.

## Whisker Compatibility

Whisker tests prove that the Go runtime exposes the expected debug/pipeline event semantics for official Pipecat debugging tooling.

This covers the low-level pipeline/debug protocol surface.

The two suites must remain separate because success in one does not imply success in the other.

A V2 compatibility CI gate should conceptually include:

```text
Core behavior
RTVI conformance
Whisker conformance
Pipecat reference behavior comparison
```

Intentional differences must be documented in `docs/pipecat-compatibility.md`.

---

# 30.2 Additional Audio / Call Parity Capabilities

To move Framework V2 closer to mature Pipecat audio/call behavior, the following capabilities should be included in the V2 specification.

These are intentionally tagged by priority so the later implementation plan can sequence them without treating every item as equally urgent.

Priority meanings:

```text
P0 — required for V2 audio/call parity or production correctness
P1 — strongly recommended for mature voice behavior
P2 — useful parity / extensibility feature that may be scheduled later in V2
```

The implementation plan may reorder individual items based on dependency and complexity, but should preserve the intent of these priorities.

---

## P0 — Input Mute / Unmute Control

Support explicit user-input muting independent of transport disconnect.

Required behaviors:

- mute incoming user audio while keeping the session alive,
- unmute without recreating the session,
- expose state through normal framework control semantics,
- ensure VAD/STT do not continue processing muted audio unless explicitly configured,
- allow runtime policy changes,
- emit observable state changes for tracing/debugging.

Potential control frames:

```text
InputMuteFrame
InputUnmuteFrame
InputMutedStateFrame
```

Use cases include:

- controlled interaction phases,
- echo prevention,
- temporary suppression during sensitive actions,
- application-driven user-input gating.

---

## P0 — Audio Filter / Noise Suppression Abstraction

Define a reusable streaming audio-filter abstraction.

Potential implementations include:

- noise suppression,
- denoising,
- echo-related preprocessing where practical,
- simple filters.

V2 should ship at least one real noise-suppression implementation if practical.

Requirements:

- streaming operation,
- bounded state,
- cancellation-safe,
- explicit input/output audio format,
- compatible with resampling,
- observable processing latency,
- no provider dependency.

A simple noise gate is not sufficient to satisfy this capability.

---

## P0 — Bot Speaking State

Formalize assistant/bot speaking state in the framework.

The framework must be able to represent transitions such as:

```text
BotStartedSpeaking
BotStoppedSpeaking
```

This state should be based on output audio behavior rather than merely "TTS request exists."

It should integrate with:

- RTVI events,
- interruption,
- traces,
- test/debug UI,
- output pacing.

---

## P0 — Interruptible vs Uninterruptible Output

Not all output audio should necessarily be flushed on user interruption.

Support output classification such as:

```text
interruptible
uninterruptible
```

or an equivalent policy.

Examples:

- normal assistant speech → interruptible,
- critical notification/earcon → optionally uninterruptible.

Interruption/flush logic must preserve the distinction.

---

## P0 — Output Flush Semantics

Harden flush semantics beyond general barge-in.

The framework must define deterministic behavior for:

- queued TTS text,
- active TTS request,
- buffered TTS audio,
- already-paced audio,
- transport-bound audio,
- control frames.

Flush ordering and completion should be testable.

---

## P0 — Turn Management Controller

Add a higher-level turn-management abstraction in addition to VAD and turn detection.

Responsibilities may include:

- turn start,
- turn stop,
- interruption policy,
- idle policy,
- turn timeout policy,
- runtime policy updates,
- coordination between VAD and turn detector.

The controller should emit normal framework events rather than directly controlling application state outside the pipeline.

---

## P0 — STT Commit / Finalization Policy

Introduce provider-neutral transcript commit behavior.

Capabilities should include:

- interim stability threshold,
- finalization timeout,
- delayed final commit,
- late-final reconciliation,
- duplicate final suppression,
- configurable end-of-turn grace period.

The goal is to avoid hardcoding provider-specific transcript timing into conversation context logic.

---

## P0 — Turn-Scoped Audio Buffer

Provide a reusable processor that captures the exact audio associated with a user turn.

Potential uses:

- debugging,
- evals,
- replay,
- fallback STT,
- offline analysis,
- trace attachments where allowed.

Requirements:

- bounded memory,
- configurable retention,
- privacy-aware,
- no raw audio persistence unless explicitly enabled.

---

## P0 — Behavioral Eval Harness

Framework V2 must include a reusable eval harness for voice-runtime behavior.

This is distinct from unit tests and compatibility tests.

The harness should support deterministic scenarios for:

```text
barge-in
turn completion
silence timeout
no-speech timeout
audio flush
transcript stabilization
tool interruption
output pacing
session teardown
RTVI compatibility
Whisker compatibility
```

Results should be machine-readable so regressions can be tracked in CI.

The harness should reuse the same runtime path as production where practical.

---

## P1 — Runtime Service Settings Updates

Support selected processor/runtime settings changes without rebuilding the session.

Examples:

```text
silence timeout
turn timeout
interruption enabled
input mute state
language
selected runtime thresholds
```

Updates should flow through controlled framework messages/frames.

Do not allow arbitrary mutation of processor internals.

---

## P1 — Context Summarization / Compaction Hook

Long-running sessions need a provider-neutral mechanism for context reduction.

Support a context compaction interface/hook.

Potential strategies:

- truncation,
- summarization,
- application-provided compaction,
- provider-aware policy.

V2 does not need to prescribe one universal summarization model.

The core requirement is that context aggregation exposes a clean compaction point.

---

## P1 — Audio Recording / Capture Processor

Provide a reusable audio capture processor.

Potential capture modes:

```text
user only
assistant only
mixed
separate tracks
```

Requirements:

- opt-in,
- explicit privacy policy,
- bounded/runtime-safe,
- sink abstraction,
- no storage dependency in core.

Server persistence of recordings is not required by Framework V2 unless explicitly planned.

---

## P1 — Urgent Control Frames / Priority Lane

Some control messages should not wait behind ordinary data/audio queues.

Support explicit priority semantics for:

- cancel,
- interruption,
- flush,
- end,
- critical error.

This may use:

```text
control queue
priority envelope
urgent frame class
```

or another Go-native implementation.

The observable semantic requirement is more important than the internal data structure.

---

## P1 — Processor Failure / Unusable Policy

Not every processor failure should necessarily kill the session immediately.

Define a framework-level failure policy model.

Potential policies:

```text
fail session
disable processor
degrade capability
retry if safe
propagate error frame
```

V2 may implement only a subset, but the abstraction should avoid hardcoding every processor error into session death.

Retries must be explicit and side-effect safe.

---

## P1 — Usage Metrics

Expand metrics beyond latency.

Where providers expose usage information, support provider-neutral usage events such as:

```text
LLM input/output tokens
STT audio duration
TTS characters/audio duration
tool calls
provider request count
```

These should integrate with tracing/observers without forcing billing logic into core.

---

## P1 — Bot Output / Playback State Observer

Expose useful state beyond BotStartedSpeaking/BotStoppedSpeaking.

Potential states:

```text
TTS generating
audio buffered
audio playing
audio drained
interrupted
```

This is useful for:

- UI,
- debugging,
- turn coordination,
- evals.

Keep this as observer/state semantics rather than application-specific product state.

---

## P1 — Serializer Abstraction

Define a clear serializer boundary for protocol/transport messages.

Use cases:

- RTVI,
- Whisker debug protocol,
- future transports/protocols.

Serializers should be responsible for converting between:

```text
framework events/frames
↔
wire representation
```

without becoming general-purpose pipeline processors unless that is semantically appropriate.

---

## P1 — Frame / Pipeline Observer API

Formalize a non-mutating observer API.

Observers should be able to inspect:

- frames,
- direction,
- processor position,
- timing,
- lifecycle events,

without being allowed to alter frame flow.

This supports:

- tracing,
- Whisker,
- debugging,
- eval harness,
- metrics.

---

## P2 — DTMF / Tone Frames

Even though telephony is deferred, generic tone/DTMF support is useful for parity and future transports.

Potential frames:

```text
OutputToneFrame
OutputDTMFFrame
```

The framework should keep these generic and transport-neutral.

Actual telephone signaling support remains deferred.

---

## P2 — Audio Mixer

Provide a simple audio mixer abstraction for combining:

- assistant speech,
- tones,
- earcons,
- waiting audio,
- application-provided audio.

Requirements:

- bounded buffering,
- deterministic timing,
- output format agreement,
- interaction with interruption policy.

This is lower priority than core conversational audio behavior.

---

## P2 — Background Audio / Earcon Processor

Provide a reusable way to inject:

- start tones,
- confirmation tones,
- waiting sounds,
- application earcons.

This can build on the mixer/output-source abstraction.

---

## P2 — Audio Source Abstraction

Allow reusable non-TTS audio sources to feed output.

Examples:

- generated tone,
- file/clip,
- application stream.

Sources must still obey:

- pacing,
- buffering,
- interruption policy,
- output format.

---

## P2 — Advanced Audio Filter Chain

Allow multiple audio filters to be composed:

```text
resample
→ denoise
→ gain normalize
→ VAD
```

The implementation should avoid unnecessary copies.

This is useful but should follow the simpler single-filter abstraction.

---

## P2 — Runtime Pipeline Settings Introspection

Expose read-only runtime configuration/state for debugging.

Examples:

```text
current turn thresholds
mute state
active audio format
provider stream state
buffer depths
```

This should support Whisker/debug tooling without exposing arbitrary mutable internals.

---

## P2 — Processor Capability Metadata

Processors may expose optional capability metadata useful for tooling/docs.

Examples:

```text
supports interruption
supports runtime updates
produces audio
consumes audio
async
stateful
```

This should remain optional and lightweight.

Potential uses include:

- Whisker,
- documentation,
- debug UI,
- pipeline validation.

---

## P2 — Provider/Processor Readiness Events

Expose runtime readiness transitions for components such as:

```text
STT stream connected
LLM provider ready
TTS stream ready
transport output ready
```

These should aid:

- startup debugging,
- traces,
- graceful degradation.

Avoid blocking the entire session on optional capabilities unless policy requires it.

---

## P2 — Turn Recording / Replay Hooks

Build on TurnAudioBuffer and trace infrastructure to allow application-provided replay/eval hooks.

Core should expose hooks rather than owning persistence.

---

## P2 — Extended Behavioral Parity Cases

Expand the eval/parity suite to include:

```text
false speech start
rapid repeated barge-in
late STT final
provider stream reconnect
TTS finishes during interruption
tool completes during interruption
mute/unmute mid-turn
runtime timeout update
buffer overflow
urgent control under congestion
processor degradation policy
```

These cases are valuable for mature parity but can follow the P0 behavioral suite.

---

## Planning Priority Guidance

The implementation plan should group this work approximately as follows.

### First Wave — P0

Prioritize capabilities that directly affect conversational correctness, isolation, latency, or Pipecat compatibility:

```text
input mute/unmute
noise suppression/filter abstraction
bot speaking state
interruptible output
deterministic flush
turn management
STT commit policy
turn audio buffer
behavioral eval harness
```

### Second Wave — P1

Then harden operability and long-session behavior:

```text
runtime setting updates
context compaction
recording hooks
urgent control lane
failure/degradation policy
usage metrics
output state observers
serializer abstraction
observer API
```

### Third Wave — P2

Finally fill lower-risk parity/extensibility gaps:

```text
DTMF/tones
mixer/background audio
audio source abstraction
advanced filter chains
runtime introspection
processor capability metadata
readiness events
replay hooks
extended parity scenarios
```

The planning phase may promote a P1/P2 item when it is a prerequisite for another feature, but should not demote a P0 feature without explicitly documenting why.

---

# 31. Framework Performance Regression Suite

Retain and expand benchmark coverage from the POC.

Track:

- session startup,
- marginal session memory,
- processor traversal,
- audio resampling overhead,
- VAD latency,
- interruption latency,
- tool-loop overhead,
- trace overhead enabled/disabled.

Tracing must have a measurable disabled-mode overhead budget.

---

# PART II — SERVER V2

# 32. Server V2 Objective

Server V2 turns the V1 config-driven runtime into a persistent voice-agent product.

A user should be able to:

```text
install/start server
→ open UI
→ configure provider keys
→ create an agent
→ test voice conversation
→ inspect session traces
→ modify the agent
→ operate the server
```

without editing Go code.

---

# 33. Persistent Storage

V2 requires persistent storage.

Support:

```text
SQLite
PostgreSQL
```

SQLite should provide the simplest single-node deployment.

PostgreSQL should support production/multi-instance deployment.

Use a storage abstraction so product services do not depend directly on one SQL engine.

---

# 34. Schema / Migrations

The server must own explicit database migrations.

Persistent entities should be versioned.

Potential core entities:

```text
agents
provider_credentials
sessions
turns
traces/spans
tool_calls
server metadata
```

Exact normalization belongs in the implementation plan.

---

# 35. Persistent Agent Model

Agent definitions move from static configuration into persistence.

An agent should include at minimum:

- ID,
- name,
- description,
- system prompt,
- STT configuration,
- LLM configuration,
- TTS configuration,
- VAD/turn settings,
- interruption settings,
- tool configuration,
- optional greeting,
- enabled/disabled state,
- created/updated timestamps.

Agent definitions should be versionable or at minimum auditable enough to know what configuration produced a historical session.

---

# 36. Agent Runtime Snapshot

Sessions must run against a stable agent configuration snapshot.

Changing an agent in the UI must not mutate an already-running session unpredictably.

A session record should retain enough information to identify the agent version/configuration used.

---

# 37. Agent Management API

Add server APIs for:

```text
create agent
read agent
update agent
delete/archive agent
list agents
duplicate agent
test agent
```

API design should support the UI without coupling persistence objects directly to framework internals.

---

# 38. Agent Wizard

The UI must include a guided agent-creation flow.

Suggested flow:

```text
1. Name / purpose
2. Provider credentials
3. STT selection
4. LLM selection
5. TTS / voice selection
6. System prompt
7. Voice behavior
8. Test
9. Save
```

The wizard should optimize for getting a working voice agent quickly.

---

# 39. Functional-Agent Validation

A functioning voice agent requires at least:

```text
one STT capability
one LLM capability
one TTS capability
```

The server must validate this.

An agent cannot be activated/tested as a complete voice agent unless all three capabilities resolve successfully.

VAD and transport are server/framework defaults unless overridden.

---

# 40. Provider Credential Management

Provider credentials become persisted server-managed resources.

Credentials should be encrypted at rest where appropriate.

At minimum support:

- add credential,
- update credential,
- delete credential,
- validate credential,
- display masked value,
- show provider,
- show detected capabilities,
- show validation status.

Raw secrets must never be returned to the browser after creation.

---

# 41. Automatic Provider Capability Detection

When a provider credential is added, the server should determine which voice-stack capabilities that provider can satisfy.

Examples:

```text
xAI
  ✓ LLM
  ✓ STT
  ✓ TTS

ElevenLabs
  ✗ LLM
  ✓ STT (if supported by selected integration)
  ✓ TTS
```

Capability detection should primarily come from the server's provider registry, not from guessing based on API-key contents.

The credential flow should:

1. identify provider,
2. validate key,
3. retrieve/verify supported capabilities where practical,
4. register available capabilities,
5. make them selectable in the agent wizard.

---

# 42. Provider Capability Registry

The server should maintain structured provider metadata.

Conceptually:

```text
Provider
├── authentication requirements
├── STT capability
│   └── models / languages / options
├── LLM capability
│   └── models / tool support / options
└── TTS capability
    └── voices / models / languages / options
```

The UI should be generated from this metadata where reasonable rather than hardcoding every provider form.

---

# 43. Model and Voice Discovery

Where provider APIs support discovery, the server may retrieve:

- models,
- voices,
- languages,
- capability metadata.

Where provider APIs do not support discovery, provider modules may expose static metadata.

The UI should present only options relevant to the selected provider/capability.

---

# 44. Onboarding

V2 must include first-run onboarding.

Minimum onboarding goal:

> Get from a fresh server to a working test conversation with minimal configuration.

Suggested flow:

```text
Welcome
 ↓
Add provider credentials
 ↓
Check available STT / LLM / TTS coverage
 ↓
Prompt user for missing capability
 ↓
Create first agent
 ↓
Open test console
```

The UI should clearly show capability readiness:

```text
STT   ✓ Google
LLM   ✓ xAI
TTS   ✓ xAI
```

or:

```text
STT   ✓
LLM   ✓
TTS   missing
```

The server should not present itself as ready for voice use until the minimum stack can be satisfied.

---

# 45. Server Configuration View

Deployment/server configuration remains file/environment driven in V2.

The UI should expose a **read-only Server Configuration** view.

Examples:

- listen address,
- storage driver,
- database target (sanitized),
- VAD executor mode,
- session limits,
- admission-control settings,
- tracing configuration,
- log level,
- enabled transports,
- build/version.

The UI must not imply that changing these values in the agent UI changes process/deployment configuration.

This preserves a clean boundary:

```text
deployment config → ops/admin
agent config      → product/UI
```

---

# 46. Trace Persistence

The Server implements the Framework tracing interface.

It persists selected spans/events to storage.

Persisted trace data should support:

- session timeline,
- turn timeline,
- provider latency,
- tool calls,
- errors,
- interruptions,
- audio-state events,
- correlation between nested work.

The server controls trace retention.

---

# 47. Session History

Persist session-level metadata.

At minimum:

- session ID,
- agent ID/version,
- started at,
- ended at,
- duration,
- status,
- provider stack,
- turn count,
- error summary,
- latency summary.

Text content should be governed by configurable data-retention/privacy policy.

---

# 48. Turn History

Persist per-turn metadata when tracing/history is enabled.

Potential fields:

- turn ID,
- speech start/stop,
- final transcript,
- assistant response,
- STT latency,
- LLM latency,
- TTS latency,
- total latency,
- interruption status,
- tools invoked.

Storage of transcripts/assistant content must be configurable.

---

# 49. Tool Call Persistence

When tools execute, persist traceable metadata such as:

- tool name,
- turn,
- started/completed timestamps,
- duration,
- status,
- sanitized arguments,
- sanitized result summary,
- error.

Arguments/results require data-safety/redaction controls.

---

# 50. Session Trace UI

The UI should provide a session detail screen.

Conceptually:

```text
Session
├── metadata
├── agent/version
├── provider stack
├── latency summary
└── timeline
    ├── user speech
    ├── STT
    ├── LLM
    ├── tool call
    ├── TTS
    ├── interruption
    └── errors
```

This should be one of the key V2 product features.

---

# 51. Timeline Visualization

Display trace spans in chronological order.

Useful metrics include:

```text
speech started
VAD decision
STT first partial
STT final
LLM first token
tool started
tool complete
TTS first audio
assistant playback start
interruption
```

The UI should make latency bottlenecks visually obvious.

---

# 52. Agent Detail Test and Debug Experience

The Agent detail page must include an embedded real-time test and debugging experience.

V2 should **not** build a separate bespoke voice playground when the official Pipecat client/UI ecosystem already provides the required interaction primitives.

The primary agent test surface should be built from the official Pipecat UI components and client SDKs, adapted to the Server's RTVI-compatible transport.

The test experience must run against the same production voice path used by normal clients:

```text
Agent Admin UI
    ↓
Pipecat client / UI components
    ↓
RTVI-compatible transport
    ↓
Voice Server
    ↓
Agent runtime
    ↓
normal framework pipeline
```

There must not be a simplified test-only execution path.

## 52.1 Agent Detail Layout

A useful Agent detail page should combine:

```text
Agent configuration
Agent readiness
Provider stack
MCP tools
Live test/debug panel
Recent sessions
```

The live panel should be available directly on the Agent page so an administrator can modify an agent and test it without navigating to a separate application.

## 52.2 Pipecat UI Integration

Use the official Pipecat UI/client components where they satisfy the requirement.

The embedded test experience should support at minimum:

- connect/disconnect,
- microphone input,
- microphone/device state,
- user audio visualization,
- assistant audio playback,
- live conversation/transcript,
- user/assistant speaking state,
- session state,
- interruption/barge-in,
- basic live metrics,
- RTVI lifecycle events.

The Server UI may style and compose these components to match the admin product, but should avoid reimplementing equivalent voice-client functionality unnecessarily.

## 52.3 Debugging Console

Where practical, include Pipecat UI's debugging-oriented components/blocks in the Agent detail experience.

The debugging panel should make useful live session information visible during a test, for example:

```text
connection state
agent state
user speech state
bot speech state
transcriptions
assistant text
latency / metrics
RTVI events
errors
```

The exact component composition should be decided in the implementation plan based on the current Pipecat UI package/registry.

## 52.4 Framework Trace Integration

Pipecat UI provides the real-time client/testing surface.

The Server's own Framework tracing remains the authoritative server-side execution trace.

During an Agent test session, the UI should correlate the active client session with the Server trace so an administrator can move from:

```text
live test
    ↓
completed session
    ↓
full server trace
```

without manually finding the session.

After the test ends, provide a direct action such as:

```text
View Full Trace
```

which opens the persisted session timeline containing:

- VAD,
- STT,
- LLM,
- MCP tool calls,
- TTS,
- interruptions,
- provider errors.

## 52.5 Whisker / Low-Level Pipeline Debugging

Pipecat Whisker is a lower-level debugger focused on pipelines, processors, frames, jobs, and message flow.

Whisker compatibility is a **formal V2 target**, not merely an optional UI enhancement.

The Go framework/server should expose a debug-event stream compatible with the relevant Whisker protocol/expectations where practical, while keeping Whisker-specific concerns outside the core FrameProcessor API.

The Agent detail page may offer an advanced mode such as:

```text
Voice Test
Pipeline Debug
```

where Pipeline Debug embeds, links to, or interoperates with Whisker.

The distinction is:

```text
Pipecat UI / RTVI client
    → test the agent as a user experiences it

Framework traces
    → inspect server-side timing and execution history

Whisker
    → inspect low-level frame/pipeline behavior and verify debug-protocol compatibility
```

These should complement rather than replace one another.

### Whisker Compatibility Suite

V2 must include a compatibility/conformance suite covering the observable behaviors Whisker depends on.

At minimum test scenarios should cover:

```text
session start
audio frame flow
user speech start/stop
STT interim/final frames
LLM output frames
TTS lifecycle/audio frames
barge-in/interruption
flush behavior
tool call/result flow
provider errors
session teardown
```

For each scenario, verify as applicable:

- emitted event/message types,
- ordering,
- required fields,
- stable identifiers/correlation,
- frame direction,
- processor/pipeline relationships,
- lifecycle boundaries,
- serialization format,
- Whisker acceptance/rendering.

Compatibility should be **semantic**, not byte-for-byte, unless the relevant protocol explicitly requires exact serialization.

Where practical, the same scenario should be run against:

```text
Pipecat reference implementation
Go runtime
```

and their externally observable behavior compared.

The goal is to catch regressions where the Go rewrite drifts from Pipecat-compatible pipeline/debug semantics while still allowing intentional Go-specific runtime internals.

Whisker compatibility should run in CI alongside:

```text
core framework tests
provider contract tests
RTVI compatibility tests
Pipecat behavioral parity tests
```

## 52.6 No Dedicated Standalone Test UI Requirement

A separate standalone testing application is not required for V2.

The Agent detail page is the canonical administration/test experience.

Standalone examples or developer clients may still exist for framework development and compatibility testing.

---

# 53. Agent Configuration UI

The agent editor should expose high-level voice-agent concepts.

At minimum:

## General

- name,
- description,
- enabled state.

## Prompt

- system prompt.

## STT

- provider credential,
- model,
- language,
- supported provider options.

## LLM

- provider credential,
- model,
- generation options,
- tools.

## TTS

- provider credential,
- model,
- voice,
- language,
- speed/style where supported.

## Voice Behavior

- interruption,
- turn detection,
- silence timeout,
- greeting,
- maximum session duration.

Avoid exposing low-level runtime implementation details such as goroutine counts.

---

# 54. MCP as the Server Tool Integration Model

Server V2 should use the Model Context Protocol (MCP) as the first-class product integration model for externally configured tools.

The Server should **not** invent a parallel declarative HTTP-tool DSL in V2.

The architectural boundary is:

```text
Framework
    ToolLoop
       ↓
    ToolRegistry
       ↓
    provider-neutral ToolHandler
       ↑
---------------- server boundary ----------------
       ↑
    MCP Tool Wrapper
       ↓
    MCP Client
       ↓
    Configured MCP Server
```

Core understands tools.

Server understands MCP.

Provider modules understand how to expose framework tool definitions to their LLM provider.

MCP-specific types must not leak into the core tool-loop APIs.

---

## 54.1 MCP Server as a Persistent Domain Object

A configured MCP server is a reusable server-level resource.

Conceptually it should contain:

```text
MCPServer
  id
  name
  description
  transport
  connection configuration
  credential reference
  enabled state
  validation state
  last connected at
  last discovered at
  created at
  updated at
```

The exact persistence schema belongs in the implementation plan.

An MCP server is configured once and may be used by many agents.

Example:

```text
Shopify MCP
   ├── Support Agent
   ├── Returns Agent
   └── Sales Agent
```

Credentials and connection settings belong to the MCP server resource rather than being duplicated into each agent.

---

## 54.2 MCP Transports

V2 should support the MCP transports required for the chosen production use cases.

At minimum the implementation plan should evaluate:

```text
remote MCP server
local/process MCP server
```

Remote MCP should be the default product path.

Local/process MCP should be admin/config gated because it introduces arbitrary-process and filesystem security concerns.

The UI must not allow untrusted users to configure arbitrary local executables.

---

## 54.3 MCP Discovery

When an MCP server is added or refreshed, Server V2 should:

```text
connect
  ↓
initialize MCP session
  ↓
discover tools
  ↓
validate tool metadata/schema
  ↓
persist discovery snapshot
```

For every discovered tool, retain enough metadata to present and later wrap it:

```text
MCPTool
  server ID
  name
  description
  input schema
  schema hash/version marker
  discovered at
  current availability
```

Discovery metadata is server product data.

It is not part of the core framework.

---

## 54.4 MCP Server Management View

The management UI must include a dedicated **MCP Servers** area.

Top-level navigation should treat MCP as a reusable integration surface alongside Agents and Providers.

A server detail view should show at minimum:

```text
Name
Connection / transport
Status
Authentication state
Last validation
Last discovery
Attached agents
Discovered tools
```

The page should allow:

```text
Connect / Validate
Refresh Tools
Disable
Delete
```

depending on the server state and user permissions.

---

## 54.5 MCP Tool Catalog

An MCP server detail page must show its discovered tools.

For every tool, display:

- name,
- description,
- input schema,
- availability,
- schema/discovery status,
- agents currently using it where useful.

The UI should make tool discovery inspectable rather than treating it as hidden runtime metadata.

---

## 54.6 MCP Tool Validation / Test Console

Server V2 must provide a way to manually execute discovered MCP tools outside an agent conversation.

This is a required V2 feature.

From an MCP server detail page, the user should be able to select a tool and open a dedicated test panel.

The test panel should:

1. render an input form from the tool's discovered input schema where practical,
2. allow raw JSON input as an advanced mode,
3. validate arguments locally where possible,
4. execute the tool through the same MCP client/execution path used by agents,
5. display execution status,
6. display duration,
7. display the sanitized result,
8. display structured MCP/server errors,
9. create a trace/span for the test execution.

Conceptually:

```text
MCP Server: Shopify

Tool: get_order

Arguments
{
  "order_id": "12345"
}

[ Execute ]

Status: Success
Duration: 327 ms

Result
{
  ...
}
```

The purpose is to distinguish:

```text
MCP connectivity/tool problem
```

from:

```text
LLM/tool-selection/agent problem
```

before the tool is attached to an agent.

Tool test executions must obey the same:

- authentication,
- timeouts,
- response-size limits,
- redaction rules,
- network/security policy

as normal agent tool execution.

---

## 54.7 MCP Validation State

An MCP server should expose a clear validation state.

Example states:

```text
Not Validated
Healthy
Authentication Failed
Connection Failed
Discovery Failed
Tool Schema Invalid
Disabled
Needs Refresh
```

Validation should include:

- connection establishment,
- protocol initialization,
- tool discovery,
- tool-schema parsing,
- optional explicit test execution initiated by the user.

A successful connection alone must not imply that every discovered tool has been manually tested.

---

## 54.8 Agent Tool Selection

Agents select tools from configured MCP servers.

The agent editor should present available tools grouped by MCP server.

Example:

```text
Tools

Shopify MCP
  ✓ get_order
  ✓ search_orders
  ☐ refund_order

CRM MCP
  ✓ find_customer
  ☐ delete_customer
```

An agent may select:

- no tools,
- some tools from one MCP server,
- tools from several MCP servers.

Tool selection should reference stable server/tool identities rather than copying the complete MCP configuration into the agent.

---

## 54.9 Agent MCP Associations

Persist explicit agent-to-MCP-tool associations.

Conceptually:

```text
AgentMCPTool
  agent/version ID
  MCP server ID
  tool identity
  selected schema hash
  enabled
  optional policy overrides
```

The exact schema belongs in the implementation plan.

Agent configuration snapshots must preserve which MCP tool schema/configuration a session was created against.

---

## 54.10 Agent Runtime Wiring

When an agent runtime is constructed, Server V2 should:

```text
load agent snapshot
   ↓
resolve selected MCP servers/tools
   ↓
validate availability
   ↓
construct MCP tool wrappers
   ↓
register wrappers in Framework ToolRegistry
   ↓
start session
```

From the Framework ToolLoop's perspective, an MCP-backed tool is just another framework-native tool handler.

Conceptually:

```text
LLM
 ↓
ToolCallFrame
 ↓
Framework ToolLoop
 ↓
MCP Tool Wrapper
 ↓
MCP Client
 ↓
tools/call
 ↓
MCP Tool Result
 ↓
ToolResultFrame
 ↓
LLM
```

---

## 54.11 MCP Tool Schema Changes

Tool discovery must not assume schemas are immutable.

When refreshed discovery shows that a selected tool's schema has changed:

- record the new discovery snapshot,
- compare the schema identity/hash with what agent versions reference,
- do not silently rewrite historical agent versions,
- mark affected current agent configurations as needing review where the change may be incompatible.

The UI should surface:

```text
Tool schema changed
Tool disappeared
MCP server unavailable
```

as actionable agent readiness information.

---

## 54.12 MCP Server Availability and Agent Readiness

An agent using MCP tools should expose tool readiness separately from the minimum STT/LLM/TTS voice-stack readiness.

Possible states:

```text
Ready
Ready with tool warning
Tool dependency unavailable
Needs tool review
```

If a selected MCP tool is marked **required**, an unavailable tool may make the agent not ready for new sessions.

If it is optional, the agent may remain voice-ready but degraded.

The exact required/optional policy should be resolved in the implementation plan.

---

## 54.13 MCP Server Disable

Disabling an MCP server is the preferred reversible operation.

Disable behavior:

- server configuration remains persisted,
- discovered tool catalog remains visible,
- new agent sessions must not receive wrappers for disabled tools,
- affected agents show degraded/not-ready status according to tool policy,
- existing active sessions may continue using their already-created runtime snapshot until they end, unless an administrator explicitly terminates them.

This provides a safe operational alternative to deletion.

---

## 54.14 MCP Server Deletion and Referential Integrity

MCP server deletion must never silently break agents.

### Default Delete

If an MCP server is referenced by any current agent configuration, normal deletion must be blocked.

The API should return a dependency/conflict result and the UI must list affected agents.

Conceptually:

```text
Cannot delete Shopify MCP

Used by:
- Support Agent
- Returns Agent
- Sales Agent

[ Cancel ]
[ Disable Instead ]
[ Detach and Delete... ]
```

### Detach and Delete

An explicit destructive flow may be provided.

It must:

1. show every affected agent,
2. show every selected tool that will be removed,
3. require explicit confirmation,
4. create/update agent configurations so the MCP tools are no longer selected,
5. stop assigning the MCP server to new sessions,
6. preserve historical agent/session snapshots,
7. allow active sessions already using the MCP server to drain,
8. delete the live MCP configuration only after no active runtime depends on it.

Affected agents should be marked for review/degraded where appropriate rather than silently treated as unchanged.

### Active Sessions

Hard deletion must not invalidate in-flight tool handlers.

If active sessions reference the MCP server, deletion should become:

```text
pending deletion
```

until those sessions finish, or require an explicit administrative force action that terminates/invalidates those sessions.

The default UI flow should drain rather than force.

### Historical Data

Deleting an MCP server configuration must not delete historical session/tool-call evidence.

Historical traces should retain immutable/snapshotted metadata such as:

```text
MCP server display name
tool name
tool schema/version identity
execution timing/status
```

Historical records should not require the live MCP-server row to continue existing.

### Database Referential Integrity

The persistence model should enforce these rules with explicit foreign-key/repository semantics.

Do not use cascading deletion from MCP servers into agents, sessions, traces, or tool-call history.

---

## 54.15 MCP Tool Tracing

MCP tool execution must participate in the Framework tracing model.

A tool span should include safe metadata such as:

```text
tool name
MCP server identity/name
turn ID
start/end
duration
status
error category
```

Arguments and results are controlled by the configured trace/privacy policy.

A normal session trace may show:

```text
Turn
 ├── STT
 ├── LLM
 ├── MCP Tool: get_order
 │    ├── MCP server: Shopify
 │    ├── execute
 │    └── result
 ├── LLM
 └── TTS
```

---

## 54.16 MCP Security

MCP integration must honor the same security posture as other server integrations.

At minimum:

- credentials remain server-side,
- credentials are never exposed to agents/clients,
- secrets are redacted from traces and errors,
- remote destinations follow server network policy,
- local/process MCP is admin/config gated,
- tool inputs/results have configurable size limits,
- execution has explicit timeouts,
- tool cancellation propagates where supported.

The implementation plan should include a threat review for remote and local MCP transports.

---

# 55. Server Authentication

V2 should introduce management authentication before being treated as production-grade.

At minimum protect:

- management UI,
- agent management APIs,
- credential APIs,
- session trace APIs,
- server administration APIs.

The exact auth approach is implementation-plan territory.

Public/client voice connections may use separate agent/session tokens.

---

# 56. Client Connection Tokens

Avoid exposing server-management credentials to voice clients.

The server should support short-lived or scoped client connection credentials for testing/production connections.

RTVI client compatibility must remain intact.

---

# 57. API-Key Security

Provider secrets must:

- be encrypted at rest where practical,
- never be returned after creation,
- be masked in UI,
- never appear in logs/traces,
- be scrubbed from errors,
- be inaccessible to normal voice clients.

---

# 58. Data Retention

Server V2 needs configurable retention for:

- sessions,
- traces,
- transcripts,
- tool-call payloads.

Example policies:

```text
metadata only
7 days
30 days
custom
```

Retention policy design should distinguish metadata from conversation content.

---

# 59. Privacy Controls

Allow operators to configure whether the server stores:

- transcripts,
- assistant text,
- tool arguments,
- tool results.

Operational timing/trace metadata should remain useful even when content persistence is disabled.

---

# 60. Dashboard

The initial UI dashboard should show operationally useful information.

Examples:

- active sessions,
- sessions over time,
- success/error rate,
- median/p95 latency,
- provider errors,
- VAD pressure,
- configured agents.

Avoid building a broad analytics product in V2.

---

# 61. Sessions View

Provide:

- active sessions,
- recent completed sessions,
- filter by agent,
- status,
- provider,
- errors,
- date/time.

Allow opening a session trace.

---

# 62. Providers View

Provide:

- configured credentials,
- provider,
- detected capabilities,
- validation state,
- last validation,
- edit/delete controls.

Do not expose secret values.

---

# 63. Agents View

Provide:

- agent list,
- status,
- provider stack,
- updated time,
- test action,
- duplicate action,
- edit action,
- archive/delete.

---

# 64. Server View

Read-only presentation of process/deployment configuration.

Also show runtime information such as:

- version,
- uptime,
- Go/runtime version,
- build information,
- database mode,
- VAD executor,
- active session capacity,
- health.

---

# 65. Health and Readiness

Expand V1 health endpoints.

Readiness should consider required server dependencies such as:

- database connectivity,
- migrations complete,
- VAD executor available,
- management storage available.

Provider credentials should generally affect agent readiness, not necessarily whole-server readiness.

---

# 66. Agent Readiness

Agents should expose their own readiness.

For example:

```text
Ready
Missing TTS credential
Invalid xAI key
Selected model unavailable
Provider validation stale
```

The UI should surface actionable reasons.

---

# 67. Persistence Failure Behavior

Database problems must not corrupt running voice sessions.

Define explicit behavior for:

- database unavailable during session creation,
- trace persistence failure,
- history write failure,
- agent lookup failure,
- credential lookup failure.

Trace/history persistence should generally not crash active sessions.

Agent/credential loading failures may prevent new sessions.

---

# 68. Config and Persistence Boundary

Server deployment configuration remains authoritative for infrastructure/runtime settings.

Persistent storage is authoritative for product-managed resources.

## File/environment controlled

Examples:

- listen address,
- database connection,
- VAD executor,
- server/session capacity,
- encryption keys,
- tracing retention defaults,
- log level.

## Database/UI controlled

Examples:

- agents,
- provider credentials,
- prompts,
- provider/model/voice selections,
- tool definitions,
- voice behavior.

The boundary must be documented.

---

# 69. Import from V1 Config

Provide a migration path from V1 config-defined agents.

At minimum, Server V2 should be able to create/import an agent from an existing V1 pipeline configuration.

This avoids forcing users to reconstruct configurations manually.

> **Superseded:** YAML `pipelines:` has since been removed and is rejected at startup, and the `/api/v1/agents/import-v1` route was removed with it. Calls run stored agents only; create them with `POST /api/v1/agents` (schema v2, see `docs/api/v1.md`).

---

# 70. API Versioning

Management/product APIs should begin an explicit versioning scheme if not already present.

For example:

```text
/api/v1/agents
/api/v1/sessions
/api/v1/providers
```

RTVI remains governed by RTVI protocol compatibility.

---

# 71. UI Technology

The exact frontend stack is not specified here.

Requirements:

- browser-based,
- responsive enough for development/operations,
- microphone access for agent testing,
- compatible with RTVI client behavior,
- typed API client generation or equivalent strongly preferred.

The UI should remain a separate product layer rather than embedding product concepts into core Go packages.

---

# 72. Production Deployment

V2 should ship a straightforward production deployment story.

At minimum:

- production Docker image,
- SQLite single-node mode,
- PostgreSQL mode,
- environment-based configuration,
- health/readiness,
- graceful shutdown,
- migration command/startup behavior,
- persistent volume guidance,
- reverse-proxy/TLS guidance.

Kubernetes-specific automation may remain documentation-level.

---

# 73. Backup / Recovery

For production persistence, document:

- SQLite backup expectations,
- PostgreSQL backup expectations,
- credential-encryption-key handling,
- migration rollback/forward strategy.

A full backup service is not required.

---

# 74. Observability Integration

The framework tracer should be adaptable to standard tracing systems.

Server V2 should support:

- its own persisted traces for UI,
- optional OpenTelemetry export.

These may run simultaneously.

The framework must not require OpenTelemetry directly if a generic tracing interface is sufficient.

---

# 75. V2 Framework Release Criteria

Framework V2 is complete when:

- AI tracing interface exists,
- turn IDs and turn lifecycle are formalized,
- interruption behavior is hardened,
- audio flush behavior is deterministic,
- silence detection exists,
- turn detection is pluggable,
- silence/no-speech processors exist,
- transcript stabilization/deduplication utilities exist,
- text/TTS aggregation processors exist,
- audio resampling exists,
- audio format/channel conversion exists,
- bounded audio buffering and pre-roll are reusable,
- output pacing exists,
- interruption/flush processor behavior is deterministic,
- context aggregation handles interrupted/tool turns correctly,
- provider-neutral tool definitions exist,
- tool loops work with Google and xAI,
- tool loops support limits/cancellation/timeouts,
- standard trace events exist,
- Pipecat audio/call parity matrix is updated,
- all P0 audio/call parity capabilities are implemented or explicitly waived with rationale,
- P1/P2 parity capabilities are tracked with implementation status,
- behavioral eval harness covers all P0 conversational correctness scenarios,
- RTVI compatibility suite passes,
- Whisker compatibility/conformance suite passes,
- intentional compatibility differences are documented,
- performance regressions remain within agreed limits.

---

# 76. V2 Server Release Criteria

Server V2 is complete when:

- SQLite works,
- PostgreSQL works,
- schema migrations exist,
- agents are persistent,
- agents can be created/edited through UI,
- provider credentials can be added securely,
- provider capabilities are detected,
- onboarding verifies STT + LLM + TTS availability,
- agent readiness is visible,
- Agent detail embeds the official Pipecat-based voice test/debug experience,
- Agent testing exercises the real production RTVI voice path,
- completed Agent tests link directly to persisted server traces,
- session history exists,
- framework traces are persisted,
- trace timeline is visible in UI,
- tool calls appear in traces,
- MCP servers are persistent first-class resources,
- MCP server connection/discovery validation works,
- MCP tools are inspectable in a dedicated UI,
- discovered MCP tools can be manually executed from the MCP test console,
- agents can select tools across configured MCP servers,
- MCP tool wrappers are wired into Framework ToolLoop at agent/session construction,
- MCP schema disappearance/change is surfaced to affected agents,
- MCP server deletion is dependency-safe and preserves historical traces,
- server config is visible read-only,
- management APIs are authenticated,
- credential secrets are protected,
- configurable data-retention/privacy behavior exists,
- graceful production deployment works.

---

# 77. V2 End-to-End Demo

A fresh deployment should support the following flow.

## First Run

```text
start server
→ open UI
→ onboarding
→ add xAI credential
→ server detects LLM/STT/TTS capabilities
→ create agent
→ choose models/voice
→ save
```

## Test

```text
open agent detail
→ use embedded Pipecat voice/debug UI
→ open MCP Servers
→ validate configured MCP server
→ inspect discovered tools
→ manually execute one tool successfully
→ attach selected MCP tool(s) to agent
→ open agent test console
→ connect through normal RTVI transport
→ speak
→ receive response
→ interrupt response
→ trigger MCP-backed tool call
→ continue conversation
```

## Inspect

```text
open completed session
→ inspect transcript if enabled
→ inspect VAD/STT/LLM/tool/TTS timeline
→ inspect latency
→ inspect interruption
→ inspect tool input/result
```

## Operate

```text
edit agent
→ new sessions use updated configuration
→ existing sessions retain their configuration snapshot
```

The same framework must continue to be usable independently by Go applications without the Server product.

---

# 78. Explicitly Deferred Beyond V2

Unless required to complete the V2 goals, defer:

- PSTN/telephony,
- SIP,
- Twilio,
- call-center integrations,
- multi-agent orchestration,
- visual arbitrary pipeline builder,
- hosted SaaS control plane,
- billing,
- organizations/teams unless required by auth design,
- full RAG/knowledge-base product,
- workflow engine,
- MCP marketplace,
- provider marketplace,
- distributed session migration,
- global edge routing,
- autoscaling control plane,
- video/vision agents.

V2 should prioritize making the voice runtime and server product excellent rather than broad.

---

# 79. Implementation Plan Follow-Up

This specification intentionally defines **what V2 must achieve** without fully prescribing task-level implementation.

The implementation plan should next resolve:

1. exact Pipecat audio/call parity target and priority-tagged V2 processor/capability catalog,
2. dependency ordering across P0/P1/P2 parity items,
3. processor/observer/tracer/serializer interface boundaries,
4. exact trace interfaces and event schema,
5. tool-loop implementation strategy and Go library evaluation,
6. turn-management and turn-detector implementation choices,
7. audio filter/noise suppression implementation,
8. resampler/audio utility choices,
9. STT commit/finalization policy,
10. behavioral eval harness design and fixtures,
11. database library and migration system,
12. persistence schema,
13. credential encryption model,
14. authentication model,
15. provider capability metadata format,
16. API structure,
17. UI stack,
18. trace storage/retention design,
19. V1 agent-config import path,
20. MCP client/library selection and supported transports,
21. MCP persistence/discovery schema,
22. MCP validation/test-console execution model,
23. MCP deletion/drain/referential-integrity semantics,
24. Pipecat UI component integration strategy for the Agent detail page,
25. Whisker debug-protocol compatibility target and event mapping,
26. Whisker conformance-test fixtures/reference scenarios,
27. RTVI vs Whisker compatibility boundaries,
28. staged milestones and dependency order.

The plan should keep Framework V2 and Server V2 as separate workstreams with explicit integration points.

The Framework V2 plan should schedule P0 parity items first unless an explicit dependency justifies a different order.
