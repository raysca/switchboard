# Offline speech stack

Runs whole calls on local models: Whisper hears the caller, an Ollama model answers, Kokoro speaks. No key, and audio and text never leave the host. The adapter is published on `127.0.0.1` only (it is open when `OFFLINE_TOKEN` is empty); widen the port mapping in `compose.yaml` deliberately, and set a token if you do. Air-gapped use: after `make offline-models`, set `HF_HUB_OFFLINE=1` on the `speaches` service so the model hub is never contacted (it must stay unset while downloading models); this was not tested. The original design spec was removed from the tree; read it with git show e3465cf^:docs/history/superpowers/specs/2026-10-15-offline-speech-service-design.md.

```
Switchboard server (provider "offline")
   |  xai processors, endpoints below
   +--> Ollama /v1/responses                      (LLM, called directly)
   +--> offline-adapter :8099  (xAI STT/TTS wire protocol)
            +--> Speaches :8000  (faster-whisper + Kokoro)
```

## Run it

Prerequisites: Docker (OrbStack or Docker Desktop), Go 1.27.1, Node 20+ (for the CLI call), about 6 GB of disk for images and models. On Apple Silicon install Ollama natively (`brew install ollama`) and use `OFFLINE_PROFILE=host-llm` below: containers cannot use the Mac GPU. On Linux or CI leave the profile at its default (`container-llm`) and Ollama runs in a container. Pass the same `OFFLINE_PROFILE` to every `make offline-*` command.

**1. Start the stack** (adapter and Speaches, plus Ollama unless `host-llm`):

```bash
make offline-up OFFLINE_PROFILE=host-llm     # drop the variable on Linux
```

**2. Download the models once** (the only step that needs the network; about 3 GB: Whisper, Kokoro and the LLM):

```bash
make offline-models OFFLINE_PROFILE=host-llm
```

**3. Check it works** (real xAI clients through the stack; the first run is slow while models load):

```bash
make offline-test
```

**4. Start the Switchboard server.** No provider keys are needed:

```bash
make ort build                                    # once; ort downloads the ONNX Runtime
export GOVOICE_MANAGEMENT_API_KEY="$(openssl rand -hex 32)"
./bin/server -config config/development.yaml     # public :8080, admin :9090; creates .govoice-dev/ on first start
```

`make dev` also works (server, admin UI and seeded agents), but it seeds agents from the providers it detects, so open the console and check which provider each stage got. Set a stage to **Offline (local models)** to use this stack.

**5. Create a flow on the `offline` provider** (in another terminal, same `GOVOICE_MANAGEMENT_API_KEY`):

```bash
ADMIN=http://127.0.0.1:9090
AUTH="Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY"
curl -fsS "$ADMIN/api/v1/providers" -H "$AUTH" | jq -c '.data[] | select(.id=="offline") | {id, detected}'
FLOW_ID=$(curl -fsS -X POST "$ADMIN/api/v1/flows" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "Offline demo",
  "config": {
    "stt": {"provider": "offline"},
    "llm": {"provider": "offline", "model": "govoice-test", "system_prompt": "You are a friendly voice assistant. Keep answers short."},
    "tts": {"provider": "offline"}
  }}' | jq -r .data.id)
curl -fsS "$ADMIN/api/v1/flows/$FLOW_ID" -H "$AUTH" | jq -r .data.readiness.state     # ready
```

**6. Make a call.** The CLI streams a recorded clip ("Hi!") as the caller and prints what happened. Real models on a CPU are slow, so raise the timeout:

```bash
cd examples/rtvi_client && npm ci
node cli.js --flow "$FLOW_ID" --timeout 90000
```

Expected summary (measured on Apple Silicon, CPU only):

```
 User Speech Detected:    PASS
 User Transcript:         "Hi!"
 Bot LLM Response:        "How can I help you today?"
 Bot Audio Synthesized:   PASS (72 frames, 68608 bytes)
 STT -> First LLM Token:  3409 ms
 STT -> First Audio Out:  6381 ms
```

For a live microphone call use `make dev` and the console's test call, or `make example-web`, with the flow above. Send a text turn instead of audio with `--text "hello"`.

**7. Stop.** `make offline-down` stops the containers and keeps the model volumes.

### If something does not work

