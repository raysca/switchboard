# Task 12 Google provider report

## Scope and decisions

- Implemented Google-only provider mapping between `core.ToolDefinition`,
  `core.ToolCall`, `core.ToolResult`, and the Task 11 `core/tools.ToolLoop`.
  Provider usage counters/readiness, MCP, and framework lifecycle ownership
  remain out of scope.
- Used the existing official `google.golang.org/genai` SDK (`v1.71.0` in the
  provider module) types. `FunctionDeclaration.ParametersJsonSchema` receives
  a deep-copied JSON Schema value; `FunctionCall` and `FunctionResponse` SDK
  values are used for streamed calls and continuation history. The SDK
  documents these fields in its
  [Go package reference](https://pkg.go.dev/google.golang.org/genai) and
  [official Go SDK repository](https://github.com/googleapis/go-genai). No
  dependency update was needed for this adapter.
- Framework tool names retain Task 11's 64-character `[A-Za-z0-9_.-]`
  contract even though Google's SDK accepts a broader name set. Schemas must
  be JSON objects, at most 32 KiB and depth 16. The adapter preserves schema
  keywords and does not attempt full JSON Schema validation; Task 11 applies
  the same byte/depth limits to arguments before execution.
- Google provider call IDs are preserved. When Google omits one, the adapter
  hashes the response ID, call index, name, arguments, and available turn
  identity into a deterministic fallback ID. This supports exact replay; a
  missing Google response ID still makes a provider-level replay indistinct
  from a repeated identical call within the same turn.
- Calls are accumulated in streamed candidate/part order, then passed to
  ToolLoop. Task 11 owns deterministic sequential execution, identity claims,
  cancellation, timeout, replay, and generation fencing. The LLM processor
  does not separately publish tool-call/result frames into ContextAggregator:
  ToolLoop atomically claims and projects each call/result to its configured
  `ToolHistory`, preventing a second context insertion. `ToolHistory` should
  be the same `ConversationContext` observed by `ContextProvider`.
- Each continuation request reads the updated conversation history and maps
  the assistant call to a model `FunctionCall` part and the result to a user
  `FunctionResponse` part. Success returns `{"output": ...}`; failure and
  cancellation return only safe normalized categories. Raw handler errors,
  provider arguments, results, and credentials are not copied into adapter
  errors or traces. For the active model exchange, calls and results are
  projected as ordered grouped SDK content parts so a parallel call batch
  stays together even though TaskLoop commits calls sequentially.
- Existing retry policy is applied to each model request. Once text or a tool
  call has been emitted, that request is treated as committed and is not
  retried. Transient failures before output retry both the initial request and
  tool continuation request. Tool execution uses the processor's active
  generation context and Task 11's current-generation predicate; interruption
  cancels execution and prevents provider continuation.
- Unknown names and malformed/deep/oversized arguments are retained as
  provider calls and surfaced to ToolLoop, which turns ordinary invalid or
  unknown calls into bounded tool results. Conflicting duplicate IDs fail
  closed under the core identity ledger.

## TDD and verification

- RED: `go test ./... -run TestGoogleToolAdapter` initially failed to compile
  because the schema mapper, call parser, response mapper, and history mapper
  did not exist.
- GREEN: focused adapter and mocked provider-loop tests passed after the
  SDK-backed mappings were added.
- Coverage includes schema keyword/description/name mapping and copy
  isolation; invalid, deep, and oversized schemas; multiple call ordering and
  stable IDs; safe success/error/cancel result mapping; function-call/result
  continuation history with grouped parallel calls; one-time execution for
  exact replay; transient continuation retry; interruption cancellation and
  stale-generation continuation fencing.
- `cd providers/google && go test -race ./...`: PASS.
- `cd providers/google && go vet ./...`: PASS.
- `cd core && go test -race ./tools`: PASS.
- `gofmt` and `git diff --check`: PASS.

Task 12 and Gate C remain open for independent review and tester verification.
