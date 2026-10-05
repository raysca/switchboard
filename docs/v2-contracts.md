# Voice Platform V2 Contracts

**Status:** Phase 0 contract freeze for V2 planning
**Authority:** `docs/v2-spec.md` and `docs/v2-plan.md`
**Scope:** Stable cross-workstream boundaries; implementation may refine internal details without changing these observable contracts.

## 1. Architecture and ownership

```text
Framework (`core`, providers, transports)
        ↓ public APIs
Server (`server`, persistence, management APIs)
        ↓ versioned HTTP management APIs
Admin UI (`ui`)
```

The Framework owns frames, processors, pipelines, sessions, turn identity and lifecycle, supervision, interruption, audio processing, VAD, turn detection, context, tool execution primitives, tracing and observer interfaces, provider integration interfaces, and transport abstractions. The Framework has no dependency on Server, persistence, UI, product authentication, or MCP.

The Server owns persistent agents and immutable versions, provider credentials and capabilities, MCP configuration and discovery, agent configuration, storage, trace/session history, management APIs and authentication, onboarding, provider discovery, agent readiness, and deployment/runtime configuration presentation. Server runtime construction uses the same public Framework APIs available to a standalone Go application.

The UI owns presentation and browser interaction. It calls authenticated, versioned management APIs and uses the normal RTVI connection flow for the embedded voice test. It does not call database repositories or construct a private test-only runtime.

## 2. Framework contracts

### 2.1 Session, turn, and generation identity

`core.SessionID` and `core.TurnID` are distinct opaque defined string types. They are created once at their respective lifecycle boundaries, propagated through frames, trace events, observer events, tool calls, and provider usage events, and are never parsed to infer another value. A `TurnID` identifies one conversational turn; session-local turn ordering is separate metadata. Server-issued UUIDv7 strings may be used as the underlying session and turn values.

`core.Generation` is a monotonically increasing unsigned cancellation fence scoped to a session. A generation may mark work stale and prevent stale frames from being delivered after interruption or turn replacement. It is not an identifier for a session, user turn, assistant turn, trace, or persistent record. Turn identity remains stable when generation changes.

Task 1 publishes these as distinct defined types in `core/identity.go`. Existing V1 session and frame methods still use their historical `uint64`/`string` fields; the P0 turn and frame integration will migrate those runtime paths.

Task 2 adds `TurnLifecycle` as the one-shot start/complete/interruption state primitive. It accepts an opaque `TurnID` and a session-scoped `Generation`; terminal decisions require the expected `TurnID`, so a late completion or interruption cannot end the next turn. Duplicate starts, completions, interruptions, and idle false starts are ignored. Existing V1 frame fields remain in use at the adapter boundary pending the broader frame migration. The generation fence advances once when a flush is committed and stale queued or late assistant text, synthesis units, TTS frames, and output audio are discarded before processor delivery or at the session edge. A partial assistant context event for audio already delivered remains visible, as does explicitly uninterruptible output. Generation never becomes a turn identifier.

`InputMuteFrame` and `InputUnmuteFrame` are session controls. Post, reentry, and processor-emitted mute controls all enter the same ordered control queue. Control admission and audio tagging are serialized: audio admitted while muted is marked for discard before any processor (including VAD/STT). `InputMuted()` reports applied state, updated in control-queue order with `InputMutedStateFrame`; a queued control can therefore change admission policy before its state event is delivered. Unmute keeps the same session and generation. A posted or reentered `FlushFrame` must carry a nonzero target generation; a reentry generation argument must match the frame target. Invalid or conflicting targets are rejected before advancing the fence, and an admitted flush advances it exactly once. Flush and lifecycle controls remain in the V1 control queue; duplicate or stale interruption/flush controls do not advance the current turn, while a committed barrier can propagate its target-generation control. Stale write-completion acknowledgements are discarded because they must not commit old media. The P0 flush barrier here covers core queues and the fake transport buffer; real WebSocket pacing/disposition integration remains Task 8, and this contract makes no claim about audio already buffered by a client. The `RuntimeSettingsUpdate` envelope declares revisioned, typed optional values, but validation and atomic application are deferred to Task 27; queue prioritization is deferred to Task 30.

