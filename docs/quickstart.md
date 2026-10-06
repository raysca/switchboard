# Voice Framework V1 — Quick Start Guide

This guide gets you up and running with a real, live voice conversation using Voice Framework V1 and the official `@pipecat-ai/client-js` library.

---

## 1. Prerequisites

- **Go**: Version 1.27.1 or higher
- **Node.js**: Version 20+ (Node 22 LTS recommended)
- **Google credential**: either a Gemini/Google API key, or a service-account JSON key with `Speech-to-Text API`, `Cloud Text-to-Speech API`, and `Vertex AI` enabled.
- **xAI API key** (optional): xAI can serve the LLM stage. xAI STT/TTS exist only in builds made with `-tags xaispeech`.
- `curl`, `openssl` and `jq` for the commands below.

Provider credentials are not read from the environment. `GOOGLE_API_KEY`, `GOOGLE_APPLICATION_CREDENTIALS` and `XAI_API_KEY` are ignored by the server. You add credentials through the management API, and the server stores them encrypted.

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

`config/development.yaml` works as-is. YAML holds only infrastructure: listeners, TLS, limits, VAD, the token issuer, provider infrastructure (Google project/location/stream quotas, xAI endpoints) and `storage.dialect`. Models, voices, prompts and credentials belong to stored agents. A `pipelines:` section is rejected at startup.

The agent store is required. Set its DSN, the credential encryption keyring and the management API key:

In the development profile you can skip the first two variables: the server then creates `.govoice-dev/govoice.db` and a persisted credential key in `.govoice-dev/credential.key`. Production always requires both.

```bash
mkdir -p data
# SQLite file (development.yaml uses storage.dialect: sqlite). For postgres use a postgres:// URL.
export GOVOICE_DATABASE_DSN="$PWD/data/govoice.db"
# "id:base64(32-byte key)[,id:...]"; the first entry encrypts new secrets. Keep it: losing it makes stored credentials unreadable.
export GOVOICE_CREDENTIAL_KEYS="k1:$(openssl rand -base64 32)"
# Management API bearer key, at least 32 bytes.
export GOVOICE_MANAGEMENT_API_KEY="$(openssl rand -hex 32)"
```

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

## 5. Add a Provider Credential, Create an Agent, Issue a Token

### 5a. Add a Google credential

The secret is either an API key or a service-account JSON key (`"type": "service_account"`). Pretty-printed JSON is accepted; API keys must be a single token.

```bash
# API key:
SECRET="your-google-api-key"
# Or a service-account key:
# SECRET="$(jq -c . /path/to/sa-key.json)"

CRED_ID=$(jq -n --arg s "$SECRET" '{provider: "google", secret: $s}' |
  curl -fsS -X POST "$ADMIN/api/v1/providers/credentials" \
    -H "$AUTH" -H 'Content-Type: application/json' --data-binary @- | jq -r .data.id)
echo "$CRED_ID"
```

The response holds only a masked suffix. `GET /api/v1/providers/credentials` lists credentials. For xAI, post `{"provider": "xai", "secret": "<xai-api-key>"}`.

### 5b. Create an agent

```bash
AGENT_ID=$(curl -fsS -X POST "$ADMIN/api/v1/agents" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{
    "name": "Quickstart agent",
    "config": {
      "stt": {"provider": "google", "model": "chirp_2", "language": "en-US"},
      "llm": {"provider": "google", "model": "gemini-2.0-flash", "system_prompt": "You are a friendly voice assistant. Keep answers short."},
      "tts": {"provider": "google", "voice": "en-US-Journey-F"}
    }
  }' | jq -r .data.id)

curl -fsS "$ADMIN/api/v1/agents/$AGENT_ID" -H "$AUTH" | jq -r .data.readiness.state
# ready
```

Each stage must name its `credential_id`: an agent is attached to its providers and their credentials when it is created, and creation fails with `400 agent_not_attached` if a stage has no provider, no credential, or a credential of another provider. A credential cannot be deleted while an agent is attached to it (`409 credential_in_use` lists the agents); if it is marked invalid, the agent becomes not ready (`invalid_credential_<stage>`). To use xAI for the LLM, set `"llm": {"provider": "xai", "model": "<grok-model>", ...}` after adding an xAI credential.

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
