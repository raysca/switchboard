> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# MCP Test Servers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Test MCP integration end to end against real streamable-HTTP MCP servers that work, fail in each way, are slow, are unreachable, or need credentials, in Go tests, Playwright and `make dev`.

**Architecture:** One in-process-capable test server package (`server/mcp/mcptest`, built on the go-sdk the server already uses) with a thin binary (`cmd/mcptest`). A development/test-only config flag lets the MCP service and its SDK client reach `http://` loopback destinations; production refuses it. The fake LLM learns to call any MCP tool by a phrase with JSON arguments and to say what came back, so every call-path case is deterministic and free.

**Tech Stack:** Go (`github.com/modelcontextprotocol/go-sdk` v1.8.0), `httptest`, the `fakeproviders` build tag, Playwright (`e2e/`), Node seed script.

**Spec:** `docs/superpowers/specs/2026-10-12-mcp-test-servers-design.md`

## Global Constraints

- Streamable HTTP only; `stdio` stays refused (`ErrLocalDisabled`).
- `mcp.allow_local_http` permits `http` or `https` to loopback only: `127.0.0.0/8`, `::1`, and the host name `localhost` (resolving to loopback). Private LAN ranges, link-local (`169.254.0.0/16`, `fe80::/10`), metadata, `0.0.0.0`, embedded credentials, query strings and fragments stay refused. Public HTTPS is unchanged. Production profile + `allow_local_http: true` fails `ValidateConfig` with `mcp.allow_local_http is for development and tests`.
- `mcp.timeout` (duration, default 15s, bounds 1s..2m as `NewService` already clamps) is passed to `ServiceConfig.Timeout`. Tests use short values; no test sleeps longer than 3s.
- Test server paths and behaviour exactly as the spec table: `/ok` (bearer `MCPTEST_TOKEN`, default `test-token`), `/open`, `/fail/tools`, `/fail/auth`, `/fail/list`, `/slow`, separate black-hole listener. Tools: `lookup_order(order_id)`, `echo(text)`, `tool_error`, `protocol_error`, `drop`, `too_big`, `slow_lookup(delay_ms)` (cap 120000).
- The request record keeps at most 100 entries; it stores `auth: "none" | "match" | "mismatch"`, never a token value. `GET /_record` / `DELETE /_record` on the same port.
- Fake LLM phrase: `call tool <remote_name>` with an optional JSON object later in the message (the first `{...}` in the text) as arguments; the reply after the tool is `Tool <remote_name> returned <result JSON>` or `Tool <remote_name> failed: <error category or message>`. Only in the `fakeproviders` build.
- Package location ruling (spec said `tools/mcptest`): the go-sdk is a dependency of the `govoice/server` module, not the root module; the package lives at `server/mcp/mcptest`, the binary at `cmd/mcptest` (root module, like `cmd/server`).
- Commits: conventional messages ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Go from `/Users/raymondottun/Projects/go-voice/server` for server-module packages.
- Gate per task: `gofmt -l`, `go vet` and `go test -race` for the touched packages; the last task runs `make verify`, UI tests, `make test-graph-e2e`, `make e2e-fake`.

## Review Focus

1. **SSRF stays closed with the flag on** (most likely to bite): a `localhost` name that resolves to a LAN or metadata address, an IPv4-mapped IPv6 loopback, `0.0.0.0`, `http://169.254.169.254`, a redirect from loopback to a private address. Expected: only loopback is reachable; redirects are not followed (they are not today: `CheckRedirect` returns `ErrUseLastResponse`). Pin: Task 1 table test.
2. **Dial-time check matches validate-time check:** the URL validator and the dialer must agree; a name that validates as loopback but resolves elsewhere at dial time must fail at dial. Pin: Task 1 test with an injected resolver returning a private IP for `localhost`.
3. **No goroutine, connection or timer left after timeouts, black hole and cancellation:** Pin: Task 4/5 assert the goroutine count returns to baseline within 2s (`runtime.NumGoroutine`, polled) and the black-hole listener sees its connection closed.
4. **Token never leaks and never goes to the wrong server:** Pin: Task 5 extends `TestNoSecretLeavesTheServer`; Task 4 asserts `/open` gets no token unless that server is configured with one.
5. **Barge-in during a slow tool:** the in-flight MCP call is cancelled and a late result is not spoken. Pin: Task 5.
6. **Production profile refuses the flag** even if set through a config overlay. Pin: Task 1.

---

### Task 1: `mcp.allow_local_http` and `mcp.timeout`

