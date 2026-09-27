# Voice Framework V1 — Architecture Specification

Voice Framework V1 is a low-latency, resilient voice agent runtime implemented in Go. It terminates bidirectional WebSocket client connections, processes real-time audio and control messages, orchestrates neural Voice Activity Detection (Silero VAD), and connects Speech-to-Text (STT), Large Language Models (LLM), and Text-to-Speech (TTS) pipelines with deterministic turn-taking, barge-in interruption, and bounded memory.

---

## 1. System Overview

```
                      +---------------------------------------+
                      |        Untrusted Client Network       |
                      +---------------------------------------+
                                          |
                                          | WSS (:8080)
                                          v
                      +---------------------------------------+
                      |             Nginx Proxy               |
                      |   - TLS Termination                   |
                      |   - Redacts ?token query from logs    |
                      +---------------------------------------+
                                          |
                                          | HTTP / WS (127.0.0.1:8080)
                                          v
                      +---------------------------------------+
                      |          Go Public Listener           |
                      |   - Token Validation & Replay Gate    |
                      |   - Admission Control & Rate Limiting |
                      |   - Origin Verification               |
                      +---------------------------------------+
                                          |
                                          | Post-Admission
                                          v
                      +---------------------------------------+
                      |          Session Supervisor           |
                      |   - Bounded Resource Budget           |
                      |   - Generation Fencing & Watchdog     |
                      |   - 7-Stage Core Pipeline             |
                      +---------------------------------------+
                           |            |            |
             +-------------+            |            +-------------+
             |                          |                          |
             v                          v                          v
  +--------------------+      +--------------------+      +--------------------+
  |   Silero VAD IPC   |      | Google Cloud STT   |      | xAI / Google LLM   |
  |  Isolated Worker   |      | & Cloud TTS        |      | Stream Synthesis   |
  +--------------------+      +--------------------+      +--------------------+
```

---

## 2. Core Runtime Model

The core runtime (`core/`) depends exclusively on the Go standard library. It defines the foundational abstractions:

### Frames
All data and control signals flow as strongly typed `core.Frame` implementations:
- **Audio Frames**: `InputAudioFrame` (client microphone audio), `OutputAudioFrame` (synthesized bot audio).
- **Speech-State Frames**: `UserStartedSpeakingFrame`, `UserStoppedSpeakingFrame`.
- **Transcription Frames**: `InterimTranscriptionFrame`, `FinalTranscriptionFrame`.
- **Text & Synthesis Frames**: `LLMTextDeltaFrame`, `LLMTextFrame`, `SynthesisUnitFrame`.
- **Control Frames**: `InterruptionFrame`, `FlushFrame`, `FlushProcessedFrame`, `CancelFrame`.

### Processors & Emitters
- **`Processor`**: The fundamental unit of execution:
  ```go
  type Processor interface {
      ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error
  }
  ```
- **`Emitter`**: Allows a processor to emit zero, one, or multiple frames either `Downstream` or `Upstream`:
  ```go
  type Emitter interface {
      Push(f Frame) error
      PushDirection(f Frame, dir Direction) error
  }
  ```
- **Execution Guarantee**: Processors execute sequentially on the session run loop. They never execute concurrently with themselves, eliminating race conditions across state mutations.

### Asynchronous Re-entry & Attacher
- Processors implementing `Attacher` receive access to `core.SessionController` at startup:
  ```go
  type Attacher interface {
      Attach(s SessionController, index int) error
  }
  ```
- Processors spawn background tasks (such as streamed HTTP/gRPC requests) via `s.Go(func(ctx context.Context))`. Results re-enter the pipeline via `s.Reenter(f, dir, resumeIndex, gen)`.

---

## 3. The 7-Stage Pipeline

Every voice conversation session constructs an identical 7-stage pipeline:

```
[Inbound Audio / Control]
          |
          v
  1. vad.Processor (Silero VAD)
          |
          v
  2. STTProcessor (Google Cloud STT v2 - Chirp 2)
          |
          v
  3. UserContextProcessor (Turn pair management & deduplication)
          |
          v
  4. LLMProcessor (Gemini 2.5 Flash / Grok 2)
          |
          v
  5. AssistantContextProcessor (Synthesis unit segmentation <= 250 chars)
          |
          v
  6. TTSProcessor (Google Cloud TTS - en-US-Journey-F)
          |
          v
  7. TransportOutput (Media-clock paced WebSocket writer)
```

---

## 4. Generations, Fencing, and Barge-In Interruption

Voice interactions require deterministic handling of barge-in interruptions:

1. **User Speaks**: When the VAD detector identifies user speech (`UserStartedSpeakingFrame`), the session supervisor atomically increments the turn generation (`s.IncrementGeneration()`).
2. **Generation Fencing**: Every frame carries the generation ID under which it was created. Any asynchronous task re-entering with an old generation (`gen < currentGen`) is immediately dropped before mutating processor state or conversation context (`s.StaleDiscarded.Add(1)`).
3. **Interruption Barrier**: The server issues an `InterruptionFrame` downstream and signals the client via RTVI `bot-stopped-speaking`.
4. **Transport Flush**: `TransportOutput` immediately clears its unpaced audio queue (bounded to <= 100ms) and emits `FlushProcessedFrame`.
5. **Partial Context Rule**: The `AssistantContextProcessor` only commits a synthesis unit to the conversation context when its final audio chunk has been completely written to the transport. If interrupted mid-unit, the unwritten portion is discarded, ensuring the LLM context accurately mirrors what the user actually heard.

---

## 5. Session Lifecycle and Resource Safety

Every session is governed by a strict `ResourceBudget`:
- **Task Budget**: Maximum concurrent goroutines per session (`MaxTasks = 16`).
- **Queue Limits**: Dedicated priority control queue (`MaxControlFrames = 64`) and data queue (`MaxQueuedFrames = 256`).
- **Data Shedding**: If the data queue overflows, audio frames are dropped with atomic counters (`AudioDropped`), preserving session integrity and control signal responsiveness.
- **Teardown & Leaked Work**: Upon session close, all managed goroutines are given a bounded timeout (`TeardownTimeout = 2s`). If a goroutine hangs or fails to exit, it is explicitly classified as `StateLeaked` and exported via the `voice_session_tasks_leaked_total` Prometheus counter for operator alerting.

---

## 6. Network Surface Separation

To prevent public exploitation of internal telemetry and debug interfaces, Voice Framework V1 enforces strict port separation:
- **Public Surface (`:8080`)**:
  - `GET /ws`: WebSocket upgrade requiring valid single-use JWT.
  - No administrative endpoints are mounted.
- **Admin Surface (`:9090`)**:
  - `GET /health`: Ultra-cheap local liveness check (no provider calls).
  - `GET /ready`: Compound readiness check inspecting startup, shutdown, admission state, VAD worker pool health, and circuit breakers.
  - `GET /metrics`: Full Prometheus metrics exposition matching Spec §35.
  - `GET /timeline`: Bounded per-session event timeline for incident diagnostics.
