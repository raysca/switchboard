# Silero model comparison: go-voice vs Pipecat

Run 2026-10-11, offline, with no provider cost. It decides whether go-voice should adopt Pipecat's bundled Silero VAD model now that `vad.stop_ms` defaults to 200 ms (Pipecat's `VAD_STOP_SECS`).

## What was compared

- **Models:**
  - go-voice `third_party/silero_vad.onnx`, sha256 `1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3`;
  - Pipecat `pipecat-impl/src/pipecat/audio/vad/data/silero_vad.onnx`, sha256 `597d30b3ec076608d059…`.
- **Clips:** the 18 committed clips in `assets/speech/` (16 kHz mono). Each clip ends mid-speech, so 1.2 s of silence is appended for the stop to fire.
- **Inference:** each model runs through go-voice's own runner, framed exactly like `Processor.trySubmit` (64 samples of context plus a 512-sample chunk, recurrent state carried over).
- **Detector:** go-voice's own `Processor.detect`, with start at 0.5 for 2 chunks and stop at 0.35, at VAD stops of 200 and 800 ms.
- **Inference parity check:** Pipecat's Python wrapper (`SileroOnnxModel`, called directly without the analyzer's periodic reset) runs on the same clips, and the per-chunk probabilities are compared with our runner using Pipecat's file.

## Results

Inference parity: our runner reproduces Pipecat's probabilities exactly (max |Δp| = 0.0000 over 1823 chunks). Any difference below comes from the model file, not from our inference.

Each clip is padded with 1200 ms of silence, so the last end is the stop after the utterance.

| model | stop | extra segments (mid-utterance splits) | summed last end (ms) |
|---|---|---|---|
| ours | 200ms | 2 | 59904 |
| ours | 800ms | 0 | 70272 |
| pipecat | 200ms | 4 | 59424 |
| pipecat | 800ms | 0 | 69792 |

Inference parity (our Go runner with Pipecat's file vs Pipecat's Python wrapper): max |Δp| = 0.0000 over 1823 chunks in 18 clips.

- **Extra segments:** a single spoken utterance is split into more than one VAD segment. With the turn manager on, those splits are rejoined by the speech timeout's resume window, but each split is a chance to end the turn early.
- **Summed last end:** the stop after each utterance, summed over the 18 clips. Lower means stops come sooner.

## Decision

**Keep go-voice's model.**

- **The rule** (from the spec): switch only if, at 200 ms, Pipecat's file has no more extra segments than ours, and its summed last end is not later.
- **Splits:** Pipecat's file splits 4 utterances (clips 3, 6, 9 and 18) against our 2 (clips 3 and 6).
- **Stop times:** its stops are 480 ms earlier in total, about 27 ms per clip, which is less than one 32 ms chunk.

Fewer mid-utterance splits matter more at a 200 ms stop than one chunk of latency.

## Reproduce

```bash
pipecat-impl/.venv/bin/python tools/vadcompare/pipecat_probs.py assets/speech > /tmp/pipecat_probs.json   # main checkout
```

```bash
VAD_COMPARE=1 VAD_COMPARE_PIPECAT_MODEL=pipecat-impl/src/pipecat/audio/vad/data/silero_vad.onnx \
  VAD_COMPARE_PIPECAT_PROBS=/tmp/pipecat_probs.json VAD_COMPARE_OUT=/tmp/silero-compare.md \
  go test ./vad/ -run TestCompareSileroModels -count=1 -v
```

## Per clip

