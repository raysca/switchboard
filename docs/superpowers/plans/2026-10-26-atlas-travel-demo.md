# Atlas Travel Concierge demo — implementation plan

Date: 2026-10-26. Branch: `atlas-concierge-demo`. Spec:
[2026-10-26-atlas-travel-demo-design.md](../specs/2026-10-26-atlas-travel-demo-design.md).

Slices are ordered by dependency. Write scopes are disjoint; the Lead owns the demo
installer/API/config files so the two central slices cannot race.

## S0 — Spec review (before any code)

Independent reviewer read the spec and this plan, read-only. Verdict: proceed with
fixes; nothing infeasible, and nothing serious on secret handling, argument/result
retention, SQLite/Postgres or migrations. Resolutions:

| # | Severity | Finding | Resolution |
|---|---|---|---|
| F1 | High | Two "Load demo" buttons would break the dental e2e (strict mode) | Dental button keeps `Load demo`; Atlas button is `Load Atlas demo`; each card is a labelled region. S4 verifies `60-dental` as well. |
| F2 | High | `61-atlas` would fail on the paid xAI run (no demo provider keys) | `test.skip(LIVE, ...)` in the new spec, as `60-dental` does. |
| F3 | Medium | Missing `MCP_TOKEN_ATLAS` gave a generic 502, card still offered | Status `available` and the install both require the token; the 409 names it; a test covers it. |
| F4 | Medium | Re-install could not repair a stale pin while status said "Installed" | The installer compares the demo-owned pin set (server + both HTTP tools, hashes, required) and archives/recreates on mismatch; exact pins with foreign tools are left. Tests added. |
| F5 | Medium | Spec table disagreed on the Concierge greeting | Table and text say three handoff greetings; the start step is model-greeted. |
| F6 | Medium | Plan pointed at a non-existent API demo section | `docs/api/v1.md` gains a "Demo install endpoints" section; the dentistry doc gets the cross-link. |
| F7 | Medium | `xaispeech` build requirement was missing | Spec, plan and `docs/demo/atlas.md` state it; Go tests skip without the tag like the dental rig. |
| F8 | Medium | "one per ordered pair" was arithmetically wrong at 8 steps | "at most one per ordered pair" (32 of 56 possible). |
| F9 | Low | A copied `/mcp` suffix in `ATLAS_DEMO_URL` was unguarded | Base URL rejects a `/mcp` suffix with a message; unit test added. |
| F10 | Low | Canvas 8/32 acceptance had only a screenshot | The browser test counts 8 nodes and 32 edges before the screenshot. |
| F11 | Low | No partial-install test | `TestInstallAtlasFinishesAHalfDoneInstall` added. |
| F12 | Low | Provider-key prerequisite undocumented | Failure paths and `docs/demo/atlas.md` list Google, xAI and ElevenLabs keys. |
| F13 | Low | Non-goal reasoning was wrong for `failover` | Non-goals say the agent has no failover because the demo names no fallback providers. |
| F14 | Low | Cross-instance install race unaddressed | Documented as per instance; a lost `http_tools.name` create race is re-read and reused (unit-tested through the installer path). |
| F15 | Low | Fixture HTTP endpoints would bind every interface | `cmd/atlas-mcp` binds 127.0.0.1 by default. |
| F16 | Low | Vocabulary trap wording | The new docs/code use "step"/"handoff"/"endpoint". |
| F17 | Low | Plan/code naming drift | Recorded below: `atlasmcp.HTTPTools` (not HTTPEndpoints), `demo.HTTPTools` interface (List/Create/SetEnabled), `stopFlights` map. |

No finding was rejected. The documents above and the slices below carry the fixes; code
for S1/S4 started after the reviewer was briefed and the central slices (S2/S3) carry
every finding that changes behaviour.

## S1 — Atlas fixture server: MCP tools and HTTP endpoints

Files (owner: build teammate, write scope `server/mcp/atlasmcp/`, `cmd/atlas-mcp/`,
`Makefile`):

- `server/mcp/atlasmcp/atlasmcp.go` — `New(Options)`, `Handler()` with `GET /health`,
  bearer-protected `/mcp`, `GET /api/weather`, `GET /api/advisory`; in-memory seed
  (3 travellers, 5 cities of stays/flights/transfers, payment methods, bookings);
  18 MCP tools; `ToolNames` and `HTTPEndpoints` exported for the installer.
- `server/mcp/atlasmcp/atlasmcp_test.go` — table tests over the SDK client for search →
  book → change/cancel, refunds, loyalty points, disruption rebooking, callback
  creation, bounds and bad ids; HTTP endpoint tests; auth refusal test.
- `cmd/atlas-mcp/main.go` — `PORT` (default 18768), `ATLAS_MCP_TOKEN` (required),
  graceful shutdown.
- `Makefile` — `atlas-mcp` target beside `dental-mcp` (port 18768, token
  `dev-atlas-token`), `.PHONY` updated.

Tests: `cd server && go test ./mcp/atlasmcp`; `go build ./cmd/atlas-mcp`.

