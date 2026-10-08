# Provider conformance matrix

Stage-level conformance suite (`tools/provtest`): the same checks run against every STT, TTS and LLM provider, built through the server's own registry. Add a provider by adding one row to `Table` in `tools/provtest/providers.go`. Providers without credentials are skipped, not failed.

Reproduce: `GOVOICE_LIVE=1 make provider-matrix PROVIDERS=google,nvidia STAGES=stt,tts,llm` (needs `examples/.env`; spends about 17 provider requests per provider; `-write-docs` rewrites this file). Without credentials the logic is unit-tested against fakes: `go test ./tools/provtest/`. `-merge a.json,b.json` re-renders saved results without calls. xAI STT/TTS compile only with `make provider-matrix TAGS=xaispeech`.

## Checks

Numbers in a cell are the check's headline value (ms, s, WER, characters).

- **TTS**: synth (3 sentences as synthesis units), format (PCM16 mono at the 24 kHz pipeline rate, exact 20 ms frames), final_flags (one IsFinal per unit as its last frame, TurnFinal only on the turn's last), stopped (TTSStopped after each unit), usage_chars (billed characters equal text), ttfa (first audio per unit, value = the cold first unit), duration_plausible (0.02-0.25 s of audio per character), non_silent (RMS), no_riff (raw PCM, no WAV header), total_time, interrupt (barge-in stops audio within 300 ms, the cancelled unit neither completes nor errors; an explicit skip when the unit finished before the barge-in), bad_credential (client error class, no retry: under 10 s, secret absent from the error), close_leaks (session tasks and goroutines after Close).
- **STT** (16 kHz clips at real time with UserStarted/Stopped and TurnEvent Started/Stopped/Completed): wer (per clip against the known text after normalisation: case, punctuation, hyphens, small numerals, common contractions; per-clip limit max_wer in clips.json, 0.20-0.25 = one error on the short clips; Google and NVIDIA score 0.00 on every clip), interim_before_final (skip when the provider emits no interims), final_latency (final after the VAD stop), cumulative (each final extends the previous one), turn_binding (candidates carry the started turn and generation), resumed_speech (two sentences separated by a 1 s pause in one turn keep both), reuse_after_idle (second turn after an idle gap), barge_in_rebind (same-turn TurnEventStarted at a newer generation keeps results flowing under it), bad_credential, close_leaks.
- **LLM**: stream (deltas equal the complete text and answer 2+2), ttft, total_time, usage (prompt and completion tokens), tool_round_trip (get_weather executed with valid JSON arguments whose city is Paris, and the reply uses the result), cancel (deltas stop within 300 ms of a barge-in; an explicit skip when the reply completed first), bad_credential, bad_model (both client errors, redacted), close_leaks.

A row passes only when every check that ran passed; a row the call budget (-max-calls) cut short is a skip ("incomplete"), and once the budget is spent the remaining rows are not built or called.

Fixtures: tools/provtest/testdata/clips (Google TTS, resampled to 16 kHz, IMA ADPCM WAV, under 100 KB, decoded to PCM16 before streaming; the codec keeps about 34 dB SNR on these clips, so plain PCM would not change WER).

## Environment

Values come from `examples/.env` and are never printed; `go run ./tools/provtest/cmd/provtest -list-env` shows which rows would run. Planned units are an upper-bound estimate; the actual column is measured at the provider boundary (retries included). xai.llm uses the server default endpoint (`providers.xai_llm.endpoint`, the Responses API that tools need); `XAI_ENDPOINT` is an examples/ override production never reads and is ignored.

| provider | stage | build tag | needs | optional |
|---|---|---|---|---|
| google | stt | - | GOOGLE_APPLICATION_CREDENTIALS |  |
| xai | stt | xaispeech | XAI_API_KEY |  |
| deepgram | stt | - | DEEPGRAM_API_KEY |  |
| cartesia | stt | - | CARTESIA_API_KEY |  |
| elevenlabs | stt | - | ELEVENLABS_API_KEY |  |
| nvidia | stt | - | NVIDIA_API_KEY | NVIDIA_STT_FUNCTION_ID |
| google | tts | - | GOOGLE_APPLICATION_CREDENTIALS or GOOGLE_API_KEY |  |
| xai | tts | xaispeech | XAI_API_KEY |  |
| cartesia | tts | - | CARTESIA_API_KEY |  |
| elevenlabs | tts | - | ELEVENLABS_API_KEY |  |
| nvidia | tts | - | NVIDIA_API_KEY | NVIDIA_TTS_FUNCTION_ID |
| google | llm | - | GOOGLE_API_KEY |  |
| xai | llm | - | XAI_API_KEY | XAI_MODEL |
| openai | llm | - | OPENAI_API_KEY | OPENAI_BASE_URL, OPENAI_MODEL |
| nvidia | llm | - | NVIDIA_API_KEY | NVIDIA_LLM_MODEL |

Run 2026-10-08 15:14 UTC, mode `live`, 22 provider calls (STT + TTS for google and nvidia) (summed over merged runs), repeat 1. Numbers are small-n timings from one machine (medians over repeats), not benchmarks. No secrets appear here.

### STT

| provider | result | build | no_error | wer | interim_before_final | final_latency | cumulative | turn_binding | resumed_speech | reuse_after_idle | barge_in_rebind | bad_credential | close_leaks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| google | pass | pass | pass | pass 0.00 | skip | pass 794 ms | pass | pass | pass 0.00 | pass | pass 0.00 | pass 1653 ms | pass |
| xai | pass | pass | pass | pass 0.00 | pass 1457 ms | pass 197 ms | pass | pass | pass 0.00 | pass | pass 0.00 | pass 554 ms | pass |
| deepgram | skip: needs DEEPGRAM_API_KEY | | | | | | | | | | | | |
| cartesia | skip: needs CARTESIA_API_KEY | | | | | | | | | | | | |
| elevenlabs | skip: needs ELEVENLABS_API_KEY | | | | | | | | | | | | |
| nvidia | pass | pass | pass | pass 0.00 | pass 230 ms | pass 83 ms | pass | pass | pass 0.00 | pass | pass 0.00 | pass 1653 ms | pass |

### TTS

| provider | result | build | no_error | synth | format | final_flags | stopped | usage_chars | ttfa | duration_plausible | non_silent | no_riff | total_time | interrupt | bad_credential | close_leaks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| google | pass | pass | pass | pass | pass | pass | pass | pass 134 | pass 1687 ms | pass 7.7 s | pass | pass | pass 3.9 s | pass | pass 92 ms | pass |
| xai | pass | pass | pass | pass | pass | pass | pass | pass 134 | pass 851 ms | pass 8.3 s | pass | pass | pass 2.7 s | pass | pass 113 ms | pass 1 goroutines |
| cartesia | skip: needs CARTESIA_API_KEY | | | | | | | | | | | | | | | |
| elevenlabs | skip: needs ELEVENLABS_API_KEY | | | | | | | | | | | | | | | |
| nvidia | pass | pass | pass | pass | pass | pass | pass | pass 134 | pass 475 ms | pass 6.5 s | pass | pass | pass 0.9 s | pass | pass 189 ms | pass |

### LLM

LLM rows are from the 2026-10-08 14:58 UTC run (12 calls), before the tool-argument JSON check and the cancel no-op skip were added; not re-run here to stay within the call budget.

| provider | result | build | no_error | stream | ttft | total_time | usage | tool_round_trip | cancel | bad_credential | bad_model | close_leaks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| google | pass | pass | pass | pass | pass 915 ms | pass 0.9 s | pass | pass | pass | pass 33 ms | pass 132 ms | pass 1 goroutines |
| xai | pass | pass | pass | pass | pass 629 ms | pass 0.6 s | pass | pass | pass | pass 164 ms | pass 124 ms | pass 1 goroutines |
| openai | skip: needs OPENAI_API_KEY | | | | | | | | | | | |
| nvidia | pass | pass | pass | pass | pass 933 ms | pass 1.0 s | pass | pass | pass | pass 100 ms | pass 24 ms | pass 1 goroutines |

xAI rows (grok-4.20-non-reasoning on the Responses API, STT grok-voice-transcribe-2.0, TTS voice eve) are from the 2026-10-08 xAI run, after the fixes in [xai-live.md](xai-live.md); the google and nvidia rows above are unchanged.
