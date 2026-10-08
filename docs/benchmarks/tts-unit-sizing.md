# TTS unit sizing for unary providers (xAI HTTP, Google)

Offline simulator: `make tts-sim` (`tools/ttssim`, no network, no keys, free). Unary means one request per synthesis unit and the
whole unit's audio returns before any of it can play, so unit size trades requests and gaps against first-audio latency.
The simulator calls the production segmenter (`core.SplitNext`, a thin export of `nextSplit`), so `min`, `max` and
`first_clause_chars` follow the same rules as in `AssistantContextProcessor` (`max` fixed at the default 150).

## Model

- LLM: first token after 600 ms, then 40 / 80 / 150 chars/s in 60 ms deltas. Replies are 2-6 sentences drawn from an 18-sentence pool (5 to 120 chars,
  mean reply about 220 chars). 300 replies per cell and cps, fixed seed.
- TTS: latency = overhead + chars / rate, requests strictly in order (as `core/ttsunit` runs them). Playback is real time at the measured
  audio per char (xAI 60 ms, Google 57 ms). A gap is silence over 100 ms between two units.
- Data: `tools/ttssim/testdata/tts_timing.json`, a live `provider-matrix` capture (2026-10-08, `STAGES=tts PROVIDERS=xai,google TAGS=xaispeech`,
  11 requests): three 42-48 char units per provider took 1.02-1.27 s each (Google 1075-1275 ms, xAI 1018-1251 ms). **Overhead and rate cannot be
  fitted from three near-equal lengths**, so the sim fixes the overhead (300 / 800 / 1000 ms) and sets the rate so that 45 chars take the measured
  1.17-1.18 s. The qualitative conclusions below hold across all three; the magnitudes shift with overhead.
- Limits: Google 250 chars per request (pinned in `providers/google/tts.go`; longer units are split into extra requests, the sim counts them). xAI: no limit
  is pinned in this repo and I could not check the docs offline; the sim uses 600 as a practical cap.

## Strategies

- `current`: one unit per sentence, `min` 10, no first clause.
- `minN`: the production `MinUnitLength` raised to N (sentences shorter than N merge into the next one).
- `slack fF mM [capC]`: first unit may end at a clause after F chars (`first_clause_chars`); sentences are then held in a pending unit and sent at the last
  moment that its estimated synthesis (queue-aware: `max(now, synth_free) + overhead + chars/rate`) still finishes M ms before the queued audio runs out,
  or immediately when merging the next sentence would break that, or at the cap, or at reply end. Never sends later than the current rule would for
  the first unit.

## Results (xAI, overhead 800 ms, LLM 40 cps; the case where gaps are worst)

| strategy | first audio mean / p95 s | gap-free replies | gaps/reply | gap s/reply | requests/reply | chars/reply |
|---|---|---|---|---|---|---|
| current (min10) | 3.76 / 5.87 | 71% | 0.30 | 0.36 | 3.51 | 219 |
| min40 | 4.48 / 6.63 | 86% | 0.14 | 0.12 | 2.95 | 220 |
| min60 | 5.26 / 7.23 | 98% | 0.02 | 0.01 | 2.48 | 220 |
| min80 | 5.65 / 7.23 | 99% | 0.01 | 0.00 | 2.25 | 220 |
| slack f0 m300 | 3.76 / 5.87 | 71% | 0.30 | 0.36 | 2.80 | 220 |
| slack f40 m300 | 3.31 / 5.79 | 70% | 0.32 | 0.37 | 3.09 | 220 |
| slack f30 m300 | 3.23 / 5.79 | 62% | 0.39 | 0.40 | 3.12 | 220 |
| slack f20 m300 | 2.91 / 4.56 | 54% | 0.47 | 0.59 | 3.28 | 219 |

At 150 cps (fast LLM), same provider: current 2.31 s / 95% gap-free / 3.51 req; min40 2.59 s / 100% / 2.95; slack f0 m300 2.31 s / 95% / 2.27;
slack f40 m300 2.13 s / 95% / 2.40. Pooled over 40/80/150 cps and the overhead sweep (full tables: `make tts-sim`):

