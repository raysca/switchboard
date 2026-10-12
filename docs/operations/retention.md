# Retention and Privacy Operations

This document describes the operational management of trace retention, privacy controls, and OpenTelemetry export in Switchboard.

## Overview

Switchboard distinguishes operational metadata from conversational content:
- **Operational metadata:** Session start/end timestamps, turn ordering, latencies, provider names, models, safe error categories, and span trees needed for reliability monitoring and diagnostic debugging.
- **Conversational content:** User speech transcripts, assistant response text, tool call arguments, and tool results containing user inputs or private data.

Operators can configure independent retention lifetimes for content and metadata to satisfy privacy regulations (e.g. GDPR, HIPAA) while maintaining operational observability.

---

## Configuration

Retention and OpenTelemetry export are configured under the `retention` and `otel` sections of `config.yaml`:

```yaml
# Data retention and cleanup
retention:
  enabled: true
  interval: 1h
  metadata_retention_days: 30
  content_retention_days: 7
  batch_size: 500

# OpenTelemetry export (optional)
otel:
  enabled: false
  endpoint: "localhost:4317"
  protocol: "grpc" # "grpc" | "http"
  service_name: "govoice"
  queue_size: 2048
  batch_timeout: 5s
  export_interval: 10s
```

### Safe Defaults

When not explicitly configured:
- **Retention:**
  - `enabled`: `false` (explicit opt-in required for automated background purge)
  - `interval`: `1h`
  - `metadata_retention_days`: `30`
  - `content_retention_days`: `7`
  - `batch_size`: `500`
- **OTel:**
  - `enabled`: `false`
  - `service_name`: `"govoice"`
  - `queue_size`: `2048`

---

## Retention Job Lifecycle

The retention cleanup runner (`server/retention/job.go`) runs periodically according to `retention.interval`. Each run executes in two distinct phases:

### 1. Content Purge

When `content_retention_days` has expired for ended sessions:
- `turns.transcript` is set to `NULL`.
- `turns.assistant_text` is set to `NULL`.
- `tool_calls.arguments_summary` is set to `""`.
- `tool_calls.result_summary` is set to `""`.
- `trace_spans` and `trace_events` attributes are sanitized to remove any text fields (`transcript`, `prompt`, `text`, `tool.arguments`, `tool.result`).

**Preserved Metadata:** Session records, turn rows, ordinal sequence, latencies, tool call status, error categories, and span trees are completely preserved for operations and performance dashboards.

### 2. Metadata Purge

When `metadata_retention_days` has expired for ended sessions:
- All trace events associated with the session are deleted.
- Trace spans are unlinked (`parent_id = NULL`) and deleted.
- Tool call records are deleted.
- Turns are deleted.
- Session MCP tool mapping associations (`session_mcp_tools`) are deleted.
- The session record is deleted.
- Standalone trace spans and events older than the metadata cutoff are cleaned up.

---

## Deletion Invariants

The retention system enforces strict invariants at both the service and database layers:

1. **Active and Draining Sessions:**
   Sessions with `status = 'active'` or `status = 'draining'` are **never** deleted and their content is **never** purged, regardless of session start time or duration.
2. **Agent Versions:**
   `agent_versions` rows are immutable and are **never** deleted by retention jobs. Database triggers enforce immutability (`agent_versions_immutable_delete`).
3. **MCP Discoveries and Tool Schemas:**
   `mcp_discoveries`, `mcp_tools`, and `agent_mcp_tools` are immutable and are **never** touched by retention jobs.
4. **Foreign Key Integrity:**
   Deletions occur in strict topological order (`trace_events` → `trace_spans` → `tool_calls` → `turns` → `session_mcp_tools` → `sessions`) within isolated transactions, satisfying `ON DELETE RESTRICT` constraints on both SQLite and PostgreSQL.

---

## Batch Bounded Execution

To ensure database availability under heavy production load:
- Deletions and updates are executed in batches bounded by `batch_size` (default `500`).
- Within each batch, operations are processed in smaller sub-batches of 25 sessions per transaction, committing iteratively to release exclusive SQLite write locks and prevent stalling concurrent trace insertions.
- Large backlogs are drained iteratively across short-lived transactions rather than locking tables in a single massive query.

---

## OpenTelemetry Integration

When `otel.enabled` is `true`:
- The exporter operates asynchronously via a bounded queue (default `2048` operations).
- **Outage Isolation:** If the OpenTelemetry collector is unreachable, spans are dropped safely and error metrics incremented. Voice pipelines and server operations are never blocked.
- **Trace Correlation:** Internal UUIDv7 IDs map directly to OpenTelemetry standard fields:
  - `SessionID` → 16-byte `TraceID` and `session_id` attribute.
  - `SpanID` → 8-byte `SpanID` and `span_id` attribute.
  - `ParentID` → parent `SpanID` and `parent_id` attribute.
  - `TurnID` → `turn_id` attribute.
- **Privacy Compliance:** In metadata mode (default), raw audio, transcripts, credentials, and tool payload summaries are strictly stripped before reaching the collector.
