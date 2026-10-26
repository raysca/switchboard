# Atlas Travel Concierge demo — design

Status: draft for review. Date: 2026-10-26. Branch: `atlas-concierge-demo`.

## Goal

Add a second installable demo, **Atlas Travel Concierge**, that exercises the agentic
surface at its current limits: a stored agent with the maximum 8 steps and the maximum
32 handoffs, 18 MCP tools across every step, two HTTP endpoint tools, an async MCP tool,
parallel tool calls, per-step LLM/TTS voices, fixed per-step greetings, and `end_call`
on every step. A caller is triaged by a concierge step and handed between seven
specialists; the whole call is visible in the admin UI (agent canvas, call path,
step-labelled transcript, tool calls) and runs free of charge on the echo provider in
the browser end-to-end suite.

The existing Smile Dental demo stays unchanged: three steps, two handoffs, one MCP
server. It is the small walkthrough; Atlas is the stress case that shows the platform
holds a dense, tool-heavy, multi-provider agent.

## What exists today (facts the design relies on)

- A stored agent is a `Definition`: `start`, 1..8 `steps`, at most 32 `handoffs`, and an
  optional `handoff_cue` (`server/agent/definition.go`). Each step is a full `Config`
  (LLM, TTS, optional greeting, tools) plus `end_call`.
- A handoff becomes a `transfer_to_<slug(step name)>` tool on its source step; the
  condition is the tool description. A call may take at most 10 transfers
  (`core/router.go`, `Max` default 10); a transfer may carry a model-written note of at
  most 500 bytes.
- Only `llm`, `tts`, `audio.background` and `greeting` may differ on a non-start step.
  STT, VAD, context limits, idle, turn, recording, `max_call_duration_seconds`,
  `async_tools`, failover and `tts.clean_text` belong to the start step.
- Step `tools` are references to the agent version's selected tools:
  `<mcp_server_id>/<remote_name>` for MCP and `http:<tool id>/<name>` for HTTP tools.
- HTTP tools are stored definitions with immutable versions; the service validates the
  destination, and `mcp.allow_local_http` (development/tests only) permits loopback.
  A definition pins by `(tool id, definition hash)` in the same selection table as MCP
  tools (`server/httptool`, `server/agent/service.go`).
- The dental demo is an installer in `server/demo`, surfaced by `GET /api/v1/demo` and
  `POST /api/v1/demo/dental`, a Dashboard card, `demo.auto_install: dental`, and an
  MCP server binary (`cmd/dental-mcp`) started beside the stack.

## Decisions

### D1 — One new server binary, two surfaces

`server/mcp/atlasmcp` serves the demo's fake travel data; `cmd/atlas-mcp` exposes it as
one process:

- `GET /health` — open, for the e2e stack and operators.
- `/mcp` — the MCP streamable-HTTP endpoint, bearer token from `ATLAS_MCP_TOKEN`.
- `/api/weather`, `/api/advisory` — plain JSON endpoints for the two HTTP tools.

One process keeps the demo runnable with one extra terminal (`make atlas-mcp`) and one
pair of environment variables, while still showing both tool kinds.

### D2 — Environment surface

The server learns about the demo through one base URL and one token name:

- `ATLAS_DEMO_URL` — e.g. `http://127.0.0.1:18768` (the origin, not the MCP endpoint); the
  installer derives `<base>/mcp` for the MCP server and `<base>/api/...` for the HTTP tool
  URLs. A base ending in `/mcp` is rejected with a message pointing at the origin.
- `MCP_TOKEN_ATLAS` — the bearer token name (value read at use, never stored), like
  `MCP_TOKEN_DENTAL`.

The demo is available (Dashboard card, status `available`) only when both are set. The
install endpoint returns `409 demo_unavailable` naming the missing variable, rather than
failing discovery with a generic authentication error. The install is per instance: the
singleflight is in-process and MCP server/agent names are not unique in the store, so two
replicas sharing a database may both install; a lost `http_tools.name` race is re-read
and reused instead of becoming a 502.

### D3 — The agent is at the limits

