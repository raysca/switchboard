# MCP servers: operator guide

An MCP server gives agents remote tools. This page covers what the states mean, how the server keeps them current, and what to do when something breaks. The API is in [api/mcp.md](../api/mcp.md); the security rules are in [security/mcp.md](../security/mcp.md).

## States

| State | Meaning | What to do |
|---|---|---|
| `not_validated` | Registered or edited, not checked yet. Calls cannot use its tools. | Run Sync. |
| `healthy` | The last check connected and listed the tools. | Nothing. |
| `authentication_failed` | The server refused the token, or the token variable is not set. | Set the variable (see below) and run Sync. |
| `connection_failed` | The server could not be reached, or the check timed out. | Check the endpoint and the network; run Sync. |
| `discovery_failed` | It connected, but listing the tools failed. | Check the remote server's logs; run Sync. |
| `tool_schema_invalid` | A tool's schema is outside what Switchboard accepts. | Fix the schema on the remote server; run Sync. |
| `disabled` | You disabled it. New calls do not use it. | Enable it, then Sync. |

Only a `healthy` server's tools are available to new calls. A failed check never removes the last good tool list.

## Automatic checks

Every `mcp.sync_interval` the server syncs each enabled server, one after another. The default is `5m`. `0` turns it off. When on, the value must be between `30s` and `24h`. Each sync is bounded by `mcp.timeout`.

```yaml
mcp:
  sync_interval: 5m
```

The two-strike rule: a healthy server is marked failed only on its second background failure in a row. One success clears the count, so a single network blip changes nothing. A server that is already failing is updated on every check.

A manual Sync always writes its result at once. The count is kept by the background check: after a manual Sync heals a failed server, it again takes two background failures in a row to mark it failed. If the tool list is unchanged, no new discovery is stored; only the state and the check times change. If a tool's schema changed, a new discovery is stored and agents that pinned the old schema are flagged (see below).

The server detail page shows when the server was last checked (`last_checked_at`) and the interval. `GET /api/v1/server-config` returns it as `mcp_sync_interval_seconds` (0 means off).

Observe it:

- The metric `voice_mcp_sync_total{result="ok|failed"}` counts background syncs.
- A log line is written when a server changes between healthy and failed. There is no line per check.

## Manual Sync

Use Sync to check now, for example after you fix the remote server or rotate a token. Its result is written immediately, with no second chance.

## Rotate a token

A server's token is a named secret: an `MCP_TOKEN_*` environment variable, a file in the secrets directory, or a secret in the encrypted store (the Secrets page; see [credentials](../security/credentials.md#tool-and-mcp-secrets)). A server stores the name, never the value.

1. Put the new value where the secret lives: replace it on the Secrets page (or the file in the secrets directory), or set the environment variable under the same name and restart the server.
2. Run Sync on the MCP server.

The name stays the same. New calls use the new value. Calls already running keep the token they captured until they end; end them if the old token must stop at once. If the secret does not resolve, the server shows that the secret is not set, and agents that need its tools are not callable (`auth_env_missing`). Only the variable name is shown.

## Edit a server

Use Edit on the server page, or `PATCH /api/v1/mcp-servers/{id}`. You can change the name, description, endpoint and `auth_env`. An empty `auth_env` removes the token.

- Changing the endpoint or `auth_env` resets the server to `not_validated` and syncs it at once.
- The server id does not change, so agents keep their tool selections. Nothing is detached.
- Calls already running keep the endpoint and token they captured.
- A refused create or edit gives a reason: `invalid_endpoint`, `destination_denied`, `invalid_auth_env`, `invalid_metadata` or `transport_unsupported`.

## Recover an agent in `needs_tool_review`

An agent shows `needs_tool_review` when a tool it requires changed its schema or no longer exists, or when the server was removed with detach-and-delete.

1. Open the MCP server and find the tool. Compare the new schema with what the agent expects.
2. If the new schema is fine, accept it for the agent (re-pin). This publishes a new agent version with the current schema.
3. If the tool is gone, remove it from the agent or restore it on the remote server and Sync.

Running calls keep the version they started with.

## What the model sees when a tool fails

The model gets `{"error": "<category>"}`. Categories:

| Category | Cause |
|---|---|
| `invalid_arguments` | The arguments did not match the tool's schema. |
| `timeout` | The tool did not answer within `mcp.timeout`. |
| `unavailable` | Connection, initialization or authentication failed. |
| `result_too_large` | The result was over the size limit. |
| `tool_error` | The tool answered with an error. The model also gets `message`. |
| `cancelled` | The call was cut off, for example by barge-in. |

For `tool_error`, `message` is the tool's own error text with control characters removed and the credential redacted, at most 512 bytes. It is data from the remote server, as a normal result is.

The category is stored on the tool call (`error_category`) and shown on the Calls page. The Try it console shows the category and message. Traces carry the category only, never the message.
