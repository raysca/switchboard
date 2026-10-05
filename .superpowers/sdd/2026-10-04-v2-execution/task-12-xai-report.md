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
  output items from streaming and terminal response events are deduplicated;
  conflicting reuse of an item/call identity is rejected. ToolLoop claims and
  records calls/results in its configured ConversationContext. The adapter
  does not emit duplicate context projection frames, so claim/record happens
  before any ContextAggregator can observe downstream model frames.
- Each continuation request sends `function_call_output` items keyed by the
  original `call_id`, preserves result order/status through ToolLoop's
  normalized result payload, and keeps `store:false`. To continue without
  server-side response storage, it replays the exact prior Responses output
  items plus the ordered function outputs in the next `input`; this follows
  xAI's documented stateless input-list continuation path. Provider streaming
  retries remain owned by `ExecuteWithRetry`; receiving text or a tool call
  marks a request committed and prevents replaying a side-effecting exchange.
- Interruption/cancel and session context cancellation reach HTTP requests and
  ToolLoop. Both the active processor generation and live SessionController
  generation are checked before executing calls, forwarding deltas, and
  continuing. Old-generation calls cannot trigger tool execution or a followup
  model request.
- xAI's current function tools use the Responses API. Configuring tools against
  the legacy Chat Completions endpoint is rejected. The existing stdlib HTTP
  and SSE implementation is retained; no SDK dependency is introduced because
  the project currently uses its own HTTP wire client and no Go xAI SDK types.
- No usage/readiness capability is added here; those remain the separate tasks
  identified in the execution plan.

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
  missing-call-ID rejection, exact event replay and conflicting identity,
  ordered multi-call execution, failed-result status and secret sanitization,
  full Responses continuation payload preservation, cancellation, and stale
  generation fencing.
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
