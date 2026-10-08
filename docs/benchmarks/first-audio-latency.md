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
