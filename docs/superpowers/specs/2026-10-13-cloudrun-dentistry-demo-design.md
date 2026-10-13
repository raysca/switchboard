# Cloud Run production deployment and the dentistry front-desk demo

Status: draft for review. Date: 2026-10-13.

## Goal

A production build of go-voice running on **Google Cloud Run**, deployed by a **GitHub Action**, with **Google, xAI and ElevenLabs** as providers and **SQLite** as the store, running one demo that exercises handoffs, shared context, tool use and call termination: a dentistry front desk. The demo is driven from the admin UI (Test call) and every call is inspected in the admin UI (Calls: path, step-labelled transcript, tool calls, latencies, how it ended).

## The demo

"Smile Dental" is one flow of three steps, each on a different provider mix so a handoff is audible and all three providers are in the call:

| Step | STT | LLM | TTS | MCP tools | Job |
|---|---|---|---|---|---|
| **Front desk** (start) | Google (call-level) | Gemini | ElevenLabs voice | none | Greet, take the caller's name, ask whether they are new or already a patient, route. Can answer opening hours and end the call. |
| **New patient** | (call-level) | xAI Grok | xAI voice | `register_patient`, `find_slots`, `book_appointment` | Greet by name, take phone and reason, offer slots, book, confirm, say goodbye, end the call. |
| **Returning patient** | (call-level) | Gemini | Google Chirp 3 HD voice | `find_patient`, `list_appointments`, `find_slots`, `reschedule_appointment`, `cancel_appointment` | Greet by name, find them, move or cancel the appointment, confirm, say goodbye, end the call. |

Routes: Front desk -> New patient ("has never been a patient here and wants to register or book"); Front desk -> Returning patient ("is already a patient and wants to change, move or cancel an appointment").

