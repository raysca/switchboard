# Capacity and SLO notes

September 27, 2026

This file replaces an earlier report that claimed exact handshake, VAD, LLM, TTS, interruption, and teardown latencies and a certification of 250 concurrent calls. Those numbers were not measured. They are withdrawn.

## What was measured

One local development call, recorded in `e2e_call_result.json`:

- Transport: `wss://127.0.0.1:18181/ws` with a temporary certificate.
- VAD: Silero, sharded in-process.
- Providers: Google STT, Google LLM, Google TTS.
- Two user transcripts and a follow-up answer that quoted the first question.
- 103680 bytes of 24 kHz PCM output audio.
- Wall time of the test: 16.40s, including speech playback and a pause between turns.

No percentile was computed. One sample is not an SLO.

## What was not measured

- Concurrent capacity. There is no 250-call run and no raw trace that could support one.
- Regional or cross-network handshake latency.
- A 30-minute soak. One live rotation was measured separately: `TestLiveSTTRotation` held a Google STT stream for 250s and recorded `voice_stt_rotations_total 1` in `long_call_result.json`.
- systemd resource use. The container image `govoice:v1-audit` was built and exited 0 after SIGTERM; that is not a capacity measurement.
- Interruption latency against the pinned browser client. The playback-clear probe failed; see `docs/compatibility/playback-clear-decision.json`.

## Design limits (not measurements)

These are code settings, not observed capacity:

- Output pacer queue: 100ms or 4800 bytes, then `ErrSlowClient`.
- Session data queue: 64 frames. Output audio waits up to `MaxFrameProcessing` and then ends the session. It is not dropped.
- STT location default: `us-central1`.

Do not treat the VAD-only POC concurrency figures as this server's call capacity.
