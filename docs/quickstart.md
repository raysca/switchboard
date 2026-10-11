# Voice Framework V1 — Quick Start Guide

This guide gets you up and running with a real, live voice conversation using Voice Framework V1 and the official `@pipecat-ai/client-js` library.

---

## 1. Prerequisites

- **Go**: Version 1.27.1 or higher
- **Node.js**: Version 20+ (Node 22 LTS recommended)
- **Google**: a service-account JSON key file with `Speech-to-Text API`, `Cloud Text-to-Speech API` and `Vertex AI` enabled (STT, TTS and the LLM), optionally plus a Gemini API key (preferred for the LLM).
- **xAI API key** (optional): xAI can serve the LLM stage. xAI STT/TTS exist only in builds made with `-tags xaispeech`.
- `curl`, `openssl` and `jq` for the commands below.
- **ONNX Runtime 1.29.0**: not committed (ignored by version control), so a fresh clone needs `make ort` once (downloads the pinned release for your OS/arch). `make build` fails with that hint when it is missing. The Silero and Smart Turn models are tracked in `third_party/`.
- `make build`, `make test` and `make verify` build with `-tags xaispeech` (xAI STT/TTS), like the production image.

Provider keys come from the server's environment only: the server never stores them and no API accepts them. [providers.md](providers.md) lists every variable and what it unlocks.

> **Upgrading from a build with stored credentials:** delete `.govoice-dev` (or any development database): the schema changed in place; nothing is migrated. Then set the provider variables below and recreate your agents (without `credential_id`). Self-hosted NVIDIA NIM and Riva endpoints are not supported in this release: `nvidia` always uses the hosted NIM (`integrate.api.nvidia.com`) and NVCF Riva endpoints, and `providers.nvidia.allow_private` is removed (a config that still sets it fails to load). They come back as catalog environment fields when needed.

---

## 2. Clone and Build

```bash
git clone https://github.com/example/go-voice.git
cd go-voice

# Build all server and worker binaries as well as examples
make build
```

This generates:
- `bin/server`: Config-driven voice edge server that runs stored agents
- `bin/vadworker`: Isolated child worker process for Silero VAD

---

## 3. Configuration and Secrets

`config/development.yaml` works as-is. YAML holds only infrastructure: listeners, TLS, limits, VAD, the token issuer, provider infrastructure (Google project/location/stream quotas, xAI endpoints) and `storage.dialect`. Models, voices and prompts belong to stored agents; provider keys belong to the environment. A `pipelines:` section is rejected at startup.

The turn manager is on by default (see "Turn taking" below). Optional features (Smart Turn, idle, DTMF, recording, OTel, redaction, audio filters, failover) are off by default and listed as commented examples at the end of `config/development.yaml`; defaults and semantics per key are in the "Default" column of [pipecat-parity-matrix.md](pipecat-parity-matrix.md).

TTS streams by default. Two optional kill switches: `providers.google_tts.disable_streaming: true` serves Google voices with the unary request, and `providers.xai_tts.disable_streaming: true` does the same for xAI.

The agent store is required. Set its DSN, the management API key and the provider variables:

In the development profile you can skip the DSN: the server then creates `.govoice-dev/govoice.db`. Production always requires it.

```bash
mkdir -p data
# SQLite file (development.yaml uses storage.dialect: sqlite). For postgres use a postgres:// URL.
export GOVOICE_DATABASE_DSN="$PWD/data/govoice.db"
# Management API bearer key, at least 32 bytes.
export GOVOICE_MANAGEMENT_API_KEY="$(openssl rand -hex 32)"
# Provider keys (set the ones you use; see docs/providers.md for the full table).
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa-key.json  # Google STT, TTS (and LLM via Vertex AI)
export GOOGLE_API_KEY="your-gemini-api-key"                  # optional: Google LLM via the Gemini API
# export XAI_API_KEY="your-xai-api-key"                      # optional: xAI LLM, STT and TTS
```

| Variable | Unlocks |
|---|---|
| `GOOGLE_APPLICATION_CREDENTIALS` | Google STT, TTS, LLM (path to a service-account JSON file) |
| `GOOGLE_API_KEY` | Google LLM |
| `XAI_API_KEY` | xAI LLM, STT, TTS |
| `OPENAI_API_KEY` (+ optional `OPENAI_BASE_URL`) | OpenAI-compatible LLM |
| `NVIDIA_API_KEY` (+ `NVIDIA_STT_FUNCTION_ID`, `NVIDIA_TTS_FUNCTION_ID`) | NVIDIA LLM (and Riva STT, TTS) |
| `DEEPGRAM_API_KEY` | Deepgram STT |
| `CARTESIA_API_KEY` | Cartesia STT, TTS |
| `ELEVENLABS_API_KEY` | ElevenLabs STT, TTS |
| `MCP_TOKEN_<NAME>` | Bearer token of an MCP server (`auth_env`) |

