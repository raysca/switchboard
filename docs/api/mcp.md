# MCP management API

The MCP API is mounted under `/api/v1/mcp-servers` behind the management bearer authentication. `Server.EnableManagement` (`server/management.go`) builds the service and installs `mcp.Handler(service)`; `mcp.Handler` takes only the service and performs no authentication of its own. The server's `/api/v1` entry point (`handleAPI` in `server/api_routes.go`) answers 503 `management_unavailable` when no management key is configured, authenticates every request (401, or 429 after repeated failures) and propagates its trusted request ID before routing to the handler, so the handler must never be mounted anywhere else. Request and response bodies use JSON; responses are `no-store` through the shared API writer.

| Method and path | Purpose |
|---|---|
| `GET /api/v1/mcp-servers` | List configured servers in stable name/ID order. |
| `POST /api/v1/mcp-servers` | Create a remote Streamable HTTP server. Body fields: `name`, optional `description`, `endpoint`, optional `transport`, optional `auth_env`, and `enabled`. The HTTPS endpoint may not contain user-info or query parameters; use `auth_env`. With `mcp.allow_local_http` (development and tests only) an `http` endpoint on loopback is also accepted; see [security/mcp.md](../security/mcp.md). `auth_env` names an environment variable of the server process and must match `MCP_TOKEN_[A-Z0-9_]{1,48}` (for example `MCP_TOKEN_CRM`); its value is sent as the Bearer token. Server views return `auth_env` and `auth_env_set` (whether the variable is currently set to a usable single-token value), never the token. Unknown fields such as the removed `credential_id` are rejected. |
| `GET /api/v1/mcp-servers/{id}` | Read a server and its latest immutable discovery snapshot, if present. |
| `POST /api/v1/mcp-servers/{id}/disable` | Reversibly disable assignment to new sessions while preserving the catalog and active captured sessions. |
| `POST /api/v1/mcp-servers/{id}/enable` | Re-enable assignment; the server returns to `not_validated` and must be validated before required-tool readiness recovers. |
| `DELETE /api/v1/mcp-servers/{id}` | Delete only when no current flow version or active captured session refers to the server; otherwise returns 409 with current flow/tool dependencies. |
| `POST /api/v1/mcp-servers/{id}/detach-delete` | Requires `{"confirm":true}`. Publishes new flow versions without this server's selections and drains active sessions before deleting the live server row. |
| `POST /api/v1/mcp-servers/{id}/validate` | Connect, discover, validate schemas, and persist a new snapshot on success. |
| `POST /api/v1/mcp-servers/{id}/tools/{tool}/test` | Execute a discovered tool outside a conversation. Body: `{"arguments":{...}}`. |

The server DTO contains the `auth_env` variable name and `auth_env_set`, never the token. The `MCP_TOKEN_` prefix is checked again every time the token is read, so a row written outside the API with any other variable name never sends that variable's value. Endpoint query values are masked in responses. Validation and tool execution errors are bounded safe category messages and omit remote response details. Discovery results are sorted by remote tool name and include the canonical schema hash per tool plus the snapshot hash.

Detach-and-delete preserves prior flow versions, discovery snapshots, session runtime metadata, and historical tool-call evidence. If no session is active the live server row is removed immediately; otherwise the response is `202` with `pending_deletion:true`, and the last session close completes deletion. Existing in-memory wrappers hold the bearer token captured at session start until the session ends. Rotating the token (change the `MCP_TOKEN_*` variable and restart) applies to new sessions; end active sessions when the old token must stop immediately.

Management body parsing is bounded by the shared 64 KiB API decoder. Tool arguments, schema, and result limits are independently enforced by the MCP service. A successful test returns `execution_id`, `mcp_server_id`, `tool_name`, `status`, `duration_ms` (integer milliseconds), and sanitized JSON in `result`.

The config-driven voice server does not construct the MCP service automatically because that runtime does not own a configured persistent storage handle. The injectable route can be mounted by supplying a composed service with the configured `storage.Store`, token source (`mcp.EnvTokens`), and remote client.
