# MCP Ship Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the MCP integration truthful and operable: servers are re-synced periodically (interval in the server config), readiness knows about a missing token variable, tool failures reach the model and the operator with a category and the tool's own message, and servers can be edited in place.

**Architecture:** `server/mcp` gains a sync result that is computed once and written once, a background `Syncer` (owned by the management lifecycle) that applies a two-strike rule, error categories with a `ToolError` that carries the tool's message, and an `Update` method behind `PATCH`. Storage stops inserting duplicate discoveries and records an error category per tool call. Flow readiness consults the token variable. The admin UI shows the new states, edits servers, and shows categories and messages.

**Tech Stack:** Go (`server`, `server/mcp`, `server/flow`, `server/runtime`, `server/storage`, goose migrations for SQLite and Postgres), React + zod + vitest (`ui/`), Playwright (`e2e/`).

**Spec:** `docs/superpowers/specs/2026-10-22-mcp-ship-readiness-design.md`

## Global Constraints

- `mcp.sync_interval`: Go duration, default `5m`, `0` = off, otherwise `30s` to `24h` (startup validation error outside that range).
- Two strikes: a background failure on a currently `healthy` server is not written the first time; the second consecutive failure is. A success clears the count. A manual Sync (`POST /validate`) always writes its result.
- Error categories (closed set, exact strings): `invalid_arguments`, `timeout`, `unavailable`, `result_too_large`, `tool_error`, `cancelled`. The model receives `{"error":"<category>"}`, plus `"message"` only for `tool_error` (text parts joined, control characters removed, credential removed, at most 512 bytes, cut on a rune boundary). Traces never carry the message.
- Create/edit reason codes (exact strings): `invalid_endpoint`, `destination_denied`, `invalid_auth_env`, `invalid_metadata`, `transport_unsupported`. No code reveals what an address resolved to.
- Secrets: a token value never appears in an API response, a log line, a trace, a metric label or a stored row. `missing_env` lists variable NAMES only.
- AGENTS.md: bounded, goroutines with a termination path and a recover, no locks across I/O, monotonic time, `go test -race`, goleak in `server` tests stays clean.
- No migration rewrites: add `008_tool_call_error_category.sql` for both dialects.
- Verification gate per AGENTS.md: `gofmt -w .`, `make verify`, `make deadcode`, `make e2e-echo`; independent review at the end.
- Commits: conventional subjects, ending with the attribution trailer the session supplies.

## Review Focus

- A server that fails exactly every other background check must stay healthy (two strikes are consecutive); a server that is down must come back on the first success.
- A manual Sync during a background round must not be overwritten by the round's older result in a way that hides a newer failure for longer than one interval.
- A tool error message that contains the token, control characters, a 10 KiB string, or a JSON-looking server marker must be cleaned and capped before the model sees it.
- Editing an endpoint while a call is using the server must not change that call's captured endpoint or token.
- An `auth_env` variable that is set to an empty string counts as missing.

---

### Task 1: storage — no duplicate discoveries, error category on tool calls

**Files:**
- Modify: `server/storage/repositories.go` (`AddDiscovery`), `server/storage/history.go` (`ToolCallRecord`, insert, select), `server/api/traces.go` (`ToolCallDTO`, `TurnToolDTO` and their mapping)
- Create: `server/migrations/sqlite/008_tool_call_error_category.sql`, `server/migrations/postgres/008_tool_call_error_category.sql`
- Test: `server/storage/*_test.go` (the MCP repository and history tests: mirror their store setup), `server/api/*_test.go`

**Interfaces:**
- Produces: `AddDiscovery` returns the existing latest discovery (same ID) when the new tool list hashes to the latest discovery's hash, and inserts nothing; `ToolCallRecord.ErrorCategory string`; API `tool_calls[].error_category` and `turns[].tools[].error_category` (omitted when empty).

