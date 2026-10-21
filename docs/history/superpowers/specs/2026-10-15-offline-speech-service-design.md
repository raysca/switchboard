> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# Offline Speech Service (xAI-compatible) Design

**Date:** 2026-10-15
**Status:** draft; awaiting review. No code written.

## Intent

Run whole calls, with real audio, on a machine with no network and no paid provider. Two uses:
1. **Repeatable flow tests.** Replay real caller audio through the real pipeline (VAD, STT, LLM, TTS, barge-in) again and again, and see real worst-case latency on CPU.
2. **Privacy users.** Audio and text never leave the host.

The `echo` provider stays the fast tier (no models, no audio fidelity). This is the slower tier above it.

**Approach.** One standalone service that speaks xAI's wire protocol for LLM, STT and TTS. The existing `xai` adapters are pointed at it through their endpoint settings. **No Go change to `core/`, `providers/` or `server/` is needed for v1.** The only new thing is the service.

## Why xAI and not Google

All three xAI stages already take an endpoint from server config (`providers.xai_llm.endpoint`, `xai_stt.endpoint`, `xai_tts.endpoint`; `server/registry.go:320`, `server/registry_xaispeech.go:29,48`). The xAI HTTP pool has no private-address guard (unlike `openai`), so `http://localhost` and `ws://localhost` work. Google STT and TTS are gRPC to a hard-wired `:443` TLS host with Google auth, so mimicking them means implementing generated protobuf services.

## Wire contract (read from the adapters on 2026-10-15)

The adapters are the contract. Anything not listed here is not read by the adapters and need not be sent.

### Auth (all endpoints)
`Authorization: Bearer <key>`. The registry requires a key to exist (`apiKey(spec.Secret)`); the service accepts any token unless `OFFLINE_SPEECH_TOKEN` is set, in which case it must match. A wrong token is HTTP 401 on the upgrade or request.

### STT: `GET /v1/stt` (WebSocket) (`providers/xai/stt.go`)
- Query: `sample_rate` (16000), `encoding=pcm` (signed 16-bit little-endian, mono), `language`, `interim_results=true`, `model`.
- Server sends first: `{"type":"transcript.created","id":"..."}`. The adapter buffers audio (up to 60 s) until it sees this, so send it right after the upgrade.
- Client sends binary frames (20 ms, 640 bytes at 16 kHz, but any size) and finally the text frame `{"type":"audio.done"}`.
- Server sends `{"type":"transcript.partial","text","is_final","speech_final","start","duration","language"}`:
  - interim: `is_final=false, speech_final=false`, `text` is the current segment.
  - `speech_final=true`: end of an utterance. **`text` must be the whole utterance**: the adapter computes `join(finished utterances, text)` and ignores locked chunks.
  - `is_final=true` alone closes a chunk inside an utterance. **v1 never sends it**, so interim text is always the whole utterance so far and the adapter's chunk bookkeeping stays empty.
  - Empty `text` is ignored.
- On `audio.done` the server finalizes any open utterance with a `speech_final` partial, then sends `{"type":"transcript.done","text":"<last utterance>"}` and closes. The adapter finalizes from `transcript.done.text` if no `speech_final` arrived, but sending `speech_final` is cleaner.
- Errors: `{"type":"error","message","code"}` then close.
- A stream may carry several utterances before `audio.done`.
- Closing the socket without `transcript.done` after `audio.done`, or at all while a turn is bound, is treated as a lost utterance (retryable error, failover). So close only after `transcript.done`.

### TTS streaming: `GET /v1/tts` (WebSocket) (`providers/xai/tts_stream.go`)
- Query: `voice`, `language`, `codec=pcm`, `sample_rate` (24000 default), `optimize_streaming_latency`.
- One connection per assistant reply, reused across its sentence units. Per unit the client sends `{"type":"text.delta","delta":"<text>"}` then `{"type":"text.done"}` and waits for `audio.done` before sending the next unit.
- Server replies with any number of `{"type":"audio.delta","delta":"<base64 PCM16LE at sample_rate>"}` then `{"type":"audio.done"}`. **The connection stays open after `audio.done`.** Each chunk must be an even number of bytes.
- Error: `{"type":"error","message"}`. It is retried as the unary request, so the unary path must work.
- The client reads with an idle timeout (default `cfg.Timeout`), so the first `audio.delta` must arrive within it. Synthesis latency matters here.
- Barge-in: the client just closes the socket. The service must stop synthesis for that connection promptly (cancel the running job, do not finish it).

