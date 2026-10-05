# MCP management API

The MCP API is mounted under `/api/v1` behind the existing management bearer authentication. Compose the service and call `Server.SetMCPManagementAPI(mcp.Handler(service, management))`; the management listener authenticates first and propagates its trusted request ID. The `server/mcp.Handler` also independently fails closed if authentication is absent. Request and response bodies use JSON; responses are `no-store` through the shared API writer.

| Method and path | Purpose |
|---|---|
| `GET /api/v1/mcp-servers` | List configured servers in stable name/ID order. |
| `POST /api/v1/mcp-servers` | Create a remote Streamable HTTP server. Body fields: `name`, optional `description`, `endpoint`, optional `transport`, optional `credential_id`, and `enabled`. The HTTPS endpoint may not contain user-info or query parameters; use a credential reference. |
| `GET /api/v1/mcp-servers/{id}` | Read a server and its latest immutable discovery snapshot, if present. |
| `POST /api/v1/mcp-servers/{id}/validate` | Connect, discover, validate schemas, and persist a new snapshot on success. |
| `POST /api/v1/mcp-servers/{id}/tools/{tool}/test` | Execute a discovered tool outside a conversation. Body: `{"arguments":{...}}`. |

The server DTO contains the opaque credential reference, never credential bytes. Endpoint query values are masked in responses. Validation and tool execution errors are bounded safe category messages and omit remote response details. Discovery results are sorted by remote tool name and include the canonical schema hash per tool plus the snapshot hash.

Management body parsing is bounded by the shared 64 KiB API decoder. Tool arguments, schema, and result limits are independently enforced by the MCP service. A successful test returns `execution_id`, `mcp_server_id`, `tool_name`, `status`, `duration_ms` (integer milliseconds), and sanitized JSON in `result`.

The config-driven voice server does not construct the MCP service automatically because that runtime does not own a configured persistent storage handle. The injectable route can be mounted by supplying a composed service with the configured `storage.Store`, credential resolver, and remote client.