- [ ] **Step 1: failing tests.** (a) Add a discovery, add the same tools again: the second call returns the first discovery's ID and the discovery count for the server is still 1 (count with a direct `SELECT COUNT(*) FROM mcp_discoveries WHERE server_id=?` in the test); a changed schema inserts a new one. (b) A `ToolCallRecord` with `ErrorCategory: "tool_error"` round-trips through insert and `ToolCalls`. (c) The traces API returns `error_category` for that row.
- [ ] **Step 2: run, see them fail** (`cd server && go test ./storage/... ./api/... -run 'Discovery|ToolCall'`).
- [ ] **Step 3: implement.**
  - Migration (both dialects, goose format like 007): `ALTER TABLE tool_calls ADD COLUMN error_category TEXT NOT NULL DEFAULT '';` with a matching `-- +goose Down` dropping it.
  - `AddDiscovery`: inside the existing transaction, before inserting, read the server's current latest discovery hash: `SELECT d.id, d.discovery_hash, d.created_at FROM mcp_discoveries d JOIN mcp_servers s ON s.latest_discovery_id = d.id WHERE s.id = ?`. If the hash equals the new one, load and return that discovery (use the existing `Discovery` read for its tools after the transaction) and skip the inserts.
  - History: add the column to the insert and the select; `ToolCallRecord.ErrorCategory`.
  - API: `ErrorCategory string \`json:"error_category,omitempty"\`` on both DTOs, filled from the record.
- [ ] **Step 4: run, see them pass**, plus `go test -race ./storage/... ./api/...` and the Postgres tests if `TEST_POSTGRES_DSN` is set (`make test-postgres`), otherwise note them as not run.
- [ ] **Step 5: commit** `feat(storage): no duplicate MCP discoveries; tool calls record an error category`.

---

### Task 2: error categories and the tool's own message

**Files:**
- Modify: `server/mcp/sdk_client.go` (`CallTool`), `server/mcp/service.go` (errors, `executeCaptured`, `Execute`), `server/mcp/http.go` (test endpoint error body), `server/runtime/mcp.go` (executor result), `server/call.go` (`settleLocked` stores the category)
- Test: `server/mcp/*_test.go`, `server/runtime/*_test.go`, `server/mcp_call_e2e_test.go` (or the file where MCP call e2e tests live; mirror its rig)

**Interfaces:**
- Produces:
  - `type ToolError struct{ Message string }` with `Error() string { return "MCP tool returned an error" }` — never includes the message (it may hold caller data; logs use Error()).
  - `func ErrorCategory(err error) (category, message string)` mapping: `errors.As(err, &*ToolError)` → `tool_error`, its cleaned message; `ErrInvalidArguments` → `invalid_arguments`; `context.DeadlineExceeded` → `timeout`; `context.Canceled` → `cancelled`; `ErrResultTooLarge` → `result_too_large`; anything else → `unavailable`.
  - Executor result: `core.ToolResult{Status: core.ToolFailed (core.ToolCancelled for cancelled), ErrorCategory: category, Result: {"error":category[,"message":msg]}}`.
  - Test endpoint: on failure the error body's `details` is `{"category": ..., "message": ...}` (message only for tool_error), status codes unchanged.

