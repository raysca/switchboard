# go-voice e2e (Playwright)

Full-stack browser tests against real providers. Global setup builds and starts
the whole stack; global teardown stops it (also when setup fails).

## Run

```sh
make e2e                                     # from the repo root: install + run everything
cd e2e && npx playwright test tests/10-full-stack.spec.ts   # the full scenario only
npx playwright test tests/00-stack.spec.ts   # smoke: stack + admin UI login
```

Needs: Go toolchain (`make build`), `ui/node_modules` and
`examples/rtvi_client/node_modules` installed, and Google secrets (env or
`examples/.env`):

- `GOOGLE_APPLICATION_CREDENTIALS`: service-account JSON (path, relative to
  `examples/`), stored as the speech credential (STT + TTS). Streaming
  Speech-to-Text v2 does not accept API keys.
- `GOOGLE_API_KEY`: Gemini API key, stored as the LLM credential (service
  accounts often lack Vertex AI `aiplatform.endpoints.predict`).

`E2E_GOOGLE_SECRET` / `E2E_GOOGLE_LLM_SECRET` override either. Without a Google
secret the stack is not started and every test is skipped.

## Scenario (`tests/10-full-stack.spec.ts`)

1. Admin UI: add both Google credentials (masked, never echoed), create agent
   "E2E Concierge" (STT/TTS on the service account, LLM `gemini-2.5-flash` on
   the API key), check it is `ready` and each stage saved the right credential.
2. Admin debug client: voice call (the fake mic asks "What is two plus two?",
   bot answers "4"), then mute and a typed text turn ("capital of France" ->
   "Paris"), then disconnect.
3. Custom client (`examples/rtvi_client`): calls the agent by `agent_id`, voice
   and text turns.
4. Admin UI: delete the agent; it leaves the list and a new voice token for it
   is refused (409).

## Stack

| Process | URL | Log |
|---|---|---|
| `bin/server -vad-mode inprocess` (development profile, throwaway agent store in a temp `GOVOICE_DEV_DIR`) | public `http://127.0.0.1:18580` (WS `/ws`), admin `http://127.0.0.1:19590` | `test-results/logs/server.log` |
| Admin UI (Vite via `vite.e2e.config.mjs`, `/api` -> admin port) | `http://127.0.0.1:15173` | `test-results/logs/ui.log` |
| Custom client `examples/rtvi_client/server.js` | `http://127.0.0.1:13000` | `test-results/logs/custom-client.log` |

Ports avoid 8080/9090/3000/5173. Random `GOVOICE_MANAGEMENT_API_KEY` and
`TOKEN_SECRET` are generated per run. Endpoints, keys, temp dirs and PIDs are in
`e2e/.stack.json` while the run is live (git-ignored, mode 600); specs get them
via the `stack` fixture:

```ts
import { test, expect, fakeMic } from "../lib/stack";
import { step } from "../lib/evidence";

test("…", async ({ page, stack }) => {
  await page.goto(stack.uiUrl);
  await step(page, "admin UI opened"); // -> evidence/01-admin-ui-opened.png
});
```

## Fake microphone

Chromium runs with `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream
--use-file-for-fake-audio-capture=fixtures/voice-debug.wav` and microphone
permission granted. Switch the WAV per spec file with
`test.use(fakeMic("voice-custom.wav"))`.

- `fixtures/voice-debug.wav`: "What is two plus two?"
- `fixtures/voice-custom.wav`: "What is the capital of France?"

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