**Files:**
- Modify: `server/config.go` (add `MCP struct { AllowLocalHTTP bool \`yaml:"allow_local_http"\`; Timeout time.Duration \`yaml:"timeout"\` } \`yaml:"mcp"\``), `server/config_defaults.go` (Timeout default 15s), `server/config_validate.go` (`validateProductionSecurity` refuses the flag), `config/development.yaml` (`mcp: { allow_local_http: true }` with a comment), `server/mcp/service.go` (new `LoopbackHTTPDestinationPolicy`), `server/mcp/sdk_client.go` (option to allow loopback HTTP at request and dial time), `server/management.go` (wire both), the effective-config goldens (`server/config_effective_test.go` testdata: regenerate the way that test documents).
- Test: `server/mcp/destination_test.go` (create), `server/config_validate_test.go` (add cases)

**Interfaces:**
- Produces: `mcp.LoopbackHTTPDestinationPolicy{}` (Validate accepts `http|https` loopback, delegates everything else to `HTTPSDestinationPolicy`); `mcp.NewSDKClient(opts ...SDKOption)` with `mcp.AllowLoopbackHTTP()`; the existing `testOnlyAllowPrivate` seam stays for the existing tests.

- [ ] **Step 1: Failing tests.** `destination_test.go`: a table over `LoopbackHTTPDestinationPolicy.Validate` with an injected resolver (add an unexported `lookup` field defaulting to `net.DefaultResolver.LookupIPAddr`): accept `http://127.0.0.1:9/x`, `http://[::1]:9/x`, `http://localhost:9/x` (resolver -> 127.0.0.1), `https://public.example` (resolver -> a public IP); refuse `http://10.0.0.5/`, `http://169.254.169.254/`, `http://0.0.0.0/`, `http://[::ffff:10.0.0.1]/`, `http://localhost/` when the resolver returns `10.0.0.5`, `http://user:pw@127.0.0.1/`, `http://127.0.0.1/?q=1`, `http://public.example/` (plain HTTP to public). Dial test: `dialEgress` with `allowLoopback=true` and a resolver returning `10.0.0.5` for `localhost` fails (Review Focus 2); with `127.0.0.1` it dials. Config: `LoadConfigProfile` production + `mcp.allow_local_http: true` -> error containing `mcp.allow_local_http is for development and tests`; development accepts it; `mcp.timeout` defaults to 15s.
- [ ] **Step 2: Run** `cd server && go test ./mcp/ -run 'Destination|Dial' && go test . -run 'Config'` — expect FAIL (undefined policy/option, missing validation).
- [ ] **Step 3: Implement.** Loopback test: `netip.Addr.Unmap().IsLoopback()`. In `SDKClient.requestContext`, accept `http` only when the option is on and the host validates as loopback; in the dialer, filter resolved IPs with `isLoopback || isPublicEgressIP` when the option is on (still refusing private ones). In `EnableManagement`: when `s.cfg.MCP.AllowLocalHTTP`, pass `Destinations: mcp.LoopbackHTTPDestinationPolicy{}` and `mcp.NewSDKClient(mcp.AllowLoopbackHTTP())`; pass `Timeout: s.cfg.MCP.Timeout` always. Log one WARN at startup when the flag is on ("MCP allows plain HTTP to loopback (development and tests)").
- [ ] **Step 4: Run** the Step 2 commands, then `go test -race ./mcp/ ./ -run 'MCP|Config|Effective'` (regenerate goldens if the effective-config test says so), PASS.
- [ ] **Step 5: Commit** `feat(mcp): development-only plain HTTP to loopback MCP servers; configurable MCP timeout`.

### Task 2: The test server (`server/mcp/mcptest`, `cmd/mcptest`)

**Files:**
- Create: `server/mcp/mcptest/mcptest.go`, `server/mcp/mcptest/mcptest_test.go`, `cmd/mcptest/main.go`
- Modify: `Makefile` (`mcp-test` target: `go run ./cmd/mcptest -addr 127.0.0.1:18765 -blackhole-addr 127.0.0.1:18766`)

**Interfaces:**
- Produces:
  - `mcptest.New(opts Options) *Server` where `Options{Token string}` (default `test-token`); `(*Server).Handler() http.Handler` (all paths incl. `/_record`); `(*Server).Records() []Record`; `(*Server).Reset()`.
  - `type Record struct { Path, Method, Tool string; Arguments json.RawMessage; Auth string; Cancelled bool; At time.Time }`.
  - `mcptest.BlackHole(l net.Listener) (stop func())` accepts and holds connections without writing, closes them on stop; `(*BlackHoleStats) Accepted() int`.
  - `cmd/mcptest` flags: `-addr`, `-blackhole-addr`, `-token` (default from `MCPTEST_TOKEN`, else `test-token`); prints `mcptest: http://<addr>` on start; exits on SIGINT/SIGTERM with graceful shutdown.

