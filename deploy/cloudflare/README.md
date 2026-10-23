# Cloudflare voice demo

This target runs the existing Switchboard Docker image in Cloudflare Containers and sends requests through a Worker and a single named Durable Object. It is for a low-volume demo and test calls, not a horizontally scaled production deployment.

## Requirements

- A Cloudflare Workers Paid account with Containers enabled.
- Wrangler 4.136.0 or newer and a Docker-compatible image builder.
- Provider credentials for a real-provider call. Echo calls need no provider secret.
- A strong `TOKEN_SECRET` and a distinct `GOVOICE_MANAGEMENT_API_KEY`, each at least 32 characters.

The image must be built for `linux/amd64`. Wrangler uses `deploy/Dockerfile` with the repository root as the build context. The container uses `/etc/govoice/cloudflare-demo.yaml`, SQLite at `/tmp/switchboard.db`, and a 10-minute maximum call duration.

## Configure secrets

From this directory, set the required Worker secrets:

```sh
npx wrangler secret put TOKEN_SECRET
npx wrangler secret put GOVOICE_MANAGEMENT_API_KEY
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

Container disk is ephemeral. SQLite data, created agents, and call history can disappear after restart, sleep, or redeploy. The dental MCP is intentionally not enabled here; it will be deployed as a later milestone.

See [the implementation plan](../../docs/deployment-cloudflare-plan.md) for scope and acceptance criteria.