| provider, overhead | current: first audio, gap-free, req | min40 | slack f0 m300 | slack f40 m300 |
|---|---|---|---|---|
| xAI 300 ms | 3.17 s, 79%, 3.51 | 3.86 s, 92%, 2.95 | 3.17 s, 79%, 2.66 | 2.73 s, 77%, 2.90 |
| xAI 800 ms | 2.97 s, 86%, 3.51 | 3.44 s, 95%, 2.95 | 2.97 s, 85%, 2.51 | 2.66 s, 85%, 2.72 |
| xAI 1000 ms | 2.88 s, 88%, 3.51 | 3.28 s, 96%, 2.95 | 2.88 s, 88%, 2.44 | 2.64 s, 88%, 2.61 |
| Google 300 ms | 3.18 s, 77%, 3.51 | 3.88 s, 91%, 2.95 | 3.18 s, 77%, 2.71 | 2.75 s, 74%, 2.96 |
| Google 800 ms | 2.98 s, 83%, 3.51 | 3.46 s, 95%, 2.95 | 2.98 s, 83%, 2.59 | 2.68 s, 82%, 2.79 |
| Google 1000 ms | 2.90 s, 85%, 3.51 | 3.30 s, 95%, 2.95 | 2.90 s, 85%, 2.53 | 2.65 s, 85%, 2.71 |

## Findings

1. **Most gaps are LLM-bound, not synthesis-bound.** With an LLM at 40 cps the next sentence has not finished generating when the previous
   unit's audio ends (speech is about 16 chars/s). No sizing rule that only decides *when to send* completed text can close those gaps; only waiting
   longer before starting playback does, which is what a larger `min` effectively does. Gaps nearly vanish at 150 cps.
2. **Slack-aware merging is free requests.** `slack f0` has the same first-audio latency and the same gap-free rate as `current` (within 1 point, noise) at
   20-35% fewer requests (3.51 to 2.3-2.8 per reply; 20% at 40 cps, 35% at 150 cps). The merge only happens when the merged unit's synthesis still finishes before the audio runs out.
   Cost is the same in characters (the same text is sent, 220 chars/reply in every row).
3. **A small first unit buys latency, and costs gaps only when it is too small.** `first_clause_chars` 40 cuts first audio 0.25-0.45 s (mean) and about 1.6 s at
   pooled p95 (only 0.1 s at p95 with a 40 cps LLM) for 0-3 points of gap-free rate and 0.2-0.25 requests; 30 and 20 lose 8-17 points at 40 cps. Below the point where one clause's audio (about 60 ms/char) covers the next
   unit's synthesis (overhead plus chars/rate, about 1.2 s), a gap is unavoidable.
4. **`minN` is the gap-free lever, at a latency price.** At 40 cps min40 is +0.7 s first audio (mean) for +15 points gap-free and -16% requests (pooled: +0.4-0.7 s, +8-14 points);
   min60 reaches 98% gap-free at +1.5 s (pooled: 98-100% at +0.8-1.4 s). It also reduces requests, but only by merging short sentences, not by merging when slack allows.
5. **Margin barely matters in the sim** (0 to 1000 ms adds about 0.2 requests and moves gap-free by at most 1 point; 300 ms costs about 0.06 requests).
   The sim has no latency variance, so it cannot show what the margin buys; that needs live data.
   **The cap rarely binds at 250 or above**: for xAI (800 ms, f40 m300) a 250 cap instead of 600 adds 0.03 requests/reply; 200 adds about 0.1 and 120 about
   0.45 (cap rows). Keep the cap equal to the provider limit.
6. Characters billed are identical across strategies (text is not duplicated), so request count is the only cost lever; the saving is per-request overhead, not billed chars.

## Recommendation

Goal ordering is gap-free first, then first-audio latency, then requests.

- **Rule: first unit small, later units slack-aware** (`slack f40 m300`) for both xAI and Google: first unit ends at a clause after 40 chars
  (`first_clause_chars: 40`; the current default 0 is off); every later sentence is held and merged while a request issued now
  would still finish at least 300 ms before the queued audio ends; send at the last safe moment, at the cap, or at the end of the reply.
  Versus `current` (pooled): first audio -0.25 to -0.45 s mean, gap-free within 3 points, requests -16% to -26%.
- Defaults: `first_clause_chars` 40, `margin` 300 ms, `cap` = provider limit (**Google 250**, **xAI 250** to be conservative; versus 600 it costs about 0.03 requests/reply),
  `min` stays 10, `max` stays 150, flush at reply end. The slack estimate needs per-provider `overhead_ms` and `chars_per_sec` (start: xAI 800 ms and 120 cps, Google 800 ms and 118 cps; an assumed 800 ms fitted to one live point, not a measurement, to be
  replaced by an online running estimate from observed unit wall times).
- If gap-free matters more than first-audio latency (gaps audible with a slow LLM): drop the clause (`first_clause_chars` 0) and set `min_unit_length` 40, which gets 86% gap-free
  at 40 cps and 91-96% pooled. This is the one-line config fallback that needs no K4 work. Going to 60 gets 98-100% (pooled) for another 0.4-0.8 s of latency.
