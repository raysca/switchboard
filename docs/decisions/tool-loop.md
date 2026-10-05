# ToolLoop library evaluation and policy

**Reviewed:** 2026-10-05
**Decision:** implement the bounded loop natively in `core/tools`; add no dependency.

## Library evaluation

The current maintained Go options were checked against structured schemas,
multiple providers, multi-step execution, cancellation, and framework coupling.

| Candidate | Current evidence | Decision |
|---|---|---|
| LangChainGo | The upstream release list identifies v0.1.14 as its latest stable release. Its Agent API combines planning with `chains`, provider model implementations, and `tools.Tool`; `Plan` accepts a context, and an Executor runs the agent. [Release list](https://github.com/tmc/langchaingo/releases), [Agent interface](https://github.com/tmc/langchaingo/blob/main/agents/agents.go) | Reject as a dependency. It covers tool schemas and agent loops, but its agent/chain runtime takes ownership of decisions and introduces a second model/tool abstraction into a framework whose loop must remain independent of provider and pipeline runtimes. |
| Google Gen AI Go SDK | The official SDK changelog lists v1.72.0 dated 2026-10-01 and continues to add Google-specific tool/function features. [Official SDK changelog](https://github.com/googleapis/go-genai/blob/main/CHANGELOG.md) | Reject for core. It is a provider SDK, useful to Google adapters in Task 12, and does not provide the provider-neutral loop boundary Task 11 needs. |

Neither candidate can be reused for just a schema without adding an otherwise
unneeded runtime or provider module. The existing contract stores schema and
arguments as caller-owned `json.RawMessage`, so core can validate and bound
the small policy surface directly. The core module's `go.mod` is unchanged.

## Chosen behavior

- Calls execute sequentially in the order returned by a provider adapter. This
  is deterministic even if a provider presents a batch that could be run in
  parallel. Parallel execution would complicate interruption and context order
  and is not required by the framework contract.
- One `Run` receives the initial call batch and an optional continuation
  callback. After each round, ordered tool results are committed to
  `ConversationContext`; only then is the callback invoked. The callback
  belongs to the provider adapter and can return another batch or finish.
- Defaults bound a loop to four rounds, sixteen calls, 32 KiB per argument,
  64 KiB per result, depth 16, 30 seconds per call, and two minutes per round.
  Generated safe error payloads are capped by the configured result bound;
  configurations must allow at least 30 bytes, enough for the largest
  canonical loop error payload. Applications can tune each bound explicitly;
  invalid bounds are rejected rather than silently repaired.
- Exact replay uses the context's bounded tool identity ledger. Its atomic
  claim permits only one executor for a pending call, returns the stored result
  for a completed exact replay, and rejects conflicting IDs or an in-flight
  replay. Failed call insertion rolls back message and eviction changes before
  any executor runs. A failed result insertion retains the pending replay fence
  so an already-run side effect cannot be invoked twice.
- Invalid arguments, invalid tool names, unknown definitions, executor errors
  and panics, stale generation checks, and timeouts become small safe JSON
  results. Call identity failures, context insertion failures, cancellation,
  and exhausted limits stop the loop.
- Generation fences run before execution, after the handler returns, before
  context result insertion and continuation, and before a completed replay is returned. A call
  that becomes stale during execution records a normalized cancellation result
  and terminates the loop without passing the executor payload to context or
  invoking continuation. A stale replay returns the same safe outcome without
  rewriting its previously committed context exchange. Span end receives nil;
  raw executor errors are never handed to tracing.
- Each executor receives a child context with its effective tool timeout; each
  round and continuation receives a round context. Executors and continuation
  callbacks must honor cancellation. The loop runs them synchronously and
  creates no detached goroutine per call: Go cannot forcibly stop a callback
  that ignores its context without leaving that goroutine behind. Applications
  requiring isolation for untrusted/non-cooperative handlers must provide it
  in their executor implementation.
- An optional generation predicate fences stale calls without giving core a
  SessionController or session lifecycle dependency. Tool spans use the
  existing trace values and carry only call/session/turn/generation identity
  plus metadata-only content policy.

## Costs and limits

Sequential execution may increase latency for independent calls, and the
context ledger can fail closed at its configured capacity. Those bounds
preserve deterministic conversation order and prevent re-execution after
history eviction. The native loop does not parse provider response formats,
convert JSON Schema, issue model requests, implement confirmation UX, or
provide process isolation; those responsibilities remain with adapters and
applications.

Task 12 integration must invoke the ToolLoop atomic claim before projecting
the same provider ToolCall into the existing context aggregator, or add an
explicit distinction between an already recorded call and a newly claimed
call. If the aggregator inserts first, ToolLoop sees a pending replay and
correctly refuses to execute it. Task 11 does not change aggregator behavior.
