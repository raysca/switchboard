# Smart Turn v3.2 inference (P1-3)

Model `third_party/smart_turn_v3.2_cpu.onnx` (sha256 pinned in `docs/compatibility/v1-lock.json`, BSD-2-Clause, Daily).
Measured with `go test -run TestInferenceLatencyP95 -v ./smartturn/` (200 requests after 10 warm-up, full path: IPC to the
`turnworker` child, 8 s Whisper log-mel features in Go, ONNX Runtime 1.29.0, 2 intra-op threads), Apple M4 Pro:

| p50 | p95 | p99 | max |
|---|---|---|---|
| 23.7 ms | 25.4 ms | 28.4 ms | 35.0 ms |

Acceptance (p95 < 50 ms) holds; the test fails above 50 ms. Breakdown: features 2.8 ms (4 goroutines; 11 ms single
thread), ORT 45 ms at 1 thread, 29 ms at 2, 22 ms at 4 (worse at 6). Default `IntraOp` is 2.

Parity: the Go log-mel features are bit-identical to pipecat's `_whisper_features.py` (after
`truncate_audio_to_last_n_seconds`) on five signals in `smartturn/testdata/golden.json`: 1.5 s front-padded, exactly 8 s,
10 s truncated to the last 8 s, odd length with DC offset at tiny amplitude, and int16-saturated. The waveform
normalization runs in float32 as numpy does; float64 there was off by up to 1.6e-5. Model probabilities still differ by
up to 0.06 from onnxruntime-python 1.24 (Go uses ORT 1.29). This quantized model's output depends on ORT graph fusions:
python alone spans 0.80 to 0.83 across optimization levels. The >0.5 verdict agrees on every fixture.

Config (`turn.manager: true` required):

```yaml
turn:
  manager: true
  stop: [turn-analyzer, speech-timeout]
  speech_timeout: 700ms      # fallback when the model cannot answer
  analyzer: {mode: process, workers: 1, stop_secs: 3s, budget: 150ms}
```

Metrics: `voice_smartturn_inference_ms` (histogram), `voice_smartturn_fallbacks_total{reason}`
(timeout|error|overload|unavailable|no_audio), `voice_smartturn_verdicts_total{verdict}`,
`voice_smartturn_worker_crashes_total`, `voice_smartturn_workers{state}`.
