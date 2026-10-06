# Standalone Pipecat RTVI Client

A CLI test driver and an interactive web client that call a **stored agent** on a
Go Voice server over the RTVI WebSocket protocol.

Calls are authorised by a single-use voice token issued by the management API:

```
POST {admin}/api/v1/voice-tokens
Authorization: Bearer <management key>
{"principal": "...", "agent_id": "..."}
→ {"data": {"token": "...", "expires_at": "...", "scope": "voice:connect"}}
```

`404` means the agent is unknown, `409 agent_not_ready` means it is not ready to
take calls. The call then connects to `{public}/ws?token=<token>`. Tokens are
single-use, so every call fetches a new one.

```bash
npm install
```

---

## 1. CLI

```bash
export GOVOICE_MANAGEMENT_API_KEY=<management key>

# Voice turn: streams a 16kHz mono PCM16 WAV (default ../../assets/speech/clip1.wav)
node cli.js --agent <agent-id>
node cli.js --agent <agent-id> --audio path/to/utterance.wav

# Text turn (RTVI send-text, no STT)
node cli.js --agent <agent-id> --text "Hello, who am I talking to?"
```

| Option | Default | |
|---|---|---|
| `--agent <id>` | (required) | stored agent to call |
| `--admin <url>` | `http://127.0.0.1:9090` | management API base URL |
| `--ws <url>` | `ws://127.0.0.1:8080/ws` | public WebSocket URL |
| `--principal <p>` | `rtvi-cli-tester` | token principal |
| `--key <key>` | `$GOVOICE_MANAGEMENT_API_KEY` | management API key |
| `--text "<msg>"` | | send a text turn instead of audio |
| `--audio <wav>` | `../../assets/speech/clip1.wav` | audio for the voice turn |
| `--timeout <ms>` | `20000` | how long to wait for the reply |

The CLI prints transcripts and the streamed assistant text, and exits `0` once a
bot reply completes (the bot stops speaking, or 3s pass without new LLM text for
agents that do not speak). It exits non-zero on a token refusal, a server error
or a timeout.

npm shortcuts: `npm run call -- --agent <id>`, `npm run call:text -- --agent <id>`.

---

## 2. Web client

```bash
npm start                      # http://127.0.0.1:3000
PORT=3100 npm start            # another port
HOST=0.0.0.0 npm start         # listen on all interfaces
```

Open the page in Chrome or Edge, fill in **Admin URL**, **Management key**,
**Agent ID** (and **Server WebSocket URL** if it is not the default), then click
**Connect**. The page asks `server.js` for a token (`POST /api/voice-token`), which
relays the request to the management API so the browser needs no CORS access to
it; the key is only ever typed into the page.

Once connected, speak into the microphone (16kHz mono PCM16 up, 24kHz playback
down, barge-in supported) or type into **Message** and click **Send text**.

Test hooks: `data-testid="connection-status"` (`Disconnected` / `Connecting...` /
`Connected` after `bot-ready`), `data-testid="connection-error"`, and transcript
entries `data-testid="transcript-user"` / `data-testid="transcript-bot"`.
