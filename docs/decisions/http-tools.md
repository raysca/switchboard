# HTTP tools: plan

Status: implemented (slices 0 to 5). Phase 2 of [roadmap-next-phases.md](../roadmap-next-phases.md). Reference: [api/http-tools.md](../api/http-tools.md), [security/http-tools.md](../security/http-tools.md). Where the build differs from the plan below:

- An agent version pins an HTTP tool in the existing tool pin table with the pseudo server id `http:<tool id>` and the tool's name as `remote_name`, instead of new `agent_http_tools` and `session_http_tools` tables. Step references are `http:<id>/<name>`. Tool calls record the same pseudo id.
- Error categories are the MCP ones; a non-2xx answer is `tool_error` with `the service answered HTTP <code>`, not a new `http_error` category.
- The development switch is the existing `mcp.allow_local_http`, not a second one.
- There is no per-tool in-flight cap; the model loop's `max_parallel` bounds concurrency.
- Text bodies are `text/plain` only.
- Secret writes and deletes are recorded in a persistent audit table (`secret_audit`), not only logged as the plan said.
- The delete guard refuses a tool any agent version has pinned, so a tool with history is disabled instead of deleted (an active call keeps its captured definition in memory).

## Goal

An agent author can give a step a tool that calls one HTTP endpoint, without writing an MCP server. The author configures the endpoint once, tests it, selects it on an agent next to MCP tools, and the published agent version pins exactly what was selected.

Not in scope: OAuth, a knowledge service, skills, response transformation languages, streaming or websocket endpoints, file upload.

## How MCP tools work today (what we reuse)

From the code, not from memory:

- A call starts by capturing each pinned tool (`server/runtime/mcp.go`) into a `core.ToolDefinition` plus an executor entry. Everything after that is transport-neutral: `core/tools/loop.go` (rounds, calls, argument and result bounds, per-call timeout), `stepDefinitions` and `stepExecutor` in `server/agent_call.go`, and the `tool_calls` record in `server/call.go`.
- The model sees a runtime name `mcp__<id8>_<name><hash4>` (at most 64 characters), disjoint from `transfer_to_*`, `end_call` and `send_dtmf`.
- Tool arguments and results are never stored, traced or exported. A `tool_calls` row holds ids, status, error category and times; spans carry only metadata.
- A pin is `(server, remote name, schema hash, required)` in `agent_mcp_tools`, copied into `session_mcp_tools` when a call starts. Publish validates the pin inside the publish transaction. Readiness reports `schema_drift`, `tool_missing`, `needs_tool_review` and the server states.
- Secrets are environment variable names only (`MCP_TOKEN_*`), checked against a pattern and re-read at use. Destinations are HTTPS only, resolved and pinned at dial time to public addresses, with redirects refused (`server/mcp/service.go`, `sdk_client.go`).

What does not carry over: discovery and drift come from a remote `tools/list`, health comes from a sync loop, and every table, trigger, readiness state and UI screen is keyed by an MCP server. An HTTP tool has no remote to discover from and no server to be healthy.

## Decisions

Defaults chosen so the plan can proceed; each is easy to change before slice 1.

