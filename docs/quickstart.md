# Voice Framework V1 — Quick Start Guide

This guide gets you up and running with a real, live voice conversation using Voice Framework V1 and the official `@pipecat-ai/client-js` library.

---

## 1. Prerequisites

- **Go**: Version 1.27.1 or higher
- **Node.js**: Version 20+ (Node 22 LTS recommended)
- **Google Cloud Credentials**: Service account key with `Speech-to-Text API`, `Cloud Text-to-Speech API`, and `Vertex AI` (or Gemini API key `GEMINI_API_KEY`) enabled.
- **xAI API Key** (optional, for Scenario C): `XAI_API_KEY`

---

## 2. Clone and Build

```bash
git clone https://github.com/example/go-voice.git
cd go-voice

# Build all server and worker binaries as well as examples
make build
```

This generates:
- `bin/server`: Config-driven voice edge & pipeline server
- `bin/vadworker`: Isolated child worker process for Silero VAD
- `bin/bench`: Capacity and latency benchmarking harness

---

## 3. Configuration

In development, you can run with the provided development configuration:

```bash
cp config/development.yaml config/my-development.yaml
```

Set your credentials:
```bash
export GEMINI_API_KEY="your-gemini-api-key"
# Or for GCP Service Account:
# export GOOGLE_APPLICATION_CREDENTIALS="/path/to/sa-key.json"
```

Verify that `config/development.yaml` contains:
```yaml
environment: "development"
server:
  public_address: ":8080"
  admin_address: ":9090"
auth:
  secret: "dev-insecure-secret-key-at-least-32-chars-long"
  allow_missing_origin: true
```

---

## 4. Minting an Admission Token

Voice Framework V1 requires a short-lived single-use HMAC-SHA256 JWT admission token to connect.
You can mint a test token using Go or Node.js:

```bash
# Mint a token valid for 5 minutes using the development secret
node -e '
const jwt = require("jsonwebtoken");
const token = jwt.sign(
  {
    sub: "user-1234",
    aud: "govoice",
    iss: "govoice-auth",
    exp: Math.floor(Date.now() / 1000) + 300,
    iat: Math.floor(Date.now() / 1000)
  },
  "dev-insecure-secret-key-at-least-32-chars-long",
  { algorithm: "HS256" }
);
console.log(token);
'
```

---

## 5. Starting the Voice Server

Start the server using `bin/server`:

```bash
./bin/server -config config/development.yaml
```

The server initializes:
- Public listener at `http://127.0.0.1:8080` (WebSocket endpoint `/ws`)
- Admin listener at `http://127.0.0.1:9090` (`/health`, `/ready`, `/metrics`, `/timeline`)

Check server readiness:
```bash
curl http://127.0.0.1:9090/health
# {"status":"ok"}

curl http://127.0.0.1:9090/ready
# {"status":"ready","uptime":"2s","checks":{"admission":true,"circuits":true,"shutdown":false,"startup":true,"vad_capacity":true}}
```

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

const token = "<MINTED_ADMISSION_TOKEN>";
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

Voice Framework V1 includes runnable standalone examples in `examples/` that construct complete voice pipelines programmatically without the HTTP/WebSocket server:

```bash
# 1. Complete Google Voice Pipeline (Silero VAD + Google STT + Gemini LLM + Cloud TTS)
go run ./examples/google_pipeline

# 2. Scenario C Mixed Pipeline (Google STT + xAI Grok LLM + Cloud TTS)
go run ./examples/xai_pipeline

# 3. Custom Application Processor (Filter / Audit / Logging)
go run ./examples/custom_processor
```
