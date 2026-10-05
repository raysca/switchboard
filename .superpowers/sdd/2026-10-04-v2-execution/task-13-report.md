# Task 13 — Framework trace instrumentation and content policy

Base: `69bb069` on `codex/v2-implementation`.

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
- CAVEAT: root `go test -race -count=1 -tags xaispeech ./...` fails in
  Task 14's `TestPinnedPipecatTurnInterruptionPairedBehavior`: the fixture
  expects `started` states but the current Go trace reports only
  `interrupted`/`active`. Task 13 files are not implicated and were left
  untouched.

## Review status

Not independently reviewed. Task 13 remains unchecked pending review.
