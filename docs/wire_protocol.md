# Voice Framework V1 — Wire Protocol & RTVI Specification

This document details the exact wire protocol, Protobuf framing, RTVI event negotiation, audio format constraints, media-clock pacing, and network operating envelope for Voice Framework V1.

---

## 1. Protobuf Wire Framing

All data exchanged over the WebSocket connection (`/ws`) uses binary Protobuf frames serialized according to `proto/frames.proto`.

### Protocol Definition (`frames.proto`)

```protobuf
syntax = "proto3";

package pipecat;

option go_package = "govoice/core/transport/v1/framespb";

message FrameAudio {
  bytes audio = 1;
  uint32 sample_rate = 2;
  uint32 num_channels = 3;
}

message FrameMessage {
  string data = 1; // JSON serialized string
}

message Frame {
  oneof frame {
    FrameAudio audio = 1;
    FrameMessage message = 2;
  }
}
```

### Frame Rules
1. **Audio Messages (`field 1`)**: Carries raw linear PCM16LE audio samples.
2. **Text / Control Messages (`field 2`)**: Carries a JSON-serialized string formatted according to the RTVI protocol.
3. **Strict Validation**: The server rejects frames exceeding 64KB, frames with unknown fields, or frames with empty payloads.

---

## 2. RTVI Event Negotiation

Voice Framework V1 implements the exact pinned RTVI 2.1.0 protocol specification compatible with `@pipecat-ai/client-js@1.13.1`.

### Envelope Structure
Every `FrameMessage` contains a JSON payload with `label: "rtvi-ai"`:

```json
{
  "id": "msg-uuid-or-counter",
  "label": "rtvi-ai",
  "type": "<event-type>",
  "data": { ... }
}
```

### Supported RTVI Event Set

| Event Type | Direction | Description | Data Payload |
|---|---|---|---|
| `client-ready` | Inbound (Client -> Server) | Sent once when client establishes WebSocket connection | `{"version": "2.1.0"}` |
| `bot-ready` | Outbound (Server -> Client) | Sent once server has validated token and initialized pipeline | `{"version": "2.1.0", "config": [...]}` |
| `user-started-speaking` | Outbound (Server -> Client) | Emitted when VAD detects speech onset | `{}` |
| `user-stopped-speaking` | Outbound (Server -> Client) | Emitted when VAD detects speech termination | `{}` |
| `user-transcription` | Outbound (Server -> Client) | Interim or final transcription from STT | `{"text": "...", "final": true/false}` |
| `bot-llm-text` | Outbound (Server -> Client) | Streamed token/chunk emitted from LLM | `{"text": "..."}` |
| `bot-started-speaking` | Outbound (Server -> Client) | Emitted before first audio frame of assistant turn | `{}` |
| `bot-stopped-speaking` | Outbound (Server -> Client) | Emitted when assistant speech completes or is interrupted | `{}` |
| `error` | Outbound (Server -> Client) | Emitted when a session or provider fault occurs | `{"error": "...", "fatal": true/false}` |

---

## 3. Audio Formats & Streaming Resampling

### Inbound (Client Microphone)
- **Supported Sample Rates**: `16000 Hz`, `24000 Hz`, `48000 Hz`.
- **Channel Format**: Mono (1 channel).
- **Encoding**: Linear PCM 16-bit Little Endian (`PCM16LE`).
- **Resampling**: The server employs a stateful, streaming pure-Go sinc resampler (`transport/resample`) that converts `24kHz` and `48kHz` inputs directly to `16000 Hz` without introducing phase artifacts or memory leaks.

### Outbound (Bot Speaker)
- **Fixed Sample Rate**: Strictly `24000 Hz`.
- **Channel Format**: Mono (1 channel).
- **Encoding**: Linear PCM 16-bit Little Endian (`PCM16LE`), headerless.
- **Chunk Duration**: Paced in discrete 20ms frames (480 samples = 960 bytes per frame).

---

## 4. Media-Clock Pacing & Interruption Barrier

### The Unpaced Playback Problem (Phase 0 Finding)
Standard WebSocket clients buffer incoming audio chunks in local JavaScript memory. In the event of an interruption, unmodified clients cannot clear their hardware playback buffer upon receiving a wire event.

### Remedy: Server-Side Media-Clock Pacing
1. **Cadence**: The server transmits audio at exactly real-time speed (one 20ms frame every 20ms, governed by a monotonic high-resolution ticker).
2. **Buffer Ceiling**: The server maintains at most `100ms` of unpaced lookahead in the writer queue.
3. **Interruption Flush**: When a barge-in event occurs, the server writer drops all queued audio instantly and emits `bot-stopped-speaking`. At most 100ms of audio can ever be queued on the client, meeting the p99 <= 200ms interruption latency SLO.

---

## 5. Supported Operating Envelope

| Parameter | Supported Range | Rejection / Action Threshold |
|---|---|---|
| **Round Trip Time (RTT)** | <= 150 ms | Degradation > 300 ms |
| **Network Jitter** | <= 30 ms | Bounded by 100ms transport queue |
| **Packet Loss** | <= 1% | WebSocket TCP transport guarantees ordered delivery |
| **Client Write Stall** | <= 500 ms | Disconnect client with close code 1008 if write deadline is breached |
| **Max Call Duration** | Configurable (Default: 3600s) | Graceful termination upon reaching limit |
