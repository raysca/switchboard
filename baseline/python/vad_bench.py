"""Python Silero VAD baseline: onnxruntime direct, intra=1 inter=1, sequential, batch 1.

Each process runs K streams on a real-time clock (512-sample / 32 ms chunks, streaming state
per stream). Latency = result time - chunk due time. Finds max K per process meeting
p99 <= 50 ms, then runs 12 processes at that K. Writes results/python_vad.csv and
results/python_vad_micro.csv.
Run: .venv/bin/python baseline/python/vad_bench.py
"""
import csv
import glob
import heapq
import multiprocessing as mp
import os
import sys
import time
import wave

import numpy as np

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MODEL = os.path.join(ROOT, "third_party", "silero_vad.onnx")
RES = os.path.join(ROOT, "results")
CHUNK, CTX, PERIOD = 512, 64, 0.032
SLO_MS = 50.0
WARMUP_S, MEASURE_S = 2.0, 15.0


def load_audio():
    parts = []
    for p in sorted(glob.glob(os.path.join(ROOT, "assets", "speech", "*.wav"))):
        with wave.open(p) as w:
            assert w.getframerate() == 16000 and w.getsampwidth() == 2 and w.getnchannels() == 1
            parts.append(np.frombuffer(w.readframes(w.getnframes()), np.int16))
    return (np.concatenate(parts).astype(np.float32) / 32768.0)


def session():
    import onnxruntime as ort

    o = ort.SessionOptions()
    o.intra_op_num_threads = 1
    o.inter_op_num_threads = 1
    o.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
    return ort.InferenceSession(MODEL, o, providers=["CPUExecutionProvider"])


class Stream:
    def __init__(self, audio, pos):
        self.audio, self.pos = audio, pos
        self.x = np.zeros((1, CTX + CHUNK), np.float32)
        self.state = np.zeros((2, 1, 128), np.float32)

    def step(self, sess, sr):
        n = len(self.audio)
        self.x[0, :CTX] = self.x[0, CHUNK:]  # last 64 samples become context
        idx = (np.arange(self.pos, self.pos + CHUNK)) % n if self.pos + CHUNK > n else slice(self.pos, self.pos + CHUNK)
        self.x[0, CTX:] = self.audio[idx]
        self.pos = (self.pos + CHUNK) % n
        out, self.state = sess.run(None, {"input": self.x, "state": self.state, "sr": sr})
        return float(out[0, 0])


def worker(k, t_start, seed, q):
    sess, audio, sr = session(), load_audio(), np.array(16000, np.int64)
    rng = np.random.default_rng(seed)
    streams = [Stream(audio, int(rng.integers(len(audio)))) for _ in range(k)]
    # stagger stream phases across the 32 ms period
    heap = [(t_start + i * PERIOD / k, i) for i in range(k)]
    heapq.heapify(heap)
    t_meas, t_end = t_start + WARMUP_S, t_start + WARMUP_S + MEASURE_S
    lat, inf = [], []
    cpu0 = None
    while True:
        due, i = heap[0]
        if due >= t_end:
            break
        now = time.monotonic()
        if due > now:
            time.sleep(due - now)
        if cpu0 is None and due >= t_meas:
            cpu0 = time.process_time()
        a = time.perf_counter()
        streams[i].step(sess, sr)
        b = time.perf_counter()
        if due >= t_meas:
            lat.append((time.monotonic() - due) * 1e3)
            inf.append((b - a) * 1e6)
        heapq.heapreplace(heap, (due + PERIOD, i))
    cpu_pct = (time.process_time() - (cpu0 or 0)) / MEASURE_S * 100
    q.put((np.array(lat, np.float32), np.array(inf, np.float32), cpu_pct))


def run(procs, k):
    ctx = mp.get_context("spawn")
    q = ctx.Queue()
    t_start = time.monotonic() + 3.0  # time for children to import + load model
    ps = [ctx.Process(target=worker, args=(k, t_start, s, q)) for s in range(procs)]
    for p in ps:
        p.start()
    res = [q.get() for _ in ps]
    for p in ps:
        p.join()
    lat = np.concatenate([r[0] for r in res])
    inf = np.concatenate([r[1] for r in res])
    row = dict(procs=procs, streams_per_proc=k, total_streams=procs * k,
               infer_us_p50=round(float(np.percentile(inf, 50)), 1),
               infer_us_p99=round(float(np.percentile(inf, 99)), 1),
               chunk_latency_ms_p50=round(float(np.percentile(lat, 50)), 2),
               chunk_latency_ms_p99=round(float(np.percentile(lat, 99)), 2),
               cpu_pct=round(sum(r[2] for r in res), 1))
    row["realtime_ok"] = row["chunk_latency_ms_p99"] <= SLO_MS
    print(row, flush=True)
    return row


def micro(seconds=5.0):
    sess, audio, sr = session(), load_audio(), np.array(16000, np.int64)
    s = Stream(audio, 0)
    for _ in range(200):
        s.step(sess, sr)
    probs, n, t0 = [], 0, time.perf_counter()
    while time.perf_counter() - t0 < seconds:
        probs.append(s.step(sess, sr))
        n += 1
    dt = time.perf_counter() - t0
    row = dict(threads=1, inferences=n, seconds=round(dt, 2), inferences_per_sec=round(n / dt),
               us_per_inference=round(dt / n * 1e6, 1),
               speech_chunk_frac=round(float(np.mean(np.array(probs) > 0.5)), 3))
    print(row, flush=True)
    return row


def write(name, rows):
    with open(os.path.join(RES, name), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0]))
        w.writeheader()
        w.writerows(rows)


def main():
    write("python_vad_micro.csv", [micro()])
    rows, ok = [], {}
    # ponytail: exponential probe then bisection on K for one process; ~10 runs x 20 s
    k = 16
    while True:
        r = run(1, k); rows.append(r); ok[k] = r["realtime_ok"]
        if not ok[k]:
            break
        k *= 2
    lo, hi = k // 2, k
    while hi - lo > max(2, lo // 20):
        mid = (lo + hi) // 2
        r = run(1, mid); rows.append(r); ok[mid] = r["realtime_ok"]
        lo, hi = (mid, hi) if ok[mid] else (lo, mid)
    kmax = lo
    # host total: 12 procs at kmax; step K down until the SLO holds
    k = kmax
    while k > 0:
        r = run(12, k); rows.append(r)
        if r["realtime_ok"]:
            break
        k = int(k * 0.9)
    write("python_vad.csv", rows)
    print(f"max K/proc (1 proc) = {kmax}; host = 12 x {k} = {12 * k}")


if __name__ == "__main__":
    sys.exit(main())
