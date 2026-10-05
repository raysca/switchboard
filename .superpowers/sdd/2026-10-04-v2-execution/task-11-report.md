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

## Correction round 1

The independent reviewer required terminal generation fencing, trace error
sanitization, a result-byte minimum covering every generated fallback, and
accurate Done reporting on errors. RED regressions first failed to compile on
the new stale-call sentinel and byte-minimum contract; the prior suite also
reproduced the stale result behavior and too-small fallback configuration.

GREEN changes recheck the caller's generation after execution and before result
insertion, and reject stale completed replays before returning their cached
payload. A stale outcome is a bounded cancelled result and stops continuation.
Trace spans now end with nil rather than raw handler errors. Configurations
below the 30-byte canonical fallback requirement are rejected. Done begins
false for active loops and changes to true only on successful terminal
completion (or an empty initial request).

Task 12 integration caveat: the provider adapter must call the ToolLoop atomic
claim before projecting the same ToolCall through ContextAggregator, or provide
an explicit way to distinguish already-recorded calls from new claims.
Otherwise the loop sees the context's pending replay and correctly fails
closed. Task 11 leaves ContextAggregator unchanged.

Correction verification:

- `cd core && go test -race -count=50 -run TestToolLoop ./tools`: PASS.
- Full core race/vet with the documented Xcode MacOS26 SDK override: PASS.
- Google provider race/vet: PASS.
- xAI provider race/vet: PASS.
- Root xAI-tagged race/vet: PASS, including RTVI and WebSocket transport tests.
- `gofmt` and `git diff --check`: PASS.

Task 11 and Gate C remain open pending independent re-review and tester.

## Correction round 2

The second independent review found that ending every span with nil discarded
useful outcome classification, and that completed replay bypassed a new loop's
stricter result byte/depth bounds. It also identified that an earlier test's
supposed oversized payload fit within its 30-byte limit.

RED: trace tests observed nil for executor error, panic, timeout, and cancellation;
strict byte/depth replay tests returned the cached payload and invoked
continuation; the oversized test now uses a payload longer than its configured
bound and checks the `result_too_large` category.

GREEN: spans end with nil on success and only safe normalized loop categories on
failure; neither raw executor errors nor handler-provided categories are sent
to tracing. Completed replay revalidates the cached payload against the current
loop's byte/depth policy and fails closed with `ErrReplayResultOutOfBounds`.
This does not re-execute the handler, invoke continuation, or modify committed
context history. The original committed result remains available to loops whose
current bounds accept it.

Task 12 ordering caveat from round 1 remains: the adapter must atomically claim
before ContextAggregator projects the same call, or distinguish prior
projection from a new claim.

Correction round 2 verification:

- `go test -race -count=50 -run TestToolLoop ./core/tools`: PASS.
- Full core race and vet with the documented Xcode MacOS26 SDK override: PASS.
- Google and xAI provider vet/race: PASS.
- Root `-tags xai` race/vet and transport race tests: PASS. The first root
  attempt without the SDK override hit the known MacOS27 command-line tools
  linker incompatibility; the same matrix passed with the documented override.
- `gofmt` and `git diff --check`: PASS.

## Independent closeout

- Final independent review: **APPROVE** for `9f569fb..c38919d`. Safe trace
  classifications distinguish success, executor error, panic, cancellation,
  timeout, and stale generation without exposing raw handler data. Completed
  replay respects each loop's current byte/depth limits and fails closed
  without execution, continuation, or history mutation.
- Final independent test: **PASS** at `c38919d`. Focused ToolLoop race tests
  passed 50 repetitions; full core/root race and vet, Google/xAI provider
  suites, transport race tests, and the tagged xAI compile probe passed.
- Task 11 is complete. Gate C remains open for Task 12 provider adapters and
  the Phase 3 independent gate. The claim-before-ContextAggregator ordering
  requirement remains an explicit Task 12 integration constraint.