- Not worth building: a gap-free guarantee through sizing alone (finding 1); that needs a playout hold (delay playback until the
  queue covers the remaining projected text), which is a pipeline change outside unit sizing.

## Caveats

- Overhead and rate are assumptions anchored on one live point (3 requests per provider, 42-48 chars, single machine). Run `make provider-matrix` with a longer unit
  (e.g. the 400-char `ttsLong` text timed in `tools/provtest/tts.go`) to fit the slope; the sim reads `units[]` from any `results.json` via `-fixture`.
- Every request takes exactly the model time: no latency variance, though the three live samples already spread 1.02-1.27 s. Gap-free rates are
  optimistic for every strategy, most for slack merging, which schedules right up to the deadline minus the margin.
- The sentence pool is synthetic and short-sentence-heavy; the speech rate is a constant per provider.
- In production the slack estimate uses observed audio durations of in-flight units; the sim assumes the estimate equals the true model (optimistic by the estimator's error, which the 300 ms margin must cover).

## Live confirmation (2026-10-08, n=1 per cell)

Harness: `tools/livecheck` scenario **S9** (one multi-sentence reply to "Tell me about the planet Mars.", turn modes off) under two server modes,
`off` and `merge` (`tts_merge: {enabled: true, margin: 300ms, cap: 250}`, `first_clause_chars` implied 40). Command:
`GOVOICE_LIVE=1 PROFILES=xai,google SCENARIOS=S9 MODES=off,merge LIVE_ARGS="-clips /tmp/lc-clips -out /tmp/lc-out" make agent-matrix`.
S9 measures first audio (end of user speech to first bot audio chunk at the client), TTS requests (server `/metrics` delta), and gaps with a
simulated real-time player (a chunk arriving more than 100 ms after the queued audio ran out). Gaps are not LLM-vs-TTS attributable here.

| profile | mode | TTS requests | reply chars | sentences | first audio | gaps >100 ms | audio |
|---|---|---|---|---|---|---|---|
| xai | off | 12 | 1262 | 7 | 4.07 s | 0 | 61.0 s |
| xai | merge | 6 | 1204 | 7 | 3.95 s | 0 | 61.0 s |
| google | off | 6 | 752 | 6 | 6.35 s | 5 (5.4 s total) | 46.4 s |
| google | merge | no data | | | | | |

- **xAI: half the requests (12 to 6), no gaps in either mode, first audio 3.95 s vs 4.07 s (within noise).** The LLM wrote different replies each time (1262 vs 1204 chars),
  so chars are not identical; requests per 1000 chars were 9.5 vs 5.0. Text was not lost (7 estimated sentences both; reply text present).
- **Google, merge: not measured.** The harness hit its cap: measured provider requests were **41** against `-max-calls 40` (planned upper bound 35).
  Root cause: the estimator had no long-reply shape and counted 9 for an S9 cell that actually cost 15 (xAI off, 12 TTS requests). The watchdog only fires once the
  measured total *exceeds* the cap, so a trip always costs at least one request over it; it ended the Google merge call after 4 of its TTS requests. The partial run
  is discarded. Google off alone had 6 requests, 6.35 s first audio and 5 gaps (5.4 s), with a 3.5 s mean TTS time to first byte (likely the main contributor; not isolated).
  **Google merge is unconfirmed and the run spent 1 request over its budget.** Fixed after the run: S9 now estimates 3x the per-reply TTS (xAI 19, Google 16 per cell,
  so this 4-cell plan estimates 70, 71 with the clip, and is refused at the default cap), and the watchdog polls every 250 ms instead of 1 s.
- Request breakdown (actual, server-side): xai: stt 4, llm 2, tts 18; google: stt 4, llm 2, tts 10; plus 1 Google TTS clip synthesis = 41.
- Versus the simulator: the sim's replies average 220 chars (3.5 requests current, 2.3-2.8 with slack); these replies were 3.4-5.7x longer and `max` 150 splits long sentences, so
  requests are not comparable in absolute terms. The direction (fewer requests at equal text, no first-audio penalty) agrees; the size of the xAI drop (-50%) is larger than the sim's
  -20% to -35%, plausibly because long replies leave the audio queue deep, so merging is almost always allowed. One run per cell: treat as an existence check, not a rate.
- Not confirmed: that a short reply gains first audio from `first_clause_chars` 40 (S9 replies are long and the first sentence is the same length either way), and Google at all.
- Harness note: S9 and the `merge` mode are new (`tools/livecheck/scenarios.go`, `server.go`); the mars clip is synthesized with Google TTS once (cache with `-clips`).
  Results were not written to `agent-matrix.md/json` (no `-write-docs`).