- [ ] **Step 1: Failing tests** (`mcptest_test.go`, using the server's own `mcp.SDKClient` with `AllowLoopbackHTTP()` against `httptest.NewServer(srv.Handler())`):
  - `/ok` lists `lookup_order` and `echo`; `lookup_order {"order_id":"42"}` returns `{"order_id":"42","status":"shipped","items":2}`; record shows `auth: match`; a wrong token gives `mcp.ErrAuthenticationFailed`, record `auth: mismatch`; no token: `auth: none`.
  - `/open` works with and without a token (record `none` / `mismatch`).
  - `/fail/tools`: `tool_error` returns a result flagged as error (`RemoteResult` error path the client maps; assert the client surfaces it as the existing execution-failed or tool-error outcome, whichever `CallTool` returns today, and record it); `protocol_error` yields `ErrExecutionFailed`; `drop` yields a connection error; `too_big` returns a result over `defaultMaxResultBytes` and the client refuses it.
  - `/fail/auth`: `ListTools` -> `ErrAuthenticationFailed`. `/fail/list`: `ListTools` -> `ErrDiscoveryFailed`.
  - `/slow`: `slow_lookup {"delay_ms":200}` returns after >=200ms; with a 100ms context the call fails with `context.DeadlineExceeded` and, within 1s, the record shows `Cancelled: true`.
  - black hole: `ListTools` with a 300ms context fails by deadline; `Accepted() == 1`; after `stop()` the goroutine count returns to baseline.
  - record is capped at 100 and never contains the token string (marshal records, search for the token).
- [ ] **Step 2: Run** `cd server && go test ./mcp/mcptest/` — FAIL (package missing).
- [ ] **Step 3: Implement** with `mcp.NewServer` + `mcp.AddTool` per path and `mcp.NewStreamableHTTPHandler(..., &mcp.StreamableHTTPOptions{Stateless: true})`; wrap each handler to compute `auth` and append records under a mutex (ring of 100). `drop`: hijack the HTTP connection and close it. `protocol_error`: return a non-nil error from the tool handler so the SDK sends a JSON-RPC error. `too_big`: a result of `defaultMaxResultBytes+1` bytes (export a constant from the mcp package or pass the size through `Options`; do not import an unexported value). `slow_lookup`: `select` on `time.After(delay)` and `ctx.Done()`, set `Cancelled` on the record when `ctx.Done()` wins. `/fail/auth`: 401 before the MCP handler. `/fail/list`: a server whose tool listing errors (the SDK supports a custom `ListTools` middleware; if not, return a JSON-RPC error from a raw handler for method `tools/list`).
- [ ] **Step 4: Run** `go test -race ./mcp/mcptest/`, PASS; `go build ./cmd/mcptest` from the repo root; run it, `curl -s localhost:18765/_record` returns `[]`.
- [ ] **Step 5: Commit** `test(mcp): mcptest server with ok, open, failing, slow and black-hole endpoints`.

### Task 3: The fake LLM can call any MCP tool and say what came back

**Files:**
- Modify: `core/fakes.go` (`FakeLLMConfig.SayToolResult bool`; `toolFor` returns name and arguments: the first balanced `{...}` in the message if it parses as a JSON object, else `{}`), `core/fake_llm_tools_test.go`, `server/registry_fake.go` (for each `spec.Tools` with prefix `mcp__`, register phrase `call tool <remote>` where `<remote>` is the name with the `mcp__<server>_` prefix and the 8-hex suffix removed; set `SayToolResult: true`)

**Interfaces:**
- Consumes: `runtime.ToolName` naming (`mcp__` + up to 8 chars of server id + `_` + cleaned remote + 8 hex). Two attached servers with the same remote name: the phrase maps to the first in sorted order (document it in the comment; tests attach one server per agent).
- Produces: the reply strings in Global Constraints.

- [ ] **Step 1: Failing tests** in `core/fake_llm_tools_test.go`: a message `please call tool lookup_order {"order_id":"42"}` calls the executor with that JSON and, with `SayToolResult`, replies `Tool lookup_order returned {"status":"shipped"}` given a stub result; an executor error replies `Tool lookup_order failed: <message>`; a non-JSON brace text passes `{}`; existing transfer tests unchanged. A registry_fake test (in `server`, fakeproviders tag) that a spec with tool `mcp__abcd1234_lookup_order0a1b2c3d` registers `call tool lookup_order`.
- [ ] **Step 2: Run** `cd core && go test -run FakeLLM ./...` — FAIL.
- [ ] **Step 3: Implement** (keep the transfer path's `AfterTool` behaviour for transfer tools: `SayToolResult` only applies to tools registered through the MCP phrase; carry that per phrase, e.g. a second map `SayResult map[string]bool`, whichever is smaller in code).
- [ ] **Step 4: Run** core and `cd server && go test -race -tags fakeproviders -run 'Fake|Graph' .`, PASS.
- [ ] **Step 5: Commit** `test(core): fake LLM calls MCP tools by phrase with arguments and reports the result`.

### Task 4: Admin flows against every server (Go)

**Files:**
- Create: `server/mcp_integration_test.go` (fakeproviders tag not needed), helper `newMCPRig(t)` that starts `mcptest` on `httptest.NewServer`, a black-hole listener on `127.0.0.1:0`, a server with `cfg.MCP.AllowLocalHTTP=true`, `cfg.MCP.Timeout=800ms`, `s.env` from a map the test controls (`MCP_TOKEN_TEST`), `enableTestManagement`, and an admin `httptest` server for `/api/v1/mcp-servers`.

**Interfaces:**
- Consumes: Task 1 config and policies, Task 2 `mcptest`, existing routes `POST /api/v1/mcp-servers`, `POST /api/v1/mcp-servers/{id}/validate`, `POST /api/v1/mcp-servers/{id}/tools/{tool}/test`, `GET /api/v1/mcp-servers/{id}`.

- [ ] **Step 1: Write the tests (they are the deliverable; each must fail on a stub `newMCPRig` that skips enabling the flag):**
  - `TestMCPAdminHealthyServers`: create + validate `/ok` (auth_env `MCP_TOKEN_TEST` set) and `/open`: tools discovered, server healthy; tool test `lookup_order` returns the record; `/_record` shows `auth: match` for `/ok` and `none` for `/open`.
  - `TestMCPAdminFailures`: `/fail/auth` -> typed authentication error in the response (the API's error code for `ErrAuthenticationFailed`), `/fail/list` -> discovery error, refused port, `http://mcp.example.invalid/mcp` (refused at create or validate with the destination error; assert which, from current behaviour), black hole -> times out within `Timeout + 500ms`. Each leaves the server listed with its failure, and the goroutine count returns to baseline within 2s.
  - `TestMCPAdminWentAwayAndCameBack`: validate ok, close the `mcptest` server, validate -> connection error; start a new one on the same address (listen on the saved address), validate -> healthy.
  - `TestMCPCredentialMatrix`: the spec's table rows, through create/validate/tool-test, asserting the API outcome and the `/_record` auth field; rotation: change the env map between two tool tests, the second record matches the new token (mcptest started with that token for the second half).
  - `TestMCPAllowLocalHTTPOffRefusesLoopback`: with the flag off, creating `http://127.0.0.1:.../ok` is refused with the existing destination error.
- [ ] **Step 2: Run** `cd server && go test -race -run 'MCPAdmin|MCPCredential|MCPAllow' .`; fix only production code that is wrong (each fix ledgered); PASS.
- [ ] **Step 3: Commit** `test(mcp): admin flows against healthy, failing, slow, unreachable and credentialed servers`.

### Task 5: Tool calls during a voice call (Go, fake providers)

**Files:**
- Create: `server/mcp_call_e2e_test.go` (`//go:build fakeproviders`), reusing the graph rig's call helpers (`e2eCall`) and Task 4's `newMCPRig`.
- Modify: `server/secret_leak_test.go` (attach an `/ok` tool with `MCP_TOKEN_TEST=SENTINEL-mcp-call`, call it, keep the existing scans).
- Modify: `Makefile` `test-graph-e2e` `-run` regex to include `MCPCall`.

- [ ] **Step 1: Tests:**
  - `TestMCPCallOK`: an agent with `/ok` `lookup_order` attached; text turn `call tool lookup_order {"order_id":"42"}` -> bot reply contains `"status":"shipped"`; trace has the MCP execution span naming the server and tool; record shows the arguments.
  - `TestMCPCallFailures`: for `tool_error`, `protocol_error`, `drop`, `too_big`: the reply starts `Tool <name> failed:` and the call goes on (a following `hello` turn is answered).
  - `TestMCPCallSlow`: `slow_lookup` with `delay_ms` 200 (timeout 800ms) -> returned; with 2000 -> fails at ~800ms, record `Cancelled: true`, next turn answered.
  - `TestMCPCallBargeInCancelsSlowTool`: start `slow_lookup` 2000, send a new user turn after 200ms (the rig's interruption helper or a second text turn, whichever the rig uses for barge-in today), assert the record shows cancellation within 500ms and no `Tool slow_lookup returned` text arrives afterwards.
  - `TestMCPCallAsyncTool`: `slow_lookup` listed in `async_tools`, delay 300ms: the turn is answered at once and the result arrives later (follow the existing async-tool test's assertion style).
  - `TestMCPCallUnreachable`: a server pointing at the refused port attached as optional: agent readiness is `ready_with_tool_warning`, the call starts, the tool fails within the timeout, the agent answers.
- [ ] **Step 2: Run** `cd server && go test -race -tags fakeproviders -run 'MCPCall|NoSecretLeavesTheServer' .`; PASS.
- [ ] **Step 3: Commit** `test(mcp): tool calls during voice calls: ok, failures, slow, barge-in, async, unreachable`.

### Task 6: Developer use

**Files:**
- Modify: `scripts/dev.sh` (build and start `cmd/mcptest` on `MCPTEST_PORT` default 18765 and the black hole on 18766 before the server; stop it on exit; add `MCP_TOKEN_TEST=test-token` to the server environment), `scripts/dev-seed.mjs` (create, if absent by name, "Test MCP: ok" (`/ok`, auth_env `MCP_TOKEN_TEST`), "Test MCP: failing" (`/fail/tools`), "Test MCP: slow" (`/slow`), then validate each; a failure is logged, never fatal; stdout contract unchanged), `examples/.env.example` (`# MCP_TOKEN_TEST=test-token  # used by make dev's test MCP servers`), `README.md` (one sentence), `Makefile` (`mcp-test` help text).

- [ ] **Step 1:** Run `make dev` (ports as needed), confirm the three servers appear on the MCP page with their tools (ok, failing) and the slow one discovered; seed twice, no duplicates.
- [ ] **Step 2: Commit** `feat(dev): run test MCP servers and seed them in make dev`.

### Task 7: Playwright: the MCP page and one call

**Files:**
- Modify: `e2e/global-setup-graph.ts` (build `cmd/mcptest` once, start it like the fake server on a free port, add `mcpUrl` to the stack file, pass `MCP_TOKEN_TEST` to the server environment; add `mcp:\n  allow_local_http: true\n  timeout: 2s` to the generated server config), `e2e/global-teardown*.ts` if it lists child processes.
- Create: `e2e/tests-graph/20-mcp.spec.ts`

- [ ] **Step 1: Tests** (fake mode only: `test.skip(LIVE, ...)`):
  - `mcp: healthy, failing and unreachable servers show their state`: add `/ok` (auth env `MCP_TOKEN_TEST`), `/fail/auth` and a refused port through the MCP page form; the first shows its tools and "variable is set", the second an authentication failure, the third a connection failure (assert the visible text the page renders for each today).
  - `mcp: an agent calls an MCP tool during a call`: attach `/ok` `lookup_order` to a plain agent through the UI flow the MCP view offers (or the API if the UI has no attach flow; check `MCPView.tsx`), start a call, send `call tool lookup_order {"order_id":"42"}`, the bot line contains `shipped`, the session trace shows the MCP span.
- [ ] **Step 2: Run** `cd e2e && npx playwright test -c playwright.graph.config.ts`, all pass.
- [ ] **Step 3: Commit** `test(e2e): MCP page states and an MCP tool call in the browser`.

### Task 8: Docs, gate, review

- [ ] **Step 1: Docs.** `docs/api/mcp.md` and `docs/security/mcp.md`: the `mcp.allow_local_http` and `mcp.timeout` settings (production refuses the first), and a "Testing MCP integration" section listing the `mcptest` endpoints and `make mcp-test`.
- [ ] **Step 2: Gate.** `gofmt -l .` empty; `make verify`; `cd ui && npx vitest run && npx tsc --noEmit`; `make test-graph-e2e`; `make e2e-fake`.
- [ ] **Step 3: Independent review** of the branch diff per AGENTS.md (YAGNI, SSRF with the flag on, goroutine/connection cleanup, token handling, test flakiness from timing); fix findings test-first; re-run the gate.
- [ ] **Step 4: Commit** `docs(mcp): local test servers and the development-only destination setting`.
