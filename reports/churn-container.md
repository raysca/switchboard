# govoice load report (load)

2026-10-09 21:15 BST, darwin/arm64, 12 CPUs, go1.27.1, Apple M4 Pro. Machine-specific numbers. Fake providers, localhost, one speech clip.

Config: sessions 16, duration 90 s (ramp 5 s), turns/call 5, think 1000 ms, clip `e2e/fixtures/voice-debug.wav` (1600 ms speech), vad process, turn manager false.

## Verdict: PASS

| check | value | limit | |
|---|---|---|---|
| error_pct | 0 | 1 | PASS failed calls + turn timeouts over attempts |
| admission_rejects | 0 | 0 | PASS limits are set far above -sessions; any reject is a finding |
| leaked_work_sessions | 0 | 0 | PASS  |
| server_sessions_failed | 0 | 0 | PASS server-side call failures (e.g. slow-client terminations) |
| vad_worker_restarts | 0 | 0 | PASS  |
| vad_dropped_chunks | 0 | 0 | PASS VAD overload drops audio |

## Sessions and turns

max concurrent 16 of 16 target; started 68, completed 52, failed 0, rejected 0. Turns 354 attempted, 349 completed, 0 timeouts. Error rate 0.00%.

## Latency (ms)

| metric | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| voice_to_voice (client: last speech chunk sent -> first bot audio) | 349 | 943.2 | 964.1 | 966.1 | 966.7 |
| server voice_to_voice (histogram, bucket resolution) | 349 | 250.0 | 295.0 | 299.0 | 300.0 |
| server stt_final (histogram, bucket resolution) | 349 | 250.0 | 295.0 | 299.0 | 300.0 |
| server llm_ttft (histogram, bucket resolution) | 349 | 25.0 | 47.5 | 49.5 | 50.0 |
| server tts_ttfb (histogram, bucket resolution) | 349 | 25.0 | 47.5 | 49.5 | 50.0 |
| harness pacing lag (late 20 ms sends; load-generator health) | 70169 | 1.0 | 1.0 | 1.0 | 5.0 |

## Resources (server process tree)

| | value |
|---|---|
| CPU seconds | 36.0 (avg 0.40 cores) |
| CPU-seconds per session-minute | 1.540 |
| RSS idle / steady / peak MB | 289 / 240 / 291 |
| MB per session (steady - idle) | 0.00 |
| goroutines idle / peak / after drain | 19 / 228 / 20 |
| heap peak MB | 48 |
| open fds idle / peak / after drain | 84 / 101 / 85 |

Growth, first vs last 20 s window after warmup: RSS -24.5%, heap +1.1%, goroutines +0, fds +0; goroutines +1 vs idle after drain.

Server counters: admission rejects map[], vad dropped 0, vad stale 0, sessions failed 0, stt/llm/tts errors 0/0/0, protocol errors 0, vad worker restarts 0.
