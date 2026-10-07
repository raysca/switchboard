"""Regenerates golden.json from the Pipecat reference.
Run: pipecat-impl/.venv/bin/python smartturn/testdata/gen_golden.py
"""
import json, math, os, sys
import numpy as np
import onnxruntime as ort

ROOT = os.path.join(os.path.dirname(__file__), "..", "..")
sys.path.insert(0, os.path.join(ROOT, "pipecat-impl", "src", "pipecat", "audio", "turn", "smart_turn"))
from _whisper_features import compute_whisper_log_mel_features  # noqa: E402

sess = ort.InferenceSession(os.path.join(ROOT, "third_party", "smart_turn_v3.2_cpu.onnx"))


def lcg_signal(n, amp1=8000.0, amp2=3000.0, noise_amp=1000, dc=0.0, env_hz=0.0):
    """Mirrored by signal() in features_test.go."""
    state = 12345
    pcm = []
    for i in range(n):
        state = (state * 1103515245 + 12345) & 0x7FFFFFFF
        noise = ((state >> 16) % (2 * noise_amp + 1) - noise_amp) if noise_amp else 0
        env = 0.5 + 0.5 * math.sin(2 * math.pi * env_hz * i / 16000) if env_hz else 1.0
        v = dc + env * (amp1 * math.sin(2 * math.pi * 220 * i / 16000) + amp2 * math.sin(2 * math.pi * 1337 * i / 16000 + 0.5)) + noise
        pcm.append(int(min(32767, max(-32768, math.floor(v + 0.5)))))
    return np.array(pcm, dtype=np.int16)


def truncate_audio_to_last_n_seconds(audio_array, n_seconds=8, sample_rate=16000):
    # Copied from LocalSmartTurnAnalyzerV3._predict_endpoint.
    max_samples = n_seconds * sample_rate
    if len(audio_array) > max_samples:
        return audio_array[-max_samples:]
    elif len(audio_array) < max_samples:
        padding = max_samples - len(audio_array)
        return np.pad(audio_array, (padding, 0), mode="constant", constant_values=0)
    return audio_array


def features(pcm):
    x = truncate_audio_to_last_n_seconds(pcm.astype(np.float32) / 32768.0)
    return compute_whisper_log_mel_features(x, do_normalize=True)


def prob(f):
    return float(sess.run(None, {"input_features": f[None]})[0].reshape(-1)[0])


# Original fixture: 1.5 s, front-padded.
f = features(lcg_signal(24000))
cols = [0, 100, 600, 649, 650, 700, 750, 799]
out = {"cols": cols, "values": [[float(f[m, c]) for m in range(80)] for c in cols],
       "sum": float(f.astype(np.float64).sum()), "prob": prob(f)}

# Extra signals: every 25th frame plus the last one, all 80 mels.
SIGNALS = {
    "speech_8s": dict(n=128000, amp1=12000, amp2=4000, env_hz=4),  # exactly the window
    "long_10s": dict(n=160000, env_hz=3),  # truncation keeps the last 8 s
    "odd_dc_quiet": dict(n=12345, amp1=40, amp2=15, noise_amp=5, dc=2000),  # DC offset, tiny amplitude
    "clipped": dict(n=40001, amp1=40000, amp2=9000),  # saturates int16
}
sub = list(range(0, 800, 25)) + [799]
out["signals"] = []
for name, spec in SIGNALS.items():
    f = features(lcg_signal(**spec))
    out["signals"].append({"name": name, "spec": spec, "cols": sub,
                           "values": [[float(f[m, c]) for m in range(80)] for c in sub],
                           "sum": float(f.astype(np.float64).sum()), "prob": prob(f)})
    if os.environ.get("GOLDEN_DUMP"):  # full (80, 800) float32 for an exhaustive compare
        f.astype("<f4").tofile(os.path.join(os.environ["GOLDEN_DUMP"], name + ".f32"))
    print(name, f.shape, out["signals"][-1]["prob"])

json.dump(out, open(os.path.join(os.path.dirname(__file__), "golden.json"), "w"))
print("base", out["prob"])
