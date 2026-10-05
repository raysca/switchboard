# Task 12 — xAI provider tool adapter

## Scope and decisions

- Added provider-neutral `core.ToolDefinition` mapping to xAI Responses API
  function tools (`type`, `name`, `description`, `parameters`). Schema bytes are
  copied in both directions. Definitions enforce xAI/core name, schema byte,
  schema root, nesting depth, duplicate-name, and 350-tool bounds.
- The adapter accepts a configured shared `core/tools.ToolLoop`; provider calls
  pass through its common argument limits, name/identity checks, atomic
  context claim, safe result normalization, executor timeout, retry fencing,
  and generation predicate. Provider definitions without a ToolLoop fail at
  construction. Callers must construct the provider definitions and ToolLoop
  from the same registration set; the core loop intentionally exposes no
  provider-specific introspection API.
- xAI `function_call` output items become ordered provider-neutral calls. The
  adapter uses only the protocol `call_id` as `ToolCall.ID`; the distinct output
  item `id` is not a substitute. Missing IDs, malformed JSON, and unknown names
  are preserved and rejected by ToolLoop rather than repaired in the adapter.
- Multiple calls execute in provider order through ToolLoop. Exact repeated
  output items from streaming and terminal response events are deduplicated.
  Function item ID, `call_id`, name, and completed arguments are immutable
  identity; lifecycle status and partial-to-complete arguments are allowed to
  evolve. Message/reasoning item payload changes are not mistaken for function
  identity conflicts. ToolLoop claims and records calls/results in its
  configured ConversationContext. The adapter does not emit duplicate context
  projection frames, so claim/record happens before any ContextAggregator can
  observe downstream model frames.
- Each continuation request sends `function_call_output` items keyed by the
  original `call_id`, encodes success as `{"output": ...}` and failure or
  cancellation as `{"error": "<safe-category>"}`, and keeps `store:false`.
  Historical tool calls/results from ContextProvider use the same native
  Responses items and envelopes. To continue without
  server-side response storage, it replays the exact prior Responses output
  items plus the ordered function outputs in the next `input`; this follows
  xAI's documented stateless input-list continuation path. Provider streaming
  retries remain owned by `ExecuteWithRetry`; receiving text or a tool call
  marks a request committed and prevents replaying a side-effecting exchange.
- Interruption/cancel and session context cancellation reach HTTP requests and
  ToolLoop. A derived ToolLoop composes its existing application generation
  predicate with provider session/turn/generation identity. Both the active
  processor generation and live SessionController generation are checked
  before executing/committing calls, forwarding deltas, and continuing.
  Old-generation calls cannot trigger further tool execution or a followup
  model request; a handler that finishes after generation changes commits a
  cancelled result rather than its stale success.
- xAI's current function tools use the Responses API. Configuring tools against
  the legacy Chat Completions endpoint is rejected. The existing stdlib HTTP
  and SSE implementation is retained; no SDK dependency is introduced because
  the project currently uses its own HTTP wire client and no Go xAI SDK types.
- No usage/readiness capability is added here; those remain the separate tasks
  identified in the execution plan.
- Schema mapping validates JSON, name/description/byte/depth bounds, and the
  xAI-required root object shape. It passes nested schema keywords through
  unchanged and supports `oneOf`/`anyOf` only when every root branch is an
  object; it does not locally validate arguments against nested properties or
  `required` constraints. The adapter enforces xAI's documented 350-tool cap.

## Protocol reference

Reviewed xAI's current primary documentation on 2026-10-05:

