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
  be valid JSON objects, at most 32 KiB and depth 16. These checks do not
  validate nested schema semantics or type-check arguments against a schema;
  Task 11 applies JSON validity, byte, and depth bounds to arguments before
  execution.
- Google provider call IDs are preserved. When Google omits one, the adapter
  hashes the response ID, request-wide stream ordinal, name, arguments, and
  session/turn/generation identity into a deterministic fallback ID. The
  ordinal advances across response chunks and resets for a retried request,
  distinguishing separate calls while retaining replay identity.
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

## Correction round 1

The independent Google review overlay initially reproduced five failures:
the continuation dropped Google's native thought signature, a buffered tool
call could execute after a terminal stream error, no-ID calls in separate
stream chunks collided, successful arrays/scalars/large integers were changed
or rejected, and processor configuration retained caller-owned tool
definitions. The same review also required preserving the application's
generation predicate while adding the provider session fence.

Changes and assumptions:

- Native SDK `Content` parts are now carried through the provider-owned active
  continuation state. Tool-call IDs missing from native parts are filled from
  the parsed framework calls on a copied `Content`, preserving signatures and
  original other parts. A separate optional native-aware client interface
  keeps older `LLMToolStreamClient` mocks source-compatible; the real GenAI
  client uses the native-aware interface.
- A model response is successful only after both the chunk and terminal error
  channels close without an error. The processor gives each stream a child
  context and cancels it on every exit; calls stay buffered until terminal
  success, so a failed stream cannot trigger side effects.
- Missing IDs include response scope and a request-wide ordinal. The ordinal
  increments across response chunks, while retrying an identical request
  starts at zero again. If Google omits response IDs, the turn/session identity
  and ordinal still distinguish calls within the stream and remain replayable.
- Successful JSON payloads decode with `UseNumber` and are passed through the
  same `GoogleFunctionResponse` mapper used for public result conversion.
  Arrays, strings, numbers, and object integers are preserved.
- Tool definitions and schema bytes are copied when the processor is built.
- Provider session generation is AND-composed through Task 11's immutable
  `ToolLoop.WithCurrentFence`; the configured application predicate remains
  active. Generation changes during handler execution stop continuation.
- Existing Task 11 ordering and limits remain authoritative. This round adds
  only root object/JSON/size/depth checks on schemas and JSON/size/depth checks
  on arguments, not nested schema or argument conformance validation.

Correction verification:

- Initial `-overlay=/tmp/task12-cross-review.8l51Sa/overlay.json` review run:
  failed on all five reproduced regressions before changes.
- The exact overlay command passed after correction, including the streamed
  native signature and fallback-ID HTTP fixtures.
- Tracked correction tests passed under race x10 for JSON preservation,
  config-copy isolation, failed-stream side-effect prevention, application
  predicate composition, execution-time generation changes, thought-signature
  preservation, and stream-wide fallback IDs.
- Google full race/vet, core ToolLoop race, and root `-tags xai` tests passed
  with the documented Xcode macOS 26 SDK override.
- Public `GoogleFunctionResponse` and continuation history both passed the
  array/string/scalar/large-integer preservation cases.

## Correction round 2

The second independent review found that native state retained only chunks
containing function calls and keyed those chunks by call ID, which lost
surrounding text/signatures and collapsed replayed occurrences.

- The real GenAI stream adapter now copies every candidate `Content` in
  provider order, including text-only/reasoning/signature-bearing chunks. It
  fills omitted call IDs on the copies using the already-parsed call order.
- During continuation mapping, contiguous framework call messages consume
  native call occurrences with a cursor rather than a call-ID map. This keeps
  each repeated exchange/signature and groups streamed parallel call fragments
  into one model turn. Original parts and signatures are copied in order;
  trailing text-only chunks remain on that turn.
- When native contents exist, accumulated SDK text is not appended again as a
  separate assistant message. The native parts are authoritative for text and
  provider state; clients without native content retain the text-only fallback.
- Assumption: separate SDK `Content` fragments that collectively represent
  one streamed tool-call batch are flattened into one model `Content`, in their
  original part order, because that batch is represented by contiguous
  ToolLoop calls. Separate replayed exchanges remain separate model contents.

Correction round 2 verification:

- Both exact review overlays (`overlay.json` and `overlay-round2.json`) passed
  under `-race`; round 2 covers complete streamed model content, parallel
  fragments, and repeated call-ID exchanges.
- Tracked native-content/continuation/fallback-ID regressions passed under
  race x10.
- Google full race/vet, core ToolLoop race, and root `-tags xai` tests passed
  with the macOS 26 SDK override.
- `gofmt` and `git diff --check` passed.

## Correction round 3

The third review found that flattening native contents across model requests
could greedily attach the next response's signed prefix to an earlier call
batch, placing it before that batch's tool result.

- Native continuation state now records explicit response exchanges: the
  ordered call IDs produced by that model request and all copied native
  candidate contents from its stream. History mapping associates each
  contiguous call batch with its own exchange, preserving the complete ordered
  parts/signatures and placing the grouped model exchange at that batch's
  chronological location relative to tool results.
- Existing exact replay behavior remains occurrence-based: repeated call IDs
  are resolved using the ordered exchange cursor, not a call-ID lookup.
- Assumption: streamed chunks from one model request are represented as one
  model `Content` with parts concatenated in their original order. This keeps
  one model exchange grouped while preserving the SDK parts, signatures, and
  boundaries between distinct model requests.

Correction verification:

- The round 3 reproduction failed before the change and passed after it.
- Round 2 and round 3 review overlays passed under race; round 1 overlay and
  the full regression matrix were rerun for this correction.
