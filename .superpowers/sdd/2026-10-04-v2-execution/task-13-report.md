# Task 13 — Framework trace instrumentation and content policy

Base: `69bb069` on `codex/v2-implementation`; fourth correction based on `8db25c7`.

## Scope delivered

- Added session-owned tracing configuration and asynchronous exporter isolation.
  Runtime events carry opaque session, turn, generation, and tool-call
  correlation through session, turn, frame/processor, tool, interruption,
  flush, output, and terminal lifecycles.
- Added metadata-default frame mapping for speech/VAD, STT, context, LLM,
  ToolLoop, TTS, output transport, interruption, and flush paths. Audio is
  represented only by finite counts/format/duration metadata.
- Added safe terminal categories. Raw processor, executor, and session errors
  never reach `Span.End`.
- Added the bounded `AsyncTracer`. Ordinary operations are best effort; an
  accepted start retains exactly one terminal operation so overload cannot leak
  its span. Tracer/redactor panics are contained.
- ToolLoop records a single correlated `tool.execute` span. Optional tool
  arguments pass the existing bounded redactor only after call validation.
  Result payloads are deliberately not emitted before normalization/context
  commit, preventing raw executor output from reaching a trace.
- Documented the privacy admission boundary and async failure behavior in
  `docs/privacy.md` and clarified the runtime contract.

## RED / GREEN

RED:

- Changed the ToolLoop trace assertion to require one execution event and no
  result payload. The partial implementation emitted a second `tool.result`
  event containing a redacted but pre-normalization payload.
- Added a full-queue async test. It failed because an accepted span's `End`
  operation was discarded with ordinary queue work.

GREEN:

- ToolLoop now emits one correlated execution span and does not expose raw or
  unvalidated result bytes.
- The async dispatcher retains terminal operations separately and drains them
  after queued starts, preserving parentage and closing accepted spans exactly
  once.

## Ambiguity decisions

- Metadata is the default for every runtime path. Text is bounded and opt-in;
  raw audio, credentials, arbitrary attributes, raw provider errors, and raw
  tool results are never admitted.
- "Tool results pass explicit redaction" is an admission rule, not a mandate
  to record every result. The initial loop omits result content until it has an
  explicitly authorized post-normalization trace boundary.
- The async queue may drop ordinary trace work to protect voice latency, but
  never drops the terminal operation of an accepted start. Callbacks run
  outside session locks.

## Verification

- PASS: `go test -race -count=30 ./core ./core/tools -run 'Test(SessionTrace|AsyncTracer|ToolLoopTrace|TraceContract)'`.
- PASS: `cd core && SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk go test -race -count=1 ./... && SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk go vet ./...`.
- PASS: `cd providers/google && SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk go test -race -count=1 ./... && SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk go vet ./...`.
- PASS: `cd providers/xai && SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk go test -race -count=1 -tags xaispeech ./... && SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk go vet -tags xaispeech ./...`.
- PASS: `go test -race -count=1 ./transport/...` and `go build -tags xaispeech ./...` with the same SDK override.
- PASS: sentinel scan found no test sentinel or secret-bearing production trace
  value; `gofmt -l` was empty for Task 13 Go files; `git diff --check` passed.
- Historical caveat: an earlier Task 14 parity fixture failed on the root
  tagged race command. The current root tagged test and build commands pass.

## Review status

Correction round 1 addressed independent review findings: custom metadata is
numeric/boolean-only across trace, observer, and debug constructors; terminal
capacity is reserved at start admission; exporter End runs after dispatcher
locks are released; inline emitter frames receive semantic spans; panic
recovery supplies the safe processor category; ToolLoop exporter callbacks are
asynchronous; and Google/xAI retain the ProcessFrame trace context when they
derive cancellable request contexts. Focused race x50 plus full core and
provider race/vet suites passed after this correction. The default NoopTracer
path bypasses trace allocation/defer work entirely; this restored the xAI STT
turn-semantics race stability (20/20 after five timeouts in the prior run).
Task 13 remains
unchecked pending follow-up review.

Correction round 2 additionally restores exactly-once inline semantic origin
delivery by suppressing same-frame pass-through duplicates, carries the inline
semantic span into downstream processor context, and gives inline terminal turn
events the same turn-span close path. ToolLoop now owns one bounded dispatcher
per loop (rather than one per call), exposes `Close`, and both xAI and the
per-request Google loop lifecycle close it. The provider-specific lifecycle
span vocabulary requested by the second review remains pending; existing
provider frame/reentry semantics are not claimed to replace that work.

Round 3 adds `TraceContext` propagation through processor contexts and starts
Google/xAI LLM request spans at their asynchronous request boundaries, with one
first-output event per request. This is intentionally only the first provider
boundary slice; STT/TTS/VAD/transport operation spans remain outstanding and
Task 13 must stay open.

