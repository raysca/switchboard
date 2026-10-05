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

## Correction round 1

Independent review found seven commit-invariant gaps. RED regressions covered
the late-final three-unit `ack 1 + ack 3` case, direct tool-message bypass,
post-cancel frame emission, typed mutable metadata, conflicting tool-result
statuses, phantom eviction accounting after rollback, and interrupted partial
commit rejection. The fixes make both assistant finality paths use the all-unit
barrier; reject LLM output while no user turn is active; centralize tool writes;
retain result status; recursively copy bounded JSON-safe metadata and reject
unsupported values; restore messages plus all eviction accounting; and return
partial-commit failures before emitting a commit frame.

`go test -race -count=50 -run 'Test(Context|AssistantContext|InterruptedAssistant)' ./core`: PASS.

The cancellation fix requires an actual user-turn boundary before model output.
Production Google and xAI processors already forward `UserMessageFrame`; the
fake LLM was aligned with that contract so integration tests no longer depended
on accepting output with an empty active turn.

Correction verification: full core race/vet, Google race/vet, xAI-tagged
provider race/vet, root xAI-tagged race/vet, and RTVI/WebSocket integration all
PASS with the documented SDK override where CGO is involved.

## Correction round 2

RED covered a generationless V1 turn whose `Cancel(current)` missed generation
zero, exact/conflicting tool replay after the original messages were evicted,
invalid raw JSON and nested non-finite metadata, and a rollback case with an
actually eligible older completed turn. The recommended policy is to normalize
V1 generation zero at the user-turn boundary, retain tool replay identities in
a configurable bounded ledger (default 256), and fail closed when that ledger
is saturated rather than forget a replay fence. Metadata validation is atomic
and all accepted values remain JSON-marshalable.

Round 2 verification: focused context race x50, full core race/vet, Google
race/vet, xAI-tagged provider race/vet, and root xAI-tagged race/vet including
RTVI/WebSocket integration all PASS with the documented SDK override where
required.

## Correction round 3

RED reproduced two pending-call eviction holes: the replay ledger authorized a
first result after its call message was gone, and budget enforcement could evict
the newly completed exchange while returning success. The chosen policy keeps
only completed call/result identities authoritative after message eviction.
Pending calls require a retained call message, and first-result insertion is a
transaction that restores messages plus eviction accounting if the call/result
exchange does not remain in context.

Round 3 verification: focused context race x50, full core race/vet, Google
race/vet, xAI-tagged provider race/vet, and root xAI-tagged race/vet including
RTVI/WebSocket integration all PASS.

## Correction round 4

RED: `TestContextToolCallTransactionRejectsSelfEviction` reproduced the
`MaxMessages: 3` case: user t1, assistant t1, user t2, then a tool call for
t1 returned nil even though insertion evicted that same call. GREEN: call
insertion now verifies the new call remains in history before reserving its
replay identity. Otherwise it restores messages, eviction count, and evicted
turn pairs. The regression retries the rejected call, checks unchanged
accounting, then reuses its ID and the sole ledger slot for a retained t2
call/result exchange.

Verification: focused context race x50; full core race/vet with the documented
Xcode SDK override; Google race/vet; xAI-tagged provider race/vet; root
xAI-tagged race/vet; and transport race tests all PASS.

## Independent closeout

- Final independent review: **APPROVE** for `daf5fa6..1425276`. The reviewer
  verified transactional tool-call insertion, unchanged eviction accounting,
  replay-ID reuse, related result rollback, completed replay after eviction,
  conflicting status rejection, and concurrent replay under the race detector.
- Final independent test: **PASS** at `1425276`. Focused context race tests
  passed 50 repetitions; the self-evicted-call regression passed 100; sibling
  replay/result/saturation cases passed 50; and full core race/vet passed.
  Prior correction rounds also passed the provider, tagged-root, and transport
  integration suites affected by shared contract changes.
- Task 10 is complete. ToolLoop execution, provider adapters, and context
  summarization/compaction remain assigned to Tasks 11, 12, and 28.