To rotate a key, change the variable and restart the server.

---

## 4. Starting the Voice Server

```bash
./bin/server -config config/development.yaml
```

The server initializes:
- Public listener at `http://127.0.0.1:8080` (WebSocket endpoint `/ws`)
- Admin listener at `http://127.0.0.1:9090` (`/health`, `/ready`, `/metrics`, `/debug/timeline` and the management API `/api/v1/*`)

Check server readiness:
```bash
curl http://127.0.0.1:9090/health
# {"status":"live"}

curl http://127.0.0.1:9090/ready
# {"circuits":{"google_llm":"closed",...},"status":"ready"}
```

`/ready` is unready if the agent store is not enabled.

The remaining steps use the management API on the admin listener:

```bash
ADMIN=http://127.0.0.1:9090
AUTH="Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY"
```

---

## 5. Check Providers, Create an Agent, Issue a Token

### 5a. Check which providers are available

```bash
curl -fsS "$ADMIN/api/v1/providers" -H "$AUTH" | jq '.data[] | {id, detected, capabilities}'
```

A provider whose variables are not set reports each service with `"reason": "missing_env"` and the variable names in `missing_env`. Values are never returned.

### 5b. Create an agent

```bash
AGENT_ID=$(curl -fsS -X POST "$ADMIN/api/v1/agents" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{
    "name": "Quickstart agent",
    "config": {
      "stt": {"provider": "google", "model": "chirp_2", "language": "en-US"},
      "llm": {"provider": "google", "model": "gemini-2.5-flash", "system_prompt": "You are a friendly voice assistant. Keep answers short."},
      "tts": {"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"}
    }
  }' | jq -r .data.id)

curl -fsS "$ADMIN/api/v1/agents/$AGENT_ID" -H "$AUTH" | jq -r .data.readiness.state
# ready
```

Each stage names only a provider: creation fails with `400 agent_not_attached` if a stage has no provider, or its provider's variables for that service are not set (`missing_credential_<stage>`). If a variable is removed later, the agent becomes not ready and `readiness.missing_env` names the variable. To use xAI for the LLM, set `XAI_API_KEY`, restart, and use `"llm": {"provider": "xai", "model": "grok-2-1212", ...}`.

### 5c. Issue a voice token

```bash
TOKEN=$(curl -fsS -X POST "$ADMIN/api/v1/voice-tokens" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"principal\": \"user-1234\", \"agent_id\": \"$AGENT_ID\"}" | jq -r .data.token)
```

The token is single-use and lasts at most 60 seconds, so connect right away. An unknown agent returns 404. An agent that is not ready returns 409 `agent_not_ready`, with the readiness reasons in `error.details`. Each call pins the agent's current version, so later edits do not change a running call.

---

## 6. Connecting with the Pipecat Client

Install the official pinned client packages:

```bash
npm install @pipecat-ai/client-js@1.13.1 @pipecat-ai/websocket-transport@1.7.2
```

Connect and converse in your web or Node.js application:

```javascript
import { PipecatClient } from "@pipecat-ai/client-js";
import { WebSocketTransport } from "@pipecat-ai/websocket-transport";

const token = "<TOKEN_FROM_STEP_5C>";
const wsUrl = `ws://127.0.0.1:8080/ws?token=${token}`;

const transport = new WebSocketTransport({
  url: wsUrl,
  options: {
    recorderSampleRate: 16000, // 16kHz microphone capture
    playerSampleRate: 24000,   // 24kHz audio playback
  },
});

const client = new PipecatClient({
  transport: transport,
  enableMic: true,
  enableCam: false,
});

// Event listeners
client.on("bot-ready", () => {
  console.log("Connected! Bot is ready. Speak into your microphone.");
});

client.on("user-started-speaking", () => {
  console.log("User started speaking...");
});

client.on("user-transcription", (data) => {
  if (data.final) {
    console.log(`User: ${data.text}`);
  }
});

client.on("bot-llm-text", (data) => {
  process.stdout.write(data.text);
});

client.on("bot-started-speaking", () => {
  console.log("\nBot started speaking.");
});

