# Standalone Pipecat RTVI Client App

This directory contains a standalone Pipecat RTVI client test driver and interactive web client for Voice Framework V1.

---

## 1. Quick Start (Automated CLI Test Drive)

Ensure the server is running (e.g. `./bin/server -config config/development.yaml`).

### Test Drive 1: Complete Google Pipeline
Runs end-to-end voice test using **Google Cloud STT (Chirp 2) -> Google Gemini 2.5 Flash -> Google Cloud TTS (Journey-F)**:
```bash
npm run test:google
# or
node cli.js --pipeline default
```

### Test Drive 2: Scenario C Mixed Pipeline (Google + xAI Grok)
Runs end-to-end voice test using **Google Cloud STT (Chirp 2) -> xAI Grok LLM -> Google Cloud TTS (Journey-F)**:
```bash
npm run test:xai
# or
node cli.js --pipeline xai
```

---

## 2. Interactive Browser Web UI

Launch the local web client:
```bash
npm start
# or
node server.js
```
Open **http://localhost:3000** in Chrome or Edge:
1. Select **Google** or **xAI Grok** pipeline from the dropdown.
2. Click **"Connect & Start Talking"**.
3. Speak into your microphone — the UI captures microphone audio at 16kHz mono PCM16, displays interim and final STT transcripts, streams response tokens from the LLM, and plays synthesized voice back through your speakers at 24kHz!
4. Interrupt the assistant while it is speaking to experience instantaneous barge-in.
