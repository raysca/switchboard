# Task 11 implementation report

## Scope and decisions

- Added `core/tools.ToolLoop` on Task 1's public tool aliases and Task 10's
  `ConversationContext`. The adapter supplies the first call batch and an
  optional continuation callback; core executes calls and returns ordered
  results without issuing provider requests.
- Calls are sequential in input order, including batches that could be
  parallelized. This keeps context ordering and interruption behavior
  deterministic. Cost if wrong: independent slow tools add latency; parallel
  scheduling would need a new policy for cancellation and commit order.
- Maximum rounds, total calls, argument/result byte sizes, JSON depth, per-call
  timeout, and per-round timeout are explicit and positive. Defaults are four
  rounds, sixteen calls, 32 KiB arguments, 64 KiB results, depth 16, 30 seconds
  per call, and two minutes per round.
- Extended `ConversationContext` with an atomic tool-call claim. One loop can
  execute a new call, exact completed replay returns a copied stored result,
  conflicting reuse fails, and an in-flight replay is rejected. The existing
  bounded context ledger owns replay capacity; there is no second loop-local
  identity table.
- Context stores the call before handler execution and the normalized result
  before continuation. A call insertion failure rolls back context and does
  not invoke the executor. If result insertion fails after a handler ran, the
  pending identity remains fenced and future replay does not execute a side
  effect twice; the continuation is not called. Cost if wrong: the caller must
  treat that exchange as failed/retired because core cannot recover the handler
  result after context rejected it.
- Executors and continuation callbacks run synchronously and receive contexts
  with per-call/per-round deadlines. They must honor cancellation; core avoids
  detached per-call goroutines that cannot be forcibly stopped and could leak.
  Cost if wrong: a non-cooperative application callback can still block its
  caller. Applications needing isolation must supply it in their executor.
- The optional generation predicate is application-owned and does not expose
  SessionController to tools. Trace correlation uses the existing TraceEvent
  identity and metadata-only policy; no provider or server type is introduced.
- Tool schemas remain `json.RawMessage` and constructor validation checks
  that a schema is valid JSON. This task validates argument JSON shape, size,
  and depth, but not full JSON Schema conformance; Task 12 provider mapping and
  application validation own that adapter behavior. Cost if wrong:
  malformed-but-valid arguments can reach a handler unless its application
  validates the schema.

## Library evaluation

Reviewed official project sources on 2026-10-05. LangChainGo v0.1.14 includes
tools and an agent/executor runtime with context-bearing planning, but its
chain/provider ownership is too broad for core. The official Google Gen AI Go
SDK changelog listed v1.72.0 on 2026-10-01; it is useful for Google-specific
function schemas but does not supply this common runtime boundary. Decision:
native implementation, no dependency or `go.mod` change. Full rationale and
links are in `docs/decisions/tool-loop.md`.

## TDD evidence

- RED: focused `TestToolLoop` first failed to compile because ToolLoop,
  config, typed errors, and the context claim API did not exist.
- RED: `TestContextClaimToolCallElectsOneExecutorAndReturnsCompletedReplay`
  failed to compile on the missing `ClaimToolCall` method.
- GREEN: focused suites passed after implementing sequential calls, bounded
  results, callback continuation, atomic claim/replay, and cancellation:
  `cd core && go test -run 'Test(ToolLoop|ContextClaimToolCall)' ./tools .`.
- `cd core && go test -race -count=50 -run TestToolLoop ./tools`: PASS.
- Full core race/vet with the recorded SDK override: PASS.
- Root xAI-tagged race/vet, Google provider race/vet, xAI provider race/vet,
  and RTVI/WebSocket transport race coverage: PASS.
- `gofmt` and `git diff --check`: PASS.

Task 11 remains unchecked pending independent code review and tester review.
