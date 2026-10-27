# Smile Dental front-desk demo

The larger, limit-testing example is the [Atlas Travel Concierge](../atlas/README.md): 8 steps, 32 handoffs, MCP and HTTP tools.

"Smile Dental" is a three-step voice agent that shows handoffs, shared context, tool use and call termination. A caller reaches the front desk, is handed off to a new-patient or returning-patient step, and the call ends when the agent says goodbye.

| Step | LLM | TTS | MCP tools | Job |
|---|---|---|---|---|
| **Front desk** (start) | Gemini | ElevenLabs | none | Greet, take the name, ask if new or returning, hand off. Answers opening hours. Can end the call. |
| **New patient** | xAI Grok | xAI | `register_patient`, `find_slots`, `book_appointment` | Greet by name, register, offer slots, book, confirm, end the call. |
| **Returning patient** | Gemini | Google Chirp 3 HD | `find_patient`, `list_appointments`, `find_slots`, `reschedule_appointment`, `cancel_appointment` | Find the patient, move or cancel the appointment, confirm, end the call. |

STT is Google (Chirp 2) for the whole call. The handoffs are plain-language conditions the model uses to call `transfer_to_new_patient` or `transfer_to_returning_patient`. Each transfer takes an optional `note` (at most 500 bytes) that is placed in the next step's greeting, so the name and intent survive even when the history is long.

Each step's `end_call` flag gives it the `end_call` tool; the server waits for the bot to finish speaking, then closes the call. The three steps all set it.

## What is seeded

`demos/seed --demo dental` wipes any previous Smile Dental demo and creates it fresh:

- MCP server **"Smile Dental scheduling"** at `<mcp-base>/mcp`, with its bearer token stored
  in the server's encrypted secret store as `demo-dental-mcp`;
- the agent **"Smile Dental front desk"** with all seven scheduling tools pinned.

The MCP fixture is `demos/dental/mcp/dentalmcp`, run as `demos/dental/cmd/dental-mcp`. It
holds fake patients and appointments in memory only and resets on restart (the seeded agent
stays; its tool calls simply see the seed data again).

A server deploy is clean-room and installs no demo. Seed the demo after the server is up (the
[deployment docs](../README.md#deploy-a-demo) cover Cloud Run and Cloudflare), or use the dev
stack below.

## Run it locally

Provider keys for Google, xAI and ElevenLabs go in `examples/.env` (see
[providers.md](../../docs/providers.md)). The xAI TTS voice needs the `xaispeech` build tag,
which `make build` and `./dev up` use.

Terminal 1, the MCP fixture (port 18767, token `dev-dental-token`):

```bash
./dev demo-start dental
```

Terminal 2, the dev stack. It generates `SWITCHBOARD_SECRETS_KEY` and the management key, and
does not need any demo environment variables:

```bash
./dev up
```

Terminal 3, wipe and seed the demo:

```bash
./dev demo-seed dental
```

Open the Admin UI printed by `./dev up`, sign in, open **Smile Dental front desk**, and use
**Test call** to talk to it. The demo no longer has a Dashboard install button; seeding is the
same operation locally and in a deployment.

## Acceptance script

1. New patient: say your name, say it is your first visit. The call hands off to the new-patient step (the voice changes), the agent greets you by name, books a slot, says goodbye and the call ends by itself.
2. Returning patient: say a name, ask to move an appointment. The call hands off, the agent finds and moves the appointment, says goodbye, and the call ends.
3. Calls page: both calls show the path (Front desk -> New patient / Returning patient), the transcript labelled by step, latencies, and "Ended by the agent" with the step.

## Limits

- The Calls page shows the path, the step-labelled transcript and how the call ended, and its per-turn timeline lists the tool calls (the "Show traces" toggle). The server stores a row per tool call (name and status, never arguments or results): `GET /api/v1/sessions/{id}/traces` lists them, and [agent evals](../../docs/operations/agent-evals.md) assert on them.
- An `end_call` turn with no spoken words at all waits the 15 s cap before hanging up on the Google, OpenAI and NVIDIA providers (xAI always sends a final reply frame, so it ends at once).
- Speech-to-text is Google for the whole call; ElevenLabs STT is registered but unused in this agent.
- Call history lives in the store. A persistent Postgres server keeps both the seeded demo and its history across restarts; an ephemeral SQLite store does not. Re-running the seed resets the demo (it does not migrate it).
- Keep recording off and retention short on any demo host: transcripts hold what callers say.
