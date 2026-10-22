# MCP ship readiness: truthful state, useful errors, editable servers

Status: draft for review. Date: 2026-10-22.

## Why

An audit of the MCP integration (registration, sync, Try it, in-call use, security, tests) found it safe and well tested but not ready for real operators:

1. **One failed Sync takes tools out of calls until someone syncs again.** A call can only capture a tool whose server's stored state is `healthy` (`server/mcp/service.go` `CaptureTool`), and that state is written only by the manual "Sync tools" button (or the dental demo installer). Nothing re-checks it.
2. **"Connected" can be false.** A server that dies after its last sync still shows Connected and its flows read "ready". A token variable that is missing after a restart is ignored by readiness, yet calls that need the tool are rejected.
3. **The model cannot react to tool errors.** Every failure reaches the model as `{"error":"mcp_tool_failed"}` (`server/runtime/mcp.go`): bad arguments, timeouts, auth failures and the tool's own error ("no such order") look the same, so the caller hears "something went wrong".
4. **A server cannot be edited.** Changing an endpoint or token variable means detach-delete, which puts every flow using the server (even optionally) into `needs_tool_review`.

Two smaller gaps travel with these because the fixes need them: each Sync stores a new copy of the discovered tools even when nothing changed (with a periodic sync that would grow the database without bound), and every create failure shows one generic message.

## Decisions made

- **Periodic full sync, interval set by the operator in the server config.** Not a reachability ping: a full sync also catches schema drift, so flows move to "needs tool review" on their own.
- **Editing a server is in this pass.**

## Design

### 1. Periodic sync (`mcp.sync_interval`)

- New server config key `mcp.sync_interval` (Go duration). Default `5m`. `0` turns it off. Allowed range when on: `30s` to `24h` (validated at startup).
- One background loop owned by the server's management lifecycle: it starts when management is enabled, stops at shutdown (bounded by the shutdown context), never runs twice at once, and has a recover so a panic cannot take the server down.
- Each tick syncs every enabled server that is not pending deletion, one after another, each bounded by the existing `mcp.timeout`. A small random delay at start spreads instances. A manual Sync can run at the same time; the store writes are last-writer-wins and both describe the same remote.
- **Two strikes before "down".** A background sync failure on a server that is currently healthy is recorded in memory; the server is marked failed only on the second failure in a row. A success clears the count. A manual Sync always writes its result at once (an operator who clicks Sync sees the truth now). A server that is already failing is updated on every tick.
- **No duplicate discoveries.** A sync whose tool list hashes to the same value as the latest stored discovery does not insert a new one; it only updates the server's state and its last-connected and last-discovered times.
- Drift needs nothing new: a changed schema produces a new discovery, and the existing flow-readiness logic flags flows that pinned the old one.
- Visible: the server detail page shows when the server was last checked and that it is checked automatically every N minutes (or "automatic checks are off"); `GET /api/v1/server-config` exposes `mcp_sync_interval_seconds`. A log line is written on state changes only (healthy to failed and back), never per tick. Metrics: `voice_mcp_sync_total{result="ok|failed"}` (two fixed label values).

### 2. Readiness knows about a missing token variable

- A server whose `auth_env` names a variable that is not set on this server makes each of its tool dependencies `auth_env_missing`: a required tool makes the flow `tool_unavailable` (not callable), an optional one gives a warning, and the flow's `missing_env` lists the variable name (the name only, never a value).
- The MCP page shows "Token variable MCP_TOKEN_X is not set on the server" as the server's status, not as small print.
- The periodic sync also marks such a server `authentication_failed` within one interval; the readiness check makes it immediate.

### 3. Errors reach the model with a category and the tool's own message

- Tool failures map to a small, fixed set of categories: `invalid_arguments`, `timeout`, `unavailable` (connection, initialization or authentication), `result_too_large`, `tool_error` (the tool answered with an error), `cancelled`.
- The model receives `{"error":"<category>"}`; for `tool_error` it also receives `"message"`: the text parts of the tool's error result joined, control characters removed, the credential removed (as for results today), capped at 512 bytes. Tool error text is no more trusted than a successful result, which already reaches the model.
- The category is stored on the tool-call row (new column `error_category`, migration 008 for SQLite and Postgres), shown on the Calls page next to the status, and shown with the message in the Try it console. Traces carry the category only, never the message.

### 4. Editing a server

- `PATCH /api/v1/mcp-servers/{id}` accepts any of `name`, `description`, `endpoint`, `auth_env`, with the same validation as create (destination policy, `MCP_TOKEN_*` pattern, metadata limits).
- Changing `endpoint` or `auth_env` resets the state to `not_validated` and runs a sync immediately; the response carries the new state. Flows keep their references (the server id does not change), so nothing is detached. Calls already running keep the endpoint and token they captured until they end.
- The UI gets an Edit action on the server detail page, reusing the connect dialog in an edit mode.

### 5. Create and edit errors say why

- Instead of one message, create and edit return a stable reason code: `invalid_endpoint`, `destination_denied`, `invalid_auth_env`, `invalid_metadata`, `transport_unsupported`. The dialog shows a matching sentence. No code reveals internal network details (the destination check does not say what an address resolved to).

### 6. Operator documentation

- A new `docs/operations/mcp.md`: what each state means, that servers are synced automatically every `mcp.sync_interval` and on demand, the two-strike rule, how to rotate a token (set the new value and restart; the variable name stays), how to edit a server, and how to recover a flow in `needs_tool_review`.
- Fix the MCP page sentence "Private and loopback addresses are refused", which is wrong in development with `mcp.allow_local_http`.

## Testing

- Unit: the sync loop (ticks, two strikes, recovery, shutdown, no overlap, panic recovery) with an injected clock; no duplicate discovery on an unchanged list; readiness with a missing token variable; error categories and message extraction (control characters, cap, credential removed); PATCH validation and the re-sync on endpoint change.
- Go end-to-end with the MCP test servers (`server/mcp/mcptest`): a server that goes down and comes back is marked failed after two background failures and healthy again after one success; a call sees `tool_error` with the tool's message; an edited endpoint works without re-pinning any flow.
- UI (vitest): status lines, the Edit dialog, reason-code messages, the category on the Calls page and in Try it.
- Browser (Playwright, echo provider, local test servers): edit a server's endpoint and see it reconnect; a failing tool's message shown in Try it.
- Gate: `make verify`, `make deadcode`, `make e2e-echo`; independent review of the branch.

## Out of scope

A form built from the JSON schema in Try it; a confirmation before Try it runs a tool with side effects; refining the key-name redaction of results; Playwright coverage for delete, detach and enable/disable; support for an egress proxy.

## Risks

- **Load on remote servers:** every enabled server is synced every interval. The interval is the operator's control; the default (5 minutes) is one list call per server.
- **Many servers:** sequential syncs, each bounded by `mcp.timeout`, can take longer than the interval; the loop never overlaps itself and simply starts the next round late.
- **Flapping:** the two-strike rule hides a single blip; a server that fails every other check stays healthy. That is the intended trade.