Eight steps, exactly 32 handoffs (at most one per ordered pair; all steps reachable
from the start), all with `end_call: true`. Start step owns Google STT, context limits, a
10-minute call cap, and `async_tools` naming `rebook_disruption`. Steps mix Google and
xAI LLMs and Google, xAI and ElevenLabs TTS, so a call visibly changes voice at
handoffs. Three handoff steps use a fixed greeting (the transfer note is still injected
as context); the start step and the other steps let the model greet.

| # | Step (id) | LLM | TTS | MCP tools | HTTP tools | Greeting |
|---|---|---|---|---|---|---|
| 1 | Concierge (`concierge`, start) | Google Gemini 2.5 Flash | ElevenLabs | `get_traveller`, `list_bookings` | — | — (model greets) |
| 2 | Flights (`flights`) | xAI Grok | xAI | `search_flights`, `book_flight`, `change_flight`, `cancel_booking` | — | fixed handoff greeting |
| 3 | Stays (`stays`) | Google Gemini 2.5 Flash | Google Chirp 3 HD | `search_stays`, `book_stay`, `cancel_booking` | `weather_forecast` | — |
| 4 | Ground transport (`ground`) | Google Gemini 2.5 Flash | ElevenLabs | `search_transfers`, `book_transfer` | — | fixed handoff greeting |
| 5 | Payments (`payments`) | xAI Grok | ElevenLabs | `list_payment_methods`, `charge_booking`, `issue_refund` | — | — |
| 6 | Loyalty (`loyalty`) | Google Gemini 2.5 Flash | Google Chirp 3 HD | `get_loyalty`, `apply_points` | — | — |
| 7 | Disruptions (`disruptions`) | xAI Grok | xAI | `list_bookings`, `report_disruption`, `rebook_disruption` (async) | `travel_advisory` | — |
| 8 | Human handoff (`human`) | Google Gemini 2.5 Flash | ElevenLabs | `create_callback` | — | fixed handoff greeting |

The agent version selects 20 tools (18 MCP + 2 HTTP); the platform allows at most 64
(`server/storage/repositories.go`). HTTP tools cannot be async (`server/runtime/mcp.go`), so
the async tool is an MCP tool. `llm.max_parallel: 3` is set on Flights and Disruptions so a single model turn may run
up to three tool calls concurrently.