**Acceptance criteria**

- Compile-time-distinct `SessionID` and `TurnID` values cannot be assigned to one another without an explicit conversion.
- A turn keeps the same `TurnID` across its speech, transcript, model, tool, and output events, including interruption; cancellation advances the generation without synthesizing a new identity.
- Session and turn IDs remain opaque at framework boundaries; no consumer derives identity from generation, sequence number, or a database row number.
- A restart or concurrent session cannot reuse a server durable ID.

### 2.2 Trace and span API

The provider-neutral public API is in `core`:

```go
type Tracer interface {
	StartSpan(ctx context.Context, event TraceEvent) Span
}

type Span interface {
	SetAttribute(key string, value any)
	AddEvent(event TraceEvent)
	End(err error)
}
```

`TraceEvent` carries a stable event name, event time, optional `SessionID`, optional `TurnID`, optional cancellation `Generation`, and structured attributes. The returned span is a no-op span when tracing is disabled; callers need not branch on tracer presence. Span parentage follows the context passed to `StartSpan`. The API does not expose persistence or exporter types.

The initial surface provides `core.NoopTracer`, `core.ContextWithSpan` / `core.SpanFromContext`, and `core.NewTraceEvent`. The constructor copies and filters attributes; `Attributes()` returns a new map. `TraceEvent.ToolCallID` is a scalar correlation field so concurrent calls in one turn remain distinct without admitting payload text. For example, `child := tracer.StartSpan(core.ContextWithSpan(ctx, parent), event)` carries parentage through the context. Runtime instrumentation and exporter behavior belong to Task 13.

The standard event vocabulary includes session start/end/failure; turn start/end; speech start/stop and VAD latency; STT start, partial, final, and finalization; context commit; LLM request, first output, completion, and failure; tool requested/start/complete/failure; TTS request, first audio, completion, and failure; interruption and flush; provider readiness/usage/error; transport disconnect; and session failure. Custom processors may add namespaced events and structured metadata.

**Acceptance criteria**

- A disabled tracer has a no-op path and creates no exporter, database, or server dependency.
- Spans correlate by typed session and turn IDs and preserve parent/child timing.
- The standard vocabulary can represent the complete user-speech → STT → context → LLM → tool loop → TTS → output path and interruption ordering.
- Tracing errors cannot fail or delay a voice session.

### 2.3 Trace content policy

`TracePolicy.Content` has exactly `none`, `metadata`, or `text` modes. Production defaults to `metadata`. Raw audio bytes are never included in traces. Metadata mode records event names, timing, identifiers, counts, sample formats, provider/model names, status, and safe error categories, but not transcript text, prompts, tool arguments, tool results, keys, or audio. Text mode permits selected text fields; tool arguments/results still pass through an explicit redaction policy. No mode permits credentials or raw audio.

The Task 1 constructor defaults an unset mode to `metadata`, admits a fixed set of scalar metadata keys, and drops unknown keys and mutable values. An unknown content mode drops all attributes. Namespaced `custom.*` attributes may hold nested `map[string]any` and `[]any` values containing only finite numbers and booleans; they are deeply copied on input and output, limited to eight levels and 64 entries per collection. A shared 1,024-node work budget applies across custom attributes on each copy. Values that fail validation or exhaust the budget are dropped; more than 1,024 custom keys drops all custom metadata. Keys are processed in lexical order for deterministic truncation. This permits structured counters, thresholds, and scores without a text-content path. Text mode admits `transcript`, `prompt`, and `text`; `tool.arguments` and `tool.result` require `RedactToolContent`. Callers supply safe provider/model/status/category labels rather than raw provider errors.

**Acceptance criteria**

