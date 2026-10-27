# Demos extraction, clean-room server, and optional demo deployments — design

Status: draft for review. Date: 2026-10-27. Branch: `demos-extraction`.

## Goal

Remove every demo from the Switchboard server and ship demos as optional deployments:

- `server/` (and the server image) contains no demo MCP servers, installers, demo API, demo
  config, or demo env vars. A clean deployment starts with an empty agent store.
- `demos/` contains the demo assets: each demo's MCP fixture server(s), agent definition,
  deploy assets, docs, and tests.
- `demos/seed` is a standalone management-API client that **wipes and recreates** a demo on
  any server given its admin URL and management key. Operators do not expect demo updates to
  be preserved; every seed is a clean demo.
- Demo deployments are optional and have their own pipeline, targeting **Cloud Run or
  Cloudflare**. After deploying the demo's MCP service(s), the pipeline auto-seeds a server
  when given its admin URL and management key, or prints the seed command.

Existing behaviour to preserve: the demo agents themselves (Smile Dental: 3 steps; Atlas: 8
steps, 32 handoffs), the browser e2e proof of both demos, and the ability to run demos locally
without a deployment.

## Why

The server currently imports demo MCP packages from `server/mcp/dentalmcp` and
`server/mcp/atlasmcp`, carries ~700 lines of demo installers, a demo management API
(`/api/v1/demo*`), a `demo.auto_install` config, demo env vars, a Dashboard card, and a dental
image in the server deployment workflow. That is demo product code inside the runtime: it
enlarges the server surface, couples server releases to fixture data, and makes the demo
deployment look like part of the server deployment.

The split also fixes deployment semantics: the server pipeline is clean-room; demos are
optional resources deployed and seeded separately, on more than one platform.

## Decisions

### D1 — `demos/` in the root Go module, with a checked boundary

Move demo code to a top-level `demos/` tree in the root module. No new `go.mod`; this keeps
`make test`/`make deadcode` plumbing small. Module requirements are not a guarantee on their
own, so a static check (`go list -deps ./server/...` must not contain `govoice/demos`) runs in
the gate. `server/` must not import `demos/` in any direction.

```
demos/
  README.md
  spec/                      # importable types: Demo, Resolution, specs, pins
  registry.go                # package demos: the demo registry (imports spec + demos/<name>)
  seed/                      # package main: bin/seed (management API client)
    main.go, seed.go, seed_test.go, integration_test.go
  dental/
    README.md                # moved from docs/demo/dentistry.md
    mcp/dentalmcp/           # moved from server/mcp/dentalmcp
    cmd/dental-mcp/          # moved from cmd/dental-mcp
    demo.go                  # server/tool/agent definition (moved from server/demo/dental.go)
    demo_test.go
    deploy/
      cloudrun/Dockerfile.mcp, cloudrun.yaml, deploy.sh
      cloudflare/wrangler.jsonc, src/index.js
  atlas/
    ...same shape...
  deploy/                    # shared demo deploy helpers (seed runs as a workflow step)
```

`cmd/mcptest` stays in `cmd/` (it is a test fixture, not a demo).

### D2 — Demo definitions stay Go code; types live in `demos/spec`

No new manifest format. `demos/spec` holds the importable contract, so demo packages, the
registry and the seed cannot form an import cycle:

```go
// demos/spec
type Demo interface {
    Name() string
    MCPServers() []MCPServerSpec   // logical, name, description, endpoint path, token env, secret name, required tool names
    HTTPTools() []HTTPToolSpec     // name, method, path, description, input schema
    Agent(r Resolution) (AgentSpec, error)
}

type Resolution struct {
    ServerIDs map[string]string             // logical -> MCP server id
    HTTPPins  map[string]ToolPin            // name -> {Ref, Hash}
}
type ToolPin struct{ ServerID, RemoteName, SchemaHash string; Required bool }
```

`demos/registry.go` (package `demos`) imports `demos/spec` and the demo packages;
`demos/seed` imports `spec` + `demos`. The demo packages import `spec` only. Each MCP server
spec lists its required remote tool names, so seed can pre-flight discovery and build the
pins. The demo builds the same step/handoff config it builds today; wipe is by exact name
(no config publishing) and the only HTTP-tool reuse is the current-definition update the
server's immutable pins require.

