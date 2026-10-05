# MCP Management and Sessions/Trace UI

This document details the admin interface for Model Context Protocol (MCP) tool integration and conversational session trace inspection implemented in Task 23 (Phase 6).

## 1. Architecture Overview

The UI layer communicates exclusively with `/api/v1/*` endpoints exposed on the go-voice Server's private admin listener, using bearer authentication held in browser memory. Plaintext credentials are never exposed to or stored by browser state.

```
┌────────────────────────────────────────────────────────────────────────┐
│                            Admin UI Shell                              │
├──────────────────────────────────┬─────────────────────────────────────┤
│         MCP Management           │          Session Traces             │
│          /mcp-servers            │       /sessions, /sessions/:id      │
│  Servers, Discovery, Test Console│ Paginated History, Timeline & Traces│
└─────────────────┬────────────────┴──────────────────┬──────────────────┘
                  │                                   │
                  └─────────────────┬─────────────────┘
                                    ▼
                      Typed API Client (`client.ts`)
                                    │ (Bearer auth in memory)
                                    ▼
                     Server Admin API (`/api/v1/*`)
                                    │
                  ┌─────────────────┴─────────────────┐
                  ▼                                   ▼
        MCP Handler (`/mcp-servers`)         Traces Handler (`/sessions`)
```

## 2. MCP Management UI (`/mcp-servers`)

The MCP management view provides complete lifecycle management, tool discovery inspection, and sandboxed testing for external tool servers conforming to the Model Context Protocol:

### Server Catalog & Status Badges
- **Catalog List:** Displays all registered MCP servers with server name, description, Streamable HTTP endpoint URL (with redacted query parameters), transport type, latest discovery snapshot hash, and enabled/disabled state.
- **Status Badges:**
  - `connected` (green/good tone): Server is enabled and its validation state is `valid` or `connected`.
  - `error` (red/bad tone): Server is in an error state (`invalid`, `schema_invalid`, `authentication_failed`, or `unreachable`).
  - `disabled` (neutral tone): Server is disabled for new agent assignments.

### Add Server Workflow
- Captures server name, optional description, remote endpoint URL (e.g. `https://mcp.example.com/rpc`), transport configuration (`streamable-http`), credential selection from configured provider credentials, and enabled toggle.
- In accordance with V2 security policies, endpoint query parameters and embedded user-info are disallowed; authentication uses explicit credential references.

### Server Detail & Discovered Tools
- Inspects remote capabilities retrieved through MCP discovery snapshots:
  - Canonical tool remote name, description, and canonical SHA-256 schema hash.
  - **Schema Viewer:** Interactive expandable viewer displaying the full JSON schema of discovered tool input arguments.
  - **Schema Drift Badges:** When an active agent references an earlier version of a tool whose schema hash differs from the latest discovery snapshot, a `Schema drift` warning badge is displayed.

### Interactive Tool Test Console
- Provides sandboxed execution of discovered tools outside of active voice conversations:
  - Interactive JSON editor for tool arguments.
  - Bounded execution safeguards: enforces client-side JSON validity and strict 64 KiB payload boundaries before submission.
  - Executes tool test via `POST /api/v1/mcp-servers/{id}/tools/{tool}/test`.
  - Displays execution status (`completed` or `failed`), execution duration in milliseconds, unique `execution_id`, and formatted sanitized JSON results.
  - Handles remote failures (e.g., 400, 404, 502) with actionable error banners.

### Lifecycle Actions & Delete Protection
- **Enable / Disable Toggle:** Reversibly enables or disables assignment to new sessions while preserving active sessions and catalog bindings.
- **Refresh Discovery:** Triggers immediate reconnection and schema introspection (`POST .../validate`), persisting a new discovery snapshot upon success.
- **Delete Protection:**
  - Standard deletion (`DELETE /api/v1/mcp-servers/{id}`) fails closed if any agent version or active session currently references the server, returning HTTP 409 `mcp_server_in_use`.
  - The UI intercepts conflict errors and presents a **Delete Conflict Alert** listing the dependency conflict.
  - Operators can trigger **Confirm Detach & Delete** (`POST .../detach-delete` with `{"confirm": true}`), which atomically publishes new agent versions removing the tool selections before purging the server row.

## 3. Sessions & Historical Traces UI (`/sessions`, `/sessions/:sessionId`)

### Session Catalog (`/sessions`)
- **Paginated Table:** Lists historical and active calls with session ID, agent name, agent version snapshot, status badge (`active`, `completed`, `failed`), turn count, formatted elapsed duration, and start timestamp.
- **Filtering Controls:**
  - Status filter dropdown (`All statuses`, `active`, `completed`, `failed`, `terminated`).
  - Agent filter for filtering by specific agent identifiers.
- **Pagination Controls:** Next and Previous controls with configurable limit per page (10, 25, 50) and offset tracking.
- **Empty States:** Clear messaging when no sessions exist or when filters yield no matches.

### Session Detail & Metadata Overview (`/sessions/:sessionId`)
- Direct access to specific session records with full metadata cards:
  - Agent ID and pinned Agent Version ID.
  - Pinned canonical configuration hash (`config_hash`) proving session immutability.
  - Call duration, turn count, and safe operational summary.
  - **Captured Runtime MCP Tools:** Lists all MCP tools bound to the session at initiation, including server ID, remote name, canonical schema hash, and required/optional status.

## 4. Turn Timeline (`TurnTimeline.tsx`)

The chronological turn timeline renders the conversational and diagnostic history of a voice call:

- **Chronological Ordering:** Conversational turns are guaranteed to render in ascending ordinal sequence (`Turn #1`, `Turn #2`, etc.) regardless of API response order.
- **Speech & Response Bubbles:**
  - User speech transcript and assistant response text displayed in distinct speech bubbles.
- **Interruption Badges:** Turns where user speech interrupted bot playback display a prominent `Interrupted` badge.
- **Latency Breakdown Bar:** Formats latency metrics extracted from session turns into diagnostic pills:
  - VAD latency (`vad_ms`)
  - STT transcription latency (`stt_ms`)
  - LLM completion latency (`llm_ms`)
  - TTS audio synthesis latency (`tts_ms`)
  - End-to-end turn latency (`total_ms` / `e2e_ms`)
- **Expandable Trace Execution Tree:**
  - Operators can expand trace details correlated to each specific turn:
    - **Correlated Spans:** Displays span name (e.g. `llm.generate`, `tts.synthesize`), duration in milliseconds, and sanitized attributes.
    - **Correlated Tool Calls:** Displays remote tool name, server ID, status, execution duration, argument summary, and result summary.
    - **Correlated Events:** Diagnostic timeline events with timestamps (e.g. `speech_started`, `first_token`).

## 5. Privacy Mode Compliance & Security Invariants

- **No Secrets in Browser:** Provider and MCP credentials never appear in API responses, component state, or trace views. Only masked references or key identifiers are rendered.
- **No Raw Audio in Traces:** The UI renders no `<audio>` elements or streaming media tags. Voice telemetry contains only text transcripts and latency numbers.
- **Metadata-Only Policy Compliance:** When transcripts or responses are redacted under privacy controls, the UI displays explicit `[Metadata only - speech content omitted]` and `[Metadata only - response content omitted]` placeholders rather than blank space or errors.
- **Bounded Request Decoding:** Test argument inputs are capped at 64 KiB, matching the backend decoder boundary.
- **Diagnostic Request IDs:** Every error state surfaces the trusted `X-Request-ID` header from the response.

## 6. Verification and Test Suite

Vitest suites verify all aspects of the MCP and trace interfaces:
- `ui/src/api/client.test.ts`: MCP CRUD, discovery, execution, detach-delete, and session/turn/trace queries.
- `ui/src/routes/mcp/MCPView.test.tsx`: Server listing, status badges, creation, detail inspection, schema drift badges, interactive test console, error handling, and delete conflict protection.
- `ui/src/routes/sessions/SessionsView.test.tsx`: Session listing, status/agent filtering, pagination limits/offsets, and empty states.
- `ui/src/routes/sessions/SessionDetailView.test.tsx`: Metadata overview cards, config hash, runtime MCP tool snapshots, and timeline integration.
- `ui/src/components/timeline/TurnTimeline.test.tsx`: Chronological turn ordering, interruption badges, latency metric pills, privacy mode compliance (no audio tags), and expandable trace tree.