Round 4 fills those operation boundaries. Google and xAI STT span stream open
through their asynchronous receiver, with one first-final event and a single
completion/failure terminal; Google handles a final followed by an interim in
one response. Google and xAI TTS span each synthesis request through emitted
audio, marking first nonempty audio once and ending with a fixed status.
VAD spans submitted detector jobs, records bounded result latency and speech
transitions, and closes failed or rejected jobs. WebSocket/RTVI uses the
session's immutable root trace context for correlated connect, readiness,
disconnect, error, and close events. Google/xAI LLM failures now end their
request spans with fixed categories; xAI's terminal response text can mark
first output. Provider ToolLoops inherit the session tracer and parent context.

Two async dispatcher corrections were required: a completed span proxy keeps
its resolved exporter span for a later child start, and accepted ordinary
operations preceding `End` drain before that terminal callback. The session
turn context now retains the trace configuration when selecting a turn parent.
These issues were reproduced by new tests before implementation.

Round 4 verification: focused race `-count=50` passed for core/ToolLoop,
Google/xAI mocked provider lifecycles, VAD, and WebSocket/RTVI. Full core,
Google, xAI (`xaispeech`), and VAD/WS/RTVI race and vet suites passed with
the MacOSX26 SDK override. Root tagged tests and build passed. The Task 14
files were not edited or staged. Task 13 remains unchecked for independent
review.

## Correction round 5 — trace safety invariants

The seven independent-review findings were reproduced with targeted RED tests
before implementation:

1. A value-type Frame containing a slice panicked when used as a map key.
   Per-envelope origin tracking now keys the boxed interface identity, not
   the concrete Frame value; queued and inline origins each emit once.
2. A forwarded frame installed a noop parent, disconnecting downstream
   processors and provider child spans. It now carries the current context
   unchanged. Three processor levels and a provider child form one chain.
3. A child on another dispatcher could read an unresolved mutable parent
   proxy. Each accepted start now publishes its immutable resolved span via a
   readiness channel; foreign dispatchers wait for publication. This is an
   exporter-worker wait, never a voice runtime wait.
4. VAD Close could leave an accepted detector-job span open. It now marks the
   processor closed and finishes the pending trace as canceled before stream
   teardown; the existing once guard prevents a late callback from ending it
   twice.
5. Generic frame names claimed provider first-output/request/completion
   vocabulary, including every delta or audio chunk. Generic names are now
   `llm.delta`, `llm.text`, `tts.started`, `tts.audio_chunk`, and
   `tts.stopped`; provider operation spans remain the lifecycle owners.
6. Processor and inline-emitter panics could close spans as success. Local
   recovery now ends the affected spans with fixed `processor_error` before
   re-panicking into the session's failure handler.
7. Google/xAI STT opener panics could leave stream spans open, including
   Google's rotation opener. Opener-local guards cancel the stream context,
   emit a safe failure, end the span, and re-panic.

Mocked tests cover each regression under `-race`, including non-comparable
frames, cross-dispatcher parent resolution, VAD's late callback, and panicking
STT openers. The test recorder also now waits for the asynchronous terminal
callback before asserting exact span counts; waiting for the start event alone
was racy. Task 13 remains unchecked for independent review.

Round 5 verification: focused core/ToolLoop and Google/xAI/VAD regression
sets passed `go test -race -count=50` with the MacOSX26 SDK override. Full
core, Google, xAI (`xaispeech`), and VAD/transport package suites passed
`go test -race -count=1` and `go vet`. Root `go test -tags xaispeech ./...`
and `go build -tags xaispeech ./...` passed. `gofmt -l` and
`git diff --check` were clean; the production trace-value scan found no
test sentinels, credentials, or raw-error trace emission. Platform link warnings
about macOS 27 objects targeting the 26.5 SDK were non-fatal.

## Final rejected-parent correction

An async start can be rejected after its dispatcher closes or saturates. Its
proxy publishes readiness with no resolved exporter span. A later accepted
child on another (or the same) dispatcher must start at the trace root while
retaining cancellation and unrelated context values. It must not install a
nil span value over the rejected proxy. The dispatcher now masks only the
span-parent context key in this case. Targeted tests reproduced the prior
nil-value context behavior on both dispatcher paths before the change, then
passed under race `-count=100`. Full core race/vet and focused tagged
Google/xAI lifecycle race suites passed. Task 13 remains unchecked for
independent review.

## Independent closeout

Independent review APPROVED the final implementation at `7114682`. The final
tester passed the rejected-parent race regression for 100 repetitions,
focused trace lifecycle, parentage, panic, content-policy, and privacy stress
suites, full core/provider/VAD/transport race and vet suites, and root tagged
tests and builds using the documented macOS SDK override.

Task 13 is complete. Its scope is framework and provider runtime tracing plus
safe content policy. Provider usage remains Task 32, readiness Task 43, the
general observer API Task 35, and persistence/OTel export Tasks 19–20. Gates C
and F still require the separate independent Phase 3 gate.
