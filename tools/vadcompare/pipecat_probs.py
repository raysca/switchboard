"""Per-chunk Silero speech probabilities from Pipecat's own model wrapper.

Run with Pipecat's environment (main checkout):
  pipecat-impl/.venv/bin/python tools/vadcompare/pipecat_probs.py assets/speech > pipecat_probs.json
Output: {"<clip>.wav": [p0, p1, ...]}, one probability per 512-sample chunk at 16 kHz.
The model is called directly (SileroOnnxModel), without SileroVADAnalyzer's periodic state reset,
so the numbers compare like for like with go-voice's runner (vad/compare_test.go).
"""
import glob, json, os, sys, wave

import numpy as np
from pipecat.audio.vad.silero import SileroOnnxModel

MODEL = os.path.join(os.path.dirname(__import__("pipecat").__file__), "audio", "vad", "data", "silero_vad.onnx")


def probs(path, model):
    with wave.open(path, "rb") as w:
        assert w.getframerate() == 16000 and w.getnchannels() == 1 and w.getsampwidth() == 2, path
        pcm = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(np.float32) / 32768.0
    model.reset_states()
    return [float(np.asarray(model(pcm[i:i + 512], 16000)).reshape(-1)[0]) for i in range(0, len(pcm) - 511, 512)]


def main(d):
    model = SileroOnnxModel(MODEL, force_onnx_cpu=True)
    print(json.dumps({os.path.basename(p): probs(p, model) for p in sorted(glob.glob(os.path.join(d, "*.wav")))}))


if __name__ == "__main__":
    main(sys.argv[1])
