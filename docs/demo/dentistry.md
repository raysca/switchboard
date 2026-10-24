# Smile Dental front-desk demo

"Smile Dental" is a three-step voice agent that shows handoffs, shared context, tool use and call termination. A caller reaches the front desk, is handed off to a new-patient or returning-patient step, and the call ends when the agent says goodbye.

| Step | LLM | TTS | MCP tools | Job |
|---|---|---|---|---|
| **Front desk** (start) | Gemini | ElevenLabs | none | Greet, take the name, ask if new or returning, hand off. Answers opening hours. Can end the call. |
| **New patient** | xAI Grok | xAI | `register_patient`, `find_slots`, `book_appointment` | Greet by name, register, offer slots, book, confirm, end the call. |
| **Returning patient** | Gemini | Google Chirp 3 HD | `find_patient`, `list_appointments`, `find_slots`, `reschedule_appointment`, `cancel_appointment` | Find the patient, move or cancel the appointment, confirm, end the call. |

STT is Google (Chirp 2) for the whole call. The handoffs are plain-language conditions the model uses to call `transfer_to_new_patient` or `transfer_to_returning_patient`. Each transfer takes an optional `note` (at most 500 bytes) that is placed in the next step's greeting, so the name and intent survive even when the history is long.

Each step's `end_call` flag gives it the `end_call` tool; the server waits for the bot to finish speaking, then closes the call. The three steps all set it.

## What is installed

The demo installer (`server/demo`) is idempotent. It creates or updates an MCP server named "Smile Dental scheduling" (endpoint from `DENTAL_MCP_URL`, bearer token from `MCP_TOKEN_DENTAL`, never stored), syncs its tools, and creates the agent "Smile Dental front desk" if it is missing.

Three ways to trigger it, all the same code:

- **Load demo** button on the Dashboard. It appears only when `DENTAL_MCP_URL` is set. The API is `GET /api/v1/demo` (status) and `POST /api/v1/demo/dental` (install).
- **Config key `demo.auto_install: dental`** runs the installer once at startup. A failed install logs and never stops the server.
- The `make dev` stack does not auto-install; use the button.

The MCP server itself is `server/mcp/dentalmcp`, run as `cmd/dental-mcp`. It holds fake patients and appointments only.

## Run it locally

Provider keys for Google, xAI and ElevenLabs go in `examples/.env` (see [providers.md](../providers.md)). The xAI TTS voice needs the `xaispeech` build tag, which `make build` already uses.

Terminal 1, the MCP server (port 18767, token `dev-dental-token`):

```bash
make dental-mcp
```

Terminal 2, set the two variables the server reads, then start the dev stack. `make dev` runs the server as a child process, so it inherits them from this shell; `scripts/dev.sh` does not set them itself. The development profile already allows loopback HTTP for MCP (`mcp.allow_local_http: true` in `config/development.yaml`).

```bash
export DENTAL_MCP_URL=http://127.0.0.1:18767/mcp
export MCP_TOKEN_DENTAL=dev-dental-token
make dev
```

Then open the Admin UI URL printed by `make dev`, enter the management key, and click **Load demo** on the Dashboard. Open the Agents page to see "Smile Dental front desk", and use **Test call** to talk to it.

## Acceptance script

1. New patient: say your name, say it is your first visit. The call hands off to the new-patient step (the voice changes), the agent greets you by name, books a slot, says goodbye and the call ends by itself.
2. Returning patient: say a name, ask to move an appointment. The call hands off, the agent finds and moves the appointment, says goodbye, and the call ends.
3. Calls page: both calls show the path (Front desk -> New patient / Returning patient), the transcript labelled by step, latencies, and "Ended by the agent" with the step.

## Limits

- The Calls page shows the path, the step-labelled transcript and how the call ended, and its per-turn timeline lists the tool calls (the "Show traces" toggle). The server stores a row per tool call (name and status, never arguments or results): `GET /api/v1/sessions/{id}/traces` lists them, and [agent evals](../operations/agent-evals.md) assert on them.
- An `end_call` turn with no spoken words at all waits the 15 s cap before hanging up on the Google, OpenAI and NVIDIA providers (xAI always sends a final reply frame, so it ends at once).
- Speech-to-text is Google for the whole call; ElevenLabs STT is registered but unused in this agent.
- Call history lives in the store. Cloud Run uses Cloud SQL (Postgres), so history survives restarts; `demo.auto_install` (or the button) adds the demo server and agent only when they are missing; a demo MCP server you disabled is switched back on at each start.
- Keep recording off and retention short on any demo host: transcripts hold what callers say.
