# Cloudflare voice demo

This target runs the existing Switchboard Docker image in Cloudflare Containers and sends requests through a Worker and a single named Durable Object. It is for a low-volume demo and test calls, not a horizontally scaled production deployment.

## Requirements

- A Cloudflare Workers Paid account with Containers enabled.
- Wrangler 4.136.0 or newer and a Docker-compatible image builder.
- Provider credentials for a real-provider call. Echo calls need no provider secret.
- A strong `TOKEN_SECRET` and a distinct `GOVOICE_MANAGEMENT_API_KEY`, each at least 32 characters.
- A persistent `SWITCHBOARD_SECRETS_KEY` if the deployment runs demos: `k1:` plus the base64 of 32
  random bytes (for example `k1:$(openssl rand -base64 32)`). It enables the server's encrypted
  secret store, where the demo seed writes each demo MCP token.
- An external Postgres database (`GOVOICE_DATABASE_DSN`) if the deployment runs demos. The shipped
  SQLite file in `/tmp` is wiped on container restart, so a seeded demo would disappear; the demo
  target is not supported on the ephemeral profile.

The image must be built for `linux/amd64`. Wrangler uses `deploy/Dockerfile` with the repository root as the build context. The container uses `/etc/govoice/cloudflare-demo.yaml` with SQLite at `/tmp/switchboard.db` (or `/etc/govoice/cloudflare-demo-postgres.yaml` when `GOVOICE_DATABASE_DSN` is a `postgres://` URL), and a 10-minute maximum call duration.

## Configure secrets

From this directory, set the required Worker secrets:

```sh
npx wrangler secret put TOKEN_SECRET
npx wrangler secret put GOVOICE_MANAGEMENT_API_KEY
# Required when the deployment runs demos (the demo pipeline seeds through this Worker's public URL):
npx wrangler secret put SWITCHBOARD_SECRETS_KEY
npx wrangler secret put GOVOICE_DATABASE_DSN
```

Set only the provider secrets needed for the real-provider agent. For example:

```sh
npx wrangler secret put GOOGLE_API_KEY
npx wrangler secret put XAI_API_KEY
npx wrangler secret put OPENAI_API_KEY
npx wrangler secret put DEEPGRAM_API_KEY
npx wrangler secret put CARTESIA_API_KEY
npx wrangler secret put ELEVENLABS_API_KEY
npx wrangler secret put NVIDIA_API_KEY
```

`OPENAI_BASE_URL`, `NVIDIA_STT_FUNCTION_ID`, and `NVIDIA_TTS_FUNCTION_ID` can be added as Worker variables/secrets when needed. `GOOGLE_APPLICATION_CREDENTIALS` is a file path and cannot be populated by setting an API key. If using Google speech services that require service-account credentials, add a deliberate secret-to-file mounting mechanism before enabling that provider. Do not check credential JSON into this repository.

## Local container/Worker development

```sh
npm install --no-save wrangler
npx wrangler dev --config wrangler.jsonc
```

Use `.dev.vars` (ignored by Git) for the required local secrets. Local development requires Docker and may not reproduce Cloudflare's production network and WebSocket forwarding behavior.

## Deploy

```sh
npx wrangler deploy --config wrangler.jsonc
```

Wrangler builds the image for Cloudflare Containers and deploys the Worker/Durable Object binding. The Durable Object starts one `standard-2` container instance and enables outbound internet access for provider APIs.

## First-call checklist

1. Confirm `GET /health` returns `{"status":"live"}` through the Worker URL.
2. Open the embedded admin UI and authenticate with `GOVOICE_MANAGEMENT_API_KEY`.
3. Confirm `/metrics`, `/ready`, and `/debug/timeline` are not reachable through public ingress.
4. Create a one-step Echo agent in the UI and use **Test call** to validate WebSocket/RTVI/audio path without provider spend.
5. Configure one real-provider agent and run one call to validate outbound provider connectivity.
6. Check server and container logs for ONNX initialization errors, child-worker restarts, OOMs, or WebSocket failures.

The Worker rejects plain HTTP and cross-origin browser requests. It replaces forwarded proxy headers with Cloudflare-derived values before sending requests to the container. The server's trusted-proxy range is broad because container ingress is only available through the bound Durable Object; do not expose a separate public path to the container.

Container disk is ephemeral. SQLite data, created agents, and call history can disappear after restart, sleep, or redeploy. With an external Postgres DSN and a persistent `SWITCHBOARD_SECRETS_KEY`, agents, call history and seeded demos survive; that is the supported configuration for demos.

## Demos on Cloudflare (optional)

The server is clean-room: it installs no demo. A demo is a separate Worker + container, deployed and
seeded by the demo pipeline or by hand:

- `demos/dental/deploy/cloudflare/` and `demos/atlas/deploy/cloudflare/` each hold a Wrangler config,
  an amd64 `Dockerfile.mcp` for the demo MCP binary, and a Worker that proxies only `/mcp`, `/api/*`
  and `/health` to its container.
- The demo token is a Wrangler secret (`DENTAL_MCP_TOKEN` or `ATLAS_MCP_TOKEN`); it is never
  generated by the workflow and never stored in the repository.
- Set `SWITCHBOARD_SECRETS_KEY` and `GOVOICE_DATABASE_DSN` on this server Worker, then seed through
  its public URL with the management key and the demo token in `DENTAL_MCP_TOKEN`/`ATLAS_MCP_TOKEN`:
  `go run ./demos/seed --server <worker-url> --demo <name> --mcp <logical>=<demo-worker-url> --yes`
  (seed appends the demo's /mcp endpoint). `.github/workflows/deploy-demo.yml` automates all of
  this.
- A Cloudflare demo needs the external Postgres profile above; the ephemeral `/tmp` SQLite profile is
  not a supported demo target.

See [the implementation plan](../../docs/deployment-cloudflare-plan.md) for scope and acceptance criteria.