- Tests with unique sentinel secrets/text prove that metadata mode, logs, errors, and event attributes do not contain them.
- Text inclusion requires explicit policy configuration and does not enable raw-audio capture.
- Server retention controls decide whether selected content is persisted; the core policy only controls what the tracer is allowed to receive.

### 2.4 Observer and semantic debug events

`core.Observer` is a non-mutating observation boundary. Its immutable event values describe frame kind, direction, processor identity/index, session/turn/generation correlation, and monotonic timing/lifecycle. Observer APIs do not grant a mutable frame pointer or a callback that can replace, drop, reorder, or inject frames. Callbacks run in event order and must return promptly; observer failures are isolated from session execution and counted/reported through a safe diagnostic hook.

The framework emits protocol-neutral `DebugEvent` values for session/pipeline lifecycle, frame flow, speech/transcription, LLM/TTS lifecycle, interruption/flush, tool call/result, provider readiness/error, and teardown. An adapter maps these semantic values to Whisker-compatible messages. A `Serializer` owns conversion between semantic events/frames and a wire representation; it is not a pipeline processor and does not choose product persistence. RTVI serialization remains its separate client-facing compatibility surface.

Task 1 defines `Observer`, `ObserverEvent`, and `DebugEvent` value surfaces. Their constructors filter and copy attributes, including approved structured numeric/boolean metadata, and their accessors return deep copies. Observer values contain a frame kind rather than a frame pointer. `DebugEvent` has explicit scalar `PipelineID`, `Processor`, `ProcessorIndex`, `FrameKind`, `Direction`, `Sequence`, and `ToolCallID` fields alongside session/turn/generation correlation, so a later adapter can distinguish frame flow and concurrent tool calls without parsing attributes. Delivery, panic isolation, protocol adapters, and general `Serializer` behavior remain assigned to later tasks.

**Acceptance criteria**

- Observer delivery cannot mutate frame flow and an observer panic/error does not fail the session.
- Event order, direction, correlation IDs, processor position, and lifecycle boundaries are testable.
- A semantic event can be serialized for a selected debug protocol without adding Whisker-specific types to core processors.
- RTVI and Whisker conformance results are recorded independently.

### 2.5 Runtime settings updates

Runtime changes enter a session through a typed `RuntimeSettingsUpdate` control message. The update includes a monotonic revision and a typed set of supported values, is validated as a complete unit, and takes effect at a documented control boundary. An invalid update is rejected without partially changing processor state. Arbitrary mutation of processor internals is not public API. Initial updateable values are input mute, interruption enabled, silence/turn timeouts, language, and selected runtime thresholds.

**Acceptance criteria**

- A valid update is observed as one revision and applies atomically.
- An invalid or out-of-order revision leaves the prior settings active and produces a structured error/event.
- Updates take effect without rebuilding a session for the supported fields.

### 2.6 Audio filter

`core.AudioFilter` is a provider-neutral streaming abstraction with explicit input/output audio format, bounded per-stream state, cancellation-aware processing, reset, and close. The first implementation must include one real noise-suppression implementation; a noise gate alone does not satisfy this contract. Filter latency is observable. Resampling and channel/format conversion remain composable core audio utilities rather than provider-specific copies.

**Acceptance criteria**

- A filter declares encoding, sample rate, and channels and rejects unsupported input formats explicitly.
- Repeated chunk processing is bounded and session-safe; reset clears all prior-session signal state.
- Cancellation and close release processing resources, and measured filter latency is available to observers/traces.

