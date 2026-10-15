# Offline speech stack

Runs whole calls on local models: Whisper hears the caller, an Ollama model answers, Kokoro speaks. No key, and audio and text never leave the host. The adapter is published on `127.0.0.1` only (it is open when `OFFLINE_TOKEN` is empty); widen the port mapping in `compose.yaml` deliberately, and set a token if you do. Air-gapped use: after `make offline-models`, set `HF_HUB_OFFLINE=1` on the `speaches` service so the model hub is never contacted (it must stay unset while downloading models); this was not tested. Design: [`docs/superpowers/specs/2026-10-15-offline-speech-service-design.md`](../docs/superpowers/specs/2026-10-15-offline-speech-service-design.md).

```
Switchboard server (provider "offline")
   |  xai processors, endpoints below
   +--> Ollama /v1/responses                      (LLM, called directly)
   +--> offline-adapter :8099  (xAI STT/TTS wire protocol)
            +--> Speaches :8000  (faster-whisper + Kokoro)
```

## Run

```bash
make offline-up                           # adapter + Speaches + Ollama in containers (Linux, CI)
make offline-up OFFLINE_PROFILE=host-llm  # adapter + Speaches in containers, Ollama on the host (Apple Silicon: Metal)
make offline-models [OFFLINE_PROFILE=...] # once, needs network: Whisper, Kokoro and the LLM (llama3.2:3b via offline/Modelfile)
make offline-test  [OFFLINE_PROFILE=...]  # real xai clients through the running stack
make offline-down
```

Server config (these are the defaults): see [docs/providers.md](../docs/providers.md#offline-mode-privacy). Use provider `offline` for a stage; LLM model `govoice-test`, TTS voice `af_heart` (xAI names such as `eve` are aliased).

## The LLM

`offline/Modelfile` defines `govoice-test`: `llama3.2:3b` with `temperature 0` and `seed 42`, because the xai adapter does not send either, and repeatable flow tests need both. `llama3.2:3b` was chosen with `go run ./cmd/toolcheck -model <name>` (a tool call plus its result on `/v1/responses`): it passed in about 6 s on Apple Silicon, `gemma4` (9.6 GB) passed in about 10 s. Small models drift in wording after a tool result, so tool-dependent flow tests should assert outcomes, not wording. Any server that implements `/v1/responses` with streaming and function calls works; only Ollama is verified.

## Adapter configuration (environment)

| Variable | Default | Meaning |
|---|---|---|
| `OFFLINE_LISTEN` | `:8099` | listen address |
| `OFFLINE_TOKEN` | empty | bearer token required on `/v1/*`; empty accepts any (the server's offline provider sends a constant token, so leave it empty when the server is the client) |
| `OFFLINE_SPEACHES_URL` | `http://127.0.0.1:8000` | Speaches base URL |
| `OFFLINE_STT_MODEL`, `OFFLINE_TTS_MODEL` | faster-whisper-small.en, Kokoro-82M | Speaches model ids |
| `OFFLINE_DEFAULT_VOICE`, `OFFLINE_VOICE_ALIASES` | `af_heart`, `eve=af_heart` | voice mapping (`name=voice,...`) |
| `OFFLINE_MAX_CONCURRENCY`, `OFFLINE_QUEUE_WAIT` | 2, 5s | simultaneous Speaches requests; wait for a slot before 503 |
| `OFFLINE_INTERIM_EVERY`, `OFFLINE_ENDPOINT_SILENCE`, `OFFLINE_MAX_UTTERANCE` | 700ms, 600ms, 25s | STT interim cadence, end-of-utterance silence, forced cut |
| `OFFLINE_ENERGY_THRESHOLD` | 0.01 | RMS (fraction of full scale) that counts as speech |

## Limits

- Traces and usage say `provider: xai`: the xai processors hard-code the label.
- The endpointer is energy based: clean and synthetic audio is fine, a noisy microphone needs a real VAD.
- Speaches returns each synthesized sentence whole, so the first audio of a sentence arrives after the sentence is synthesized (about 0.4x its length on CPU).
- Whisper is batch: interims re-decode the utterance so far, so CPU grows with utterance length (capped by `OFFLINE_MAX_UTTERANCE`).