| Symptom | Check |
|---|---|
| `make offline-up` fails on `host-llm` with "Ollama is not answering" | Start Ollama (`ollama serve`, or the menu-bar app) and retry. |
| Flow readiness is not `ready`, or `offline` is missing from `/api/v1/providers` | The server must be built with `-tags xaispeech` (`make build` does this); without it `offline` has no STT/TTS. |
| The call connects but the bot never answers | `docker compose -f offline/compose.yaml logs adapter speaches`. The first call after a start is slow while Whisper and Kokoro load; raise `--timeout`. Check the models exist (`make offline-models`) and the LLM model is `govoice-test`. |
| Port 8080/9090 already in use | Another server (for example a running `make dev`) owns it. Copy the config with other ports in `public_listen_addr` and `admin_listen_addr`, and pass `--admin` and `--ws` to the CLI. |
| The bot cuts you off or never hears you | The endpointer is energy based (`OFFLINE_ENERGY_THRESHOLD`, `OFFLINE_ENDPOINT_SILENCE`); a noisy microphone needs a real VAD. |
| Another machine must reach the adapter | It is published on `127.0.0.1` only; widen the port mapping in `compose.yaml` and set `OFFLINE_TOKEN`. |

## The LLM

`offline/Modelfile` defines `govoice-test`: `llama3.2:3b` with `temperature 0` and `seed 42`, because the adapter sends neither unless a flow sets `llm.temperature` (there is no seed setting), and repeatable flow tests need both. It was chosen because it completes a tool call and its result on `/v1/responses` (about 6 s on Apple Silicon); `gemma4` (9.6 GB) also does (about 10 s). Small models drift in wording after a tool result, so tool-dependent flow tests should assert outcomes, not wording. To try another model, change `FROM` in the Modelfile, rerun `make offline-models`, and check that a flow with a tool still calls it. Any server that implements `/v1/responses` with streaming and function calls works; only Ollama is verified.

## Adapter configuration (environment)

| Variable | Default | Meaning |
|---|---|---|
| `OFFLINE_LISTEN` | `:8099` | listen address |
| `OFFLINE_TOKEN` | empty | bearer token required on `/v1/*`; empty accepts any (the server's offline provider sends a constant token, so leave it empty when the server is the client) |
| `OFFLINE_SPEACHES_URL` | `http://127.0.0.1:8000` | Speaches base URL |
| `OFFLINE_STT_MODEL`, `OFFLINE_TTS_MODEL` | faster-whisper-base.en, Kokoro-82M | Speaches model ids; see "Speed" below before choosing a bigger STT model |
| `OFFLINE_DEFAULT_VOICE`, `OFFLINE_VOICE_ALIASES` | `af_heart`, `eve=af_heart` | voice mapping (`name=voice,...`) |
| `OFFLINE_MAX_CONCURRENCY`, `OFFLINE_QUEUE_WAIT` | 1, 5s | simultaneous Speaches requests; wait for a slot before 503 |
| `OFFLINE_INTERIM_EVERY`, `OFFLINE_ENDPOINT_SILENCE`, `OFFLINE_MAX_UTTERANCE` | 700ms, 600ms, 25s | STT interim cadence, end-of-utterance silence, forced cut |
| `OFFLINE_ENERGY_THRESHOLD` | 0.01 | RMS (fraction of full scale) that counts as speech |

The `sample_rate` query value (and `output_format.sample_rate` on `POST /v1/tts`) must be between 8000 and 48000; anything else is refused with HTTP 400.

## Speed (why the defaults are small)

When the caller pauses, the server tells the STT stream `audio.done` and waits only about 2 seconds for the final transcript. After that it drops the stream, and **whatever was still being transcribed is lost: the bot answers a sentence that is missing words, or whole sentences**. Batch Whisper on a CPU decodes at roughly 0.3 to 0.7 times real time with `small.en`, so a few seconds of speech already overruns that window. The defaults therefore use `base.en` (about 1.7 times faster than `small.en` on short clips, same text on the test clips) and one decode at a time, and interim decodes never wait for a decode slot, so they cannot delay a final. If the end of long sentences still goes missing on your machine, switch to `Systran/faster-whisper-tiny.en` (download it with `curl -X POST http://127.0.0.1:8000/v1/models/Systran/faster-whisper-tiny.en`) or run the stack on a GPU. `make offline-test` includes a long-speech check (`TestStackLongSpeechIsNotCutOff` and the server's `TestOfflineStackLongCallerSpeechIsOneTurn`).

## Limits

- Traces and usage say `provider: xai`: the xai processors hard-code the label.
- The endpointer is energy based: clean and synthetic audio is fine, a noisy microphone needs a real VAD.
- Speaches returns each synthesized sentence whole, so the first audio of a sentence arrives after the sentence is synthesized (about 0.4x its length on CPU).
- Whisper can occasionally emit a degenerate repeated-word interim ("I- I- I-") for a very short clip; the final decode of that sentence was correct in testing, and interims are not used as the final text.
- Whisper is batch: interims re-decode the utterance so far, so CPU grows with utterance length (capped by `OFFLINE_MAX_UTTERANCE`).