Task 6 implements this boundary with a stream-owned SpeexDSP 1.2.1
preprocessor for mono PCM16LE at 8, 16, 24, or 48 kHz. It buffers less than
one 10 ms frame, reports 10 ms algorithmic/block latency, and rebuilds native
state on reset. The composable audio utilities use the same vendored,
BSD-licensed SpeexDSP release for quality-5 band-limited resampling and local
deterministic PCM16/float conversion, mono/stereo normalization, pre-roll,
rechunking, and opt-in turn buffering. The resampler accepts only 8, 16, 24,
or 48 kHz, at most 48,000 input samples per call, and caps any per-call output
allocation at 288,064 samples before narrowing lengths for C. Turn buffers use
`core.TurnID`; all buffers are byte-bounded and require an explicit
drop-oldest or reject-newest policy, with reject-newest reported to the caller
without changing retained bytes. Every mutable native/DSP object is per stream
and serialized; no process-global DSP state or model weights are used. Native
and production builds require CGO and a C compiler; the release container
already builds with `CGO_ENABLED=1`. Gate B remains open until the rest of the
Phase 2 audio path and its independent gate pass.

The deterministic denoiser regression uses a stationary-noise learning prefix
followed by voiced, pitch-varying harmonic bursts and pauses. It projects the
output onto the clean wanted reference, divides residual power by the fitted
gain squared, and requires at least 3 dB SNR improvement plus at least 0.5
wanted-signal gain (about -6 dB). Gain normalization makes uniform attenuation
unable to masquerade as suppression; explicit bypass, fixed-attenuation,
silence, and threshold-gate controls must fail the same predicate. This is a
repeatable DSP regression, not a perceptual quality or real-speech corpus
evaluation.

### 2.7 Turn detector and manager

`core/turn` provides the provider-neutral `TurnDetector` interface, `SilenceDetector`, `HybridDetector`, and a session-owned `Manager`. The manager receives caller-supplied `core.TurnID`, `core.Generation`, VAD/activity observations, and explicit timestamps through `Observe` and `Advance`; it does not read wall time, create timers/goroutines, mint IDs, or depend on providers, Server, storage, STT, or transports. A detector is synchronous, deterministic, fast, and side-effect free. It is called outside the manager state lock with an immutable snapshot (including copied, numerically valid semantic evidence); at most one callback is in flight. Concurrent/reentrant evaluations are coalesced into one complete newest snapshot, including its input and epoch. Both callback preflight and terminal commit compare that snapshot with actual current detector state and any newer queued input. Identical inputs reuse either a positive or negative result; otherwise queued snapshots run. No-op mute controls retain eligible detection even when their timestamp is later; rejected observations leave timestamps and detection unchanged. Genuine mute and speech-resume transitions fence the epoch, and unmute queues fresh silence state when a callback owner is still in flight. Each outer evaluation permits at most two plugin callbacks, after which the newest snapshot uses deterministic `SilenceDetector` fallback. Semantic state and epoch fencing prevent stale terminal commits. Pre-callback frames are discarded after an epoch change but retained across a time-only same-epoch advance. Panics and invalid suggestions are ignored in favor of silence fallback. Only silence and semantic completion reasons are accepted; confidence must always be finite, supplied confidence must be in `[0,1]`, semantic completion requires confidence, and absent confidence is normalized to zero for JSON-safe events.

The manager distinguishes initial no-speech timeout, accepted speech start/stop, silence-based user-turn completion, and active-turn maximum duration. Start and stop debounce plus minimum speech duration suppress short false starts; repeated idle snapshots preserve the first stable stop/silence origin and do not re-emit `Stopped`. `SetMuted` pauses turn and timeout advancement; unmute starts fresh silence timing without replaying muted observations. Timestamp regression is rejected. `Interrupt` commits only for the expected active typed turn and generation. A higher observed generation fences a pending candidate or active turn before old turn-ID filtering, including on an idle snapshot. Completed and interrupted turn IDs are retired within their generation: positive `Config.MaxRetiredTurns` (default 128) bounds this ledger, and when full the manager rejects a new same-generation speaking turn with `ErrRetiredTurnLimit` rather than evicting IDs and risking a stale restart. A generation advance or `Reset` clears the ledger; generation is not used as a substitute for turn identity. Terminal decisions are one-shot through `core.TurnLifecycle` and are returned as ordinary `core.TurnEventFrame` values for the caller to route. Semantic evidence is a provider-neutral confidence value; it can complete early only for the matching turn/generation, a timestamp within the current turn and configured freshness window, confidence above threshold, and an elapsed silence guard. Missing, invalid, stale, or low-confidence evidence falls back to silence.