Handoff graph (32 edges; condition text is the model's cue):

| From | To | Condition (short form) |
|---|---|---|
| concierge | flights | find, book, change or cancel a flight |
| concierge | stays | hotel, villa or apartment for the trip |
| concierge | ground | airport transfer, hire car or rail |
| concierge | payments | pay, query a charge, or request a refund |
| concierge | loyalty | points, tier status or an upgrade |
| concierge | disruptions | the trip was delayed, cancelled or changed by a supplier |
| concierge | human | asks for a person, or is angry or distressed |
| flights | concierge | changes subject to something the concierge triages |
| flights | stays | wants a hotel for the same trip |
| flights | ground | needs transport to or from the airport |
| flights | payments | wants to pay for or refund a flight |
| flights | disruptions | a flight is delayed or cancelled and needs rebooking |
| flights | human | asks for a person, or the issue is beyond a flight change |
| stays | concierge | changes subject to something the concierge triages |
| stays | flights | wants to add or change a flight |
| stays | ground | needs a transfer to or from the hotel |
| stays | payments | wants to pay for or refund a stay |
| stays | disruptions | the hotel is overbooked, closed or must be rebooked |
| stays | human | asks for a person, or the issue is beyond a stay change |
| ground | flights | wants to book or change a flight |
| ground | stays | wants to book or change a stay |
| ground | payments | wants to pay for or refund a transfer |
| ground | disruptions | a missed connection or failed transfer needs help |
| ground | human | asks for a person, or the issue is beyond a transfer |
| payments | flights | after paying, wants to add a flight |
| payments | stays | after paying, wants to add a stay |
| payments | loyalty | wants to use points or a tier benefit |
| payments | human | a payment dispute needs a person |
| loyalty | flights | wants to redeem points for a flight |
| loyalty | human | an account issue needs a person |
| disruptions | payments | wants a refund or compensation |
| disruptions | human | rebooking cannot be resolved by the agent |

### D4 — Tool server data and bounds

`atlasmcp` keeps in-memory fake travellers, bookings, inventory and callbacks, seeded
deterministically relative to the injected clock (as `dentalmcp` does). It bounds
travellers, bookings and refunds (200 / 400 / 200), caps text fields at 120 characters,
and never persists anything. Tool inputs are validated; unknown ids return an error the
model can recover from. `rebook_disruption` sleeps for a configurable
`Options.Delay` (default 2 s) so it is a realistic async tool.

The two HTTP endpoints return small JSON documents derived from the same in-memory
data (weather by city/date, advisory by country); they never read the MCP token.

### D5 — Installer is idempotent and partial-install safe

`demo.InstallAtlas(ctx, servers, httpTools, agents, baseURL)`:

1. Validate/normalise `baseURL`; derive the MCP endpoint and HTTP tool URLs.
2. List MCP servers; reuse "Atlas travel inventory" when its endpoint matches, enable it
   when disabled, create it otherwise. A name match with a different endpoint is an
   error naming the conflict (as dental does).
3. Refresh discovery; require every name in `atlasmcp.ToolNames`; pin
   `(server id, remote name, schema hash, required)`.
4. List HTTP tools; reuse by name when the current definition's URL matches; create
   `weather_forecast` and `travel_advisory` otherwise. A name match with a different
   URL is an error naming the conflict. A lost create race (`ErrHTTPToolNameTaken`)
   re-lists and reuses the winner. Pin `(http:<id>, name, definition hash, required)`.
5. Find the live agent. Compare the pins that belong to this demo (its MCP server and
   both HTTP tools, with their hashes and `required`) with what this run computed.
   Exact match: leave the agent, even with extra tools from other servers. Mismatch
   (another server, an edited HTTP tool, a missing pin): archive the demo's own agent
   and recreate it, because a required pin that needs review makes the agent
   uncallable while the card would still say "Installed". An agent without version
   detail cannot be judged and is left alone.
6. Create the agent with the full config and the 20 pinned tools. All writes are
   independent, so an interrupted run completes on the next install.

The status endpoint reports `atlas.available`/`installed`/`agent_id` beside the existing
`dental` object. `POST /api/v1/demo/atlas` installs it. `demo.auto_install` accepts
`dental` or `atlas`; each retries at startup like dental and never stops the server.

### D6 — UI

The Dashboard card becomes a demo list with two labelled regions (Dentistry, Atlas
Travel), each with its own availability, installed link and install button. The dental
button keeps the accessible name "Load demo"; the Atlas button is "Load Atlas demo", so
the existing dental browser test's unscoped lookup stays unique. The API client schema gains
the `atlas` status and `installAtlasDemo()`. No new screen: the existing agent canvas
draws the 8-step, 32-handoff graph; the Calls page already draws the path and
step-labelled transcript.

### D7 — Free browser proof

The browser suite (echo provider, offline) gains a spec (and a row in `e2e/README.md`) that:

1. Loads the demo from the Dashboard and asserts the agent exists.
2. Asserts the stored definition: 8 steps, 32 handoffs, every step end_call, 18 MCP
   pins, 2 HTTP tool pins, one async tool.
   The spec skips the paid xAI run (`test.skip(LIVE, ...)`) because the suite's
   placeholder provider keys exist only for the fake stack.
3. Creates an echo variant of the agent (same steps, handoffs, tools and selections;
   every stage rewritten to `echo`, as `createEchoDentalVariant` does) and drives a call:
   Concierge → Flights (MCP `search_flights`) → Stays (HTTP `weather_forecast`) →
   Payments (MCP `list_payment_methods`) → Human handoff (`create_callback`) → `end_call`.
4. Asserts the path and step labels on the call page, and asserts the canvas has 8
   `.react-flow__node` and 32 `.react-flow__edge` elements before capturing screenshots
   of the loaded demo, the canvas and the call detail.

The e2e global setup starts `bin/atlas-mcp` on its own port with the same token the
server is given, and sets `ATLAS_DEMO_URL`/`MCP_TOKEN_ATLAS` on the server process.

## API and config changes

- `GET /api/v1/demo` response: `{ "dental": {...}, "atlas": { "available": bool,
  "installed": bool, "agent_id": string? } }` (additive).
- `POST /api/v1/demo/atlas` → `demo.Result` (`mcp_server_id`, `mcp_created`, `tools`,
  `http_tools`, `agent_created`); `409 demo_unavailable` without `ATLAS_DEMO_URL` or
  `MCP_TOKEN_ATLAS` (the message names it); `502 demo_install_failed` on a failed
  install.
- `docs/api/v1.md` gains a short "Demo install endpoints" section (the file had no demo
  section, not even for dental), and the new `docs/demo/atlas.md` documents the demo.
- `demo.auto_install`: `""`, `"dental"` or `"atlas"` (the startup gate and the
  singleflight/cancel bookkeeping in `server/server.go` become per-demo).
- No storage migrations, no provider changes, no new secrets mechanism.

## Security and privacy

- The MCP token is an environment variable name only; it is never stored or returned.
- The demo data is fabricated. No real traveller data, no recording enabled, retention
  unchanged.
- HTTP tools carry no secret; their endpoints are the demo server's own read-only
  fixtures. In production the demo requires an HTTPS `ATLAS_DEMO_URL` (the HTTP tool
  service refuses plain HTTP unless `mcp.allow_local_http`, which production refuses).
- Tool arguments and results are not stored or traced, same as every MCP/HTTP tool.

## Failure paths

- Missing `ATLAS_DEMO_URL` or `MCP_TOKEN_ATLAS`: demo card hidden, install returns 409
  naming the missing variable.
- Provider credentials missing (Google, xAI, ElevenLabs) or a server built without the
  `xaispeech` tag (xAI TTS): agent validation refuses the publish, install returns 502
  with the problems in the server log, like the dental demo.
- Unreachable MCP server: discovery fails, install returns 502, the log holds the
  redacted cause, the button can retry.
- Discovery missing a tool: install fails naming the tool.
- Existing MCP server with another endpoint, or existing HTTP tool name with another
  URL: install fails with a conflict message; the operator deletes it or fixes the env.
- Existing stale or drifted agent (another MCP server, an edited HTTP tool, a missing
  pin): archived and recreated. An exact demo pin set with extra foreign tools is left
  alone.
- Server restart with `demo.auto_install: atlas`: retried for up to 60 s, failure logged.
- A call cannot transfer more than 10 times (platform limit, unchanged); the graph is
  denser than one call can traverse by design.

## Non-goals

- No paid-provider run in this change; the demo's live-provider setup is documented only.
  The browser proof uses the echo provider.
- No DTMF, recording, handoff cue sound or audio background in the Atlas agent: each
  needs server-level configuration outside an installable agent. The agent has no
  failover because the demo has no fallback providers to name (failover itself is a
  start-step setting).
- The agent needs a server built like `make build`/`make dev`, with the `xaispeech` tag
  for xAI TTS; a plain `go build ./cmd/server` registers no xAI TTS and the install is
  refused. This is the same requirement the dentistry demo documents.
- No new management API beyond the demo endpoints, no storage migration, no change to
  the dental demo's behaviour.
- No embedded UI changes to the canvas layout algorithm.

## Acceptance criteria

- `make atlas-mcp` serves `/health`, `/mcp` (token) and the two HTTP endpoints.
- On a development server with `ATLAS_DEMO_URL` and `MCP_TOKEN_ATLAS`, `POST
  /api/v1/demo/atlas` installs once and is idempotent; the stored definition decodes to
  8 steps, 32 handoffs and the tool pins above.
- The Dashboard offers and loads the demo; the browser test asserts the canvas has
  eight step nodes and 32 handoff edges.
- The echo browser call traverses at least four handoffs, runs an MCP tool and an HTTP
  tool, ends via `end_call`, and the call page shows the path, step labels and
  `Ended by the agent`.
- `make verify`, `make e2e-echo` and `make deadcode` pass; docs links and vocabulary
  guards pass.
- Screenshots of the loaded demo, the canvas and the call detail are captured from the
  real stack.

## Self-review

- Edge cases: install conflicts, partial install, missing env, stale agent, disabled MCP
  server, duplicate HTTP tool names, 32-handoff config validation, HTTP tool name
  uniqueness across the store, echo variant preserving HTTP pins.
- Compatibility: additive API/JSON; dental unchanged; SQLite and Postgres both use the
  same stores; no migrations.
- Security: token remains env-only; HTTP endpoints are read-only fixtures; no argument or
  result storage.
- Testability: unit tests for the MCP package, installer and demo API; browser test for
  the whole path; screenshots as evidence.