### TTS unary: `POST /v1/tts` (`providers/xai/tts.go`)
- Body: `{"text","voice_id","language","output_format":{"codec":"pcm","sample_rate":24000}}`.
- 200 with the raw PCM16LE body (no WAV header). Non-200: JSON error body; 400 for a bad voice (the adapter treats it as a client error), 401 for a bad token, 5xx retryable.

### LLM: `POST /v1/responses` (SSE) (`providers/xai/llm.go`, `tools.go`)
- Request: `{"model","store":false,"stream":true,"tools":[{"type":"function","name","description","parameters"}],"input":[...]}`. Input items are `{role:"system|user|assistant",content}`, `{type:"function_call",call_id,name,arguments}` and `{type:"function_call_output",call_id,output}`.
- Response: `text/event-stream`, lines `data: {json}` (an `event:` line is allowed). Event kinds the adapter reads, by the JSON `type`:
  - `response.output_text.delta` with `delta`.
  - Tool call: `response.output_item.added` and `.done` with `item:{type:"function_call",id,call_id,name,arguments}`; `response.function_call_arguments.delta` and `.done` with `item_id`, `output_index`, `delta`/`arguments`.
  - `response.completed` with `response:{usage:{input_tokens,output_tokens,total_tokens},output:[...]}`. This is the terminal event. A stream that ends with neither it nor `data: [DONE]` is an error (`ErrMissingTerminalEvent`).
  - `error` / `response.failed` for failures.
- The adapter rejects tool definitions if the endpoint contains `chat/completions`, so the endpoint must stay `.../v1/responses`.

## The service

**Shape.** A Go adapter (`offline/adapter/`, its own module in `go.work`, outside `core/` and `server/`) that speaks xAI's protocol and calls off-the-shelf model servers over HTTP. It contains no model code and no cgo. A `offline/compose.yaml` bundles the adapter with the model servers; the adapter is the only exposed port.

```
Switchboard (xai adapters, endpoints -> adapter)
        |
   Go adapter  (xAI STT/TTS protocol; endpointing, framing, limits)
        | HTTP, URLs from env
   Ollama (called directly)   Speaches: faster-whisper + Kokoro
```