This state machine does not commit transcripts or context, advance the session cancellation fence, flush output, apply runtime policy updates, or terminate a session. Those actions remain with their owning tasks and integration. The manager owns only bounded per-session state, and its frames have no implicit process or session side effect.

**Acceptance criteria**

- Timestamped start, completion, false-start recovery, no-speech/active-duration timeout, mute, and interruption transitions are deterministic and include typed identity and a reason where applicable.
- A completion or interruption is one-shot; old-turn and old-generation observations cannot alter the current turn.
- Semantic evidence can only shorten the silence path under its identity, timestamp, confidence, freshness, and guard checks; every rejected signal preserves deterministic silence fallback.
- STT/context agreement, output cancellation, and generation-fence integration remain Gate B acceptance items and are not claimed by the state-machine package alone.

### 2.8 Provider readiness and usage

Providers report typed readiness transitions and optional provider-neutral usage events. Readiness includes provider/component, state, timestamp, and safe reason category. Usage includes provider/model, session/turn IDs when available, unit name, quantity, and timestamp (for example LLM tokens, STT audio duration, TTS characters/audio duration, tool calls, or request count). These are observability values, not billing or admission decisions.

**Acceptance criteria**

- Missing provider metrics are represented as absent, never fabricated as zero usage.
- Readiness and usage can reach trace/observer consumers without a database dependency.
- Provider errors and secret-bearing response text are redacted before event emission.

### 2.9 Provider-neutral tools and ToolLoop

Core defines `ToolDefinition` (stable name, description, JSON input schema, and policy metadata), `ToolCall` (call ID, name, raw JSON arguments, session/turn/generation correlation), and `ToolResult` (call ID, structured result or safe error, status, and timing). A provider adapter translates provider-native requests/results to these types. `ToolExecutor` invokes registered handlers using a `context.Context`; an MCP wrapper is one Server-supplied handler and is invisible to core.

The Task 1 signatures live in `govoice/core/tools`: `ToolExecutor.Execute(context.Context, ToolCall) (ToolResult, error)` and the matching `ToolExecutorFunc` adapter. JSON schema, arguments, and results use `json.RawMessage`; callers own those byte slices. `ToolResult.ErrorCategory` is a safe category, not provider error text. Registration, execution, timeouts, and ToolLoop policy are deferred.

`ToolLoop` owns sequential model/tool rounds, multiple calls returned in one round, maximum iterations, maximum calls, per-tool timeout, context cancellation, result normalization, interruption behavior, and trace hooks. It returns control to the LLM after successful results and terminates with a typed error when a limit is exceeded. It does not own session lifecycle, pipeline execution, provider SDKs, persistence, or MCP.

**Acceptance criteria**

- Google, xAI, and server-supplied handlers share the same framework tool types.
- Multiple calls, malformed arguments, unknown names, provider errors, timeout, cancellation, interruption, and iteration exhaustion have deterministic results.
- Tool call/result events correlate to the originating turn; content follows trace policy.
- A tool handler cannot keep a session alive past cancellation or its configured timeout.

## 3. Server concepts and ownership

Server owns the following durable domain records. All durable IDs are UUIDv7 strings; wire and repository APIs treat IDs as opaque. Framework `SessionID`/`TurnID` types may carry the corresponding durable value but remain framework types.

