# Cloudflare voice server demo deployment plan

## Goal

Deploy one clean-room Switchboard voice-server instance on Cloudflare Containers and complete a
browser test call through the Cloudflare Worker ingress, then optionally deploy and seed one demo
through a **separate demo Worker + container**.

The server is clean-room: it ships no demo MCP server, no demo environment and no demo installer. A
demo is deployed by the optional demo pipeline, exactly as on Cloud Run. The server deployment can
use the shipped SQLite store in `/tmp` (ephemeral) for a voice-only trial, but a deployment that runs
demos requires an external Postgres database (`GOVOICE_DATABASE_DSN`) and a persistent
`SWITCHBOARD_SECRETS_KEY`; the `/tmp` SQLite profile is not a supported demo target because a seeded
demo would disappear on the next container restart.

## Target topology

```text
Browser / Pipecat client
        │ HTTPS / WebSocket
        ▼
Cloudflare Worker (server)              Cloudflare Worker (demo, optional)
        │ is sent to a named             │ is sent to a named
        ▼                                ▼
Durable Object ── Switchboard container  Durable Object ── demo MCP container
        │                                   (demos/<name>/cmd/<name>-mcp)
        └── browser ingress                 └── /mcp, /api/*, /health
                    ▲
                    │ public HTTPS + management key (seed, from the workflow runner)
                    │
server ── public HTTPS + demo bearer token ──▶ demo MCP Worker/container
```

The server Worker forwards public HTTP and WebSocket traffic to the server's existing public listener
on port 8080. The container image includes the server, VAD and Smart Turn worker binaries, ONNX
Runtime, and model files. Build the image for `linux/amd64`, as required by Cloudflare Containers.
The demo Worker forwards only `/mcp`, `/api/*` and `/health` to a separate demo container built from
`demos/<name>/deploy/cloudflare/Dockerfile.mcp`; its demo bearer token is a Wrangler secret and the
worker passes it to the container.

