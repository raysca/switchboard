# Agent Voice Test and Pipecat Debug Panel

This document specifies the architecture, wire protocol integration, audio streaming pipeline, and diagnostic capabilities of the Agent Voice Test panel and Pipecat Debug Panel implemented in Task 22 (Phase 6).

## 1. Overview and Architecture

The voice test panel enables operators to test conversational voice agents directly within the Admin UI using the production RTVI WebSocket session path. It adheres strictly to the `@pipecat-ai/client-js@1.13.1` and `@pipecat-ai/websocket-transport@1.7.2` wire specification and the Go Voice Framework's binary protobuf protocol.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                       Agent Detail View (`AgentsView`)                      │
│                                                                             │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                    Live Voice Test (`AgentVoiceTest`)                 │  │
│  │  - Call controls: Start voice test, Disconnect, Mute/Unmute mic       │  │
│  │  - Live status indicator: CONNECTING, READY, USER SPEAKING, etc.      │  │
│  │  - Interactive transcript: Finalized turns, interim user speech,     │  │
│  │    streaming assistant responses                                      │  │
│  │  - Session summary with direct link to trace: `/sessions/{id}`        │  │
│  │                                                                       │  │
│  │  ┌─────────────────────────────────────────────────────────────────┐  │  │
│  │  │             Pipecat RTVI Debug Panel (`PipecatDebugPanel`)      │  │  │
│  │  │  - Speaking telemetry: User speaking, Bot speaking, Barge-in   │  │  │
│  │  │  - Latency metrics: WS connect latency, RTVI ready RTT          │  │  │
│  │  │  - Audio metrics: Frame counts, byte throughput, fps            │  │  │
│  │  │  - Event stream: Inbound/outbound RTVI messages with inspection │  │  │
│  │  └─────────────────────────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────┬───────────────────────────────────┘  │
└──────────────────────────────────────┼──────────────────────────────────────┘
                                       │
            ┌──────────────────────────┴──────────────────────────┐
            ▼                                                     ▼
┌───────────────────────────────┐             ┌───────────────────────────────┐
│ Voice Admission Token Request │             │  WebSocket Connection (`/ws`) │
│ POST /api/v1/voice-tokens     │             │  - Protobuf Frame envelope    │
│ - Principal & agent_id        │             │  - RTVI JSON messages         │
│ - Returns short-lived token   │             │  - 16kHz input / 24kHz output │
└───────────────────────────────┘             └───────────────────────────────┘
```

## 2. Session Lifecycle and Handshake

Voice testing runs through four deterministic stages:

1. **Token Minting (`POST /api/v1/voice-tokens`):**
   - The UI requests an admission token with `principal: "agent-test-${agent.id}"` and `agent_id: agent.id`. The server returns 409 `agent_not_ready` if the agent cannot take calls.
   - The token contains claims with the session's unique identifier (`jti`), the `agent_id` the call runs, and lifetime.
   - The token is passed as a query parameter when establishing the WebSocket connection (`/ws?token=${token}`). The call runs the agent's current version with its stored provider credentials.

2. **WebSocket Upgrade & RTVI Handshake:**
   - Client connects via WebSocket (`ws://` or `wss://`).
   - Client immediately transmits a binary protobuf `Frame` containing an RTVI `client-ready` envelope:
     ```json
     {
       "label": "rtvi-ai",
       "type": "client-ready",
       "data": { "version": "2.1.0" }
     }
     ```
   - Server validates protocol version `2.1.0` (or compatible `2.1`) and replies with `bot-ready`:
     ```json
     {
       "id": "msg-1",
       "label": "rtvi-ai",
       "type": "bot-ready",
       "data": { "version": "2.1.0" }
     }
     ```
   - Ready latency ($RTT$) is recorded from `client-ready` transmission to `bot-ready` reception.

3. **Active Conversational Turn Flow:**
   - **User Speech:**
     - Microphone captures 16kHz PCM16LE mono audio, sending 512-sample chunks (32ms) encoded as `Frame_Audio`.
     - Server VAD detects speech and emits `user-started-speaking`.
     - Server STT emits interim `user-transcription` updates (`final: false`), rendered in real-time italic styling.
     - Server VAD emits `user-stopped-speaking`, followed by finalized `user-transcription` (`final: true`).
   - **Assistant Response:**
     - Server LLM streams text deltas via `bot-llm-text` (`{ text: "..." }`).
     - Server TTS emits `bot-started-speaking` and begins streaming 24kHz PCM16LE mono chunks as `Frame_Audio`.
     - Client schedules gapless playback through the Web Audio `AudioContext`.
     - When playback completes, server emits `bot-stopped-speaking`.
     - Accumulated text is committed to the permanent transcript history.

4. **Barge-in (Interruption) Detection & Buffer Clear:**
   - When the user starts speaking while the assistant is speaking (`isBotSpeaking === true`), the client detects an interruption.
   - The client immediately calls `BrowserAudioController.stopPlayback()`, stopping scheduled AudioBuffer nodes and flushing buffered audio.
   - The UI increments the barge-in counter and lights up the active barge-in alert badge.

