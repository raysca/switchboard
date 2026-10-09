# NVIDIA live check (NIM LLM + Riva STT + Riva TTS)

Run 2026-10-08, one machine in the UK talking to NVIDIA-hosted endpoints (NVCF gRPC `grpc.nvcf.nvidia.com:443`, NIM `integrate.api.nvidia.com`). Small n (a few dozen calls in total), timings are single samples, not benchmarks. No secrets appear here; function ids are public NVCF ids.

## What ran

| Stage | Endpoint / function | Result |
|---|---|---|
| LLM | NIM chat completions via `providers/openai` | works on `openai/gpt-oss-20b` (the default) and `nvidia/nemotron-3-super-120b-a12b`: streaming, usage chunk with `stream_options.include_usage`, tool-call round trip. |
| STT | NVCF `ai-parakeet-ctc-1_1b-asr` `1598d209-5e27-4d3c-8079-4751568b1081` | works: first config message accepted, empty `model`, `en-US`, interims every ~100-300 ms, finals compose across sentences. |
| TTS | NVCF `ai-magpie-tts-multilingual` `877104f7-e885-42b9-8de8-f6e4c6303969` | works, voice `Magpie-Multilingual.EN-US.Aria`. |

Other ASR functions tried: `d8dd4e9b-fbf5-4fb0-9dba-8cf436c8d965` (`ai-parakeet-ctc-0_6b-asr`) works the same (one clip). `d3fe9151-442b-4204-a70d-5fcc597fd610` (`ai-parakeet-tdt-0_6b-v2`) rejects streaming: `InvalidArgument: Unavailable model requested ... type=online` (offline-only model), mapped as a non-retryable client error. Other ids listed ACTIVE by `GET /v2/nvcf/functions`: canary-1b, whisper-large-v3, conformer-ctc, parakeet multilingual/es/zh, magpie-zeroshot, chatterbox (not tried).

## LLM model

