# Switchboard e2e (Playwright)

Full-stack browser tests against real providers. Global setup builds and starts
the whole stack; global teardown stops it (also when setup fails).

## Run

```sh
GOVOICE_LIVE=1 make e2e                      # from the repo root: install + run everything (spends real Google credit)
cd e2e && npx playwright test tests/10-full-stack.spec.ts   # the full scenario only
npx playwright test tests/00-stack.spec.ts   # smoke: stack + admin UI login
```

Needs: Go toolchain (`make build`), `ui/node_modules` and
`examples/rtvi_client/node_modules` installed, and Google secrets (env or
`examples/.env`):

- `GOOGLE_APPLICATION_CREDENTIALS`: service-account JSON (path, relative to
  `examples/`), for speech (STT + TTS). Streaming Speech-to-Text v2 does not
  accept API keys.
- `GOOGLE_API_KEY`: Gemini API key, for the LLM (service accounts often lack
  Vertex AI `aiplatform.endpoints.predict`).

`E2E_GOOGLE_SECRET` / `E2E_GOOGLE_LLM_SECRET` override either. Global setup hands
them to the server process as `GOOGLE_APPLICATION_CREDENTIALS` (the JSON written to
a mode-600 temp file) and `GOOGLE_API_KEY`; no other provider variable reaches it,
and no secret goes through the management API or the browser. Without a Google
secret the stack is not started and every test is skipped.

## Scenario (`tests/10-full-stack.spec.ts`)

1. Admin UI: the Providers page shows Google detected (and no secret in the
   page); create flow "E2E Concierge" from the catalog dropdowns (`chirp_2`,
   `gemini-2.5-flash`, `en-US-Chirp3-HD-Aoede`), check it is `ready` and the
   stored config names no credential.
2. Admin debug client: voice call (the fake mic asks "What is two plus two?",
   bot answers "4"), then mute and a typed text turn ("capital of France" ->
   "Paris"), then disconnect.
3. Custom client (`examples/rtvi_client`): calls the flow by `flow_id`, voice
   and text turns.
4. Admin UI: delete the flow; it leaves the list and a new voice token for it
   is refused (409).

## Stack

| Process | URL | Log |
|---|---|---|
| `bin/server -vad-mode inprocess` (development profile, throwaway flow store in a temp `GOVOICE_DEV_DIR`) | public `http://127.0.0.1:18580` (WS `/ws`), admin `http://127.0.0.1:19590` | `test-results/logs/server.log` |
| Admin UI (Vite via `vite.e2e.config.mjs`, `/api` -> admin port) | `http://127.0.0.1:15173` | `test-results/logs/ui.log` |
| Custom client `examples/rtvi_client/server.js` | `http://127.0.0.1:13000` | `test-results/logs/custom-client.log` |

Ports avoid 8080/9090/3000/5173. Random `GOVOICE_MANAGEMENT_API_KEY` and
`TOKEN_SECRET` are generated per run. Endpoints, keys, temp dirs and PIDs are in
`e2e/.stack.json` while the run is live (git-ignored, mode 600); specs get them
via the `stack` fixture:

```ts
import { test, expect } from "../lib/stack";
import { step } from "../lib/evidence";

test("…", async ({ page, stack }) => {
  await page.goto(stack.uiUrl);
  await step(page, "admin UI opened"); // -> evidence/01-admin-ui-opened.png
});
```

## Fake microphone

Chromium runs with `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream
--use-file-for-fake-audio-capture=fixtures/voice-debug.wav` and microphone
permission granted.

- `fixtures/voice-debug.wav`: "What is two plus two?"

16 kHz mono PCM16 from macOS `say`, with 1 s silence before and 20 s after.
Regenerate with `scripts/make-fixtures.sh` (needs `say` and `ffmpeg`).
**Chromium loops the file** for as long as capture is open; the 20 s of silence
lets the bot answer before the question repeats (a repeat is a barge-in).

## Evidence

- `evidence/NN-<slug>.png`: full-page screenshot per `step()` call, in order
  (emptied at the start of each run); also attached to the HTML report.
- `playwright-report/`: HTML report (`npm run report`), the evidence bundle.
- `test-results/`: per-test screenshots, plus video and trace on failure, and
  `logs/*.log` (build, server, ui, custom-client).

## Flow suite: fake providers or live xAI

`playwright.flows.config.ts` runs `tests-flows/` against a stack it starts itself, on its
own ports (public 18581, admin 19591, UI 15174), so it can coexist with the paid Google suite.