The initial setup uses one named Durable Object/container instance per Worker and a low session
limit. The current Cloudflare Containers documentation demonstrates forwarding WebSocket upgrades
through a Durable Object to a container: [WebSocket to container](https://developers.cloudflare.com/containers/examples/websocket/).

## Implementation stages

1. Add a Cloudflare Worker/Durable Object target for the server, an amd64 image configuration, and a
   voice-only demo server profile with ephemeral SQLite. The server installs no demo.
2. Deploy one container and validate health, the admin UI, management authentication, public listener
   boundaries, proxy metadata, and SQLite operations.
3. Create a keyless Echo agent and complete a browser call to verify WebSocket upgrade, RTVI setup,
   audio upload/playback, and teardown.
4. Configure one real-provider agent using deployment secrets and complete one call to verify provider
   egress and the full STT → LLM → TTS path.
5. For a deployment that runs demos: set the external Postgres DSN and the persistent
   `SWITCHBOARD_SECRETS_KEY` on the server Worker (the Worker selects
   `deploy/cloudflare/cloudflare-demo-postgres.yaml` when the DSN is a `postgres://` URL), deploy the
   demo's Worker/container from `demos/<name>/deploy/cloudflare/`, then seed the demo with
   `demos/seed` (or run `.github/workflows/deploy-demo.yml` with `target: cloudflare`). The demo
   pipeline needs `CLOUDFLARE_API_TOKEN` and the account id, the per-demo token as a GitHub
   environment secret, the management key as a GitHub environment secret, and `mcp_base_url` (the
   demo Worker URL, which is not discoverable without credentials).
6. Record the effective proxy configuration, instance sizing, startup behavior, logs, and call
   outcome.

## Acceptance criteria

### Deployment and ingress

- [ ] Cloudflare deploy succeeds with the image built for `linux/amd64`.
- [ ] The server and required VAD/Smart Turn child processes start without missing-library/model errors.
- [ ] The Worker waits for a healthy container before forwarding requests.
- [ ] `GET /health` returns the server's live response through the deployed Worker.
- [ ] The embedded admin UI loads through the public hostname.
- [ ] Management API requests require the configured management key.
- [ ] `/metrics`, `/ready`, and `/debug/timeline` are not publicly served.
- [ ] Plain HTTP and cross-origin browser requests are rejected at the Worker.
- [ ] Server proxy detection reports HTTPS correctly and uses the Cloudflare-derived client IP.
- [ ] SQLite data can be written and read during the container lifetime; loss after restart is accepted
      and documented, and the ephemeral profile is not used for demos.

### Echo browser call

- [ ] A browser starts a call through the deployed UI.
- [ ] The client completes the WebSocket upgrade and RTVI ready/setup exchange.
- [ ] The server receives client audio frames without protocol or transport errors.
- [ ] The browser receives and plays the Echo agent's response audio.
- [ ] The call ends normally and releases its session resources.
- [ ] No worker restarts, container OOMs, or unexplained WebSocket closures occur during the call.

### Real-provider browser call

- [ ] The configured provider is detected without exposing its secret.
- [ ] A browser call completes STT, LLM response generation, TTS, and audio playback.
- [ ] Provider outbound connections succeed from the Cloudflare container.
- [ ] The call ends normally and is visible in call history for the container's lifetime.
- [ ] CPU and memory observations are recorded for the next sizing decision.

### Demo deployment and seed (optional)

- [ ] The demo Worker from `demos/dental/deploy/cloudflare/` or `demos/atlas/deploy/cloudflare/`
      deploys with the demo bearer token as a Wrangler secret, and its container answers `/health`
      through the Worker.
- [ ] `/mcp` without the bearer token is refused; `/api/*` is served only by the Atlas demo.
- [ ] The demo token is never generated by the workflow and never stored in the repository.
- [ ] With an external Postgres DSN and a persistent `SWITCHBOARD_SECRETS_KEY` on the server Worker,
      `demos/seed` creates the demo agent against the server's public URL with the management key;
      re-running the seed leaves one ready demo.
- [ ] The seeded demo survives a container restart when the Postgres profile is used, and the
      ephemeral SQLite profile is documented as unsupported for demos.
- [ ] `.github/workflows/deploy-demo.yml` prints the seed command with environment indirection only
      when `auto_seed` is off; no secret value appears in the log.

## Risks and constraints

- Cloudflare's Worker-to-container path differs from Cloud Run; verify trusted proxy and client-IP
  behavior from the deployed service.
- Cloudflare documents WebSocket forwarding, but Switchboard's binary Protobuf RTVI frames and
  long-lived calls require end-to-end validation.
- Container disk is ephemeral by default, so the SQLite store can disappear after sleep, restart, or
  redeploy, and a seeded demo does not survive it. Durable demos need the external Postgres profile.
  See the [Containers FAQ](https://developers.cloudflare.com/containers/faq/).
- The demo Worker URL is not discoverable without credentials, so the demo pipeline requires
  `mcp_base_url` as an input.
- The real-provider call validates DNS, TLS, HTTP/2 or WebSocket connectivity required by that
  provider. See [outbound traffic](https://developers.cloudflare.com/containers/configuration/outbound-traffic/).
- Cloudflare instance sizes differ from the current Cloud Run allocation. Begin with one-call sizing,
  record CPU/memory use, then adjust. See
  [instance types and limits](https://developers.cloudflare.com/containers/platform/limits/).
- Platform lifecycle events can stop and restart a container; this demo does not promise call
  continuity across those events.

## Out of scope

- Porting the demo MCP servers to TypeScript/Hono: they run as Go containers on Cloudflare.
- Persistent storage inside the Cloudflare container itself (an external Postgres DSN is used).
- Multi-instance call distribution, horizontal scaling, or production capacity guarantees.
- Paid/live provider checks beyond the single configured test call.
- Core pipeline or provider changes unless deployment uncovers a concrete issue.
