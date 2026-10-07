# Latency baseline (P0-4): framework overhead, fake providers

Captured 2026-10-07 on Apple M4 Pro, 12 cores, darwin/arm64, go1.27.1, before the T1 turn-manager wiring landed (numbers may shift slightly; re-run and diff).

## What this measures, and what it does not

The tool runs real in-process `core.Session`s with this pipeline: user-context, assistant-context, fake LLM, fake TTS, fake transport output. For each turn it posts a `UserStoppedSpeakingFrame`, waits the simulated STT delay, then posts a `FinalTranscriptionFrame`. The P0-2 `core/observe` Tracker turns the session's debug events into stage latencies. `framework_overhead` = voice_to_voice minus the configured delays.

It **does not** include:

- **Audio input, VAD, STT or the turn manager.** No input audio frames are sent, so Silero VAD and the transport input path never run. The "user stopped" frame is posted directly, and STT is a sleep inside the tool. The `stt_final` row only shows that sleep plus a queue hop, not STT cost.
- **Real provider I/O.** There is no network, no HTTP/2 or websocket framing, and no codec work. Real-provider p50/p95 on an audio corpus, which is the original P0-4 wording, is still open.
- **Output pacing.** The fake transport writes immediately, so real-time playout is not modelled.

So the numbers show how long the frame pipeline itself takes to go from stop to first bot audio, with providers that respond instantly. They are a lower bound, and they say nothing about end-to-end latency a user would hear.

Delays default to 0. Each nonzero delay is a Go timer, and on darwin a timer overshoots by about 1.5-2 ms. That is two orders of magnitude more than the framework cost, so with delays set, `framework_overhead` mostly measures OS timer slop (about 3 x 2 ms below). Use delays only to get a realistic turn shape. Use the zero-delay rows for regressions.

Percentiles are nearest-rank. When N < 100, p99 equals max, and when N < 20, p95 equals max as well. Values are rounded to 0.01 ms (the JSON keeps microsecond precision).

Command: `make bench-latency`. Pass flags through `BENCH_ARGS`, e.g. `BENCH_ARGS="-sessions 8 -json"`. No paid calls.

## 1 session x 200 turns, zero delay (default)

| stage | n | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|
| stt_final | 200 | 0.00 | 0.00 | 0.01 | 0.02 |
| llm_ttft | 200 | 0.00 | 0.01 | 0.02 | 0.03 |
| tts_ttfb | 200 | 0.00 | 0.02 | 0.03 | 0.03 |
| voice_to_voice | 200 | 0.01 | 0.03 | 0.04 | 0.07 |
| framework_overhead | 200 | 0.01 | 0.03 | 0.04 | 0.07 |

goroutines 1 -> 1, RSS 5920 KB -> 9360 KB

## 8 concurrent sessions x 200 turns, zero delay (`-sessions 8`)

Each session starts its next turn as soon as the previous one gets first audio, so this is a CPU-saturating closed loop and not a realistic call mix. The tail comes from goroutine scheduling under contention.

| stage | n | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|
| stt_final | 1600 | 0.00 | 0.01 | 0.03 | 0.27 |
| llm_ttft | 1600 | 0.00 | 0.04 | 0.21 | 0.39 |
| tts_ttfb | 1600 | 0.00 | 0.05 | 0.21 | 0.39 |
| voice_to_voice | 1600 | 0.01 | 0.20 | 0.33 | 0.66 |
| framework_overhead | 1600 | 0.01 | 0.20 | 0.33 | 0.66 |

goroutines 1 -> 1, RSS 6080 KB -> 16512 KB

## Realistic shape: STT 100 ms, LLM TTFT 200 ms, TTS 100 ms (`-stt 100ms -llm 200ms -tts 100ms -turns 50`)

`framework_overhead` here is about three timers' worth of darwin slop, not framework cost. Compare it with the zero-delay rows above.

| stage | n | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|
| stt_final | 50 | 101.80 | 102.05 | 102.10 | 102.10 |
| llm_ttft | 50 | 201.81 | 202.08 | 202.27 | 202.27 |
| tts_ttfb | 50 | 101.52 | 102.08 | 102.14 | 102.14 |
| voice_to_voice | 50 | 404.72 | 406.13 | 406.55 | 406.55 |
| framework_overhead | 50 | 4.72 | 6.13 | 6.55 | 6.55 |

goroutines 1 -> 1, RSS 5952 KB -> 7504 KB. With 8 sessions x 25 turns: voice_to_voice p50/p95/p99 is 404.03/406.16/407.31 ms.

## Comparing

Run `go run ./tools/eval/cmd/govoice-bench -json > new.json` before and after a change with the same flags, on the same machine. Compare `Overhead.P50/P95/P99` and `GoroutinesAfter`. These numbers depend on the host. `make perf-gate` (P5-5) does this comparison against a per-machine baseline with absolute tolerances; see [README.md](README.md).

The goroutine check only catches goroutines that survive `Session.Close` for the components listed above. RSS comes from `ps` and reflects the allocator's high-water mark, so a rise is not a leak signal on its own. The P5-4 soak should track heap from `runtime/metrics` over hours.

The startup stage is not reported. In this tool it would only measure the `observe.Attach` call between `NewSession` and `Start`, and core truncates it to whole milliseconds (always 0).

## Using real VAD and audio (not done)

Running the real Silero path would need all of the following. That is a P5-4-sized change, not a flag:

- the ONNX Runtime shared library and `third_party/silero_vad.onnx`, a native dependency the CI gate does not have today
- a `vad.Executor` plus a `vad.Processor` in the pipeline
- a speech corpus from `assets/speech` fed at real-time pace (the VAD stop hangover would then appear in voice_to_voice)
- a fake STT that emits a final transcript after the VAD stop