## S2 — Demo installer (Lead)

Files (write scope `server/demo/`):

- `server/demo/atlas.go` — `InstallAtlas` (new `HTTPTools` interface and `Result`
  fields; `FindAgent` takes the demo's agent name so dental call sites change), the
  8-step/32-handoff config builder,
  prompts, tool tables, HTTP tool definitions, idempotent install, conflict errors,
  stale-agent handling; `FindAgent` gains a name parameter (dental call sites updated).
- `server/demo/atlas_test.go` — fakes for MCP servers, HTTP tools and agents: create once;
  second run changes nothing; half-done install completed; config decodes to 8
  steps/32 handoffs/all end_call/one async tool/HTTP refs; 20 pins; 32 unique ordered
  pairs; every step reachable; conflict errors (MCP endpoint, HTTP tool URL); stale agent
  archived; drifted demo pin repaired; exact pins plus a foreign tool left alone; base
  URL normalisation and `/mcp`-suffix refusal.

Tests: `cd server && go test ./demo`.

## S3 — Demo API, status and auto-install (Lead)

Files (write scope `server/demo_api.go`, `server/config.go`, `server/config_validate.go`,
`server/server.go` (startup gate + per-demo cancel map), `server/demo_api_test.go`,
`server/config_test.go`):

- `demoStatusResp` gains `atlas`; `handleDemo` gains `POST /demo/atlas`; atlas env
  accessor; per-demo singleflight keys and a per-demo map of install cancels (shutdown
  cancels every running install); auto-install switch for `dental`/`atlas` with the
  existing retry loop; the `Start` gate changes from `== "dental"` to `!= ""`.
- Tests: availability without env/token; 409 without env and without token; install-once with a real
  `atlasmcp` httptest server and real MCP/HTTP tool services; status shape; auto-install
  validation for `atlas` and rejection of other values.

Tests: `cd server && go test -run 'Demo|Config' .`.

## S4 — Admin UI demos (build teammate, write scope `ui/src/api/client.ts`,
`ui/src/api/client.test.ts`, `ui/src/routes/dashboard/`)

- `demoStatusSchema` gains `atlas`; `installAtlasDemo()`.
- `DemoCard` renders a demo list with both entries as labelled regions (availability,
  installed link, install button, failure alert), each independent. The dental button
  stays `Load demo`; the Atlas button is `Load Atlas demo`, so `60-dental.spec.ts` keeps
  a unique unscoped lookup. S4 also runs the dental spec.
- Tests: client method path/method, both demo cards, atlas install failure/retry.

Tests: `cd ui && npm test -- --run src/api/client.test.ts src/routes/dashboard`.

## S5 — E2E stack and free browser proof (build teammate, write scope `e2e/`)

- `e2e/global-setup-agents.ts` — new atlas port, build `bin/atlas-mcp`, start it, set
  `ATLAS_DEMO_URL`/`MCP_TOKEN_ATLAS` on the server, wait for `/health`, stop it in
  teardown.
- `e2e/lib/atlas-helpers.ts` — `loadAtlas`, `createEchoAtlasVariant`, tool-turn helpers
  reused from dental helpers where possible.
- `e2e/lib/stack.ts` — an optional `atlasUrl` field (or a literal port).
- `e2e/README.md` — a row for `61-atlas.spec.ts` in the spec table.
- `e2e/tests-agents/61-atlas.spec.ts` — `test.skip(LIVE, ...)`, load demo; assert 8/32/end_call/20 pins/async;
  echo call through Concierge → Flights (MCP) → Stays (HTTP) → Payments (MCP) → Human
  handoff (MCP) → end_call; assert active step cards, tool results, call path, step
  labels and end reason; count 8 `.react-flow__node` and 32 `.react-flow__edge` in the
  canvas; capture `step()` screenshots of the demo card, the canvas and the call
  detail.

Tests: `cd e2e && npx playwright test -c playwright.agents.config.ts 61-atlas`.

## S6 — Documentation (build teammate or Lead, write scope `docs/demo/atlas.md`,
`README.md`, `docs/quickstart.md`, `docs/api/v1.md`, `docs/demo/dentistry.md`)

- `docs/demo/atlas.md`: what it demonstrates, step/tool table, handoff summary, local
  run (`make atlas-mcp`, env vars, `make dev`, **Load Atlas demo**), acceptance script,
  limits (8 steps, 32 handoffs, 10 transfers/call), security notes, non-goals.
- Link it from README (Try it), quickstart's local-development section, the API demo
  section (section 8), and the dentistry doc's "other demo" line. No `docs/api/v1.md`
change: it has no demo section, and the demo API lives in the demo docs.
- No banned vocabulary: the product says "agent"/"handoff", never "flow"/"route".

Docs gate: `node scripts/check-links.mjs --paths` and
`node scripts/check-vocabulary.mjs`.

## S7 — Integration, independent review and gate (Lead)