**Acceptance script** (what "done" means):
1. Merge to `main`: the workflow builds, deploys the dentistry MCP and the voice server, the demo installs itself, smoke-tests, and prints the URL.
2. Open the URL, enter the management key: the Flows page lists "Smile Dental front desk"; Providers shows Google, xAI and ElevenLabs detected.
3. New patient call: give a name ("Sam Taylor"), say it is the first visit. The call is routed (voice changes), the new step greets "Sam", books a slot (MCP tool spans), says goodbye and the call ends by itself.
4. Returning patient call ("Maria Lopez", move Thursday's cleaning): routed, greeted "Welcome back, Maria", the appointment is found and moved, goodbye, the call ends.
5. Calls page: both calls listed with path (Front desk -> New patient / Returning patient), the transcript labelled by step, the MCP tool calls with durations, per-turn latencies, and "Ended by the flow (End call, step ...)".
6. Redeploying (or a restarted instance) brings the demo back without manual steps.

## Decisions made

- **Admin access:** one Cloud Run service. A new explicit production setting serves the built admin UI and `/api/v1` on the public port, protected by the management key over HTTPS.
- **MCP:** a separate Cloud Run service, in-memory seeded data, bearer-token protected, deployed by the same workflow.
- **Google auth:** a service-account key in Secret Manager mounted as a file (`GOOGLE_APPLICATION_CREDENTIALS`); no new code.
- **Starting point on your side:** a GCP project with billing and the provider keys exist. A GitHub repository with Actions does not yet; the plan creates it.

## What exists today (checked)

- Production Dockerfile with ONNX Runtime, the VAD and Smart Turn workers, `-tags xaispeech`; `config/production.yaml` (Postgres, loopback admin port, `trusted_proxy_mode`).
- All three providers registered (`google.*`, `xai.*` with the tag, `elevenlabs.*`); the catalog detects them from environment variables.
- Flows with steps and routes; handoff shares one conversation history across steps; a generic route greeting; MCP over HTTPS with `auth_env`; the admin UI is origin-relative (`/api/v1`, `/ws`), so it works unchanged when served from the same origin; Calls, call detail, traces and route spans.
- CI workflows (`ci.yml`, `live.yml`, `perf-gate.yml`); no remote repository.

## Gaps to close (the work)

### A. Product features for the demo

1. **End call (termination).** Today a call ends only by idle timeout or hang-up. Add a built-in `end_call` tool, enabled per step (`end_call: true` in the step config, shown in the step panel). The model calls it after its goodbye; the server waits until the bot has finished speaking (the same idle rule route flips use), then ends the call with a normal close. The stored session records `end_reason` (`ended_by_flow` with the step), a `call.end` trace span, and the Calls UI shows "Ended by the flow (step)". Bounded wait (MaxWait) so a stuck bot cannot hold the call open.
2. **Transfer note (explicit shared context).** The transfer tools take no arguments today, so context passes only through the shared history. Add an optional `note` argument (bounded, <= 500 bytes) to each transfer tool; its text goes into the new step's greeting message ("The caller was just transferred to you by Front desk. Note from the previous step: ..."), so the next agent has the name and intent even if the history is long or summarised. History sharing stays as is.
3. **Dentistry MCP server** (`cmd/dental-mcp`, package `server/mcp/dentalmcp`, built on the go-sdk already used): tools `find_patient(name, phone?)`, `register_patient(name, phone)`, `list_appointments(patient_id)`, `find_slots(from_date, days, kind)`, `book_appointment(patient_id, slot_id, kind)`, `reschedule_appointment(appointment_id, slot_id)`, `cancel_appointment(appointment_id)`. In-memory data seeded relative to today (a handful of patients, appointments over two weeks, slots every day), mutex-protected, bounded, bearer token from `DENTAL_MCP_TOKEN`, `/health`. Fixed fake patients only; no real data.
4. **Demo installer.** One server-side function (`server/demo`) embeds the Smile Dental flow definition and the MCP server entry (endpoint from `DENTAL_MCP_URL`, `auth_env` `MCP_TOKEN_DENTAL`). It is idempotent: it creates or updates the MCP server, syncs its tools, and creates or updates the flow by name. Two triggers, same code: an admin **"Load demo" button** (Dashboard, only shown when `DENTAL_MCP_URL` is set) calling `POST /api/v1/demo/dental`, and an optional run at startup (`demo.auto_install: dental`, set in `config/cloudrun.yaml`). Prompts are written for the three roles and tried with the real providers locally before deploy.

### B. Production server on one port

5. **Admin on the public port.** Config `public_admin: { enabled: true }` (production refuses it unless the management key is at least 32 bytes and `trusted_proxy_mode` is on). The public listener then also serves `/api/v1/*` through the existing management boundary and the embedded admin UI at `/` (single-page fallback), never `/metrics` or `/debug/timeline`. API responses `Cache-Control: no-store`; the UI gets a strict CSP. A startup WARN states that the management API is public.
6. **Failed-auth throttling.** Per-client-IP limit on failed management authentication (for example 10 per minute, then 429 for a minute), keyed on the proxy-trusted client IP; successes unaffected. Counters in `/metrics`.
7. **Embed the admin UI.** `server/adminui` embeds `ui/dist` (`//go:embed all:dist`, a committed placeholder so plain `go build` works); the Dockerfile gains a Node build stage that builds the UI into it. Tests: the UI is served, unknown paths fall back to `index.html`, `/api/` and `/ws` are never shadowed.
8. **Same-origin WebSocket.** `origin_allow_same_host: true` accepts an Origin whose host equals the request Host (the standard same-origin rule), so no per-deployment allowlist is needed for the Cloud Run URL.
9. **Cloud Run config.** `config/cloudrun.yaml`: profile production, public listener on `$PORT` (8080), SQLite at `/tmp/govoice.db` (via `GOVOICE_DATABASE_DSN`), admin listener on loopback, probe listener off, recording off, `trusted_proxies` for Cloud Run's front end (verified empirically on the first deploy; see Risks), limits sized for one small instance (`max_sessions` 20).
10. **Demo on start.** `demo.auto_install: dental` runs the demo installer once the server is ready, so an ephemeral SQLite instance comes up with the flow already there; the button covers a restart or a failed first install. A failed install logs loudly and never stops the server.
11. **Image.** One Dockerfile (existing, extended with the UI stage); the dentistry MCP gets its own tiny image.

### C. Deployment by GitHub Actions

12. **GitHub repository.** Create a private repo, add the remote, push `main`, protect `main`, require the existing CI checks. Branch protection names the job `graph-e2e` (kept deliberately).
13. **GCP bootstrap** (`deploy/gcp/bootstrap.sh`, idempotent, run once by you with `gcloud`): enable Cloud Run, Artifact Registry, Secret Manager, IAM Credentials; create an Artifact Registry repo; a runtime service account (Secret Manager accessor only) and a deployer service account; a Workload Identity pool and provider restricted to this repository (no long-lived GCP key in GitHub); create secrets `XAI_API_KEY`, `ELEVENLABS_API_KEY`, `GOOGLE_SA_KEY` (the JSON), `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY`, `DENTAL_MCP_TOKEN` from your shell without echoing them. GitHub stores only variables (project id, region, provider, service-account email), never provider keys.
14. **Deploy workflow** (`.github/workflows/deploy.yml`): on push to `main` after CI, and manually. Steps: authenticate with Workload Identity; build and push both images; deploy `dental-mcp` first (capture its URL); deploy `govoice` with `--set-secrets` for the keys, the Google key mounted as a file, `DENTAL_MCP_URL`, min and max instances 1, CPU always allocated, 2 vCPU and 2 GiB, concurrency 20, request timeout 3600 s, second-generation environment, startup probe on `/health`, unauthenticated invoker (the app authenticates); then smoke tests.
15. **Smoke tests** (no provider spend): `/health`; the UI index; `GET /api/v1/providers` with the management key shows google, xai and elevenlabs detected; the MCP server is connected and lists its tools; the demo flow exists and is ready (installed on start); a bad management key is refused and throttled. A manual dispatch adds one short live call (paid) through a text turn.
16. **Docs** (`docs/deploy/cloudrun.md`): one-time setup, the secrets table, rotating a key, rolling back, cost notes and the limits of the demo (SQLite resets on restart; single instance).

### D. Demo hardening

17. **Local rehearsal on the image** with Docker Compose (UI, API, WebSocket on one port, demo install, both MCP flows) using real provider keys, before touching GCP.
18. **First Cloud Run deploy as a spike**: confirm WebSocket behaviour, trusted-proxy ranges, VAD worker memory, cold start time, and measure first-audio latency from a browser.
19. **Guardrails**: a Cloud Billing budget alert, `max_sessions`, per-principal limits, and a kill switch (scale the service to zero).
20. **Admin UI polish for the demo**: the "Load demo" button with its result (created or updated, MCP tools synced); call detail shows the end reason and the MCP calls; the flow editor shows the end-call toggle and the transfer-note behaviour in the route panel.

## Sequence

```
 P0  GitHub repo + push                      (you + one script)
 P1  A1-A4 product features  ------------\
 P2  B5-B11 one-port production server  ---> P3  C12-C16 deploy workflow ---> P4  D17-D20
        (independent of P1, parallelisable)      (needs both images)             rehearsal + spike + polish
```

P1 and P2 are independent and can proceed in parallel; P3 needs both images; P4 is the proof. Three implementation plans follow this spec: **(1) demo product features** (A), **(2) one-port production server** (B), **(3) deployment and demo hardening** (C, D).

## Testing

- Unit and Go end-to-end tests with the fake providers for every server change (end call, transfer note, public admin, throttling, UI serving, same-origin WebSocket, demo-installer idempotence); the dentistry MCP with `go test` over real MCP sessions; the existing `make verify`, `make e2e-free` gates stay green.
- Playwright: the flow editor's end-call toggle, the Calls page showing the end reason and MCP calls, and a full dentistry call against the fake providers and the real dentistry MCP.
- Live (paid, manual): both demo calls with Google, xAI and ElevenLabs, locally on the image, then on Cloud Run.
- Deploy: the smoke tests above run on every deploy.

## Risks and open items

- **Proxy trust on Cloud Run.** The TCP peer of Cloud Run requests is a Google-internal address, not a documented range. The first deploy logs the peers; `trusted_proxies` is set from that and the rate limits (handshake per IP, failed-auth throttle) depend on it. Wrong trust either lets a client spoof its IP or rate-limits everyone as one client.
- **Management API on the internet.** Its only defence is the key plus the throttle. Mitigations: key at least 32 random bytes, constant-time compare (existing), throttle, no key in URLs, rotate by redeploying. A later hardening step is IAP or a private ingress.
- **SQLite on tmpfs.** Counts against instance memory and resets on restart; demo-on-start rebuilds the demo; call history is lost on restart. Acceptable for the demo; Postgres is the next step.
- **Region.** Providers are in the US; one transatlantic hop is cheaper near the providers than three behind the user. Default `us-central1`; the region is a variable.
- **Realtime on Cloud Run.** Needs CPU always allocated, a request timeout of 3600 s (a call is capped at 30 minutes by config) and one instance for SQLite; no scale-out without Postgres.
- **ElevenLabs STT** is registered but unused in this flow (one STT per call: Google). Say so in the demo notes.
- **Provider cost** per demo call is small but unbounded without limits: guardrails (item 19) are not optional.
- **Patient data:** the MCP holds fake patients only; transcripts hold what the caller says: keep recording off and retention short on the demo.

## Non-goals

Postgres and multi-instance; telephony (PSTN) transports; a custom domain or IAP; real scheduling systems; Terraform (the bootstrap is a script; infrastructure-as-code can follow).
