# Voice Framework V1 — Providers & Lifecycle Architecture

This document specifies the integration, configuration, stream rotation, error classification, retry discipline, and circuit breaker architecture for Google Cloud and xAI providers.

---

## 1. Google Cloud Providers

Google Cloud provides the primary end-to-end voice pipeline in Voice Framework V1.

### Speech-to-Text V2 (STT)
- **Endpoint**: `speech.googleapis.com`
- **Model**: `chirp_2` (pinned)
- **API**: Streaming gRPC (`speechpb.Speech_StreamingRecognizeClient`)
- **Audio Framing**: Audio is chunked into <= 25600 bytes per request (typically 640-byte 20ms frames) at near real-time cadence.
- **Stream Rotation**:
  - Google Cloud STT enforces a hard 300-second stream duration limit.
  - The runtime proactively initiates a new recognition stream at `240s` (`StreamRotationThreshold`).
  - The new stream overlaps the old stream by `500ms` (`StreamOverlap`), replaying the trailing audio buffer.
  - Transcript deduplication prevents duplicate `UserMessageFrame` or `FinalTranscriptionFrame` events across rotation boundaries.
- **Quota Headroom Management**:
  - Because stream rotation requires two concurrent streams during the 500ms overlap, admission control reserves `2.0x` stream quota headroom.
  - A server supporting 250 calls reserves 500 concurrent STT stream quota slots.

### Gemini 2.5 Flash (LLM)
- **Model**: `gemini-2.5-flash` (pinned, immutable)
- **API**: `google.golang.org/genai` streaming content generation.
- **Context Bounds**: Bounded conversation history passed on each generation turn.
- **Telemetry**: Tracks prompt tokens, candidate output tokens, and total token usage without logging sensitive payload content.

### Cloud Text-to-Speech V1 (TTS)
- **Voice**: `en-US-Journey-F` (pinned)
- **Audio Format**: Headerless Linear PCM 16-bit Little Endian (`AudioEncoding_PCM`).
- **Sample Rate**: `24000 Hz` (pinned player rate).
- **Synthesis Unit Segmentation**:
  - Assistant text from the LLM is segmented by punctuation into discrete synthesis units bounded to `<= 250 characters`.
  - Audio is chunked into 20ms frames (960 bytes) before delivery to the transport.

---

## 2. xAI LLM Provider (Scenario C Interchangeability)

Voice Framework V1 supports drop-in interchangeability for the LLM stage using xAI Grok.

### Specifications
- **Model**: `grok-2-1212` (pinned)
- **Endpoint**: `https://api.x.ai/v1/responses`
- **Protocol**: HTTP/1.1 REST with Server-Sent Events (SSE).
- **Data Privacy Policy**: Every request unconditionally sends `"store": false` to prevent server-side retention or training on voice interaction data.
- **Bounded Ingestion**:
  - SSE line limit: `64 KB`
  - SSE event limit: `1 MB`
  - SSE total response body limit: `10 MB`
- **Terminal Event**: Streams must terminate with an explicit `data: [DONE]` event. Any stream closing prematurely without `[DONE]` fails with `ErrMissingTerminalEvent`.

### Native xAI Speech-to-Text (STT)
- **Protocol**: Real-time bidirectional streaming over WebSocket (`wss://api.x.ai/v1/stt`).
- **Audio Format**: Headerless 16kHz 16-bit mono PCM (`encoding=pcm&sample_rate=16000`).
- **Events**: Consumes streaming audio chunks and processes `transcript.created`, `transcript.partial`, and `transcript.done` events, mapping `speech_final` and `is_final` to `core.FinalTranscriptionFrame`.
- **Fencing & Circuits**: Protected by generation fencing and circuit breakers.

### Native xAI Text-to-Speech (TTS)
- **Protocol**: REST API (`https://api.x.ai/v1/tts`).
- **Audio Output**: Headerless raw 24kHz 16-bit mono PCM (`{"codec": "pcm", "sample_rate": 24000}`).
- **Pacing**: Chunked into 20ms frames (960 bytes) matching Voice Framework V1 media clock, emitting `core.OutputAudioFrame`.
- **Speech path**: xAI STT and TTS are experimental. They compile only with `-tags xaispeech` and are not registered on the default server. The supported xAI surface is the LLM (`xai.llm`).

---

## 3. Provider Lifecycle, Retries, and Circuit Breakers

### Error Classification & Secret Redaction
All provider errors are mapped to provider-neutral domain errors (`ErrTransient`, `ErrPermanent`, `ErrAuthentication`, `ErrRateLimit`). Raw authorization headers, Bearer tokens, and API keys (`GEMINI_API_KEY`, `XAI_API_KEY`) are unconditionally stripped from error strings and log output.

### Retry Discipline
1. **Exponential Backoff with Full Jitter**:
   - Initial interval: 100ms
   - Multiplier: 2.0
   - Maximum interval: 2000ms
2. **Retry-After Header**: When returned by the provider on 429 or 503 responses, the client honors the `Retry-After` delay up to a maximum ceiling of 10s.
3. **Commit Rule (`ErrRetryCommitted`)**:
   - **STRICT PROHIBITION**: Once a partial token or audio sample has been committed and sent to the user, retry is strictly prohibited. Retrying after partial output would produce duplicate sentences or garbled speech. If a failure occurs after emission has begun, the operation fails immediately.

### Circuit Breakers
Each external provider interface is guarded by a 3-state circuit breaker:
- **Closed**: Normal operations.
- **Open**: Trips after 5 consecutive failures within a 60s sliding window. All calls fail fast for 30 seconds (`Cooldown`).
- **Half-Open**: Permits a single probe call. If successful, resets to Closed; if it fails, returns to Open for another 30 seconds.
- **Readiness Integration**: If any critical circuit breaker is Open, the server `/ready` compound endpoint reports `false`.
