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
  rewriting its previously committed context exchange. Completed replay also
  rechecks the current result byte/depth bounds; if the cached payload violates
  a stricter loop policy, replay fails closed with
  `ErrReplayResultOutOfBounds`, without execution, continuation, or history
  rewrite. Trace span end distinguishes success from safe normalized outcome
  categories (for example `executor_error`, `executor_panic`, `cancelled`, and
  `timeout`). It never receives raw handler errors or handler-provided error
  categories.
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

## Update: parallel and async tool calls (P3-3)

This supersedes the "Calls execute sequentially" bullet above when opted in.

- `ToolLoopConfig.MaxParallel` (default 1 = unchanged sequential behaviour)
  runs one round's calls concurrently. Every call is claimed (written to
  history) in order before any executes, executors run concurrently, and each
  result write waits for the previous call's, so history is one contiguous call
  batch followed by its results in call order (Gemini pairs them per turn).
  `MaxCalls` is checked against the batch before dispatch, so counters are only
  touched by the `Run` goroutine; `MaxResultBytes` applies per call as before.
  Per-call timeout is still `ToolPolicy.Timeout` capped by `DefaultCallTimeout`.
- Stale generations are now detected while calls run: a watcher polls
  `IsCurrent` every `StaleCheckInterval` (25ms) and cancels the round context,
  so cooperative executors stop on barge-in. The loop still waits for every
  started executor to return (no detached goroutines on this path). A stale or
  failed call aborts calls not yet started; their results are normalised.
- `ToolPolicy.Async` marks a long-running tool. The loop commits an immediate
  ack (`{"status":"running","async":true}`) as the tool result, then runs the
  executor in a background goroutine. When it finishes, the result is injected
  as a user-role context message (metadata `async_tool_call_id`) and
  `AsyncSink` (when set) receives the finished call instead and decides when to inject and trigger a model turn. The
  injection waits until no round of this loop is in flight, so it never
  precedes its ack or splits a call batch from its results; user turns added
  meanwhile simply sit between them. The message is a labelled JSON envelope
  (`call_id`, `tool`, `status`, `result`) marked as tool output, not user
  instructions. `AsyncSink` must not call `Close` (Close waits for it).
- Async calls ignore the generation fence (they survive barge-in, as Pipecat's
  `cancel_on_interruption=False`). They are bounded by `MaxAsyncInFlight`
  (default 4, excess calls fail `too_many_async`), use the same timeout and
  result normalisation, and are cancelled and dropped by `ToolLoop.Close`.
  Not done: LLM-driven cancellation of async calls.

## Update: async tools wired into a call (T2)

- Every LLM stage now owns a session-lived `ToolLoop` (Google was the last to
  build one per generation; it now keeps one for the processor and derives a
  fenced loop per generation, closing it in `Close`). Async calls therefore
  survive across generations and are cancelled only at session close.
- `ToolLoopConfig.AsyncSink` replaces immediate injection: the finished call is
  handed over with an `Inject` func (still taking the round lock). The owner
  injects the envelope only when it is about to start a model turn, so the
  history reads `... assistant reply, envelope` and the model answers the
  envelope. `OnAsyncStart` fires at acknowledgement (before the ack is written)
  so the failover side-effect counter moves at claim time. `AsyncTimeout`
  (default 5m) is the default and cap for async calls instead of the 30s
  synchronous cap.
- `core.AsyncTrigger` is a pass-through stage placed directly before the LLM
  (only built when a tool is async). It queues results (bounded at 8; on
  overflow the oldest is injected at once without its own turn, so no result
  is lost; `DroppedCount` only counts failed injections) and, once the user is not speaking (VAD/turn
  events), the bot is not speaking (upstream `SpeakingStateFrame`) and no reply
  is active (`AssistantContextProcessor.Active`) for `Settle` (300ms), injects
  every queued envelope and re-enters one `UserMessageFrame` (turn `async-N`)
  at the LLM. A signal stuck longer than `MaxWait` (60s) stops deferring.
  It never interrupts: delivery waits for the call to go idle. A real user turn
  in between simply delays it. Session close drops the queue (its worker is a
  supervised session task).
  A reply with nothing to speak clears `Active` at its final text, so it does
  not hold delivery until `MaxWait`; an LLM error still does (up to 60s).
  Counters: `voice_async_tool_turns_total`, `voice_async_tool_results_dropped_total`.
- Config: agent config `async_tools: ["<mcp_server_id>/<remote_name>"]`
  (shape validated at decode, default none) sets `ToolPolicy.Async`. An entry
  naming a tool the version does not select is inert (that tool stays sync).
  An async MCP call is bounded by `AsyncTimeout` (`mcp.LongCall`), not the MCP
  service's 15s per-call timeout, and is cancelled with the loop at close.
- `MaxAsyncInFlight` is per candidate loop, so a session with LLM fallbacks
  can hold up to 4 x candidates; deliberately not shared.
- Failover: an acknowledged async call counts as a side effect at once, so a
  generation that started one is never replayed on a fallback.