| Concept | Owner and frozen meaning |
|---|---|
| `Agent` | Mutable current product resource: identity, name/description, enabled/archive state, and pointer to current version. The API never uses the framework `Pipeline` as its persistence model. |
| `AgentVersion` | Immutable validated configuration snapshot. Stores schema version, normalized config, creation time, and lower-case SHA-256 `config_hash`. Hash excludes mutable timestamps and its own hash field. Existing sessions continue using the snapshot they started with. |
| `ProviderCredential` | Server-only secret material encrypted at rest with a key supplied by deployment configuration. API responses contain ID, provider, masked suffix, validation state/time, and capabilities; never secret bytes. |
| `ProviderCapability` | Registry-derived provider/model/voice/language capability metadata and validation result. Credential strings are not inspected to guess capabilities. |
| `MCPServer` | Mutable Server resource with endpoint/transport/auth reference, enable state, validation state, and latest immutable discovery reference. OAuth/token material remains server-side. |
| `MCPToolDiscovery` | Immutable discovery snapshot. A canonical, name-sorted representation gets a SHA-256 `discovery_hash`; each tool schema gets its own canonical SHA-256 `schema_hash`. A refresh creates a new snapshot and never rewrites history. |
| `AgentMCPTool` | Version-scoped association keyed by agent version, MCP server ID, and remote tool name. Stores selected schema hash and required/optional policy. Schema drift or disappearance marks current configuration as needing review; historical snapshots remain readable. |
| `SessionRecord` | Persistent product session metadata: UUIDv7 ID, agent/version/config hash, start/end, status, provider stack, turn count, and safe summary. Stores the immutable runtime snapshot reference needed to explain which configuration ran. |
| `TurnRecord` | Optional history row keyed by UUIDv7 and Session ID, with timing, final transcript/assistant text only when policy permits, latencies, interruption status, and tools invoked. |
| `Trace`, `Span`, `Event` | Server adapter records framework correlation and nested time relationships. Events carry typed name, safe JSON metadata, and parent/session/turn IDs. Raw audio is excluded. Content persistence is independently governed by retention/privacy settings. |
| `ToolCallRecord` | Server trace/history projection with tool/MCP identity, timing/status, schema hash, and redacted argument/result summaries. It survives live MCP configuration deletion. |

Normal MCP deletion is blocked while current agent versions reference the server. Disable is reversible and removes tools from new session snapshots. Explicit detach-and-delete updates current versions, stops new references, lets active sessions drain, then removes the live configuration; historical version, discovery, session, and tool-call evidence is retained. No database cascade removes agent/session/trace history.

### Storage boundary

The Server uses `database/sql`, `modernc.org/sqlite` for the CGO-free single-node SQLite driver, and `github.com/jackc/pgx/v5/stdlib` for PostgreSQL. Dialect-specific migrations are embedded in the Server and versioned. Thin repositories own SQL and transaction boundaries; product services own validation and domain rules. SQLite and PostgreSQL migrations implement the same repository contract. The Framework has no SQL driver or repository package.

Task 3's storage package uses Goose Provider v3.28.0 for ordered embedded SQL migrations and version bookkeeping. PostgreSQL uses Goose's session advisory lock; SQLite uses a sibling file lock during migration and one connection with `foreign_keys=ON`, WAL, and a busy timeout. Repositories persist opaque UUIDv7 IDs and UTC timestamps. Agent version config and MCP tool schemas use sorted canonical JSON and lower-case SHA-256; discovery hashes the name-sorted `(remote_name, schema_hash)` pairs. The immutable snapshot rows reject updates at the database layer. Historical MCP discovery and associations retain the original server ID as an opaque value after explicit detach-and-delete. Storage operations, backup expectations, and the optional PostgreSQL test DSN are in `docs/operations/storage.md`.

After Task 3 review, SQLite connection settings are applied through the driver DSN on every physical connection. Repository-issued durable IDs are validated as UUIDv7, and timestamps use UTC microsecond precision. PostgreSQL preserves canonical JSON bytes in `TEXT` columns rather than rewriting them through JSONB. Agent versions and selected MCP tools publish atomically; a selected tool must match the live server's latest discovery and schema hash. Database triggers enforce current-version/latest-discovery pointer ownership and session agent/version/hash provenance. Published selections cannot be appended later.