1. **Own entity, shared tool list.** HTTP tools are their own stored object, `http_tools`, with their own page and API. In the agent editor they appear in the same tool picker as MCP tools (one section each), and a step's tool subset can mix both. They do not pretend to be an MCP server.
2. **Immutable definitions.** An edit creates a new definition version identified by a hash (`storage.ConfigHash` over the canonical definition). An agent version pins `(tool id, definition hash, required)`. Old pins stay runnable because old definitions are kept, the same reason MCP keeps old discoveries. "Drift" for an HTTP tool means "the tool was edited after this agent pinned it", which is known at save time and needs no sync loop.
3. **Auth: one secret header, by secret name.** A tool may name one header (default `Authorization`) and a secret by name (see [Secrets](#secrets)). The value is sent as `Bearer <value>` for `Authorization`, and as the raw value for any other header name. Basic auth and signed requests are later. Other headers are static, non-secret, and stored in the definition.
4. **Host is fixed; path and query come from arguments.** The scheme and host (and port) are part of the definition and cannot be templated. Placeholders such as `{order_id}` are allowed in the path only and are filled from arguments, percent-encoded. For `GET` and `DELETE`, remaining arguments become the query string; for `POST`, `PUT` and `PATCH` they become a JSON body. This keeps the destination checkable once, at save time, and again at dial time.
5. **Result shape.** The model receives `{"status": <code>, "body": <value>}` for a 2xx response. JSON bodies are passed as JSON, text bodies as a string; other content types fail with `unsupported_content_type`. An optional `response.pick` list of JSON pointers keeps only those fields. A non-2xx response is a tool error `http_error` with the status code and no body.
6. **Test before publish.** A tool can be tested from the management API and the UI with sample arguments, using the same code path as a call. The test result shows status, duration and the (bounded) body; it is not stored.

## Definition

```
http_tool:
  id, name (unique, slug), description (for the model)
  method:   GET | POST | PUT | PATCH | DELETE
  url:      https://host[:port]/path/{placeholder}   (no userinfo, query or fragment)
  headers:  { name: value }                          (static; no secrets; denylist: Host, Content-Length, Transfer-Encoding, Connection, Cookie, Proxy-*)
  auth:     { header: "Authorization", secret: "crm-api" }      (optional; a secret name, never a value)
  input_schema: JSON Schema, object                  (what the model may send; placeholders must be required string or integer properties)
  timeout_ms:   default 10000, ceiling 30000
  max_response_bytes: default 65536, ceiling 262144
  response: { pick: ["/data/status", ...] }          (optional)
```

Limits above are per tool and clamped by server ceilings. The model-facing argument size is already bounded by the loop (32 KiB, depth 16).

## Security

- HTTPS only, with the existing public-address policy at save and at dial time. The dialer is the MCP one (`dialEgress`, `HTTPSDestinationPolicy`) moved to a small shared package, so there is a single implementation to audit. Redirects are refused. No proxy.
- The existing `mcp.allow_local_http` switch (development only, refused in production) gets an HTTP-tool counterpart so tests and `make dev` can call a loopback fixture.
- Argument values never reach the host part of the URL. Path values are percent-encoded and may not be empty or contain `/`, `..` or control characters.
- The secret is resolved at call time, never returned by any API, and scrubbed from errors, spans and the test endpoint. `server/secret_leak_test.go` gets a sentinel for it.
- Response handling: the body is read through a size-limited reader and the call fails with `result_too_large` rather than truncating silently; content type is checked before parsing; the existing `sanitizeResult` redaction of secret-shaped keys applies.
- Arguments and results are not stored or traced, same as MCP. A call records tool id, definition hash, status, category and duration.

## Secrets

Requiring every tool secret to be an environment variable means a redeploy for each new tool or MCP server. The product must also run on any cloud, so the answer cannot depend on one provider's secret manager. This section applies to MCP servers as well; it replaces `MCP_TOKEN_*` as the only mechanism and keeps it working.

Proposed design (needs sign-off, because it changes what the server is trusted with):

- **A resolver by name.** Tools and MCP servers reference a secret by name. A small consumer-side interface resolves a name to a value at use time. Backends, in lookup order:
  1. **Environment**, the existing mechanism: `MCP_TOKEN_*` and `HTTP_TOKEN_*` keep working unchanged, as read-only secrets.
  2. **File directory** (`secrets.dir`), one file per secret. Works wherever a platform can mount secrets as files (Kubernetes, Docker and Compose, Nomad, systemd credentials, Cloud Run volumes), and picks up changes without a restart.
  3. **Built-in encrypted store**, the portable self-service path: the dashboard and API create, replace and delete secrets through a write-only endpoint. Values live in the existing SQLite or Postgres database, encrypted with a deployment master key (see below), and are never returned. Names, update time and last-used time are listable.
- **Master key.** One 32-byte key, supplied once at deployment through an environment variable or file (`SWITCHBOARD_SECRETS_KEY`), changed only on rotation. Several keys may be configured at once (an id and a key each): new writes use the first, old rows stay readable, and a rotate command re-encrypts. Without a key, the encrypted store is disabled and the other two backends still work.
- **No handrolled cryptography.** Use an established AEAD (the standard library's AES-256-GCM with a random nonce, or Tink) with the secret name and key id as associated data, so a row cannot be swapped between names. The choice is made in slice 1 with a short note in this document.
- **Safety properties:** values never appear in any API response, log, span, error or the test endpoint; a secret in use cannot be deleted (409 with the referencing tools and servers); every create, replace and delete is logged with the secret name and request id, never the value (a persistent audit trail is a later phase); the database dump alone is not enough to read a secret.
- **Cloud secret managers** (GCP, AWS, Cloudflare and others) stay optional backends behind the same interface, added only when a deployment needs one. They are not required for any feature.
- **Out of scope:** per-user or per-tenant secret scoping (there are no tenants yet), automatic rotation of the upstream credential, and a hardware-backed key.

Effect on the earlier decisions: `auth.secret` replaces `auth.env`, MCP servers gain an optional `auth_secret` beside `auth_env`, and readiness reports `secret_missing` for either when the name does not resolve. The environment-only rule in `docs/security/credentials.md` remains true for provider keys; this adds a documented exception for tool and MCP credentials.

## Storage

New migrations, SQLite and Postgres in parallel (forward-only):

- `secrets(name, key_id, nonce, ciphertext, created_at, updated_at, last_used_at)` for the encrypted store
- `http_tools(id, name, enabled, current_hash, created_at, updated_at)`
- `http_tool_definitions(hash, tool_id, definition_json, created_at)` (immutable)
- `agent_http_tools(agent_version_id, tool_id, definition_hash, required)`
- `session_http_tools` (same shape, plus the captured definition), written when a call starts so a running call is unaffected by edits
- `tool_calls` gets nullable `http_tool_id` and `http_definition_hash`
- A delete guard like the MCP one: a tool referenced by a current agent version or an active session cannot be deleted (409 with the referencing agents); `disable` is always allowed and makes readiness report it.

## API

`/api/v1/http-tools`, behind the management key, same envelope and request ids as the MCP API:

| Method and path | Purpose |
|---|---|
| `POST /http-tools` | create (validates the definition fully) |
| `GET /http-tools`, `GET /http-tools/{id}` | list and read (never returns secret values; shows whether the env variable is set) |
| `PUT /http-tools/{id}` | new definition version |
| `POST /http-tools/{id}/enable`, `/disable` | availability |
| `POST /http-tools/{id}/test` | call with sample arguments |
| `DELETE /http-tools/{id}` | delete when unreferenced (409 `http_tool_in_use` otherwise) |
| `GET /secrets`, `PUT /secrets/{name}`, `DELETE /secrets/{name}` | list names and sources (never values), create or replace, delete when unreferenced (409 `secret_in_use`) |

Agents: `POST` and `PUT /agents` accept `http_tools: [{tool_id, definition_hash, required}]` beside `mcp_tools`. Step `tools` and `async_tools` refs gain a prefix, `http:<tool id>`, so the two kinds cannot collide with `<server id>/<name>`. Error codes: `invalid_http_tool` (with `details.reason`), `http_tool_in_use`, `http_tool_failed`.

## Runtime

- `sessionExecutor` in `server/runtime/mcp.go` stops holding `*mcp.CapturedTool` directly and takes a small interface (`Execute`, with the existing argument, timeout and result handling). MCP and HTTP captured tools both implement it. This is the only refactor of existing code on the call path.
- Runtime names `http__<id8>_<name><hash4>`, same 64-character rule and collision hash.
- `stepDefinitions` and `stepExecutor` look tools up by ref kind. Handoff, `end_call` and DTMF handling is unchanged.
- Per-call timeout uses `ToolPolicy.Timeout` (already honoured by the loop). Concurrency is the existing `MaxParallel`; each tool also gets a small in-flight cap (default 4) so one slow endpoint cannot absorb a call's parallelism. Async (long-running) HTTP tools are out of the first version.
- Error categories reuse the MCP set and add `http_error`: `invalid_arguments`, `timeout`, `cancelled`, `result_too_large`, `unavailable`, `tool_error`.
- Spans: `http_tool.execute` with tool id, method, status code, duration and byte counts; no URL path, no values.

## Readiness and publish

`agentReadiness` gains a branch per tool kind. For a pinned HTTP tool: `tool_missing` (deleted), `tool_disabled`, `secret_missing` (name does not resolve), `tool_edited` (definition hash differs; a required tool puts the agent in `needs_tool_review`, an optional one gives `ready_with_tool_warning`). Publish validates pins inside the publish transaction like MCP. Reviewing means re-selecting the tool at its current hash, shown as a diff of the definition in the publish dialog.

## UI

- A page for HTTP tools next to MCP tools in the navigation: list, create and edit form (method, URL, headers, auth variable, input schema editor, limits, response pick), a test console with sample arguments, and a delete dialog that lists referencing agents.
- The tool picker lists HTTP tools under their own heading, shows readiness states, and handles `tool_edited` like schema drift.
- Call detail shows HTTP tool calls with the tool name, status, category and duration.
- Style guard tests apply (tokens only, square corners, no new weights).

## Slices

Each slice is a commit set that passes `make verify` and `make e2e-echo`, followed by an independent review before the next.

0. **Secrets.** The resolver interface with the environment and file backends; MCP servers use it (existing `MCP_TOKEN_*` behaviour unchanged); then, if approved, the encrypted store with master key, rotation, `/secrets` API, a dashboard page, audit entries and leak tests. Delivered first because both tool kinds depend on it.
1. **Foundation.** Shared egress package extracted from `server/mcp` (no behaviour change); definition type, validation, hashing; migrations; storage repository with the delete guard; unit tests including SSRF, placeholders, header denylist and secret handling.
2. **Executor and API.** The HTTP executor with limits and error mapping; the `/http-tools` handlers and the test endpoint; the `sessionExecutor` interface refactor; Go end-to-end test with a loopback fixture (a `cmd` or `mcptest` sibling that records what it received); endpoint-table and secret-leak tests.
3. **Agents.** `http_tools` pins on create and update, step refs, publish validation, session capture, readiness states, `tool_calls` columns; end-to-end call test where the model calls an HTTP tool and the fixture sees the request.
4. **UI.** HTTP tools page, picker section, publish diff, call detail; vitest and Playwright tests (`e2e/tests-agents/`), a seeded example tool in `make dev` against the fixture.
5. **Docs.** An API page, an operations page and a security page for HTTP tools beside the MCP ones, a worked example, and the roadmap rows updated.

## Risks and open points

- **Query and path templating.** Kept to path placeholders and argument-derived queries so the host stays fixed; if authors need host templating (per-tenant subdomains), that needs its own allowlist design and is out of this phase.
- **Large bodies and content types.** JSON and text only; others are refused rather than guessed.
- **Secret scope.** One header per tool. Several credentials per tool need more than this plan covers; a named secret can already be shared by several tools.
- **Trust.** The encrypted store makes the server able to decrypt tool credentials, and the master key becomes the thing to protect. That is the main security trade for removing redeploys, and the reason it needs your decision.
- **Deletion versus history.** Definitions are never deleted while any agent version or call record references their hash; a deleted tool's old definitions stay for history.
- **Naming.** The API and UI say "HTTP tools". The vocabulary guard applies to every new doc and screen.

## Questions for review

1. Is a separate HTTP tools page right, or should it live as a tab on the MCP tools page?
2. Is one secret header per tool enough for the first version?
3. Should the first version include async (long-running) HTTP tools? The plan says no.
4. Approve the secrets design: resolver with environment and file backends, plus the built-in encrypted store with a master key? Or stop at the resolver for now?
