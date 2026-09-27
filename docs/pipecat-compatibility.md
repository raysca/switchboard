# Pipecat Compatibility & Behavioral Differences Record

This record documents the exact compatibility coordinates, preserved behaviors, architectural differences, and explicit scope exclusions between Voice Framework V1 and the upstream Pipecat ecosystem.

---

## 1. Pinned Coordinates & Provenance

All compatibility guarantees are pinned against the exact npm release packages and Git commits validated in `docs/compatibility/v1-lock.json`:

| Component | Pinned Version / Coordinate | Artifact Hash / SHA-256 |
|---|---|---|
| **`@pipecat-ai/client-js`** | `1.13.1` | `753fb509426f0d14b1ce3d0f0ebcc04d538e12ec` |
| **`@pipecat-ai/websocket-transport`** | `1.7.2` | `e2a4f48ffc774431e21b18d2643aeb482a095368` |
| **RTVI Protocol Version** | `2.1.0` | Exact string match in `client-ready` / `bot-ready` |
| **Protobuf Serializer** | `frames.proto` | Wire tag 2 (audio), wire tag 4 (message) |
| **Upstream Git Commit** | `pipecat-ai/pipecat@207b8b4` | Reference baseline commit |

---

## 2. Preserved Behaviors

Voice Framework V1 provides 100% wire-compatible drop-in interoperability with the official, unmodified Pipecat JavaScript client:

1. **Protobuf Binary Framing**: Audio and RTVI messages use the standard Pipecat Protobuf schema. No custom serializer, patch, or fork is required.
2. **Standard Handshake**: Accepts standard `client-ready` event and emits standard `bot-ready` event containing service configuration.
3. **Bi-directional Audio**:
   - Ingests microphone audio at 16000, 24000, or 48000 Hz.
   - Streams synthesized speech to the client at fixed 24000 Hz PCM16LE mono.
4. **Complete RTVI Event Flow**:
   - `user-started-speaking` and `user-stopped-speaking` events accurately track speech onset and termination.
   - `user-transcription` emits interim and final transcripts.
   - `bot-llm-text` emits streaming LLM token deltas for real-time text display.
   - `bot-started-speaking` and `bot-stopped-speaking` control UI speaking indicators.

---

## 3. Intentional Differences & Engineering Findings

### 1. Interruption & The Client Playback Clear Finding (Phase 0 Probe)
- **Upstream Assumption**: Standard voice frameworks assume the client-side transport will immediately silence audio upon receiving an interruption wire frame or `bot-stopped-speaking` message.
- **Phase 0 Empirical Finding**: Detailed inspection and automated probing (`tools/compat/probe_playback_clear.js`) proved that `@pipecat-ai/client-js@1.13.1` and `@pipecat-ai/websocket-transport@1.7.2` have **no mechanism to purge or silence audio chunks already written to the browser's Web Audio / AudioContext queue**.
- **Recorded product decision** (`docs/compatibility/playback-clear-decision.json`, id `relax-unmodified-client-playback-clear`): spec §18's unmodified-client clear is **relaxed**. The probe verdict remains `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`. Server pacing is not a passing clear.
  - Audio is written at media time. The server output queue is capped at 100ms so barge-in can drop audio that has not yet been written. Audio already inside the browser can still play out.
  - The probe exits 0 only while that decision file matches; a missing or different decision fails the gate.

### 2. Mandatory Cryptographic Admission Tokens
- **Upstream Behavior**: Many Pipecat examples allow unauthenticated connections or rely on custom HTTP endpoints.
- **Voice Framework V1**: Mandates short-lived, single-use HMAC-SHA256 JWT tokens. Connections lacking a valid token or attempting to replay a used token are rejected before WebSocket upgrade.

### 3. Clean-Session Reconnect (No Stateful Resumption)
- **Upstream Behavior**: Some Pipecat transports attempt in-band reconnects while preserving Python pipeline state.
- **Voice Framework V1**: Adopts a strict clean-session philosophy. Dropped connections tear down completely and release all cloud provider and VAD resources within 2 seconds. Reconnecting clients mint a fresh token and start a fresh call.

### 4. Strict Audio Validation
- **Upstream Behavior**: Python runners frequently perform runtime dynamic type coercion or accept variable channel layouts.
- **Voice Framework V1**: Enforces strict compile-time and runtime validation. Non-PCM16LE formats, multi-channel audio, or frames exceeding 64KB are rejected immediately.

---

## 4. Explicit Scope Exclusions (Non-Goals for V1)

To maintain focus on production reliability and low latency, the following capabilities are explicitly excluded from Voice Framework V1:

1. **WebRTC & Daily.co Transport**: V1 implements WebSocket transport exclusively. WebRTC, ICE, and Daily room management are excluded.
2. **Telephony & SIP**: No SIP, PSTN, or Twilio media stream bridges.
3. **Playout Acknowledgement**: V1 relies on server-side media-clock pacing rather than round-trip client playout acknowledgements.
4. **Dynamic Media Renegotiation**: Audio formats are fixed at handshake; dynamic sample rate switching during a call is not supported.
5. **Sender Loss Detection**: V1 relies on standard TCP/WebSocket connection drop detection rather than in-band application keepalives.
6. **xAI Speech Services**: xAI is supported strictly as an interchangeable LLM (Scenario C); xAI STT and TTS are excluded.