Storage rejects caller SQLite DSN aliases and `_pragma` that could override its mandatory connection policy. File URI migration locks use decoded, normalized paths; in-memory databases do not create lock files. UUIDv7 validation also requires the RFC 4122 variant bits.

### Management API and authentication boundary

Management routes begin at `/api/v1`; success and error bodies use versioned JSON DTOs independent of database structs and framework frames. Management UI/API authentication is distinct from public voice-client admission. Voice clients receive short-lived, scoped, single-use connection tokens and never receive management credentials or provider secrets. The Server configuration endpoint is read-only. Provider secret create/update requests accept a secret once; reads return only masked metadata.

Task 4 freezes the initial wire boundary in `docs/api/v1.md`: the admin-only management API uses a deployment environment key, hashed at startup and compared in constant time; without it, `/api/v1` is unavailable. A separate POST issues at most 60-second `voice:connect` JWTs under the existing voice signing configuration. The WebSocket validator rejects foreign explicit scopes and consumes `jti` once, but accepts historical unscoped V1 admission JWTs during migration. This compatibility exception is limited to public voice admission and does not grant management access. Read-only server configuration is a sanitized projection, never the raw deployment config. The cost of the bootstrap bearer key is deployment-side key distribution/rotation; adding user sessions or RBAC later changes management authentication while keeping the `/api/v1` resource DTO boundary. Retiring unscoped voice JWTs later requires client/token-issuer coordination.

Task 4 review round 1: scoped tokens now carry a validator-generated random issuance epoch, so a replacement validator rejects prior scoped tokens despite clock skew and an empty replay cache. Issuance and admission must be pinned to one replica until a shared replay/issuance service exists. The effective production `kid` is shared between signer and validator; durations use whole-second JWT precision with a configured minimum of two seconds. API body limits count all bytes and noncanonical version paths return the JSON error envelope. The cost is loss of cross-replica portability for API-issued scoped tokens; historical unscoped V1 tokens retain their earlier compatibility behavior.

### UI information architecture

The separate `ui/` product uses React, TypeScript, and Vite. Its primary sections are Dashboard, Agents, Providers, MCP Servers, Sessions, and read-only Server Configuration. Agent detail combines configuration, readiness, selected MCP tools, and the official Pipecat client/UI test panel on the production RTVI path. Session detail displays persisted server traces. A generated or strongly typed API client consumes only `/api/v1` DTOs.

**Acceptance criteria**

- A changed agent creates a new immutable version; an already-running session still reports the original config hash.
- A discovered tool schema change creates a new immutable discovery hash and does not silently alter old agent/session snapshots.
- Database and API deletion paths cannot cascade away historical session, tool-call, or trace evidence.
- Provider and MCP secrets never appear in browser reads, logs, traces, or error messages.
- UI functionality is reachable through the management API and no UI imports Server repositories or core implementation internals.
- SQLite and PostgreSQL pass the same repository contract suite and migrations can be applied on an empty database and upgraded from the prior schema.

## 4. Dependency gates A–G

| Gate | Required evidence to pass |
|---|---|
| A — Core Contracts Stable | Typed session/turn identity, trace model, provider-neutral tool model, and observer/debug model are stable; public contract tests pass. Required before deep provider/MCP/tool work. |
| B — P0 Audio Path Stable | Turn management, STT commit, output pacing, deterministic flush/barge-in, silence/no-speech, and audio normalization pass focused and integrated tests. Required before parity results are treated as meaningful. |
| C — Tool Loop Stable | Provider-neutral multi-step loop, Google adapter, and xAI adapter pass the same call/result, limit, timeout, cancellation, and error contracts. Required before MCP end-to-end integration. |
| D — Persistent Agent Runtime | Immutable agent snapshot, credential resolution, provider selection, and MCP tool wiring work at session construction. Required before the UI test surface is authoritative. |
| E — Trace End-to-End | Framework span → Server persistence → management API → UI timeline is correlated for a real session and remains useful under metadata-only privacy mode. |
| F — Compatibility | RTVI conformance, Whisker semantic conformance, and P0 Pipecat behavioral parity each pass their separate suites; intentional differences are documented. |
| G — Product Release | Onboarding, persistent agents, Pipecat UI test, MCP tool flow, session traces, security, and production packaging pass the seven vertical scenarios and release checklist. |