5. **Clean Disconnect and Session Summary:**
   - Operator clicks **Disconnect** or the server closes the call.
   - Client transmits a binary protobuf `Frame` with `disconnect-bot`.
   - Audio capture tracks and Web Audio contexts are released.
   - The session summary remains displayed on screen with the unique `sessionId` and a direct clickable link to the session trace timeline at `/sessions/{id}`.

## 3. Wire Protocol & Protobuf Serialization

All frames exchanged over `/ws` are encoded as binary Protocol Buffers based on `proto/frames.proto`:

- **Nested RTVI Message (`Frame_Message`):**
  - Wire tag `0x22` (field 4, length-delimited).
  - Contains `MessageFrame` with field 1 (`0x0A`, string) holding the RTVI JSON envelope (`label: "rtvi-ai"`).
- **Audio Chunks (`Frame_Audio`):**
  - Wire tag `0x12` (field 2, length-delimited).
  - Contains `AudioRawFrame` with:
    - field 2: `name` = `"audio"`
    - field 3: raw PCM16LE audio bytes
    - field 4: `sample_rate` (16000 for client mic, 24000 for server output)
    - field 5: `num_channels` = 1

The UI implements pure TypeScript varint encoding and protobuf serialization (`ui/src/voice/protocol.ts`) without third-party dependencies, verified directly against local server binary fixtures (`nested-rtvi.bin`, `client-audio.bin`, `server-audio.bin`).

## 4. Audio Architecture (`BrowserAudioController`)

The audio subsystem (`ui/src/voice/audio.ts`) handles device capture and playback:

- **Microphone Capture:**
  - Requests user media with hardware echo cancellation, noise suppression, and auto gain control:
    ```ts
    navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, sampleRate: 16000, channelCount: 1 }
    })
    ```
  - `ScriptProcessorNode` (512 samples) downsamples and converts Float32 audio to Int16 Little-Endian mono samples.
  - Mic mute toggle disables track transmission (`track.enabled = false`) without disconnecting the session.
- **Output Playback:**
  - Runs dedicated `AudioContext` configured for 24kHz playback.
  - Maintains `nextPlaybackTime` timestamp to ensure jitter-free, gapless chunk concatenation.
  - Supports instant `stopPlayback()` cancellation during barge-in.

## 5. Failure Modes & Graceful Handling

| Failure Condition | UI Behavior | Remediation Action |
|---|---|---|
| Microphone Permission Denied (`NotAllowedError`) | Displays warning alert: *"Microphone Access Denied: Microphone permission was denied. Please allow microphone access in your browser settings."* | Operator enables permission in browser bar and clicks **Retry Connection**. |
| Microphone Not Found (`NotFoundError`) | Displays warning alert: *"No microphone device was detected on your system."* | Operator connects audio device and retries. |
| Token Minting Failure | Displays error alert with server error details. | Operator verifies the management key and that the agent is ready (readiness reasons such as `missing_credential_llm`). |
| WebSocket Connection Timeout / Failure | Displays error alert: *"WebSocket connection failed. Verify server is reachable."* | Session resources are cleanly torn down; operator can click **Retry Connection**. |
| Unexpected Server Closure | Displays closure banner with close code and server reason. Session summary and trace link remain accessible. | Operator inspects trace timeline or reconnects. |

## 6. Pipecat RTVI Debug Panel (`PipecatDebugPanel`)

The debug panel provides real-time operational visibility:

- **Speaking Telemetry Badges:** Live badges reflecting User Speaking (`● User Speaking` / `● User Silent`), Bot Speaking (`● Bot Speaking` / `● Bot Silent`), and Barge-ins (`⚡ Barge-ins: N`).
- **Performance Metrics:**
  - `WS Connect Latency`: Time in ms to establish TCP/WebSocket handshake.
  - `RTVI Ready Latency`: Time in ms from `client-ready` to `bot-ready` handshake.
  - `Audio In (24kHz)`: Total frames received, data volume in KB, and current frame rate in fps.
  - `Audio Out (16kHz)`: Total mic frames sent and data volume in KB.
  - `Speaking Durations`: Cumulative user and assistant speaking durations in seconds.
- **Wire Message Inspector:**
  - Searchable list of all inbound and outbound RTVI envelopes.
  - Expandable inspector displaying formatted JSON payload, timestamps, message direction, and IDs.
  - One-click log clearing for isolation during long tests.

## 7. Verification and Testing

The implementation is verified by unit and integration suites in `ui/src/`:

- `ui/src/voice/protocol.test.ts`: Round-trip encoding/decoding of protobuf frames, varint utilities, and fixture compatibility against `nested-rtvi.bin`, `client-audio.bin`, and `server-audio.bin`.
- `ui/src/components/debug/PipecatDebugPanel.test.tsx`: Telemetry badges, latency display, search filtering, message inspection, and barge-in indicators.
- `ui/src/routes/agents/AgentVoiceTest.test.tsx`: Complete conversational flow (connect -> bot-ready -> user speaks -> bot speaks -> barge-in -> disconnect), permission rejection, connection failure, and session trace linking.

Verification commands:
```sh
cd ui && npm test -- --run && npm run typecheck && npm run build
```
