# govoice load report (capacity)

2026-10-09 20:38 BST, darwin/arm64, 12 CPUs, go1.27.1, Apple M4 Pro. Machine-specific numbers. Fake providers, localhost, one speech clip.

Config: sessions 24, duration 20 s (ramp 0 s), turns/call 5, think 1000 ms, clip `e2e/fixtures/voice-debug.wav` (1600 ms speech), vad process, turn manager false.

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

max concurrent 24 of 24 target; started 25, completed 1, failed 0, rejected 0. Turns 120 attempted, 110 completed, 0 timeouts. Error rate 0.00%.

## Latency (ms)

| metric | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| voice_to_voice (client: last speech chunk sent -> first bot audio) | 110 | 944.1 | 965.1 | 965.5 | 966.6 |
| server voice_to_voice (histogram, bucket resolution) | 111 | 250.0 | 295.0 | 299.0 | 300.0 |
| server stt_final (histogram, bucket resolution) | 111 | 250.0 | 295.0 | 299.0 | 300.0 |
| server llm_ttft (histogram, bucket resolution) | 111 | 25.0 | 47.5 | 49.5 | 50.0 |
| server tts_ttfb (histogram, bucket resolution) | 111 | 25.0 | 47.5 | 49.5 | 50.0 |
| harness pacing lag (late 20 ms sends; load-generator health) | 22621 | 1.0 | 1.0 | 2.0 | 5.0 |

## Resources (server process tree)

| | value |
|---|---|
| CPU seconds | 11.0 (avg 0.55 cores) |
| CPU-seconds per session-minute | 1.460 |
| RSS idle / steady / peak MB | 236 / 220 / 236 |
| MB per session (steady - idle) | 0.00 |
| goroutines idle / peak / after drain | 19 / 331 / 19 |
| heap peak MB | 32 |
| open fds idle / peak / after drain | 84 / 113 / 84 |

Growth, first vs last 4 s window after warmup: RSS +13.3%, heap +130.2%, goroutines +0, fds +0; goroutines +0 vs idle after drain.

Server counters: admission rejects map[], vad dropped 0, vad stale 0, sessions failed 0, stt/llm/tts errors 0/0/0, protocol errors 0, vad worker restarts 0.

## Capacity (p95 v2v budget 1117 ms)

| sessions | v2v p95 ms | error % | rejects | cores at full load | pacing lag p99 ms | |
|---|---|---|---|---|---|---|
| 4 | 966.6 | 0.00 | 0 | 0.05 | 5.0 | PASS  |
| 8 | 965.3 | 0.00 | 0 | 0.25 | 5.0 | PASS  |
| 16 | 965.4 | 0.00 | 0 | 0.36 | 5.0 | PASS  |
| 24 | 965.1 | 0.00 | 0 | 0.58 | 2.0 | PASS  |

Max sessions within budget: 24, using about 0.58 cores (CPU-seconds per session-minute x sessions / 60), so about **41.4 sessions per core**. Fake providers, localhost, load generator on the same machine as the server, one real speech clip: framework cost only, an upper bound for real deployments.
