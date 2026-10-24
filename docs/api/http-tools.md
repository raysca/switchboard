# HTTP tools API

An HTTP tool lets a step of an agent call one web endpoint, without an MCP server. The model sends arguments; the server builds the request, calls the endpoint within the tool's limits and hands the answer back. A tool's definition is immutable and identified by its hash: an agent version pins the hash it was reviewed against, and editing the tool creates a new definition. Arguments and results are never stored, traced or exported, as with MCP tools. Design record: [decisions/http-tools.md](../decisions/http-tools.md). Security: [security/http-tools.md](../security/http-tools.md).

The API is mounted at `/api/v1/http-tools` behind the management bearer key and uses the common envelope of [v1.md](v1.md).

| Method and path | Purpose |
|---|---|
| `GET /api/v1/http-tools` | List tools by name |
| `POST /api/v1/http-tools` | Create a tool from a definition (201) |
| `GET /api/v1/http-tools/{id}` | Read a tool and its current definition |
| `PUT /api/v1/http-tools/{id}` | Replace the definition: a new hash, the same id |
| `POST /api/v1/http-tools/{id}/enable`, `/disable` | Switch it on or off; a disabled tool cannot be chosen or called |
| `POST /api/v1/http-tools/{id}/test` | Call the current definition with `{"arguments": {...}}` and return what a model would see |
| `DELETE /api/v1/http-tools/{id}` | Delete a tool no agent version has ever pinned (204); a tool with history is disabled instead |

A tool view carries `id`, `name`, `enabled`, `hash`, `definition`, `created_at`, `updated_at` and, when the definition has an auth secret, `auth_secret_set` (whether the name resolves now). It never carries a secret value.

## Definition

```json
{
  "name": "get_order",
  "description": "Look up an order by id.",
  "method": "GET",
  "url": "https://api.example.com/orders/{order_id}",
  "headers": { "X-Tenant": "acme" },
  "auth": { "secret": "crm-api", "header": "Authorization" },
  "input_schema": { "type": "object", "properties": { "order_id": { "type": "string" } }, "required": ["order_id"] },
  "timeout_ms": 10000,
  "max_response_bytes": 65536,
  "response_pick": ["/data/status"]
}
```

| Field | Rules |
|---|---|
| `name` | 1-40 characters: lower-case letters, digits and `_`, starting with a letter. Unique, and fixed once the tool is created (agents and steps refer to it by name; an update that changes it is refused). It is what the model calls the tool. |
| `description` | Required, at most 1024 characters. Tells the model when to use the tool. |
| `method` | `GET`, `POST`, `PUT`, `PATCH` or `DELETE`. |
| `url` | `https` only (development servers also accept `http` to loopback). No user-info, query string or fragment. The host and port are fixed. `{name}` placeholders may appear in the path only, at most 8, each used once. |
| `headers` | Up to 16 static headers, values up to 512 characters. `Authorization`, `Cookie`, `Host`, hop-by-hop and length headers are refused: credentials go in `auth`. |
| `auth` | Optional. `secret` is a secret name: an `HTTP_TOKEN_*` environment variable or a lower-case secret ([secrets.md](secrets.md)); `MCP_TOKEN_*` names are refused. `header` defaults to `Authorization`, which is sent as `Bearer <value>`; any other header receives the raw value. |
| `input_schema` | A JSON Schema of type `object`, at most 16 KiB, checked with the same limits as MCP schemas. Every `{placeholder}` must be a required `string` or `integer` property. |
| `timeout_ms` | 100 to 30000; default 10000. |
| `max_response_bytes` | 256 to 262144; default 65536. A larger answer fails with `result_too_large`; it is never cut. |
| `response_pick` | Up to 10 JSON pointers (`/data/status`). When set, only those parts of a JSON object or array answer reach the model, as an object keyed by the pointers; other bodies pass unchanged. |

## The call

1. The arguments must satisfy `input_schema` (at most 64 KiB), or the call fails with `invalid_arguments` before any request.
2. Placeholders are filled from the arguments. A value must be a string or an integer and may not be empty, `.` or `..`, or contain `/`, a backslash or control characters; it is percent-encoded.
3. The remaining arguments become the query string for `GET` and `DELETE` (scalars, or lists of scalars as repeated parameters), or the JSON body for `POST`, `PUT` and `PATCH`.
4. The request goes to the fixed host with the static headers, `Accept: application/json, text/plain`, and the secret header if configured. Redirects are not followed.
5. A `2xx` answer becomes `{"status": 200, "body": ...}`. JSON bodies are passed as JSON, `text/plain` as a string, an empty body as `null`; any other content type fails. Secret-shaped keys and the credential itself are removed from the result.
6. Other statuses, unreadable bodies and transport failures fail with a category (below).

Error categories are the MCP ones ([operations/mcp.md](../operations/mcp.md)): `invalid_arguments`, `timeout`, `cancelled`, `result_too_large`, `tool_error` (the service answered with an error status such as `the service answered HTTP 404`, or something unreadable) and `unavailable` (the endpoint could not be reached, or the secret does not resolve). The message never contains the URL, the body or a secret.

## Errors of the management API

| Status and code | Meaning |
|---|---|
| 400 `invalid_http_tool` | the definition is not valid; `error.details.reason` says why in one sentence |
| 404 `not_found` | no such tool |
| 409 `http_tool_name_taken` | another tool has the name |
| 409 `http_tool_in_use` | on delete: an agent version pins it; `error.details` lists the current ones (`agent_id`, `agent_name`, `agent_version_id`, `required`) and is empty when only older versions do |

A test call returns `{status: "completed", duration_ms, result}` or `{status: "failed", duration_ms, error_category, message}`. It really sends the request.

## Using a tool in an agent

An agent version pins an HTTP tool in `mcp_tools` like an MCP tool, with `mcp_server_id` set to `http:<tool id>`, `remote_name` the tool's name and `schema_hash` its `hash`. A step may then list it in `tools` as `http:<tool id>/<name>`. The pin must carry the tool's current hash when the version is published.

An agent's readiness reports each pinned HTTP tool as a dependency:

| State | Meaning | Agent readiness |
|---|---|---|
| `ready` | the pinned definition is current, the tool is enabled and its secret resolves | ready |
| `tool_edited` | the tool has a newer definition than the pinned one | a required tool: `needs_tool_review`; optional: `ready_with_tool_warning`. Choose the tool again to adopt the new definition. |
| `tool_disabled` | the tool is switched off | required: `tool_dependency_unavailable`; optional: warning |
| `secret_missing` | the auth secret name resolves to nothing (`missing_env` lists the name) | same |
| `tool_missing` | the tool was deleted | required: `needs_tool_review`; optional: warning |

A call runs the definition its agent version pinned. A required tool that has been edited makes the agent need review (it cannot start calls until the tool is chosen again), an optional one is left out of new calls, and calls already running keep what they captured. Disabling a tool stops new calls from using it; the test endpoint still runs a disabled tool so it can be tried before it is enabled.

Not supported yet: calls that run in the background (`async_tools`), authentication other than one secret header, OAuth, redirects and non-JSON bodies.