client.on("bot-stopped-speaking", () => {
  console.log("Bot stopped speaking.");
});

// Connect to the session
await client.connect();
```

### Turn taking

The turn manager is on by default, and turns end the way Pipecat ends them. A user turn ends once all three hold:
- the caller has been silent for the VAD stop (`vad.stop_ms`, 200 ms) plus a resume window (`turn.speech_timeout`, 600 ms);
- a transcript exists;
- the transcript is final, or the STT's p99 latency has passed.

The p99 values are built in per provider (`core/turn/sttp99.go`: Google 1.57 s, xAI 2.14 s, Deepgram 0.35 s, ...). Override them with `turn.stt_p99` (per provider) or the agent's `stt_p99_ms`. A turn that never gets a transcript ends after `turn.no_transcript_timeout` (5 s).

Upgrading from a release with the turn manager off: `turn.manager: false` restores turns that end on the raw VAD stop, but then set `vad.stop_ms: 800` too. A 200 ms raw stop cuts callers off mid-sentence, and the server logs a warning at startup.

### Text turns

After `bot-ready`, a client can type instead of speak by sending the RTVI client
message `{"label":"rtvi-ai","type":"send-text","data":{"content":"..."}}`. The
content is trimmed and must be 1–4096 bytes of UTF-8. It enters the conversation
as a final `user-transcription`, and the reply opens with `bot-llm-started`,
streams as `bot-llm-text`, and ends with `bot-llm-stopped`. `bot-llm-stopped` is only sent when a reply
completes, not when it is interrupted or fails.

Each text turn starts an LLM request, so a connection may send 3 at once, then
one per second. A rejected text turn (too early, invalid, or over the rate) gets
a non-fatal `error` message, and the call stays open.

A client mutes the caller with `{"label":"rtvi-ai","type":"set-muted","data":{"muted":true}}`
and unmutes with `"muted":false`. While muted the server drops the caller's
audio (no turns, no STT, no barge-in) and the recording stores silence. A muted
client should still disable its mic track; the message makes the server agree.

For push-to-talk, send `"push_to_talk": true` in `client-ready` data. Then
`{"label":"rtvi-ai","type":"push-to-talk","data":{"pressed":true}}` on button
down and `"pressed":false` on release. Speech only starts a turn while the
button is held, pauses while holding do not end it, and release ends it.
Pressing and speaking while the bot talks barges in. A call that did not opt
in gets a non-fatal error for `push-to-talk`.
Push-to-talk calls ignore `turn.stop`, `turn.min_words` and the smart-turn
model, but `turn.mute` strategies still apply: with `always`, a press while
the bot talks does not interrupt it.

A backend can steer a live call with the management key: `POST /api/v1/sessions/$SESSION_ID/context` with `{"messages":[{"content":"the caller is on the billing page"}],"run":true}` adds an operator note and makes the agent respond when the call is quiet. `$SESSION_ID` is the voice token's `jti`; see [the API doc](api/v1.md).

### DTMF (keypad tones)

Off by default; enable per server under `dtmf:`. `detect: true` finds keypad
tones in the caller's audio (accepts tones of 40 ms or more). `aggregate: true`
turns key presses into one user turn (`DTMF: 1234#`), committed on `#` or after
`aggregate_timeout` (2s) of quiet; the first key interrupts the bot. Clients may
also send presses after `bot-ready` as
`{"label":"rtvi-ai","type":"dtmf","data":{"buttons":["1","2","#"]}}` (up to 32
keys, 10 keys/s with a 32-key burst). `send_tool: true` gives the LLM a
`send_dtmf` tool that plays tones toward the caller (IVR navigation). Digits
can be PINs or card numbers: they are conversation content (see
`docs/privacy.md`).

---

## 7. Running Independent Examples

Voice Framework V1 includes runnable standalone examples in `examples/` that construct complete voice pipelines programmatically without the HTTP/WebSocket server. Unlike the server, these examples read provider credentials from the environment (`GOOGLE_API_KEY` or `GOOGLE_APPLICATION_CREDENTIALS`, and `XAI_API_KEY`):

```bash
# 1. Complete Google Voice Pipeline (Silero VAD + Google STT + Gemini LLM + Cloud TTS)
go run ./examples/google_pipeline

# 2. Scenario C Mixed Pipeline (Google STT + xAI Grok LLM + Cloud TTS)
go run ./examples/xai_pipeline

# 3. Custom Application Processor (Filter / Audit / Logging)
go run ./examples/custom_processor
```