### D3 — Seed is wipe-and-create, idempotent by reset

`demos/seed` uses only `/api/v1` HTTP APIs. One run:

1. Load the demo definition; resolve `--mcp <logical>=<url>` (repeatable), `--http-base`.
   Tokens come only from environment variables named by each server spec (or a token file):
   no token value is ever a command-line argument.
2. **Wipe** the demo-owned resources, in this order:
   - page all agents; for every live agent whose name equals the demo agent name, archive it
     (no publish, no unarchive: archived agents cannot be republished, and `detach-delete`
     below handles their MCP pins at storage level);
   - for every MCP server whose name equals a demo server name, call `detach-delete` and poll
     until the row is gone (`202 pending` is a wait, not a failure; `--wait` bounds it);
   - delete the demo token secret(s); `404` is success. The secret is deleted only after the
     MCP server rows are gone, because the secret API refuses a name still used by a server;
   - **do not delete HTTP tools**. A tool with any historical pin cannot be deleted
     (`HTTPToolRepository.Delete` counts immutable agent-version pins). HTTP tools are
     demo-owned by exact name: list, `PUT` the expected definition when it differs (a new
     immutable hash; this requires `--yes` because other agents that pinned the old hash need
     review), or create when absent; a disabled tool is enabled. The returned current hash is
     pinned on the new agent. This is the only reuse in seed, and it is required by the
     server's immutability rules.
3. **Create**: write token secret(s) to the server's encrypted secret store (namespaced
   `demo-<demo>-<purpose>`, after checking no non-demo MCP server uses that name), create the
   MCP server(s), validate/discover them, create/update the HTTP tools, build the agent,
   create it with the resolved pins.
4. **Verify** the agent by its created id and readiness; exit non-zero if it is not callable.

Flags: `--wipe-only`, `--dry-run`, `--yes`, `--wait`, `--json`. Running seed twice leaves one
live demo; a failed run is repaired by running it again. Archived agents accumulate because
agent deletion is intentionally unsupported and call history is retained; that is documented.
The HTTP tool definitions are versioned, not deleted; the demo always pins the expected
current definition.

Auth: the server must have the encrypted secret store enabled (`SWITCHBOARD_SECRETS_KEY`); it
is required in the shipped Cloud Run manifest/bootstrap and the Cloudflare Worker env, so the
clean-room server carries no demo token env. If the store is off, seed fails before writing.
The management key comes from `--key-env` (default `GOVOICE_MANAGEMENT_API_KEY`); it is never
logged.

Concurrency: one seed per demo at a time (pipeline concurrency group). The seed pages the
agent list, acts on every exact-name match, verifies the agent by the id it just created, and
retries the (still possible) HTTP-tool create race by listing and reusing the winner. MCP
server and agent names are not unique in the store; uniqueness is enforced by the workflow
concurrency group and by seed's exact-name cleanup, not by the database.

### D4 — Clean-room server

Delete from `server/`:

- `server/demo/`, `server/demo_api.go`, demo tests;
- `server/mcp/dentalmcp/`, `server/mcp/atlasmcp/`;
- `cmd/dental-mcp/`, `cmd/atlas-mcp/`;
- `DemoConfig`/`demo.auto_install` and its validation;
- `DENTAL_MCP_URL`, `ATLAS_DEMO_URL` and the `/api/v1/demo*` handlers.

Compatibility: keep `GET`/`POST /api/v1/demo*` for one release as a fixed `410 demo_moved`
response telling operators to use `demos/seed`; it is the only demo-named server route and is
tested as such. Remove the `demo.auto_install` config key from the shipped configs and
document the breaking config change (a stored config still carrying `demo:` fails startup as
an unknown field; `docs/operations.md` names the replacement). The UI loses the Dashboard demo card and its client methods; the
Agents/Calls/editor surfaces are unchanged.