- [ ] **Step 1: failing tests.**
  - `ErrorCategory` table test covering every category.
  - Message cleaning: `cleanToolMessage(message, token)` turns newlines and tabs into spaces and drops every other control character; replaces the token with `[redacted]`; caps at 512 bytes on a rune boundary. Test with a 10 KiB string, a multibyte string cut at the cap, a string containing the token, and a string with control characters. (No server-marker neutralising: tool results already reach the model as data.)
  - SDK client: a fake server returning `isError: true` with text content "no such order" gives `*ToolError{Message:"no such order"}` (use the existing `mcptest` failing server's `tool_error` tool, which returns "order system unavailable").
  - Runtime: the executor turns a `*ToolError` into `{"error":"tool_error","message":"order system unavailable"}` with `ErrorCategory "tool_error"`; a timeout into `{"error":"timeout"}`.
  - Recorder: the settled row stores `ErrorCategory`.
  - Go e2e: a call through the echo provider that calls `tool_error` on the mcptest failing server: the spoken reply (echo `SayResult`) contains `order system unavailable`, and the stored tool call has `error_category = tool_error`. Read the existing MCP call e2e tests for how the echo provider phrases a tool call and what it says back.
- [ ] **Step 2: run, see them fail.**
- [ ] **Step 3: implement.** In `CallTool`, when `result.IsError`, collect `*mcp.TextContent` items' `Text` and return `&ToolError{Message: joined}` (raw; cleaning happens in the service, which knows the token). In `executeCaptured`, on `errors.As(err, &te)` return `&ToolError{Message: cleanToolMessage(te.Message, captured.token)}`; keep `end(safeCategory(err))` for the span (category only). Map the other existing returns unchanged. In `runtime/mcp.go`, replace the single `mcp_tool_failed` result with `ErrorCategory(err)` and `json.Marshal` of `struct{Error string \`json:"error"\`; Message string \`json:"message,omitempty"\`}`. In `call.go` `settleLocked`, set `p.rec.ErrorCategory = res.ErrorCategory`.
- [ ] **Step 4: run, see them pass**; `go test -race ./server/mcp/... ./server/runtime/...` and the MCP e2e tests (find the Makefile target or tags used by `make test-e2e-go`).
- [ ] **Step 5: commit** `feat(mcp): tool failures reach the model with a category and the tool's own message`.

---

### Task 3: one sync result, written once; periodic sync with two strikes

**Files:**
- Modify: `server/mcp/service.go` (`Refresh` split), `server/config.go`, `server/config_defaults.go`, `server/config_validate.go`, `server/management.go` (start/stop), `server/metrics.go`, `server/api_routes.go` (server-config field), `server/api/api.go` (`ServerConfig` DTO)
- Create: `server/mcp/sync.go`, `server/mcp/sync_test.go`
- Test: `server/config_test.go`, the effective-config goldens (`server/testdata/effective_*.json`; regenerate via the mechanism in `server/config_effective_test.go`, adding only the new key)

**Interfaces:**
- Produces:
  - `func (s *Service) syncServer(ctx context.Context, m storage.MCPServer) syncResult` with `type syncResult struct{ State string; Connected, Discovered time.Time; Discovery storage.MCPDiscovery; Err error }` — every current `SetValidation` call inside `Refresh` becomes a field of the result; nothing is written inside.
  - `Refresh(ctx, id)` = `Get` + `syncServer` + one `SetValidation` with the result (manual path; behaviour unchanged).
  - `type Syncer struct` with `func NewSyncer(s *Service, interval time.Duration, logger *slog.Logger, onResult func(ok bool)) *Syncer`, `Start()`, `Stop(ctx context.Context) error`, and `func (y *Syncer) round(ctx context.Context)` (one pass, exported to tests via the package's own test file).
  - Config `MCP.SyncInterval time.Duration \`yaml:"sync_interval"\``; API `mcp_sync_interval_seconds` in `GET /api/v1/server-config`.
  - Metrics `voice_mcp_sync_total{result="ok"}` and `{result="failed"}`.

- [ ] **Step 1: failing tests (sync_test.go, using `mcptest` servers via httptest and the store helpers the existing service tests use).**
  - `round` marks a reachable server healthy and stores one discovery; a second round with the same tools adds no discovery (Task 1).
  - Two strikes: a healthy server whose endpoint is now down stays `healthy` after round 1, becomes `connection_failed` after round 2; one successful round returns it to `healthy` and clears the count; a server that is already failing is updated on every round; fail-ok-fail-ok stays healthy throughout.
  - Disabled and pending-deletion servers are skipped.
  - Lifecycle: `Start` then `Stop` returns before its context deadline and leaks no goroutine (goleak); `Stop` while a round is in progress cancels the in-flight sync; a panic inside a round (inject a panicking `onResult`) is recovered and the loop keeps ticking; rounds never overlap (a round slower than the interval delays the next one).
  - Config: `sync_interval` defaults to 5m; `0` accepted; `10s` and `48h` rejected with a message naming `mcp.sync_interval`.
- [ ] **Step 2: run, see them fail.**
- [ ] **Step 3: implement.**
  - Split `Refresh` as described, keeping every existing state transition identical (the existing service tests are the safety net: they must still pass unchanged).
  - `Syncer`: a goroutine started by `Start` with its own cancel (owner lifetime, tagged per AGENTS.md); first round after a random delay in `[0, interval)` (use `math/rand/v2`), then every `interval` using `time.NewTimer` reset after each round (no overlap). `round`: list servers (`store.MCP().ListServers`), skip `!Enabled || PendingDelete`, for each call `syncServer` with a context bounded by the service timeout, then decide what to write: if the result is a failure and the stored state is `healthy` and `strikes[id] == 0` → `strikes[id] = 1`, write nothing; otherwise write the result and set `strikes[id] = 0` on success or `strikes[id] = 2` on a failure. `strikes` is a map owned by the loop goroutine only (no lock). Log at Info only when the written state differs from the stored state. `defer recover()` per round with an Error log.
  - Management: when `cfg.MCP.SyncInterval > 0`, create and `Start` the syncer after the MCP service exists; `Stop` it in `closeManagement` before the store closes, bounded by the shutdown context.
  - Config default, validation, golden regeneration, metrics counters (follow `voice_mcp_*` naming and the existing counter pattern in `metrics.go`), `server-config` field.
- [ ] **Step 4: run, see them pass**; `cd server && go test -race ./...`.
- [ ] **Step 5: commit** `feat(mcp): periodic sync with a two-strike rule (mcp.sync_interval)`.

---

### Task 4: readiness knows about a missing token variable

**Files:**
- Modify: `server/flow/service.go` (`flowReadiness`, a setter for the env lookup), `server/management.go` (wire it), `server/mcp/http.go` (nothing if `auth_env_set` already reflects an empty value; check `view`)
- Test: `server/flow/service_test.go` (or the readiness tests' file)

**Interfaces:**
- Produces: `func (s *Service) SetTokenEnv(isSet func(name string) bool)` on `flow.Service` (nil = every variable counts as set, which keeps existing tests and callers unchanged); dependency state `auth_env_missing`; `Readiness.MissingEnv` includes the variable name (deduplicated).

- [ ] **Step 1: failing tests.** A flow with a required tool on a healthy server whose `auth_env` is `MCP_TOKEN_X` and `isSet` returns false: readiness `tool_dependency_unavailable`, dependency state `auth_env_missing`, `missing_env` = `["MCP_TOKEN_X"]`. Optional tool: `ready_with_tool_warning`. Variable set: `ready`. An empty value counts as missing (the wiring uses `env(name) != ""`; test the wiring in a server-level test: start a test server with `MCP_TOKEN_X=""`).
- [ ] **Step 2: run, see them fail.**
- [ ] **Step 3: implement.** In `flowReadiness`, after the `Enabled` check and before the health check: `else if server.AuthEnv != "" && s.tokenSet != nil && !s.tokenSet(server.AuthEnv) { state = "auth_env_missing"; missing = append(missing, server.AuthEnv) }`; treat it like `server_unavailable` for required/optional; set `base.MissingEnv` (deduplicated, sorted) on every return that has tool dependencies. Wire `s.flows.SetTokenEnv(func(n string) bool { return s.env(n) != "" })` in `EnableManagement`. Confirm `view(...).AuthEnvSet` is false for an empty value (fix it the same way if not).
- [ ] **Step 4: run, see them pass**; `cd server && go test -race ./flow/... ./`.
- [ ] **Step 5: commit** `feat(flow): readiness names a missing MCP token variable`.

---

### Task 5: edit a server; create and edit errors say why

**Files:**
- Modify: `server/mcp/service.go` (`Create` errors, new `Update`), `server/mcp/http.go` (PATCH, error codes), `server/storage/repositories.go` (`UpdateServer`)
- Test: `server/mcp/*_test.go`, `server/api_mcp_boundary_test.go` (auth on the new route; mirror the existing boundary tests)

**Interfaces:**
- Produces:
  - `var ErrInvalidEndpoint, ErrDestinationDenied, ErrInvalidAuthEnv, ErrInvalidMetadata, ErrTransportUnsupported` (exported sentinels); `func ReasonCode(err error) string` mapping them to the exact strings in Global Constraints (`""` for anything else).
  - `type ServerUpdate struct { Name, Description, Endpoint, AuthEnv *string }` (JSON `name`, `description`, `endpoint`, `auth_env`; nil = unchanged; `auth_env: ""` clears it).
  - `func (s *Service) Update(ctx context.Context, id string, u ServerUpdate) (storage.MCPServer, error)`: validates like `Create`; when the endpoint or auth_env changed, stores the server with state `not_validated` and then calls `Refresh` (errors from the refresh are reported through the returned server's state, not as an error).
  - `PATCH /api/v1/mcp-servers/{id}` → 200 with the server view; 400 `{"error":{"code":"invalid_mcp_server","details":{"reason":"<code>"}}}`; 404 missing; 409 pending deletion; 500 store failure.
  - Create's 400 gains the same `details.reason`.
  - `MCPRepository.UpdateServer(ctx, m MCPServer) error` (name, description, endpoint, auth_env, validation_state, updated_at; `sql.ErrNoRows` when missing or pending deletion).
- [ ] **Step 1: failing tests.** Update name only (state unchanged, no sync); update endpoint to a reachable mcptest server (state becomes `healthy`, latest discovery set) and to an unreachable one (state `connection_failed`, 200); invalid endpoint, private address in production policy, bad auth_env, too-long name each give their reason code; missing id 404; pending deletion 409; a flow that selected the server's tools is still `ready` after an endpoint change to an equivalent server (same tools); a session captured before the edit still calls the OLD endpoint (capture, edit, then execute the captured tool and assert the old server received the call — use `mcptest` records); the route requires the management key.
- [ ] **Step 2: run, see them fail.**
- [ ] **Step 3: implement**, reusing one `validateServerConfig` helper for Create and Update so the rules cannot drift.
- [ ] **Step 4: run, see them pass**; `cd server && go test -race ./mcp/... ./storage/... ./`.
- [ ] **Step 5: commit** `feat(mcp): edit a server in place; create and edit errors carry a reason code`.

---

### Task 6: admin UI

**Files:**
- Modify: `ui/src/api/client.ts` (server-config field, `updateMCPServer`, error `details` typing, tool call `error_category`), `ui/src/routes/mcp/ServerDetail.tsx` (status line, last checked, automatic interval, Edit action, the loopback sentence), `ui/src/routes/mcp/ConnectServerDialog.tsx` (edit mode, reason messages), `ui/src/routes/mcp/ToolTestConsole.tsx` (category and message), `ui/src/routes/mcp/mcpStats.ts` (status text), `ui/src/components/timeline/TurnTimeline.tsx` (category next to status)
- Test: the neighbouring `*.test.tsx` / `*.test.ts` files (mirror their fixtures and mock clients)

**Interfaces:**
- Consumes: Tasks 1–5's API fields and routes.

- [ ] **Step 1: failing tests**, one per behaviour:
  - Status: a server with `auth_env` set and `auth_env_set: false` shows "Token variable MCP_TOKEN_X is not set on the server" as its status text (list and detail).
  - Detail shows "Last checked <relative time>" from `last_connected_at` and "Checked automatically every 5 minutes" from `mcp_sync_interval_seconds` (or "Automatic checks are off" for 0).
  - The loopback sentence: says private and loopback addresses are refused "in production" (use the server-config `profile` field to say "this development server allows loopback" when profile is development — read how other views get server-config).
  - Edit: the Edit button opens the dialog prefilled; saving sends only the changed fields to `updateMCPServer`; the returned state is shown; a 400 with `details.reason = destination_denied` shows "That address is not allowed: MCP servers must be public HTTPS addresses."; each reason code has a sentence (`invalid_endpoint`, `destination_denied`, `invalid_auth_env`, `invalid_metadata`, `transport_unsupported`). The connect (create) dialog shows the same sentences.
  - Try it: a 502 whose `details` is `{category:"tool_error", message:"no such order"}` shows "The tool reported an error: no such order"; `timeout` shows "The tool did not answer in time"; other categories have short sentences.
  - Calls page timeline: a tool call with `error_category: "timeout"` shows the status and "timeout".
- [ ] **Step 2: run, see them fail** (`cd ui && npx vitest run src/routes/mcp src/components/timeline`).
- [ ] **Step 3: implement** with existing CSS classes only (the colour-token tests must stay green).
- [ ] **Step 4: run** `cd ui && npx tsc --noEmit && npx vitest run` — all green.
- [ ] **Step 5: commit** `feat(ui): MCP status, automatic checks, edit server, error reasons and categories`.

---

### Task 7: browser end-to-end and Go end-to-end

**Files:**
- Modify: `e2e/tests-flows/30-mcp.spec.ts` (or a new `31-mcp-edit.spec.ts`), `e2e/global-setup-flows.ts` only if the stack needs `mcp.sync_interval` (it should set a short one, e.g. `30s`, so a check is visible within a test, or `0` if tests rely on manual Sync — decide after reading the spec)
- Test: those specs; `server/mcp_call_e2e_test.go`

- [ ] **Step 1:** Playwright, echo provider, local mcptest: (a) edit a server's endpoint from a failing path to the healthy `/ok` path and see the status become Connected without re-creating anything, and a flow that uses it stays ready; (b) Try it on `tool_error` shows "order system unavailable".
- [ ] **Step 2:** Go e2e: a server whose mcptest endpoint is stopped is still healthy after one background round and failed after two; restarting it makes it healthy after the next round (drive `round` directly, not the timer).
- [ ] **Step 3: run** `make e2e-echo` (whole free browser and Go end-to-end suite) — all green; `cd e2e && npm run typecheck`.
- [ ] **Step 4: commit** `test(e2e): MCP edit, tool error messages and periodic sync`.

---

### Task 8: documentation and the gate

**Files:**
- Create: `docs/operations/mcp.md`
- Modify: `docs/api/mcp.md` (PATCH, reason codes, error categories, `error_category`), `docs/security/mcp.md` (tool error messages reach the model; cleaned and capped), the config reference that lists `mcp.*` keys (find it via `cd server && go test -run Docs ./`), `config/*.yaml` comments where `mcp:` appears (document `sync_interval`, default left implicit)

- [ ] **Step 1:** write `docs/operations/mcp.md`: states and what each means (`not_validated`, `healthy`, `authentication_failed`, `connection_failed`, `discovery_failed`, `tool_schema_invalid`, `disabled`); automatic sync every `mcp.sync_interval` and the two-strike rule; manual Sync; rotating a token (set the new value, restart; the variable name stays); editing a server; recovering a flow in `needs_tool_review`; what the model sees when a tool fails.
- [ ] **Step 2:** run the docs checks (`cd server && go test -run Docs ./`; `make verify` runs the link checker).
- [ ] **Step 3: gate:** `gofmt -l .` (empty), `make verify`, `make deadcode`, `make e2e-echo`. All green.
- [ ] **Step 4:** independent review of the whole branch (AGENTS.md), with the Review Focus list above; fix Critical/Important findings test-first.
- [ ] **Step 5: commit** `docs(mcp): operator guide; API and security notes for sync, edit and error categories`.
