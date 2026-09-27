"""Python process-per-call startup + memory baseline.

Writes results/python_startup.csv and results/python_memory.csv.
Modes: bare (python -c), spawn (fresh `python bot.py`), forkserver (warm parent with
pipecat/openai imported forks a child that builds the Silero analyzer + pipeline).
Run: .venv/bin/python baseline/python/startup_bench.py [samples]
"""
import csv
import os
import re
import signal
import subprocess
import sys
import time

import psutil

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
RES = os.path.join(ROOT, "results")
BOT = os.path.join(HERE, "bot.py")
PLATEAUS = [0, 5, 10, 25]
MEM_POINTS = [1, 5, 10, 25]
SAMPLES = int(sys.argv[1]) if len(sys.argv) > 1 else 10
UNIT = {"B": 1 / 2**20, "KB": 1 / 1024, "MB": 1.0, "GB": 1024.0}
FP_RE = re.compile(r"\[(\d+)\]: \d+-bit\s+Footprint: ([\d.]+) (B|KB|MB|GB)")


def footprint_mb(pids):
    """phys_footprint in MB per pid, via /usr/bin/footprint (one call for all pids)."""
    args = ["/usr/bin/footprint"]
    for p in pids:
        args += ["-p", str(p)]
    out = subprocess.run(args, capture_output=True, text=True).stdout
    got = {int(m[1]): float(m[2]) * UNIT[m[3]] for m in FP_RE.finditer(out)}
    assert set(got) == set(pids), (pids, out[:500])
    return got


def rss_mb(pid):
    return psutil.Process(pid).memory_info().rss / 2**20


def read_ready(fd):
    buf = b""
    while not buf.endswith(b"\n"):
        chunk = os.read(fd, 256)
        if not chunk:
            raise RuntimeError(f"child exited before READY: {buf!r}")
        buf += chunk
    tag, ns = buf.split()[-2:]
    assert tag == b"READY", buf
    return int(ns)


def start_spawn(argv):
    t0 = time.monotonic_ns()
    p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    ts = read_ready(p.stdout.fileno())
    return p.pid, (ts - t0) / 1e6


def start_fork():
    import asyncio
    import bot  # already imported by the warm parent; cheap

    r, w = os.pipe()
    t0 = time.monotonic_ns()
    pid = os.fork()
    if pid == 0:
        os.close(r)
        try:
            asyncio.run(bot.run_bot(w, asyncio.Event()))  # runs until SIGKILL
        finally:
            os._exit(0)
    os.close(w)
    ts = read_ready(r)
    os.close(r)
    return pid, (ts - t0) / 1e6


def kill(pids):
    for p in pids:
        try:
            os.kill(p, signal.SIGKILL)
        except ProcessLookupError:
            pass
    for p in pids:
        try:
            os.waitpid(p, 0)
        except ChildProcessError:
            pass


def run_mode(mode, start, rows, mem_rows):
    existing, base_fp, fp1 = [], None, None
    parent_fp = footprint_mb([os.getpid()])[os.getpid()] if mode == "forkserver" else 0.0
    try:
        for plateau in PLATEAUS:
            while len(existing) < plateau:
                existing.append(start()[0])
                if len(existing) in MEM_POINTS and len(existing) > 1:
                    time.sleep(1.0)
                    fps = footprint_mb(existing)
                    tot_fp = sum(fps.values())
                    mem_rows.append([mode, len(existing), round(tot_fp + parent_fp, 1),
                                     round(sum(rss_mb(p) for p in existing), 1),
                                     round((tot_fp - fp1) / (len(existing) - 1), 1)])
            time.sleep(1.0)
            for s in range(SAMPLES):
                pid, ms = start()
                time.sleep(0.5)  # let the call settle before sampling memory
                fp, rss = footprint_mb([pid])[pid], rss_mb(pid)
                if plateau == 0 and s == 0:
                    fp1 = fp
                    mem_rows.append([mode, 1, round(fp + parent_fp, 1), round(rss, 1), ""])
                kill([pid])
                rows.append([mode, plateau, s, round(ms, 1), round(fp, 1), round(rss, 1)])
                print(mode, plateau, s, f"{ms:.0f}ms fp={fp:.0f}MB rss={rss:.0f}MB", flush=True)
    finally:
        kill(existing)


def main():
    rows, mem_rows = [], []
    bare = [sys.executable, "-c", "import time;print('READY',time.monotonic_ns(),flush=True)"]
    for s in range(SAMPLES):
        pid, ms = start_spawn(bare)
        kill([pid])
        rows.append(["bare", 0, s, round(ms, 1), "", ""])
    run_mode("spawn", lambda: start_spawn([sys.executable, BOT]), rows, mem_rows)
    sys.path.insert(0, HERE)
    import bot  # noqa: F401  warm parent: pipecat + openai + onnxruntime imported pre-fork

    run_mode("forkserver", start_fork, rows, mem_rows)

    with open(os.path.join(RES, "python_startup.csv"), "w", newline="") as f:
        csv.writer(f).writerows([["mode", "existing", "sample", "ready_ms", "footprint_mb", "rss_mb"], *rows])
    with open(os.path.join(RES, "python_memory.csv"), "w", newline="") as f:
        csv.writer(f).writerows([["mode", "processes", "total_footprint_mb", "total_rss_mb",
                                  "marginal_footprint_mb"], *mem_rows])


if __name__ == "__main__":
    main()