The server image and its Cloud Run/Cloudflare deployment have no demo assets. Server
deployment no longer builds, deploys or expects `dental-mcp`; its smoke checks do not require
a demo agent. The shipped Cloud Run SQLite profile loses its auto-repopulation: after a
revision or cold start it now starts empty until the demo pipeline runs. That is the intended
clean-room behaviour; the deployment docs say so and recommend Postgres for a server whose
demos must survive, or re-run the demo pipeline after deploy. `bootstrap.sh` and both server
manifests gain the persistent `SWITCHBOARD_SECRETS_KEY` secret so seeding can work at all.

### D5 — Demo deployment pipeline

One reusable workflow `.github/workflows/deploy-demo.yml` with inputs: `demo`, `target`
(`cloudrun` or `cloudflare`), `environment`, `server_admin_url`, `management_key_secret`,
`auto_seed` (default true), `image_tag`. The workflow is called by a small
`workflow_dispatch` entry workflow (or dispatched directly with those inputs); the seed step
runs the `demos/seed` Go binary on the runner, not a separately published seed image. Demo
tokens are per-demo deployment secrets (GitHub environment secret for Cloudflare; Secret
Manager for Cloud Run), never generated inside the workflow, so a re-run uses the same token;
the workflow masks them. The GCP deployer identity gains read access to the per-demo token
secret and the management key. The Cloudflare path requires `CLOUDFLARE_API_TOKEN` and
account id in the environment.

Cloud Run target (`demos/<name>/deploy/cloudrun`):

1. Build/push the demo MCP image (linux/amd64, same registry as the server).
2. Deploy the MCP service (single instance, in-memory fixture) with the demo token from the
   environment secret; wait for `/health`; read the service URL.
3. If auto-seed: run `go run ./demos/seed` against `server_admin_url` with the management key
   fetched from `management_key_secret`, the MCP URL, and the demo token env. Seed wipes and
   creates the demo.
4. `seed` verifies readiness; the pipeline fails on a non-ready demo. Print the seed command
   (env indirection only) when auto-seed is off.

Cloudflare target (`demos/<name>/deploy/cloudflare`):

1. Build the demo MCP image for linux/amd64 (Wrangler uses
   `demos/<name>/deploy/cloudflare/Dockerfile.mcp`; the container sets
   `ATLAS_MCP_ADDR=0.0.0.0:$PORT` for Atlas).
2. Deploy a demo Worker + container from `wrangler.jsonc`: the Worker proxies `/mcp`,
   `/api/*` and `/health` to the container and passes the bearer token through. The demo
   token is a Wrangler secret.
3. Wait for `/health` through the Worker; the MCP base URL is a required workflow input
   (Cloudflare Workers URL or custom domain; it is not discoverable without credentials).
4. Auto-seed exactly as above, from the workflow runner (the server's admin listener is
   reachable through its existing Worker at `public_admin` with the management key).
5. Same readiness gate. Cloudflare demos require the server to use external Postgres with a
   persistent `SWITCHBOARD_SECRETS_KEY`; the shipped `/tmp` SQLite profile is ephemeral and
   is not a supported demo target.

Server deployments stay clean-room on both platforms: the existing Cloud Run pipeline and
`make deploy-cloudflare` do not change their inputs except for removing demo bits.

### D6 — Demo seed runs against a clean server

The seed client is the only thing that knows a demo exists. It runs from the deployment
workflow/operator machine, not inside the server. It writes the demo MCP token into the
server's encrypted secret store and creates the MCP server with `auth_env` = that stored
secret name, so the clean server needs no demo environment.

Persistence: Cloud Run with Postgres and Cloudflare with external Postgres + a persistent
`SWITCHBOARD_SECRETS_KEY` keep seeded demos across restarts. The shipped Cloudflare profile
uses SQLite in `/tmp`; it is not a supported demo target. The Cloudflare demo deployment must
configure a Postgres DSN and the secrets key in the Worker/container env.

### D7 — Dev, e2e and docs

- `make dental-mcp` / `make atlas-mcp` run the moved binaries; `make seed DEMO=atlas` runs
  the seed client against the dev stack (secrets store is generated by `make dev`).
- The browser e2e stack starts the moved demo MCP fixtures, but does **not** seed globally:
  `05-first-run` needs an empty store and `50-screens` archives every agent. `60-dental` and
  `61-atlas` seed their own demo in `beforeAll` through `bin/seed` (idempotent), then assert
  the seeded agent, canvas, call path, tool calls and end reason. `DemoCard` and its UI tests
  are deleted.
