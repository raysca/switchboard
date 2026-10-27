# Demos

Optional, self-contained demos for Switchboard. The server is clean-room: it deploys with an
empty agent store and contains no demo code or demo configuration. A demo is deployed only when
wanted, and seeded into a running server with its admin URL and management key.

| Demo | Shape | MCP fixture | Docs |
|---|---|---|---|
| Smile Dental | 3 steps, 2 handoffs, 7 tools | `demos/dental` | [dental/README.md](dental/README.md) |
| Atlas Travel Concierge | 8 steps, 32 handoffs, 18 MCP + 2 HTTP tools | `demos/atlas` | [atlas/README.md](atlas/README.md) |

## What a seed does

`demos/seed` is a management-API client. It **wipes and recreates** the demo:

1. Archive every live agent with the demo's exact name.
2. `detach-delete` every MCP server with the demo's exact name and wait for the row to go.
3. Delete the demo's stored token secret(s).
4. Write the token secret(s), create the MCP server(s), discover the tools, create/update the
   two demo HTTP tools, create the agent, and verify its readiness.

HTTP tools are never deleted: the server keeps every pinned definition, so the seed updates
the current definition by exact name and pins its new hash. If a same-named tool already has a
different definition, seed refuses unless `--yes` is passed (other agents that pinned it then
need review); a disabled tool is enabled. Agents are archived, never deleted;
call history is retained. Running seed twice leaves one live demo, and a failed run is repaired
by running it again.

## Run a demo locally

```bash
# terminal 1: the demo's MCP fixture
./dev demo-start dental    # :18767, token dev-dental-token
# or: ./dev demo-start atlas       # :18768, token dev-atlas-token

# terminal 2: the dev server (generates SWITCHBOARD_SECRETS_KEY and the management key)
./dev up

# terminal 3: wipe and seed the demo
./dev demo-seed dental     # or: ./dev demo-seed atlas
```

`./dev demo-seed` reads `.govoice-dev/dev.env` (remembered ports and management key) and the
fixture tokens, targets the loopback dev server, then runs `go run ./demos/seed`. The equivalent Make targets remain for CI/legacy
(`make dental-mcp`, `make dev`, `make seed DEMO=dental`). Without the dev stack:

```bash
export GOVOICE_MANAGEMENT_API_KEY=...       # management key of the target server
export DENTAL_MCP_TOKEN=...                 # the demo MCP bearer token
go run ./demos/seed \
  --server https://switchboard.example \
  --demo dental \
  --mcp scheduling=https://dental-mcp.example \
  --yes
```

The server must have its encrypted secret store enabled (`SWITCHBOARD_SECRETS_KEY`): seed
writes the demo MCP token there and the server reads it at call time. The clean-room server
therefore never carries a demo token in its environment.

## Deploy a demo

Demo deployments are optional and separate from the server deployment:

- **Cloud Run:** `demos/<name>/deploy/cloudrun/` (image, service manifest, deploy script).
- **Cloudflare:** `demos/<name>/deploy/cloudflare/` (Worker + container assets).
- **Pipeline:** `.github/workflows/deploy-demo.yml` deploys the demo MCP service(s), health
  checks them, then (when given the server admin URL and management key) auto-seeds the demo.
  The same inputs can be used to re-seed manually.

See [deployment.md](../docs/deployment.md) and
[deployment-cloudflare-plan.md](../docs/deployment-cloudflare-plan.md) for the two-pipeline
model, required secrets/IAM, persistence and teardown. Cloudflare demos require the server to
use external Postgres with a persistent secrets key; the shipped SQLite `/tmp` profile is
ephemeral and is not a supported demo target.

## Teardown

```bash
go run ./demos/seed --server ... --demo dental --wipe-only --yes
```

This archives the demo agent and disables/deletes its MCP servers and token secret; call
history remains under the archived agent. Do not delete HTTP tools by hand: their pinned
definitions are part of the server's immutable history.
