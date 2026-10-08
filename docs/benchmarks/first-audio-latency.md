# First-audio latency breakdown (S1, short reply)

Where the time from end of the user clip to first bot audio goes, per stage, on the agent-level live harness (`tools/livecheck`, scenario S1,
"What is two plus two?", 1 turn per cell). **n = 1 per cell, one machine, 2026-10-08: a map of where time goes, not a benchmark.**
Command per cell: `GOVOICE_LIVE=1 make agent-matrix PROFILES=<p> MODES=<m> SCENARIOS=S1 MAX_CALLS=<n> LIVE_ARGS="-s1-turns 1 -clips /tmp/lc-clips"`.

## How it is measured (no extra provider calls)

S1 now stores an additive split per turn (`t<N>.bd_*_ms` in results, and a second table in the generated `agent-matrix.md`), taken from the existing server
timeline (`/debug/timeline`, same host and clock as the client) plus the client event log:

| column | from | to |
|---|---|---|
| vad | client clip end | server `speech_stop` (Silero: 25 chunks x 32 ms = 800 ms below 0.35, less the clip's 100 ms trailing silence) |
| eot | `speech_stop` | user message reaches the LLM (`stt_result`): STT final + turn commit |
| ttft | user message | first LLM token (`llm_ttft`) |
| unit wait | first token | first TTS unit dispatched (`tts_unit`: sentence / first-clause wait) |
| tts | first unit | first bot audio (`tts_ttfb`, observer event time) |
| tail | server first audio | client receives it (transport, WebSocket) |

Each stage is the earliest event of its type after the previous one, so the six parts telescope to the client v2v (same host, same wall clock; off by at most a few ms of rounding). The Prometheus `voice_latency_seconds` stages (`stt_final`, `llm_ttft`, `tts_ttfb`) agree with the split
(identical values in `off`, where turn commit adds nothing).

## Results (ms)

| profile | mode | v2v | vad | eot (STT final + commit) | ttft | unit wait | tts first byte | tail | reply |
|---|---|---|---|---|---|---|---|---|---|
| xai | off | 2157 | 740 | 375 | 559 | 22 | 458 | 0 | "Two plus two is four." |
| xai | on (turn manager, min_words 3) | 3066 | 740 | 904 | 535 | 106 | 778 | 0 | "Two plus two eq..." |
| google | off | 3798 | 740 | 1218 | 705 | 0 | 1133 | 0 | "2 + 2 = 4." |

Share of v2v: xai off: vad 34%, eot 17%, ttft 26%, tts 21%. google off: vad 19%, eot 32%, ttft 19%, tts 30%.
"merge" mode was not run: a one-sentence reply has no second unit, so it cannot change first audio (see S9 in `tts-unit-sizing.md`).

## Where the time goes

1. **The VAD end-of-speech hangover is a fixed 740 ms** in every cell (clip end to `speech_stop`), 19 to 34% of the total and the single largest item for xAI. Nothing downstream can start before it. It is `StopChunks: 25` x 512-sample (32 ms) chunks below `StopThresh` 0.35 in `vad.DefaultProcessorConfig()` (`vad/processor.go`) = 800 ms of silence, minus the 100 ms trailing margin `tools/livecheck` keeps on the clip, plus chunk alignment. It is hard-coded: `server/server.go` always uses the default and there is no `vad.*` yaml key for it (Pipecat's equivalent `stop_secs` also defaults to 0.8 s).
2. **STT final differs by 3x between providers:** xAI 375 ms vs Google chirp_2 1218 ms (no interim results; it finalizes only after its own endpointing). This is the whole Google-vs-xAI gap in `eot` (+0.84 s).
3. **The turn manager (`on`) adds about 530 ms** at the commit step (eot 904 vs 375, same STT). The `turn.speech_timeout` (700 ms, counted from the VAD stop) explains about 325 ms of it (700 minus the 375 ms STT final it hides); the other ~200 ms is unexplained at n = 1. v2v is +0.9 s on this pair, the rest being run-to-run TTS variance (tts 778 vs 458).
4. **LLM time to first token is 0.54 to 0.71 s** on both, about a quarter of the total.
5. **TTS first byte: xAI 0.46 to 0.78 s, Google 1.13 s for an 11-character unit.** Both are unary: nothing plays until the whole unit is synthesized. Google's 1.13 s for 11 chars is mostly fixed overhead, so a shorter unit buys it little; length matters more for xAI.
6. **Unit wait is negligible for a short reply (0 to 106 ms)** and tail (transport) is ~0 on localhost: neither explains the earlier numbers.
7. **Why the earlier single runs were 4.0 s (xAI) and 6.35 s (Google)** (S9, long reply "Tell me about the planet Mars."): the same fixed costs (vad + eot + ttft = 1.7 s xAI, 2.7 s Google) plus, presumably, a much larger first unit. **Inference only, not measured in this run** (S9 estimates 16 to 19 requests per cell, over the 20-request budget): the xAI extra of about 1.9 s over this run and the Google extra of about 2.5 s are first-unit wait plus synthesis of a first sentence of up to 150 chars; the S9 note already records a 3.5 s mean Google TTS first byte. Against it: xAI S9 with `merge` (first unit cut at a clause after 40 chars) was 3.95 s vs 4.07 s off, within noise, so the first unit may not be the whole story.

## Ranked cheapest fixes (expected gain from the measured numbers)

Keys verified in `server/config.go`.

| # | fix | cost | expected gain | confidence |
|---|---|---|---|---|
| 1 | With `turn.manager` on, do not stack the full silence timeout: `turn.stop: [eager, speech-timeout]` (`turn.eager_grace` default 200 ms; `speech-timeout` must stay in the list) or a lower `turn.speech_timeout` (default 700 ms) | config | eager: up to about 530 ms in `on` mode (eot 904 to about 375); lowering `speech_timeout` by X saves at most X | measured delta, n = 1; eager can split "Book a table. For two." into two turns |
| 2 | Google: STT model with interim results, `latest_long` (agent STT `model`), or the xAI STT | config/profile | `latest_long`: about 0.5 s (`turn-manager-live.md`, n = 3); xAI STT: up to about 0.8 s (eot 1218 to 375, n = 1) | measured |
| 3 | Shorten the VAD stop (`StopChunks` 25 = 800 ms): 15 to 18 chunks (480 to 576 ms) standalone; about 200 to 300 ms only with a turn analyzer holding the turn (`turn.stop: [turn-analyzer, speech-timeout]`, Pipecat's smart-turn setup) | code: no yaml key today (add one, e.g. `vad.stop_chunks`) | 220 to 320 ms on every reply, every provider (arithmetic, not noisy) | fixed cost measured; false-cut rate at shorter values unmeasured |
| 4 | Small first TTS unit on long replies: `first_clause_chars: 40` (implied by `tts_merge.enabled`) | config | **inferred only**: sim -0.25 to -0.45 s mean on average replies (`tts-unit-sizing.md`); the 1 to 2 s on long replies is a guess, and xAI S9 `merge` live was -0.12 s (noise). Short replies: 0 (unit wait is 0 to 106 ms) | unproven, needs an S9 run (about 16 to 19 requests/cell) |
| 5 | Google TTS: faster voice/provider (1133 ms for 11 chars vs xAI 458 to 778) | provider choice | 0.4 to 0.7 s | n = 1, inside the ~300 ms noise at the low end |
| 6 | Faster LLM / non-reasoning model | provider choice | 0.1 to 0.3 s (ttft 0.54 to 0.71 s) | low |

Not worth chasing: unit wait on short replies and transport (both about 0).

## Caveats

- n = 1 per cell: xAI TTS first byte varied 458 to 778 ms between two runs, so differences under about 300 ms are noise.
- `eot` ends at the `stt_result` timeline frame (the user message passing the server's frame observer), which is the LLM request to within a frame; `tail` of 0 is localhost, not a real network.
- `merge` and the long-reply scenario were not run for budget; fix 4 is the main open question.

## Request accounting

16 measured provider requests in total (cap 20): 4 per cell, 4 cells (xai off twice: the first run exposed that the timeline has no `llm_start`/`tts_audio`
events, so the harness was changed to use `stt_result` and the observer `latency` events, then rerun; google off; xai on). The question clip was cached, no synthesis spent. Per-cell upper-bound estimate was 8 to 9.

## Fix measurements (L2)

### Code change: `vad.stop_ms`

The VAD stop hangover is now a config key: `vad.stop_ms` (default 800, validated 200..2000, rounded up to 32 ms chunks; `server.Config.VADProcessorConfig()` feeds `vad.NewProcessor` per call, so it also covers `vad.mode: process`, where only inference runs in the worker and the stop decision stays in the call's processor). Default behavior is byte-for-byte unchanged (25 chunks); the shipped YAMLs and both effective-config goldens carry `stop_ms: 800`. Tests: `TestConfig_VADStopMS` (default, chunk mapping, bounds), `TestDetectHonorsStopChunks` (the processor stops after exactly N quiet chunks).
Harness: new modes `eager` (`turn.stop [eager, speech-timeout]`, `eager_grace 200ms`), `vad500` (`vad.stop_ms 500`, no turn manager), `vad300+smart` (`vad.stop_ms 300`, `turn.stop [turn-analyzer, speech-timeout]`, no `min_words`), and `-s1-short` (the estimate assumes 2 TTS requests per reply instead of 5 for a one-sentence prompt; the measured cap is unchanged). The estimator adds one LLM request for `eager` (a split turn).

### Live comparison (xAI, S1 "What is two plus two?", **n = 1 per variant, one machine**)

| variant | v2v | vad | eot (STT final + commit) | ttft | unit wait | tts first byte | reply |
|---|---|---|---|---|---|---|---|
| baseline (off) | 3360 | 840 | 422 | 744 | 11 | 1341 | "4." |
| eager (`[eager, speech-timeout]`, grace 200 ms) | 2804 | 841 | 653 | 701 | 24 | 583 | "2 + 2 equals 4." |
| vad_stop 300 + `[turn-analyzer, speech-timeout]` | 2250 | 360 | 665 | 544 | 58 | 621 | "2 + 2 equals 4." |
| vad_stop 500, no turn manager | not run | | | | | | |

How to read it, with the noise stated first:
- **Noise is larger than most differences.** The same `off` cell was 2157 ms in L1 and 3360 ms here: tts first byte 458 vs 1341 ms, ttft 559 vs 744 ms. Anything under about 1 s on v2v is not distinguishable at n = 1. Only the `vad` column is deterministic.
- The part that does not depend on the LLM or TTS is `vad + eot`: off 1262, eager 1494, smart 1025 ms (L1: off 1115, manager-on 1644). Everything after it (ttft, unit wait, tts) varied independently of the setting.
- **`vad300+smart`**: the VAD stage fell 480 ms (840 to 360) exactly as arithmetic predicts; the commit step cost +243 ms vs off (665 vs 422, analyzer plus the manager), net about -240 ms on the pre-LLM path in this one sample. The eot half is not deterministic (the same `off` eot moved about 150 ms between L1 and L2), so the net is only known to be somewhere between roughly -100 and -480 ms until n >= 5. Its v2v of 2250 is the best cell, but about 600 ms of the gap to `off` is ttft/tts variance. Replies were correct in all cells (wer 0.00).
- **`eager` did not help here**: eot 653 vs 422 (+230 ms), no gain over off at n = 1. The expected saving was against `on` with `min_words` (eot 904), not against `off`; eager is a fix for the turn manager's stacked timeout, not a gain over no manager. It only makes sense where a turn manager is needed anyway.
- `vad500` was not run: the estimator refused it once the remaining budget (5) fell under its per-cell upper bound (6). Its effect on the vad stage is the same arithmetic (800 to 500 ms: -300 ms).

### False-cut risk of a shorter VAD stop (free, no provider calls)

A cut happens exactly when a mid-utterance pause is at least `stop_ms` long (below `StopThresh` 0.35 for that long): that part is arithmetic and `TestDetectHonorsStopChunks` covers it. What matters is how long real in-sentence pauses are. `make eval-turn` cannot tell: its dataset is transcripts without timing (speech-timeout false_cut 1.0 by construction, smart-turn skipped). I ran the real Silero model (the shipped `silero_vad.onnx`, same thresholds and 2-chunk start rule) over the 18 clips in `assets/speech` (59 s of read speech) and measured every silence inside speech of at least 96 ms:

| vad stop | chunks | internal gaps that would cut mid-utterance |
|---|---|---|
| 224 ms | 7 | 2 of 16 |
| 320 ms | 10 | 0 |
| 512 ms | 16 | 0 |
| 800 ms | 25 | 0 |

All 16 gaps were 128 to 256 ms (median 160). **Caveat, important:** these are 18 fluent, scripted clips (16 gaps in total) with no hesitation, so the pause lengths are a lower bound and 0 cuts at 320 ms is a best case, not a rate. People pausing to think routinely pause longer than 300 ms, so 300 and 500 ms will split such utterances; this test cannot measure that rate. The cheap protection is what `vad300+smart` does: with `turn-analyzer` holding the turn, a VAD stop on an incomplete sentence does not end the user's turn, so the false cut is paid as a wait, not a split. Without an analyzer (plain `vad500`), every pause of 500 ms or more is a split turn.

### Recommendation (latency-first profile; defaults not changed)

1. `vad.stop_ms: 300` together with `turn: {manager: true, stop: [turn-analyzer, speech-timeout]}`: 480 ms off the VAD stage by arithmetic, about 240 ms net of the analyzer/manager commit cost in one sample (see above), with the analyzer turning most false cuts into a wait (a degraded model falls back to speech-timeout and can still split). Best single change; confirm with n >= 5 and a hesitation clip before adopting.
2. If no turn manager is wanted: `vad.stop_ms: 500` (saves 300 ms; expect some split turns on hesitations) rather than 300.
3. Use `turn.stop: [eager, speech-timeout]` only when the turn manager is on already (it removes the stacked `speech_timeout` seen in `on` mode); do not add the manager just for eager.
4. Keep 800 as the default.

### Request accounting (L2)

15 measured provider requests (cap 20): 3 from a first attempt that failed because the provtest clip is IMA ADPCM and the harness loaded it as raw PCM (no useful data; the question clip was then converted with ffmpeg into the local cache), 8 for `off` + `eager` (4 each), 4 for `vad300+smart`. The plan preview refused `vad500` with the 5 remaining. n = 2 per variant was not possible under the cap (per-cell upper bound 6 to 7, measured 4).