| clip | model | stop | segments | first start (ms) | last end (ms) |
|---|---|---|---|---|---|
| clip1.wav | ours | 200ms | 1 | 96 | 3360 |
| clip1.wav | ours | 800ms | 1 | 96 | 3936 |
| clip1.wav | pipecat | 200ms | 1 | 64 | 3360 |
| clip1.wav | pipecat | 800ms | 1 | 64 | 3936 |
| clip10.wav | ours | 200ms | 1 | 64 | 3424 |
| clip10.wav | ours | 800ms | 1 | 64 | 4000 |
| clip10.wav | pipecat | 200ms | 1 | 64 | 3392 |
| clip10.wav | pipecat | 800ms | 1 | 64 | 3968 |
| clip11.wav | ours | 200ms | 1 | 128 | 3424 |
| clip11.wav | ours | 800ms | 1 | 128 | 4000 |
| clip11.wav | pipecat | 200ms | 1 | 128 | 3424 |
| clip11.wav | pipecat | 800ms | 1 | 128 | 4000 |
| clip12.wav | ours | 200ms | 1 | 32 | 3392 |
| clip12.wav | ours | 800ms | 1 | 32 | 3968 |
| clip12.wav | pipecat | 200ms | 1 | 32 | 3360 |
| clip12.wav | pipecat | 800ms | 1 | 32 | 3936 |
| clip13.wav | ours | 200ms | 1 | 96 | 3072 |
| clip13.wav | ours | 800ms | 1 | 96 | 3648 |
| clip13.wav | pipecat | 200ms | 1 | 128 | 3040 |
| clip13.wav | pipecat | 800ms | 1 | 128 | 3616 |
| clip14.wav | ours | 200ms | 1 | 96 | 2944 |
| clip14.wav | ours | 800ms | 1 | 96 | 3520 |
| clip14.wav | pipecat | 200ms | 1 | 96 | 2880 |
| clip14.wav | pipecat | 800ms | 1 | 96 | 3456 |
| clip15.wav | ours | 200ms | 1 | 32 | 2592 |
| clip15.wav | ours | 800ms | 1 | 32 | 3168 |
| clip15.wav | pipecat | 200ms | 1 | 64 | 2560 |
| clip15.wav | pipecat | 800ms | 1 | 64 | 3136 |
| clip16.wav | ours | 200ms | 1 | 96 | 3616 |
| clip16.wav | ours | 800ms | 1 | 96 | 4192 |
| clip16.wav | pipecat | 200ms | 1 | 96 | 3552 |
| clip16.wav | pipecat | 800ms | 1 | 96 | 4128 |
| clip17.wav | ours | 200ms | 1 | 64 | 3520 |
| clip17.wav | ours | 800ms | 1 | 64 | 4096 |
| clip17.wav | pipecat | 200ms | 1 | 64 | 3488 |
| clip17.wav | pipecat | 800ms | 1 | 64 | 4064 |
| clip18.wav | ours | 200ms | 1 | 64 | 3424 |
| clip18.wav | ours | 800ms | 1 | 64 | 4000 |
| clip18.wav | pipecat | 200ms | 2 | 64 | 3392 |
| clip18.wav | pipecat | 800ms | 1 | 64 | 3968 |
| clip2.wav | ours | 200ms | 1 | 64 | 3168 |
| clip2.wav | ours | 800ms | 1 | 64 | 3744 |
| clip2.wav | pipecat | 200ms | 1 | 64 | 3168 |
| clip2.wav | pipecat | 800ms | 1 | 64 | 3744 |
| clip3.wav | ours | 200ms | 2 | 32 | 3232 |
| clip3.wav | ours | 800ms | 1 | 32 | 3808 |
| clip3.wav | pipecat | 200ms | 2 | 32 | 3232 |
| clip3.wav | pipecat | 800ms | 1 | 32 | 3808 |
| clip4.wav | ours | 200ms | 1 | 128 | 3584 |
| clip4.wav | ours | 800ms | 1 | 128 | 4160 |
| clip4.wav | pipecat | 200ms | 1 | 128 | 3552 |
| clip4.wav | pipecat | 800ms | 1 | 128 | 4128 |
| clip5.wav | ours | 200ms | 1 | 64 | 3360 |
| clip5.wav | ours | 800ms | 1 | 64 | 3936 |
| clip5.wav | pipecat | 200ms | 1 | 96 | 3328 |
| clip5.wav | pipecat | 800ms | 1 | 96 | 3904 |
| clip6.wav | ours | 200ms | 2 | 32 | 3616 |
| clip6.wav | ours | 800ms | 1 | 32 | 4192 |
| clip6.wav | pipecat | 200ms | 2 | 32 | 3584 |
| clip6.wav | pipecat | 800ms | 1 | 32 | 4160 |
| clip7.wav | ours | 200ms | 1 | 160 | 3552 |
| clip7.wav | ours | 800ms | 1 | 160 | 4128 |
| clip7.wav | pipecat | 200ms | 1 | 160 | 3520 |
| clip7.wav | pipecat | 800ms | 1 | 160 | 4096 |
| clip8.wav | ours | 200ms | 1 | 64 | 3328 |
| clip8.wav | ours | 800ms | 1 | 64 | 3904 |
| clip8.wav | pipecat | 200ms | 1 | 64 | 3328 |
| clip8.wav | pipecat | 800ms | 1 | 64 | 3904 |
| clip9.wav | ours | 200ms | 1 | 32 | 3296 |
| clip9.wav | ours | 800ms | 1 | 32 | 3872 |
| clip9.wav | pipecat | 200ms | 2 | 32 | 3264 |
| clip9.wav | pipecat | 800ms | 1 | 32 | 3840 |