- Docs: `demos/<name>/README.md` (moved from `docs/demo/`), a new `demos/README.md` for
  deploy/seed, and updates to README, quickstart, deployment (two-pipeline model), the
  Cloudflare plan, the API reference (remove the demo section), e2e README and agent-evals
  paths. The dental eval case moves under `demos/dental/eval/`.

## Failure paths

- Seed cannot reach the server or MCP service: fail with the URL and no secret; the next run
  wipes the partial state.
- Secret store off: fail before writing anything, naming `SWITCHBOARD_SECRETS_KEY`.
- MCP discovery missing a tool or schema changed: fail listing the tool; rerun after the
  fixture is healthy.
- Wipe blocked by active calls: `--wait` polls; without it, report the blocked resource and
  exit non-zero (nothing is left half-created that a rerun cannot fix).
- Wipe is by exact demo names; it does not inspect the resource's shape. A same-named MCP
  server or agent is treated as demo-owned. An HTTP tool with the same name and a different
  definition is a conflict: seed refuses without `--yes`, and with `--yes` updates the
  current definition (other agents that pinned it need review).
- Agent creation fails after resources exist: the next run wipes and retries.
- Cloudflare: Worker/container not healthy; MCP HTTP through the Worker fails; container
  restart before seeding. Pipeline fails at the health/seed step; rerun.
- Server restart with ephemeral SQLite: demo state is gone; re-run seed (documented).

## Security and privacy

- Management key and demo tokens are supplied through secrets; never logged or included in
  errors.
- Seed only touches demo-named resources; `--dry-run` lists actions; `--yes` is required for
  wipe-only.
- Demo MCP containers are public HTTPS with a bearer token (server egress accepts public
  destinations only). The MCP token is stored in the server's encrypted secret store.
- Cloudflare demo Workers expose only `/mcp`, `/api/*` and `/health`; no admin surface.
- Fixture data is fabricated; no recording; retention unchanged.

## Non-goals

- No port of demo MCP servers to TypeScript/Workers; they run as Go containers on Cloudflare.
- No agent hard-delete or call-history purge (retention/erasure handles calls).
- No persistent storage change for the server; Cloudflare persistence uses an external
  Postgres DSN.
- No generic resource labels/ownership metadata; ownership is exact names/prefixes.
- No demo state migration/update path.

## Acceptance criteria

- `server/` and the server image contain no demo installers, demo env vars or demo config;
  the only demo-named route is the fixed `410 demo_moved` compatibility shim. The server
  starts clean with an empty store.
- `demos/seed --demo dental|atlas` wipes and recreates the demo against a running server;
  running twice leaves one live demo and is race-clean; `--wipe-only` removes it.
- Demo MCP servers run from `demos/<name>`; local `make dental-mcp`/`atlas-mcp` and
  `make seed` work.
- The browser e2e suite seeds both demos through the CLI and passes the existing dental and
  atlas assertions (handoffs, MCP/HTTP tools, end_call, path, transcript).
- Demo deployment assets exist for Cloud Run and Cloudflare; workflow static tests cover the
  new manifests and paths; a deployment doc explains auto-seed inputs, token/management-key
  sources, the `SWITCHBOARD_SECRETS_KEY` prerequisite and persistence.
- A real integration test runs seed twice against a server + SQLite + MCP fixture and
  asserts one live, ready demo (not only a fake API); a static check proves `server/` does
  not depend on `demos/`.
- `make verify`, `make e2e-echo` and `make deadcode` pass; docs links and vocabulary guards
  pass.
- Evidence: screenshots of a seeded demo canvas/call from the e2e run and the seed command
  transcript.

## Self-review

- Boundaries: root module vs server module; no server import of demos; deploy assets paths.
- Reset semantics: wipe order (agent detach before tool delete), active-call blocking,
  archived agents, repeated runs, partial failure.
- Compatibility: `/api/v1/demo*` 410, config key removal, doc links, Cloud Run/Cloudflare
  workflows, Cloudflare ephemeral persistence.
- Security: management key/token handling, public demo containers, no admin exposure.
- Testability: seed unit tests, e2e seed path, deployment manifest tests, gate.