## 5. Rulings and assumptions

- Ruling: Keep `SessionID`, `TurnID`, and `Generation` as distinct framework types; use UUIDv7 strings for server durable IDs — opaque IDs simplify correlation while generation remains only a cancellation fence — cost if wrong: changing identity plumbing after tracing and provider work lands will require broad API and stored-data migrations.
- Ruling: Hash normalized immutable `AgentVersion` JSON and per-tool canonical schema JSON with SHA-256; keep the hash outside its own input — stable hashes make session provenance and MCP schema drift comparable — cost if wrong: a different canonicalization contract changes all snapshot hashes and needs explicit schema-version migration.
- Ruling: Use `database/sql`, `modernc.org/sqlite`, `pgx/v5/stdlib`, embedded dialect migrations, and thin repositories — this supports CGO-free single-node deployment and PostgreSQL without putting SQL in core — cost if wrong: changing driver or migration structure later touches every repository and deployment build.
- Ruling: Default trace content to metadata-only; raw audio is never traced and text/tool payloads require explicit policy — voice data is sensitive and timing remains useful without content — cost if wrong: stricter defaults may frustrate debugging, while looser defaults increase retention, consent, and breach exposure.
- Ruling: Use the official `github.com/modelcontextprotocol/go-sdk/mcp` package at `v1.7.0` behind a Server-owned `MCPClient` adapter; prefer Streamable HTTP for remote servers and require explicit admin configuration for local/process transport — the official SDK documents client and transport APIs and the version compatibility matrix — cost if wrong: SDK churn or remote-server dialect differences may require an adapter replacement, while a narrower initial transport set delays integrations. ([official Go SDK](https://github.com/modelcontextprotocol/go-sdk))
- Ruling: Identify an MCP tool by `(MCPServerID, remote tool name)` and bind every selected version to an immutable `schema_hash` — remote names are scoped by server and schema changes require review instead of silent identity replacement — cost if wrong: servers that reuse names for distinct resources require adding a remote stable tool ID and migrating associations.
- Ruling: Encrypt provider and MCP secrets with AES-256-GCM using a deployment-managed 32-byte key identifier kept outside the database; mask values in every API response — this supports authenticated encryption and explicit key rotation without persisting the encryption key with the ciphertext — cost if wrong: adopting a KMS envelope or hardware-backed key later requires key-version migration and a credential re-encryption pass.
- Ruling: Separate management authentication from voice admission; issue short-lived, scoped, single-use voice tokens and never expose management/provider secrets to the browser — public RTVI clients and administrators have different trust boundaries — cost if wrong: a different deployment identity model may require replacing the management session mechanism and its UI integration.
- Ruling: Use React, TypeScript, Vite, and a typed `/api/v1` client for the management UI — this keeps UI concepts outside Go core and matches the V2 admin product scope — cost if wrong: choosing another frontend toolchain changes UI build/hosting integration but not the HTTP contract.
- Ruling: Keep Whisker compatibility semantic through a Framework observer/debug adapter until an official package and protocol version are pinned — no exact Whisker wire target is specified in the approved inputs — cost if wrong: byte-level interoperability will require a versioned wire serializer and additional fixture/version maintenance.
- Ruling: A database trace/history failure is observable but cannot abort an active voice session; failure to resolve an agent or credential prevents new session creation — an existing session must outlive optional observability writes while new sessions require valid startup configuration — cost if wrong: operators may temporarily miss history during storage outages or reject a session that a degraded provider fallback could have served.
