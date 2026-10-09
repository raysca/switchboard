# govoice load report (load)

2026-10-09 20:39 BST, darwin/arm64, 12 CPUs, go1.27.1, Apple M4 Pro. Machine-specific numbers. Fake providers, localhost, one speech clip.

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

max concurrent 16 of 16 target; started 67, completed 51, failed 0, rejected 0. Turns 355 attempted, 350 completed, 0 timeouts. Error rate 0.00%.

## Latency (ms)

| metric | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| voice_to_voice (client: last speech chunk sent -> first bot audio) | 350 | 942.9 | 963.6 | 965.2 | 970.3 |
| server voice_to_voice (histogram, bucket resolution) | 351 | 250.0 | 295.0 | 299.0 | 300.0 |
| server stt_final (histogram, bucket resolution) | 351 | 250.0 | 295.0 | 299.0 | 300.0 |
| server llm_ttft (histogram, bucket resolution) | 351 | 25.0 | 47.5 | 49.5 | 50.0 |
| server tts_ttfb (histogram, bucket resolution) | 351 | 25.0 | 47.5 | 49.5 | 50.0 |
| harness pacing lag (late 20 ms sends; load-generator health) | 70171 | 1.0 | 1.0 | 1.0 | 5.0 |

## Resources (server process tree)

| | value |
|---|---|
| CPU seconds | 32.0 (avg 0.36 cores) |
| CPU-seconds per session-minute | 1.370 |
| RSS idle / steady / peak MB | 236 / 207 / 212 |
| MB per session (steady - idle) | 0.00 |
| goroutines idle / peak / after drain | 19 / 230 / 19 |
| heap peak MB | 40 |
| open fds idle / peak / after drain | 84 / 101 / 84 |

Growth, first vs last 20 s window after warmup: RSS +1.4%, heap +47.5%, goroutines +0, fds +0; goroutines +0 vs idle after drain.

Server counters: admission rejects map[], vad dropped 0, vad stale 0, sessions failed 0, stt/llm/tts errors 0/0/0, protocol errors 0, vad worker restarts 0.