**Why Go.** Same language and idioms as the repo (AGENTS.md bounds, `-race`); endpointing is a small pure-Go energy detector (the repo's `vad/` was rejected: it is cgo + onnxruntime behind worker processes; upgrade path for noisy microphones is `vad/` or sherpa-onnx); contract tests share `coder/websocket` and the `STTMessage` type with the adapters. The models stay in their own servers, so nothing here needs cgo or Python in our code.

**Backends are URLs, so they are swappable.** Docker on macOS has no GPU, so on Apple Silicon the backends are better run on the host (Metal) with only the adapter, or nothing, in Docker. Compose is the default for CI, shared test runs and privacy self-hosters; every backend URL is an env var so any of them can live elsewhere. Images are pinned by tag so a backend update cannot silently change test behavior. Weights come from a mounted volume; nothing downloads at runtime (a documented `fetch-models` step runs once, with network). The adapter's `/healthz` reports ready only when every backend answers.

**Backend candidates (to verify before building).** LLM: Ollama or llama.cpp. STT and TTS: Speaches (`/v1/audio/transcriptions` is batch, which suits re-decoding a growing utterance; `/v1/audio/speech` returns each request whole, confirmed by the spike, so `audio.delta` arrives after the sentence is synthesized). Chosen and built; Kokoro-FastAPI, Piper and sherpa-onnx remain alternatives.

All stages are bounded: a global semaphore caps concurrent decodes and syntheses, queued work waits with a deadline and then gets HTTP 503 / WS `error`. The per-stream audio buffer is capped at the utterance cap.

**Components.**

| Stage | Backend (default) | Notes |
|---|---|---|
| LLM | Ollama, called directly by the xAI adapter (not through this service) | see below |
| STT | Speaches (faster-whisper) plus a pure-Go RMS-energy endpointer (`adapter/endpoint.go`, `OFFLINE_ENERGY_THRESHOLD`, default 0.01 of full scale) | see below |
| TTS | Kokoro-FastAPI or Piper HTTP (resampled by the adapter to the requested rate) | one sentence at a time |


### LLM
**No proxy in v1: point `xai_llm.endpoint` straight at Ollama** (`http://127.0.0.1:11434/v1/responses`), as spiked below. The adapter service therefore only serves STT and TTS.
- The xAI adapter does not send `temperature` or `seed`, so repeatable test runs need the determinism on the Ollama side: a Modelfile (`PARAMETER temperature 0`, `PARAMETER seed 42`) and the agent's model set to that model name.
- Small local models call tools less reliably than xAI's. Flow tests that depend on a tool call should use a model known to support tools, and assert on outcomes, not wording.
- If a future backend lacks `/v1/responses`, a Responses to chat/completions translator can be added to the service; not needed now.

### STT (the riskiest part)
Whisper is not a streaming model, so the service builds streaming around it:
1. Per stream: resample if needed, track speech and silence with the RMS-energy endpointer on each frame (ceiling: a noisy microphone needs a real VAD).
2. **Interims:** while speech is active, every `interim_ms` (default 700) re-decode the utterance buffer so far and send an interim partial with the whole text. Only one decode in flight per stream; if one is still running, skip the tick. Decoding the whole utterance each time costs more compute as the utterance grows, so cap the utterance at `max_utterance_s` (default 25): at the cap, force a `speech_final`.
3. **End of utterance:** when VAD sees `endpoint_ms` of silence (default 600) after speech, decode once more (final text) and send `speech_final`. Then reset the buffer and wait for the next utterance.
4. `audio.done` finalizes an open utterance, sends `transcript.done`, closes.
5. **Silence and hallucination:** audio is only decoded when VAD saw speech; use Whisper's no-speech threshold and drop empty or boilerplate-only results (the usual "Thank you." on silence).
6. Interims can be switched off (`interim_results=false` in the query is already sent only when the adapter config says so). If the pipeline turns out not to depend on interims, turning them off cuts CPU a lot.

### TTS
- `voice` maps through a small table in the service config to a local voice id. xAI voice names (`eve`, ...) map to a default local voice, so shipped agent configs work unchanged. An unknown voice returns an error (WS `error`, HTTP 400), matching real behavior and the `brokenvoice` convention used by the e2e suites.
- Streaming: synthesize per sentence, convert float to PCM16LE, resample to the requested `sample_rate`, emit `audio.delta` chunks (about 100 ms each, even byte count), then `audio.done`. The connection loops for the next `text.delta`/`text.done`.
- Cancellation: closing the socket cancels the synthesis task.
- Unary: same synthesizer, whole body.

### Config (environment)
`OFFLINE_SPEECH_LISTEN`, `OFFLINE_SPEECH_TOKEN` (optional), `OFFLINE_SPEECH_LLM_UPSTREAM`, `OFFLINE_SPEECH_LLM_MODEL`, STT model and compute type, TTS backend and voice map, `MAX_CONCURRENCY`, `INTERIM_MS`, `ENDPOINT_MS`, `MAX_UTTERANCE_S`. Model files are read from a local directory; nothing is downloaded at runtime unless a documented `fetch-models` command is run first (privacy mode must work with no network).

## Spike results (2026-10-15, Apple Silicon arm64, CPU only, 3 synthetic clips from macOS `say`)

| Candidate | Result |
|---|---|
| **Speaches** `latest-cpu` image (1.57 GB; arm64 ok) + `faster-whisper-small.en` + `Kokoro-82M-v1.0-ONNX` (814 MB of weights) | One container serves `/v1/audio/transcriptions` and `/v1/audio/speech`. STT is accurate, punctuated and cased (one proper-noun miss, "Ottun" -> "Otten"); 1.9-3.1 s per 4-5 s clip (warm), batch only. |
| Speaches TTS streaming | **Not streamed within a request**: first byte arrives at the same time as the last (0.63 s of 0.63 s for 1.6 s of audio; 2.38 s of 2.38 s for 13.6 s). Synthesis runs at about 0.4x real time, so a sentence costs about 0.4x its audio length before the first `audio.delta`. Acceptable because the pipeline already sends sentence-sized units; the adapter forwards each unit as one burst. |
| **sherpa-onnx** streaming zipformer `en-2023-06-26` (320 MB, int8, 2 threads) | True streaming: a partial about every 320 ms, correct endpoints for all 3 utterances, `speech_final` about 0.6 s after speech stops, **RTF 0.098**. Accuracy is lower: uppercase, no punctuation, "Ottun" -> "ARTTON", "phone" -> "FOND". |

**Consequence.** Two viable STT paths, to be chosen per use:
- **Accuracy first (default for flow tests):** Speaches Whisper through the re-decode/endpointing design above. Costlier per interim, better text.
- **Streaming first (cheap, privacy users on small machines):** sherpa-onnx behind the adapter. The adapter maps its partial and endpoint events to `transcript.partial` / `speech_final`; no decode loop and no `vad/` reuse needed. Needs a model-specific wrapper process (sherpa-onnx ships WebSocket servers; the adapter would speak to one).
Both use Speaches for TTS and Ollama or llama.cpp for the LLM. Not yet spiked: LLM `/v1/responses` support (open question 1), Kokoro-FastAPI (not needed if Speaches TTS is acceptable).

**LLM spike (Ollama 0.40.4, `gemma4:latest` 8B Q4, Apple Silicon).** `POST /v1/responses` works end to end with the real xAI adapter code:
- Streams `response.output_text.delta`, ends with `response.completed` carrying `usage` (`input_tokens`/`output_tokens`/`total_tokens`). Time to first text delta 0.22 s warm.
- Tool call: returns a `function_call` item (`id`, `call_id`, `name`, `arguments`) with `response.function_call_arguments.delta/.done`. Replaying the recorded streams through `Processor.requestRound` (temporary test, deleted) parsed text, the call and usage with no error.
- Round trip: echoing the returned items (including Ollama's `reasoning` item) plus a `function_call_output` is accepted, and the model answers in text.
- `gemma4` is a thinking model: it emits `reasoning_summary_text` events and a `reasoning` item first. The adapter ignores both, so it costs only time.
- Ollama ignores the `Authorization` header, so any key works.

## Compose profiles

`offline/compose.yaml` has two profiles; the adapter and Speaches run in both.

| Profile | Services | Use |
|---|---|---|
| `container-llm` | adapter, Speaches, **Ollama in a container** | Linux CI and shared test runs; developers who want only Docker installed. No GPU on macOS, so a model of 3-8B is slow there. |
| `host-llm` | adapter, Speaches | Apple Silicon development: Ollama runs natively on the host (Metal). `xai_llm.endpoint` points at the host's `http://127.0.0.1:11434/v1/responses`. |

- **Default:** `container-llm`, so a fresh clone works with only Docker. Developers on Apple Silicon use `make offline-up PROFILE=host-llm`. (Open decision: swap the default if most developers are on Macs.)
- **Models are not downloaded at runtime.** `make offline-models` pulls the Whisper and Kokoro weights into the Speaches volume and the chosen LLM into Ollama's volume (or the host Ollama for `host-llm`). It is the only step that needs network; the stack itself runs with none.
- **Server in Docker.** If the Switchboard server also runs in a container, it reaches host Ollama as `host.docker.internal:11434`; the adapter is reached by its compose service name. Only the server calls Ollama, so the adapter needs no knowledge of either profile.
- **LLM model.** `gemma4` (9.6 GB) is too heavy for shared tests. Pick a 2-4 GB model with tool support through a short tool-calling benchmark (not yet run; only `gemma4` has been tested), and pin it in a Modelfile with `temperature 0` and a fixed `seed`. The Modelfile is checked in under `offline/`.
- **Other LLM servers.** Any server that implements `/v1/responses` with streaming and function calls works; only Ollama has been verified.
- **Readiness.** The adapter's `/healthz` is ready only when Speaches answers. In `container-llm` the compose healthcheck also waits for Ollama; in `host-llm` the Make target checks the host Ollama before reporting the stack up.
- **Image pinning.** Pin Speaches and Ollama images by tag (the spike used Speaches `latest-cpu`, 1.57 GB; pin a version before building).

## Wiring in the server

`server/` gets an `offline` provider (`offline.stt`, `offline.tts`, `offline.llm`) that reuses the real xai processors with their own endpoints and circuit breakers and needs no key. Config (these are the defaults, matching `offline/compose.yaml`):
```yaml
providers:
  offline:
    llm_endpoint: http://127.0.0.1:11434/v1/responses   # Ollama directly
    stt_endpoint: ws://127.0.0.1:8099/v1/stt
    tts_endpoint: http://127.0.0.1:8099/v1/tts
```
Agents choose provider `offline` per stage, so one server can run real xAI and offline agents side by side, and an offline outage cannot open xAI's breakers. `offline.llm` exists on every build; `offline.stt/tts` only with `-tags xaispeech`, like the xai speech adapters they wrap. A catalog entry (`offline.yaml`, keyless) makes it selectable in the console.

**End-to-end tests (in `server/`).** Existing e2e tests send text; these send caller **audio** over the WebSocket. (1) Hermetic, runs in `make verify`: an in-process adapter with fake model backends plus a fake Responses LLM, asserting a spoken reply comes back; and a dead adapter must not make the bot speak. (2) Opt-in, `make offline-test`: the same call on the real stack (Whisper, Ollama, Kokoro), with the caller's audio synthesized by the stack's own TTS. The server imports `govoice/offline/adapter` only from test files.

## Known limits of v1 (deliberately not solved)

1. **Labels.** Usage, traces and call records still say `provider: xai`, because the xai processors hard-code the label. Offline runs look like xAI runs in reports; fixing it means touching `providers/xai`.
2. **No token option.** The offline provider sends a constant bearer, so an adapter run with `OFFLINE_TOKEN` set is unusable from the server until a token option is added.
3. **Realism.** CPU decode times and local-model quality are not xAI's. Use it for correctness, flow and worst-case behavior, not for quality or latency baselines.
4. **No chunk finals** (`is_final`) and no word timings from STT in v1.
5. **Speaker/telephony audio.** 8 kHz telephony input depends on the pipeline resampling to the configured `sample_rate` before STT; the service assumes the rate in the query.

## Testing

- **Contract tests in Go, using the real adapters.** An env-gated test (same pattern as the existing `integration_test.go` files) runs `xai.NewSTTProcessor`, `NewStreamingTTSProcessor` and the LLM `Processor` against a running service: STT gets a WAV fixture and must produce a `speech_final` close to the known phrase; TTS must return PCM of plausible length and honor a mid-synthesis close; LLM must stream text and complete a tool-call round trip. This proves wire compatibility against the code that matters, and catches drift if the adapters change.
- **Adapter unit tests (Go, `-race`):** endpointing on synthetic speech-and-silence, utterance cap, concurrency cap, token check, even-byte chunking, cancel on close.
- **Flow test fixture generator:** a script that renders caller phrases to WAV with the same TTS, so repeated runs replay identical audio.
- Real-model tests run against the compose stack via `make offline-up` / `make offline-test` and are excluded from `make verify`.

## Rollout

1. TTS (unary and streaming) over the Speaches backend: smallest and the clearest wire format.
2. STT with VAD endpointing and interims.
3. Contract tests, compose file with both profiles, docs (`docs/providers.md` privacy-mode section, `docs/privacy.md` pointer).
4. Server `offline` provider and the end-to-end tests (see "Wiring in the server").

## Open questions

1. ~~Does Ollama implement `/v1/responses`?~~ Resolved by the spike: yes, see the LLM result below.
2. Which TTS server streams chunked PCM reliably (Kokoro-FastAPI, Piper HTTP)? Pick after a quick spike.
3. Do turn detection or barge-in depend on interim transcripts? If not, ship STT without interims first.
4. Which compose profile is the default (`container-llm` assumed)? Target hardware: CPU only, or Apple Silicon / GPU available? That decides the Whisper size and whether worst-case latency is interesting or just slow.

## Addendum: the final-transcript latency budget (found by a long-speech test, 2026-10-15)

The server sends `audio.done` when the caller pauses (VAD stop 200 ms) and waits about 2 s for the final transcript; after that the stream is dropped and any text still being decoded is lost. With `small.en` and 2 concurrent decodes on a CPU, finals took 2 to 5 s, so a real call heard only fragments ("Hi!" for "Hi, I'd like to check the status of my order please", whole sentences missing from a 22 s statement). Fixed in the adapter: default STT model `base.en`, one decode at a time, and interim decodes never wait for a decode slot (they are skipped when it is busy) and are cancelled when their utterance ends. Tests: `TestStackLongSpeechIsNotCutOff`, `TestOfflineStackShortClipIsHeardInFull`, `TestOfflineStackLongCallerSpeechIsOneTurn`. The server's wait after `audio.done` is `offlineSTTDrainTimeout` (6 s, `server/registry_offline_speech.go`) for this provider, not the cloud default of 2 s: under CPU load a 6 s segment's final took up to 5.5 s, and a 2 s wait dropped whole segments from the middle of a long statement. The last segment is still bounded by the turn finalizer's window (about 3 s after the caller stops). Remaining ceiling: a single uninterrupted stretch of speech longer than a few seconds still needs a decode inside the drain window (6 s mid-statement, about 3 s for the last segment); the fix for that would be incremental finals (`is_final` chunks at natural pauses) so only the tail is decoded at the end.
