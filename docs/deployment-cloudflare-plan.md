# Cloudflare voice server demo deployment plan

## Goal

Deploy one Switchboard voice-server instance on Cloudflare Containers and complete a browser test call through the Cloudflare Worker ingress. This first milestone covers the voice server only; MCP deployment and the Smile Dental agent follow separately.

The demo uses SQLite on the container's ephemeral filesystem. Agent configuration and call history may be lost when the container restarts, sleeps, or is redeployed. That is acceptable for this milestone.

## Target topology

```text
Browser / Pipecat client
        │ HTTPS / WebSocket
        ▼
Cloudflare Worker
        │ is sent to a named Durable Object
        ▼
Durable Object ── starts, health-checks, and forwards to ── Switchboard container
                                                              ├─ server :8080
                                                              ├─ vadworker child process(es)
                                                              └─ turnworker child process
```

The Worker forwards public HTTP and WebSocket traffic to the server's existing public listener on port 8080. The container image includes the server, VAD and Smart Turn worker binaries, ONNX Runtime, and model files. Build the image for `linux/amd64`, as required by Cloudflare Containers.

The initial setup uses one named Durable Object/container instance and a low session limit. The current Cloudflare Containers documentation demonstrates forwarding WebSocket upgrades through a Durable Object to a container: [WebSocket to container](https://developers.cloudflare.com/containers/examples/websocket/).

## Implementation stages

1. Add a Cloudflare Worker/Durable Object target, an amd64 image configuration, and a voice-only demo server profile with ephemeral SQLite. Disable dental MCP auto-install.
2. Deploy one container and validate health, the admin UI, management authentication, public listener boundaries, proxy metadata, and SQLite operations.
3. Create a keyless Echo agent and complete a browser call to verify WebSocket upgrade, RTVI setup, audio upload/playback, and teardown.
4. Configure one real-provider agent using deployment secrets and complete one call to verify provider egress and the full STT → LLM → TTS path.
5. Record the effective proxy configuration, instance sizing, startup behavior, logs, and call outcome. Plan dental MCP deployment as a separate follow-up.

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
- [ ] SQLite data can be written and read during the container lifetime; loss after restart is accepted and documented.

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

## Risks and constraints

- Cloudflare's Worker-to-container path differs from Cloud Run; verify trusted proxy and client-IP behavior from the deployed service.
- Cloudflare documents WebSocket forwarding, but Switchboard's binary Protobuf RTVI frames and long-lived calls require end-to-end validation.
- Container disk is ephemeral by default, so the SQLite demo store can disappear after sleep, restart, or redeploy. See the [Containers FAQ](https://developers.cloudflare.com/containers/faq/).
- The real-provider call validates DNS, TLS, HTTP/2 or WebSocket connectivity required by that provider. See [outbound traffic](https://developers.cloudflare.com/containers/configuration/outbound-traffic/).
- Cloudflare instance sizes differ from the current Cloud Run demo allocation. Begin with one-call sizing, record CPU/memory use, then adjust. See [instance types and limits](https://developers.cloudflare.com/containers/platform/limits/).
- Platform lifecycle events can stop and restart a container; this demo does not promise call continuity across those events.

## Out of scope

- Deploying the Smile Dental MCP or porting it to TypeScript/Hono.
- Persistent SQLite or Postgres storage.
- Multi-instance call distribution, horizontal scaling, or production capacity guarantees.
- Paid/live provider checks beyond the single configured test call.
- Core pipeline or provider changes unless deployment uncovers a concrete issue.
