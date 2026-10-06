# Privacy policy

## Runtime traces

Core tracing is metadata-only unless a caller explicitly selects
`TracePolicy{Content: TraceText}`. The runtime records lifecycle names,
timestamps, opaque session/turn/call correlation, finite counts, formats, and
safe status/category labels. It never records raw audio, credentials, raw
provider/executor errors, unrestricted text, or arbitrary attributes.

Text mode is bounded (4 KiB by default and 16 KiB maximum) and only admits the
selected `transcript`, `prompt`, and `text` fields. Tool arguments and results
need a caller-supplied redactor; the redactor is panic-isolated and over-limit
or invalid output is dropped. The core ToolLoop records only the redacted
argument at execution start. It intentionally does not trace a result payload:
that value is untrusted until the loop has normalized it and committed it to
the conversation context.

Trace attributes are copied and recursively validate only finite scalar and
bounded `custom.*` numeric/boolean metadata. Custom metadata never admits
strings, even under innocent-looking keys; selected text must use the explicit
bounded text/redaction policy. The trace policy is an admission boundary, not a
retention policy: exporters and the Server remain responsible for access,
storage, and deletion controls.

## Failure isolation

Runtime trace callbacks run through an asynchronous bounded dispatcher. A slow,
failing, or panicking exporter cannot fail or hold the voice session. Normal
attributes/events may be dropped when the queue is full; when a span start was
accepted, its one terminal operation is retained so the exporter observes a
balanced span. Admission reserves terminal capacity before a start, so overload
fails closed by declining new starts rather than retaining unbounded terminal
work. Only canonical error categories are supplied to `Span.End`.

Provider STT streams and TTS synthesis requests, VAD inference jobs, and
session-backed WebSocket/RTVI connections use the same metadata policy. Their
first-final, first-audio, speech-decision, connect/disconnect, and completion
events contain correlation IDs and bounded numeric metadata only. Provider,
executor, and transport failure spans receive fixed categories such as
`provider_failure`, `vad_failure`, `transport_failure`, `protocol_failure`, and
`canceled`; raw error messages are never passed to the tracer. A missing or
dropped ordinary event does not expose content or prevent an accepted span's
terminal operation.

## Server data retention and privacy controls

The server enforces configurable data retention and privacy controls that
distinguish operational metadata from conversational content:

- **Metadata retention (`MetadataRetentionDays` / `MetadataRetention`):**
  Defines the lifetime of session, turn, span, event, and tool call records.
  When expired, ended sessions and their associated trace trees are safely
  pruned.
- **Content retention (`ContentRetentionDays` / `ContentRetention`):**
  Defines the lifetime of conversation content (STT transcripts, LLM assistant
  responses, tool argument summaries, and tool result summaries).
  When content retention expires prior to metadata retention, the content purge
  sets `transcript` and `assistant_text` to null, clears tool argument/result
  payload summaries, and sanitizes text attributes in spans/events while
  preserving session, turn, span, event timestamps, latencies, and structural
  metadata for operational debugging.
- **Deletion invariants:**
  Active and draining sessions (`status IN ('active', 'draining')`), current
  and historical agent versions, and MCP discoveries are never deleted by
  retention jobs.
- **Batch bounded execution:**
  Purges execute in bounded batches (e.g. 500 records) to avoid database
  contention or long locks on both SQLite and PostgreSQL.

## OpenTelemetry trace export

The server provides an optional OpenTelemetry export sink (`server/otel/`):

- **Non-blocking:** Trace spans and events are enqueued to a bounded
  asynchronous queue. Queue overflow drops events with observable metrics
  rather than stalling voice calls.
- **Outage isolation:** Errors, network timeouts, or crashes in the OTel
  collector or exporter are isolated and never fail voice pipelines.
- **ID correlation:** Internal UUIDv7 IDs are mapped to standard OpenTelemetry
  constructs:
  - `SessionID` maps to the 16-byte OpenTelemetry `TraceID` and span attribute `session_id`.
  - `SpanID` maps to the OpenTelemetry `SpanID` and span attribute `span_id`.
  - `ParentID` maps to parent `SpanID` and span attribute `parent_id`.
  - `TurnID` maps to span attribute `turn_id`.
- **Privacy compliance:** The exporter respects the configured trace policy.
  By default (metadata-only), all transcripts, prompts, raw audio, credentials,
  tokens, api keys, and tool payloads are excluded from exported spans.

## Audio recording and capture sink (P1)

Removed. The `core/audio` capture processor had no production users and was deleted; the framework does not record call audio.
