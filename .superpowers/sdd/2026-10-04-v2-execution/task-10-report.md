# Task 10 implementation report

## Scope and decisions

- Reused the Task 1 tool contracts by moving their definitions to `core` and
  preserving `govoice/core/tools` as type aliases. This lets typed pipeline
  frames carry the same values without an import cycle or a private duplicate.
- Represented tool calls as assistant-role messages with `tool_call` kind and
  tool results as tool-role messages with `tool_result` kind. A tool call does
  not complete a turn for eviction; only delivered assistant text does.
- Chose call ID plus turn and generation as the idempotence/correlation key.
  Exact replay is a no-op; conflicting reuse and unmatched results fail closed.
- Kept the existing write-completion barrier as delivery authority. A whole
  assistant turn commits only when all pending units are acknowledged. An
  interruption retains only the contiguous fully delivered prefix. Cancellation
  drops uncommitted state. This intentionally makes no client playback-clear
  claim beyond the existing Gate B decision.
- Kept deterministic eviction as the P0 bounded-context policy. No summary,
  compaction callback, provider-specific tokenization, ToolLoop, or provider
  adapter behavior was added; those remain Tasks 28, 11, and 12.

## TDD evidence

RED command:

`cd core && go test -run 'Test(Context|AssistantContext|InterruptedAssistant)' ./...`

The core package failed to compile because `AddToolCall`, `AddToolResult`,
`RoleTool`, `ToolCallID`, and `ErrUnmatchedToolResult` did not exist. The same
run also encountered the local macOS CGO linker/SDK failure in `core/audio`.

GREEN command:

`cd core && go test -race -count=10 -run 'Test(Context|AssistantContext|InterruptedAssistant)' .`

Result: PASS. Coverage includes copied input/output, ordered system/user/tool
messages, public frame consumption, exact replay and concurrent replay,
unmatched results, budget rollback, out-of-order final write completion,
exactly-once assistant commit, cancellation, and late finality. Existing context
tests cover partial writes, failed output, flush/interruption, stale generation,
repeated turns, committed transcript replay, and deterministic eviction.

## Verification

- `git diff --check`: PASS.
- Focused core race suite, 10 repetitions: PASS.
- An initial full run with the default SDK reproduced the local macOS CGO
  linker/SDK failure. With the repository's documented override,
  `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w`, full
  `cd core && go test -race -count=1 ./... && go vet ./...`: PASS.
- Root `go test -race -count=1 -tags xai ./... && go vet -tags xai ./...`
  with the same linker override: PASS.
- Google provider `go test -race -count=1 ./... && go vet ./...`: PASS.
- RTVI/WebSocket race suites: PASS.

Task 10 remains unchecked pending independent review and testing.
