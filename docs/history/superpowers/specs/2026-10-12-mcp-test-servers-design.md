> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# MCP test servers: healthy, failing, slow, unreachable, with and without credentials

Status: draft for review. Date: 2026-10-12.

## Goal

Test the MCP integration end to end against real HTTP MCP servers whose behaviour is known: one that works, ones that fail in each distinct way, ones that are slow or never answer, ones that are unreachable, and the credential matrix. The same servers serve Go tests, the Playwright suites and `make dev`.

Scope: remote MCP over **streamable HTTP** only (`transport: streamable-http`). `stdio` stays disabled and untested here.

## Decisions made with the requester

- Local plain-HTTP MCP destinations are allowed in development and tests through an explicit setting that production refuses.
- Both paths are covered: the admin flows (create, validate, refresh, attach to an agent) and tool calls during a voice call.

## 1. Destination setting (the only server change)

The server today accepts only `https` endpoints on public IPs (`server/mcp/service.go` `HTTPSDestinationPolicy`). Add:

- Config `mcp.allow_local_http: bool` (default false). When true, a destination may also be `http://` to a loopback address (`127.0.0.0/8`, `::1`, `localhost`). Nothing else is loosened: private LAN ranges, link-local, metadata addresses and embedded credentials stay refused; public HTTPS behaves as today.
- `LoadConfig` with the production profile rejects `allow_local_http: true` at startup ("mcp.allow_local_http is for development and tests"). `config/development.yaml` sets it true; `config/production.yaml` does not mention it.
- Implemented as a second `DestinationPolicy` (`LocalHTTPDestinationPolicy`) wrapping the HTTPS one, chosen in `EnableManagement`; the policy interface already exists.

## 2. `tools/mcptest`: the test server

One small Go program (module `tools`, using `github.com/modelcontextprotocol/go-sdk`, already a dependency of the server) that serves several MCP servers from one port, by path. It is also a package (`tools/mcptest`) so Go tests can start it in-process with `httptest`.

| Path | Auth | Behaviour |
|---|---|---|
| `/ok` | bearer token, from its own `MCPTEST_TOKEN` (default `test-token`) | Tools `lookup_order(order_id)` returns a fixed record; `echo(text)` returns its argument. |
| `/open` | none | Same tools as `/ok`; records whether an `Authorization` header arrived anyway. |
| `/fail/tools` | none | Tools: `tool_error` (result with `isError: true` and a message), `protocol_error` (JSON-RPC error), `drop` (closes the connection mid-call), `too_big` (result over the server's `MaxResultBytes`). |
| `/fail/auth` | always 401 | Initialize is rejected. |
| `/fail/list` | none | Initialize succeeds; `tools/list` returns an error. |
| `/slow` | none | `slow_lookup(delay_ms)`: sleeps `delay_ms` (cap 120 s), honours cancellation, and records whether the call was cancelled and when. |
| black hole | none | A separate listener (`-blackhole-addr`) that accepts TCP and never writes a byte. |

Every server keeps an in-memory, bounded record (last 100 requests): path, tool, arguments, whether an `Authorization` header was present and whether it matched, cancellation. `GET /_record` returns it as JSON and `DELETE /_record` clears it; tests assert on what the server saw, not only on what the UI shows. The record never stores the token value, only `auth: "none" | "match" | "mismatch"`.

Unreachable cases need no server:
- **refused:** a free loopback port (listen, take the port, close);
- **does not resolve:** `http://mcp.example.invalid/mcp` (RFC 6761: `.invalid` never resolves);
- **went away / came back:** stop and restart the `mcptest` process (or the `httptest` server) between steps.

## 3. Credential matrix

| Server | `auth_env` / environment | Expected |
|---|---|---|
| `/open` | none | Works; record shows `auth: none`. |
| `/open` | `MCP_TOKEN_TEST` set | Works; the token is sent only to this configured server (record `auth: mismatch` or `match` is irrelevant; assert it is never sent to another server). |
| `/ok` | `MCP_TOKEN_TEST` = server token | Works; record `auth: match`. |
| `/ok` | `auth_env` set, variable unset | Refused before dialing; UI shows "variable is not set"; record is empty. |
| `/ok` | no `auth_env` | Typed authentication failure (not a generic error); record `auth: none`. |
| `/ok` | wrong value | Typed authentication failure; record `auth: mismatch`. |
| `/ok` | variable rotated between two calls | The new value is used on the next connection without restarting the voice server. |
| any | `auth_env` without the `MCP_TOKEN_` prefix | Refused at create (already unit tested; one UI check). |

Leak rule for every row: the token value never appears in API responses, traces, `/metrics`, the timeline or the server log (extend `TestNoSecretLeavesTheServer` with an MCP token sent to `/ok`).

## 4. What each case must prove

**Admin flows** (Go API tests + one Playwright pass through the MCP page):
- create + validate + refresh against each server: success for `/ok` and `/open`; for `/fail/auth`, `/fail/list`, refused, unresolvable and black hole a typed, actionable error within the budget (connect 5 s, call 15 s), the server listed as unhealthy, no goroutine or connection left (`goleak`-style check in Go, which the server tests already use where present; otherwise `runtime.NumGoroutine` settles back).
- went away: refresh reports the server down; came back: refresh recovers.
- attach tools from `/ok` to an agent; readiness shows a tool warning, not "not ready", when an optional tool's server is down.

**Voice calls** (Go call tests with fake providers; Playwright graph/fake suite for the visible parts). The fake LLM already issues tool calls on a trigger phrase; extend it to call a named MCP tool (`call tool <name> <json>`), so every case is deterministic and free:
- `/ok` `lookup_order`: the reply contains the record; trace has the MCP span with the server and tool; record shows the arguments.
- `/fail/tools` each tool: the call continues, the agent answers (the tool error text goes back to the model, not to the caller verbatim), the trace marks the failure kind.
- `/slow` under the timeout: the reply arrives after the delay; over the timeout: the tool is cancelled at 15 s, the record shows the cancellation, the agent answers without it.
- barge-in during a slow tool: the caller speaks, the in-flight call is cancelled (record shows cancellation within the interruption budget), late results are dropped (generation fencing).
- async tools (`async_tools`): a slow tool listed as async returns later without blocking the turn.
- unreachable / black hole with a tool attached: the call starts, the tool fails within its budget, the agent answers.

## 5. Developer use

- `make mcp-test` runs `tools/mcptest` on `127.0.0.1:18765` (black hole on `:18766`).
- `make dev` starts it alongside the server and `scripts/dev-seed.mjs` registers three MCP servers ("Test MCP – ok", "Test MCP – failing", "Test MCP – slow") with `MCP_TOKEN_TEST` set in the dev environment, so the MCP page and the agent form show all three states.
- The Playwright global setups start it like the fake server and pass its URL to the tests.

## Non-goals

- `stdio` transport, SSE-only transport, OAuth flows (bearer from `MCP_TOKEN_*` only, as today).
- Load or soak against MCP servers.
- Changing timeout values; the tests pin the current ones.

## Risks

- A wrong destination change would open SSRF in production: mitigated by the production startup refusal, by the policy allowing loopback only, and by tests that a LAN, link-local, metadata and credential-bearing URL are still refused with the flag on.
- Timing tests flake: delays are relative to the configured timeouts (set short in tests through `ServiceConfig.Timeout`), never wall-clock guesses.