| | `make e2e-fake` (default) | `GOVOICE_LIVE=1 make e2e-xai` |
|---|---|---|
| Server | `bin/server-echo` (`-tags xaispeech`, config `providers.echo`) | `bin/server-live` (`-tags xaispeech`) |
| Providers | the simulated `echo` provider (real Google processors, simulated backends) | real xAI STT, Grok, TTS |
| Cost | none, about 20 s | about 20 provider requests |
| Route | the fake LLM calls the transfer tool on "transfer to `<step>`" | the model calls it from the front prompt |
| Assertions | exact (the fake labels each reply with its step's prompt) | marker words (the sales prompt demands "catalogue") |

The xAI key is `XAI_API_KEY` from the environment or `examples/.env`; global setup passes it to
the server process as `XAI_API_KEY` and nothing else. The fake stack gets placeholders
(`GOOGLE_API_KEY=fake-google-key` and a stub service-account file) so the catalog detects
Google. Models and voices are the catalog defaults, except the fake's own (`fake-llm`,
`frontvoice`, `salesvoice`), entered through "Other…". The key never goes through the
management API or the browser, and live runs keep no traces. Without a key every live test
is skipped.

The specs run in file order on one stack (one worker): later files use what earlier ones left
behind. Shared helpers are in `lib/flows-helpers.ts` (admin API, navigation, the editor, the Test
call panel) and `lib/mcp-helpers.ts` (the MCP test servers).

| Spec | What it covers |
|---|---|
| `10-providers.spec.ts` | `GET /api/v1/providers` reports the suite's provider detected; the designer's dropdowns list only detected providers; the Providers page shows where the credential comes from and has no field for a key |
| `20-flow-lifecycle.spec.ts` | Draw a two-step flow with a route and publish it (STT only on the start step). A call: the front step answers, the call is routed to sales, the live flow view moves to Sales, the stored session has `route.*` spans with `from_step` and `to_step`, and the timeline shows the route cue (the `chime` sound the stack configures) starting in the gap and stopping on the sales step's first audio. Reopen, edit the route, republish (v2), a new call works. Fake only: a sales step whose voice cannot be built (`brokenvoice`) leaves the caller with the front step. A plain one-step flow is unchanged. The flows list (status, enable toggle, copy, archive) and the calls list and detail |
| `30-mcp.spec.ts` | The MCP tools page (empty state, the connect dialog, servers and a changed schema with the API faked), then the local test MCP servers (`cmd/mcptest`): healthy, failing and unreachable servers show their state, a flow calls an MCP tool during a test call, each failure kind is answered and the call goes on, a slow tool answers under the timeout and is abandoned over it. Fake only. Barge-in during a slow tool is a Go test (`server/mcp_call_e2e_test.go`): the fake STT produces no words, so a browser cannot start a user turn |
| `40-layout.spec.ts` | Dashboard, providers, MCP tools, calls, flows, the editor and a call detail do not scroll sideways at 390 px |
| `50-screens.spec.ts` | Walks the canvas boards 3 to 12 with a numbered screenshot of each, on a stack that already has flows |
| `60-dental.spec.ts` | Load demo from the Dashboard, a new-patient call and a returning-patient call with the real dentistry MCP server (`cmd/dental-mcp`): routing, MCP tool calls, the call ended by the flow; the Calls page shows the path, how it ended ("By the flow (...)") and the step-labelled transcript, and the tool-call spans are counted through the traces API (the page does not list individual tool calls). The fake LLM sends no transfer note, so the note is covered by Go unit tests, not here. Fake only |

The server-side twin of the fake suite is `server/flow_e2e_test.go` (`make test-e2e-go`): it also decodes the audio frames on the WebSocket, so the cue is checked as bytes on the wire (it plays in the gap, is paced at 20 ms, stops when sales speaks or the handoff fails, and a quick handoff or an unknown sound plays nothing). CI
runs the fake suite as the `graph-e2e` job (the name is a required status check, so it keeps its old name).

## One-port suite

`playwright.oneport.config.ts` (`make e2e-oneport`, also run by `make e2e-fake`) starts only the fake-provider
server with the UI embedded (`make ui-embed`) and `public_admin` on, on public 18582 and admin 19592 (stack file
`.stack-oneport.json`). `origin_allowlist` holds one unrelated origin, so only `origin_allow_same_host` admits
the browser. No Vite.

| Spec | What it proves |
|---|---|
| `10-oneport.spec.ts` | The embedded UI loads from the public origin and a client route survives a hard load; a text call works over the same-origin API and socket with no CSP violation; a foreign browser `Origin` is refused the socket (403); `/health` is public while `/metrics`, `/ready` and `/debug/timeline` are not; `server-config` needs the key, is `no-store` and reports the client address; a missing asset is 404; repeated wrong keys end in 429 and the page shows the server's message |