- The previous default `nvidia/llama-3.1-nemotron-70b-instruct` is present in `GET /v1/models` (80 models) but chat returns 404 `Function ... Not found for account`. Many other catalog entries 404 too (nemotron-51b, nemotron-nano-3-30b, mistral-large-2, mistral-7b v0.3, nemotron-4-340b). `meta/llama-3.1-8b-instruct` and `meta/llama-3.3-70b-instruct` return 410 (end of life 2026-08-26). The catalog is not proof a model answers. A model that 404s or 410s is reported as a non-retryable client error (no retry, breaker untouched) reading `model "..." not found or deprecated (or wrong base_url): set the model explicitly`.
- `nvidia/nemotron-3.5-lightning-30b-a3b` works but is a reasoning model: 662 completion tokens and 80 s for "Hello". Do not use for voice.
- `nvidia/nemotron-3-super-120b-a12b`: hello 1.2 s (66 completion tokens, hidden reasoning), tool call correct (`get_weather {"city":"Paris"}`, one call, sensible answer). Time to first token inside the voice pipeline varied 0.5 s to 6 s across the five turns measured (mean 1.6 s off, 3.3 s on): it reasons before answering.
- Review re-check (same day, 20 tiny direct calls, "What is two plus two?", one short sentence): also 404 for this account: gemma-3-12b-it, mistral-nemo-12b, phi-3.5-moe, granite-3.0-8b, mistral-large, mistral-nemo-minitron-8b; gemma-4-31b-it and deepseek-v4.1-flash timed out at 60 s. First token, warm:
  - `openai/gpt-oss-20b`: 0.82, 0.42, 0.80, 1.54 s (n=4), tool call 0.85 s, one `get_weather {"city":"Paris"}`. Reasoning is short (31-70 completion tokens, hidden). Through `providers/openai` (`TestLiveNIM`, `NVIDIA_LLM_MODEL=openai/gpt-oss-20b`): 0.92 s hello with usage, tool executed once, sensible answer.
  - `nvidia/nemotron-3-super-120b-a12b` (Pipecat's default): 0.65, 4.19, 4.64 s, tool call 0.75 s; with ` /no_think` appended to the system prompt still reasoning, 2.44 and 0.67 s.
  - Default chosen: `openai/gpt-oss-20b` (lower and steadier first token on this small sample). Both are reasoning models; the OpenAI processor cannot pass `reasoning_effort` or `chat_template_kwargs`. Set `model` per agent to override.
  - Reasoning handling (round 7): `reasoning_content`/`reasoning` deltas count as output, so a failure or the 30 s attempt timeout after thinking started is not retried (no 3x wait and bill). A reply with reasoning but no text raises a non-fatal `openai llm: model returned no text` error (failover class `server_error`) instead of playing silence. The default stays `openai/gpt-oss-20b`, the only measured model with a short, steady think; no non-reasoning NIM model was verified as a better default (llama-3.1/3.3 are end of life, llama-3.2-11b-vision is slower and calls tools twice). Models that think longer than the attempt timeout still fail, by design.
- `meta/llama-3.2-11b-vision-instruct`: 2.1 s hello with no reasoning, usage chunk present, tool calls work but it called the tool twice. A lower-latency, lower-quality option.

## TTS

- Sample rates requested directly from Riva, all accepted: 24000, 22050, 44100 (same text: 1.81 s, 1.77 s, 2.18 s of audio). The provider therefore requests the pipeline rate (24000) and does no local resampling.
- Raw PCM16 mono: first bytes are not `RIFF` at any rate. Spectrum of the output: 80-90 % of energy under 1 kHz, 12-13 kHz band empty, as expected for speech. Largest sample step 15291/32768 on a loud low vowel, no clicks across the join of three sentences.
- Time to first audio (processor, warm channel): 327 ms, 177 ms, 98 ms for three sentences (total 445, 201, 187 ms). The very first call on a cold channel took 1.07 s (TLS + NVCF cold route).
- Voice: the default `Magpie-Multilingual.EN-US.Aria` is valid. An empty voice also works (server default). A bad voice returns `InvalidArgument: Model is not available on server: Voice No for language en-US not found` (non-retryable).

## STT

Input was the TTS output downsampled to 16 kHz (6.5 s, three sentences) and `e2e/fixtures/voice-debug.wav` ("What is two plus two?"), streamed at real time.

- First interim 414 ms after the first audio. Interims are cumulative, 3-4 per second.
- Multi-sentence clip: no mid-stream final (the pauses are short); one final on half-close, 162 ms after the user stopped, with punctuation and capitalisation: "The quick brown fox jumps over the lazy dog. Please turn left at the next intersection. Thank you for calling and have a wonderful day." (`ai-parakeet-ctc-0_6b-asr`: "Thank. thank you ..." glitch in that sentence; 84 ms drain.)
- `voice-debug.wav`: interims "what is two plus two", server endpointing produced a final "What is two plus two?" 3.2 s in, while the clip continued to 22 s. After the user-stopped event nothing was pending, so no `EndOfUtterance` candidate followed (the final was already delivered with `Final=true`). The turn pipeline works from that final; flagged as a behaviour, not a failure.
- Config shape accepted as sent (LINEAR_PCM, 16000, en-US, punctuation on, one alternative, empty model). Keepalive: the stream stayed open through 19 s of trailing silence; no GOAWAY seen. A long-idle (minutes) stream was not tested.

## Full voice turn (tools/livecheck, `-providers nvidia`)

`GOVOICE_LIVE=1 NVIDIA_STT_FUNCTION_ID=... NVIDIA_TTS_FUNCTION_ID=... go run ./tools/livecheck -providers nvidia -only S1 -variants off,on -s1-turns 2`, with the key in `examples/.env`. Real server child process, RTVI WebSocket, question "What is two plus two?", all-nvidia agent.

| turn.manager | turn | transcript heard | reply | voice-to-voice | stt final after VAD stop | LLM ttft | TTS ttfb |
|---|---|---|---|---|---|---|---|
| off | 1 | "What is two plus two?" | "Four." | 2.09 s | 0.2 s | 1.6 s mean (n=2) | 0.21 s mean |
| off | 2 | same | "Four." | 3.62 s | 0.2 s | | |
| on (min_words 3) | 1 | same | "Two plus two equals four." | 2.58 s | 0.9 s mean (n=2) | 3.3 s mean | 0.23 s mean |
| on | 2 | same | "Two plus two equals four." | 7.79 s | | (one turn waited 6 s for the first token) | |

This run used `nvidia/nemotron-3-super-120b-a12b`, the default at the time; it was not repeated with `openai/gpt-oss-20b`. Latency is dominated by the LLM first token (reasoning model). Turn.manager on adds the end-of-turn wait (about 0.7 s more between VAD stop and the final here), which is expected; the 7.8 s second turn is the 6 s first-token outlier, not the turn manager. With STT and TTS alone the voice-to-voice floor is about 1 s.

## Caveats

- The Riva proto and the grpc stack needed no fixes: headers `function-id` + `authorization`, TLS to NVCF, config message and keepalive all worked first time. Only the LLM default model was wrong.
- Error bodies from NIM 404s name the NVIDIA account id; it reaches the operator's error frames and logs (truncated to 512 bytes). Not a secret, but do not paste such logs publicly.
- NVCF function ids and the model catalog change; re-run `go test -tags integration` in `providers/nvidia` (`live_test.go`) and `providers/openai` (`nvidia_live_test.go`) before trusting these ids.
- Not covered: self-hosted Riva/NIM, non-English, model-name field for self-hosted multi-model Riva, idle streams over minutes, concurrent sessions, failover with live fallbacks.

## Reproduce

```
cd providers/nvidia
set -a; . ../../examples/.env; set +a
NVIDIA_TTS_FUNCTION_ID=877104f7-e885-42b9-8de8-f6e4c6303969 NVIDIA_STT_FUNCTION_ID=1598d209-5e27-4d3c-8079-4751568b1081 \
  NVIDIA_LIVE_OUT=$(mktemp -d) go test -tags integration -run 'TestLive_' -v .
cd ../openai && go test -tags integration -run TestLiveNIM -v .   # NVIDIA_LLM_MODEL=... to try another model
```