- [Function Calling](https://docs.x.ai/developers/tools/function-calling):
  Responses API function definitions use `parameters`; calls contain `call_id`,
  name, and arguments; results are `function_call_output` input items.
- [Advanced Usage](https://docs.x.ai/developers/tools/advanced-usage): when
  continuing without `previous_response_id`, append the previous response's
  output items and the tool outputs to the next input list. This supports the
  existing `store:false` requirement.

## TDD evidence

- RED: focused tests first failed to compile on missing schema mapping,
  response output item, and call parsing APIs.
- GREEN: provider package tests pass, including schema/required-field mapping,
  schema invalid/depth/size rejection, immutable schema/call copies,
  object-union and 350-tool bounds, no local nested-schema conformance claim,
  missing-call-ID rejection, exact replay and actual identity-conflict
  rejection, `output_item.added` → argument deltas/done → completed assembly,
  non-text delta suppression, ordered multi-call execution, explicit result
  status and safe-category mapping, native historical function items, full
  Responses continuation payload preservation, cancellation, application
  predicate composition, and generation changes during handler execution.
- `cd providers/xai && go test ./...`: PASS.
- `cd providers/xai && go test -race ./...`: PASS.
- `cd providers/xai && go vet ./...`: PASS.
- `cd core && go test -race -count=25 ./tools && go vet ./tools`: PASS.
- Root `go vet -tags xai ./...`: PASS.
- Root `go test -race -tags xai ./...`: initial attempt failed at the known
  MacOS27 command-line SDK linker incompatibility; rerun with
  `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` passed. The
  linker emitted deployment-target warnings for MacOS27-built objects.
- `gofmt` and scoped `git diff --check`: PASS.

Task 12 and Gate C remain open pending independent review/test and the Google
adapter closeout. This report records the xAI implementation only.

## Correction round 1

The independent review probe initially reproduced five findings: function
argument/reasoning deltas were forwarded as speech, valid Responses item
lifecycle updates conflicted, generation changes during execution did not fence
the batch, historical tools were flattened into role/content messages, and
failure status could be hidden by a valid result payload.

The adapter now consumes only output-text deltas as speech. It assembles
function-call arguments privately across `output_item.added`, argument delta and
done events, `output_item.done`, and terminal response output. Lifecycle
metadata may advance while function identity remains fixed. Only changed
non-empty function identity or conflicting completed arguments fail. The
ToolLoop now receives an immutable derived loop with the provider's live
session/turn/generation fence ANDed with the application's existing predicate.
Native historical `function_call` and `function_call_output` input items are
reconstructed from ConversationContext messages. Status envelopes distinguish
success payloads from categorized failure/cancellation, even when a failed
handler returned ordinary JSON; category text is allowlisted before provider
transmission.

Correction verification:

- The review overlay's five `TestReview*` probes pass with
  `go test -race -count=10 -overlay=/tmp/task12-cross-review.8l51Sa/overlay.json -run TestReview -v .`.
- Promoted xAI lifecycle, non-text suppression, native history/status, schema
  union/350-cap, application predicate, and mid-handler generation regressions
  pass in the tracked package suite.
- `cd providers/xai && go test -race ./... && go vet ./...`: PASS.
- `cd providers/xai && go test -race -tags xai ./... && go vet -tags xai ./...`: PASS.
- `cd core && go test -race -count=50 ./tools && go vet ./tools`: PASS.
- Root `go vet -tags xai ./...`: PASS.
- Root `go test -race -tags xai ./...` with the MacOSX26 SDK override: PASS
  (existing MacOS27 deployment-target linker warnings).
- Scoped `gofmt` and `git diff --check`: PASS.

The local schema boundary is intentionally limited to JSON validity, byte/depth
bounds, tool identity and xAI's root object/object-union shape. Neither the
adapter nor Task 11 ToolLoop evaluates nested JSON Schema constraints against
tool arguments.

## Correction round 2

The independent round-two probe reproduced three issues: streamed argument
state was keyed inconsistently across item/call/output-index identifiers,
completed arguments could disagree with the argument stream, and success
results were decoded through `float64`, rounding large JSON integers. The
stream adapter now resolves those identifiers as aliases of one assembly,
supports argument deltas arriving before the item lifecycle event, attaches
assembled arguments when completed items omit them, and rejects conflicting
stream/final payloads. Duplicate item lifecycle events remain idempotent at the
canonical call list.

Successful result envelopes now splice validated, bounded raw JSON into the
envelope rather than decode/re-encode through an interface value. This keeps
integers such as `9007199254740993` exact in both historical Responses items
and active continuation output while retaining support for arbitrary JSON
scalars, arrays, and objects. The 64 KiB successful-result bound matches the
shared tool result contract.

Recommended ambiguity choices: Responses events do not provide a stable event
sequence identifier in the current SDK-facing event payload, so exact duplicate
item lifecycle updates are merged idempotently, while argument deltas remain
ordered byte fragments and are never de-duplicated by content (identical
adjacent fragments can be legitimate). Argument `done` is treated as the full
assembled argument value and must agree with prior streamed fragments; this
avoids silently discarding conflicting provider state.

Round-two regression tests cover call/item/output-index alias correlation,
delta-before-item ordering, omitted final arguments, conflicting `done` versus
completed output, duplicate completed items, exact large-number preservation,
scalar/array/object payloads, and oversized results. Verification is recorded
after the round-two matrix below.

Round-two verification:

- xAI review overlays: both the round-one and round-two overlay suites pass
  with `go test -race -count=10`.
- `cd providers/xai && go test -race ./...`: PASS.
- `cd providers/xai && go vet ./...`: PASS.
- `cd providers/xai && go test -race -tags xai ./...`: PASS.
- `cd providers/xai && go vet -tags xai ./...`: PASS.
- `cd core && go test -race -count=50 ./tools`: PASS.
- `cd core && go vet ./tools`: PASS.
- Root `go vet -tags xai ./...`: PASS.
- Root `go test -race -tags xai ./...` with
  `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk`: PASS; the
  linker reports the existing MacOS27-built object/MacOS26.5 target warnings.
- `gofmt` and scoped `git diff --check`: PASS.

Task 12 and Gate C remain open pending the cross-provider gate and independent
closeout. This correction does not change Task 12 status.
