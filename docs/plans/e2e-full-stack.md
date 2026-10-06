# Full-stack end-to-end test (Playwright)

Goal: one automated run that exercises the whole product with real providers and
captures screenshot evidence of every step.

Scenario:
1. Start the stack: `bin/server` (development profile, throwaway agent store),
   the admin UI (Vite) and the custom web client (`examples/rtvi_client`).
2. Admin UI: add a Google provider credential, create a complete agent attached
   to it, confirm it is `ready`.
3. Admin debug client (agent voice test): a **voice** call (pre-recorded speech fed
   as the browser microphone) and a **text** call; verify transcript, assistant
   reply and audio playback.
4. Custom client: call the same agent by `agent_id`; verify the reply.
5. Delete (archive) the agent in the admin UI; verify it is gone from the list
   and that new tokens for it are refused.

Framework: **Playwright** (`@playwright/test`) in a new `e2e/` package. Chromium is
started with `--use-fake-device-for-media-stream --use-file-for-fake-audio-capture=<wav>`
so the voice call hears real speech generated with macOS `say`. Every step calls
`page.screenshot()` into `e2e/evidence/`; traces and video are kept on failure;
the HTML report is the evidence bundle.

Gaps found during planning (must be built first):
- The server accepts no **text** turns: the RTVI handler only handles
  `client-ready`/`disconnect`. Add RTVI `send-text` → a committed user turn that
  bypasses STT.
- The debug client has no text input.
- The custom client still selects YAML **pipelines** (removed): move it to
  `agent_id` tokens issued by the management API.
- Agents can only be archived (session history keeps a foreign key to the agent),
  so "delete" in the UI/test is archive.

## Tasks
| ID | Task |
|---|---|
| E1 | RTVI `send-text`: inject a committed user text turn (no STT), tests |
| E2 | Debug client (admin voice test): text input + transcript of user/bot text, tests |
| E3 | Custom client: `agent_id` tokens via management API, text + voice, CLI and web |
| E4 | Admin UI: "Delete agent" (archive) with confirmation in the agent detail/list |
| E5 | `e2e/` Playwright package: config, stack start/stop (global setup), fake-mic WAVs, evidence dir |
| E6 | Scenario spec: credential → agent → voice + text debug calls → custom client call → delete, screenshots each step |
| E7 | `make e2e`, docs; run against real providers and collect evidence |
| E8 | Independent review |