1. `gofmt -w .`; `go build ./...`; targeted unit tests per slice; the UI tests run
   manually (the Makefile's `verify` does not run them).
2. Independent code-review agent (fresh context, read-only, `git diff main...HEAD`),
   reporting ranked findings with file:line; fixes or written rejections.
3. Gate, one command at a time: `make verify`; `make e2e-echo`; `make deadcode`.
4. Copy the captured screenshots to the session scratch folder; write the evidence
   report (summary, reviewer findings, gate counts, leftovers, transcript of the demo
   install).
5. Merge `main`, fast-forward, remove the worktree and branch.

## Plan-to-build deviations

Recorded as the build lands (AGENTS.md stage 2/3):

- `atlasmcp` exports `HTTPTools []HTTPTool{Name, Description, Method, Path,
  InputSchema}`, not `HTTPEndpoints`.
- `demo.HTTPTools` is `List`/`Create`/`SetEnabled`; the server adapts
  `*httptool.Service`.
- `FindAgentNamed(ctx, agents, name)` was added; `FindAgent` keeps the dental call sites.
- The single install cancel became `stopFlights map[string]context.CancelFunc`, so
  concurrent installs of different demos are all cancelled and waited for at shutdown.
- `demo.Result` gained `http_tools` and `http_tools_created` (omitted when zero, so the
  dental response is unchanged).

## Code review findings (independent, read-only) and resolution

Verdict: approve with fixes; no crash, data-loss, secret-leak, race or storage defect.
The review ran against the working tree before these fixes.

| # | Severity | Finding | Resolution |
|---|---|---|---|
| 1 | Medium | Flights/Stays prompts told the model to call `list_bookings`, which those steps do not expose; they also offered a points handoff that does not exist; Human handoff (a sink) offered a transfer tool. | Prompt text now uses the booking id from the earlier lookup (or transfers to the concierge), routes points via Payments, and Human handoff has no transfer instruction. |
| 2 | Medium | The Loyalty prompt and the demo doc claimed `apply_points` takes a booking id; it takes only traveller/points/reason. | Prompt and doc now say the tool deducts points from the balance; the acceptance script asks to use points, not to pay with them. |
| 3 | Low-Medium | The installer archived a drifted demo agent before creating its replacement, so a failed publish left no demo. | The replacement is created first and the old agent archived only after it exists; a failed publish leaves the old agent callable. The installer now also compares the stored config, so a reload rolls out prompt/handoff changes. |
| 4 | Low | Unreachable `if f.ID != ""` after the archive branch. | Removed by the create-then-archive restructure. |
| 5 | Low | Dead `MaxTravellers` bound and over-exported internal limits. | Removed; only `MaxBookings`/`MaxCallbacks` stay exported for tests. |
| 6 | Low | `GET /api/v1/demo` scanned the agent store twice. | `demo.FindAgentsNamed` collects both demo agents in one pass; status uses it. |
| 7 | Low | The `/mcp`-suffix rejection had no test. | Added to the bad-base-URL table. |
| 8 | Low | Fixture test gaps: stay/transfer rebooks, `change_flight` on a non-flight, concurrent rebook. | Tests added for all four. |
| 9 | Low | Doc drift: button label, `http_tools` fields shown for dental, missing `ATLAS_DEMO_URL` in the deployment table, stale plan line. | All four fixed (`docs/demo/atlas.md`, `docs/quickstart.md`, `docs/api/v1.md`, `docs/deployment.md`, this plan). |
| 10 | Low | The e2e claimed a per-step voice change it did not assert, and asserted the helper's local object. | README claim removed; the spec now re-reads the stored echo variant. The installed agent's differing voices remain visible in the demo tables/screenshot. |
| 11 | Low | `atlasmcp.addTools` is a long registration function; `demo_api.go` mixes routing, install flight and auto-install. | Rejected for this change: the handlers are thin and shared locking/validation is deliberate; both files are under 700/340 lines and the split is mechanical follow-up work, not a correctness fix. Reviewers may raise it again in a later refactor. |
| 12 | Low | The HTTP tool conflict check compared only method and URL, while the comment claimed any definition. | Compares the normalized definition (description, schema, timeout, response limit, pick list); a same-URL edit is a conflict. |
| 13 | Hygiene | `.gocache/` untracked; `docs/demo/atlas.md` staged with unstaged edits. | The cache directory is removed before merge and never staged; the doc is restaged. |

## Risks

- The e2e stack gains a fifth process and port; a port clash fails fast in global setup.
- HTTP tools refuse plain HTTP in production by design, so a production demo needs an
  HTTPS `ATLAS_DEMO_URL`; documented.
- The 32-handoff graph makes the canvas dense; node positions are laid out by hand in
  two columns and one hub to keep the screenshot readable.
- A partial install (MCP server created, HTTP tool creation failed) is recovered by the
  next install: every step reuses what matches and creates only what is missing.

## Out of scope

Paid live-provider verification, DTMF/recording/handoff cue/background audio, new
management APIs, storage migrations, and any change to the dental demo's behaviour.
