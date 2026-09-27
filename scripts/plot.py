"""Graphs + summary tables from results/*.csv. Run: .venv/bin/python scripts/plot.py"""
import csv, glob, os, statistics
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

R = os.path.join(os.path.dirname(__file__), "..", "results")
P = lambda n: os.path.join(R, n)
def rows(n):
    p = P(n)
    return list(csv.DictReader(open(p))) if os.path.exists(p) else []
def q(v, p):
    v = sorted(v); return v[min(len(v) - 1, int(len(v) * p))] if v else float("nan")
def save(fig, name):
    fig.tight_layout(); fig.savefig(P(name), dpi=110); plt.close(fig); print("wrote", name)

summary = []

# --- T1 startup vs existing sessions (Go in-process + HTTP vs Python) ---
t1 = rows("t1_startup.csv")
if t1:
    fig, ax = plt.subplots(figsize=(7, 4))
    ex = sorted({int(r["existing"]) for r in t1})
    for pct, st in ((.5, "-o"), (.99, "--o")):
        ax.plot([max(e, .8) for e in ex], [q([float(r["ready_us"]) / 1000 for r in t1 if int(r["existing"]) == e], pct) for e in ex], st, label=f"Go warm session ready p{int(pct*100)}")
    py = rows("python_startup.csv")
    for mode in ("spawn", "forkserver"):
        pe = sorted({int(r["existing"]) for r in py if r["mode"] == mode})
        ax.plot([max(e, .8) for e in pe], [q([float(r["ready_ms"]) for r in py if r["mode"] == mode and int(r["existing"]) == e], .5) for e in pe], "-s", label=f"Python {mode} p50")
    ax.set(xscale="log", yscale="log", xlabel="existing sessions", ylabel="ms", title="T1 call-ready latency vs existing sessions")
    ax.legend(fontsize=8); save(fig, "g1_startup.png")
    for e in ex:
        v = [float(r["ready_us"]) for r in t1 if int(r["existing"]) == e]
        summary.append(("T1", f"existing={e}", f"ready p50={q(v,.5):.1f}us p99={q(v,.99):.1f}us"))

# --- T2 burst ---
t2 = rows("t2_burst.csv")
if t2:
    fig, ax = plt.subplots(figsize=(6, 3.5))
    b = [int(r["burst"]) for r in t2]
    for k in ("ready_p50_ms", "ready_p99_ms"):
        ax.plot(b, [float(r[k]) for r in t2], "-o", label=k)
    ax.plot(b, [float(r["running_vad_p99_during_ms"]) for r in t2], "-^", label="running sessions VAD p99 during burst")
    ax.set(xscale="log", yscale="log", xlabel="simultaneous new calls", ylabel="ms", title="T2 burst startup (200 calls streaming)")
    ax.legend(fontsize=8); save(fig, "g2_burst.png")

# --- T3 memory ---
t3 = rows("t3_memory.csv")
if t3:
    fig, ax = plt.subplots(figsize=(7, 4))
    for kind in ("idle", "streaming"):
        rr = [r for r in t3 if r["kind"] == kind]
        n = [int(r["sessions"]) for r in rr]
        ax.plot(n, [float(r["footprint_mb"]) for r in rr], "-o", label=f"Go {kind} phys_footprint")
        ax.plot(n, [float(r["heap_mb"]) for r in rr], ":o", label=f"Go {kind} heap")
        if len(n) > 1:
            m = (float(rr[-1]["footprint_mb"]) - float(rr[0]["footprint_mb"])) / (n[-1] - n[0]) * 1024
            h = (float(rr[-1]["heap_mb"]) - float(rr[0]["heap_mb"])) / (n[-1] - n[0]) * 1024
            summary.append(("T3", f"Go {kind} marginal", f"footprint {m:.1f} KB/session, heap {h:.1f} KB/session (0->{n[-1]})"))
    pm = rows("python_memory.csv")
    for mode in ("spawn", "forkserver"):
        rr = [r for r in pm if r["mode"] == mode]
        ax.plot([int(r["processes"]) for r in rr], [float(r["total_footprint_mb"]) for r in rr], "-s", label=f"Python {mode} (measured)")
    ax.set(xscale="symlog", yscale="log", xlabel="sessions", ylabel="MB", title="T3 memory vs sessions")
    ax.legend(fontsize=7); save(fig, "g3_memory.png")

# --- T4 churn ---
t4 = rows("t4_churn.csv")
if t4:
    fig, ax = plt.subplots(1, 2, figsize=(10, 3.5))
    t = [int(r["t_s"]) for r in t4]
    for k in ("heap_mb", "rss_mb", "footprint_mb"):
        ax[0].plot(t, [float(r[k]) for r in t4], label=k)
    for k in ("goroutines", "threads", "vad_streams", "sessions"):
        ax[1].plot(t, [float(r[k]) for r in t4], label=k)
    ax[0].set(xlabel="s", ylabel="MB", title="T4 churn: memory"); ax[1].set(xlabel="s", title=f"T4 churn: counts ({t4[-1]['cycles']} cycles)")
    ax[0].legend(fontsize=7); ax[1].legend(fontsize=7); save(fig, "g4_churn.png")

# --- T7/T8 VAD sweeps ---
def ceiling(rr):
    best, win = 0, 0
    for r in rr:
        if r["pass"] != "true":
            break
        best, win = int(r["streams"]), float(r["window_s"])
    return best, win
cfgs = {}
for f in sorted(glob.glob(P("t7_*.csv"))):
    rr = rows(os.path.basename(f))
    if rr:
        cfgs[os.path.basename(f)[3:-4]] = rr
if cfgs:
    fig, ax = plt.subplots(1, 2, figsize=(12, 4))
    for name, rr in cfgs.items():
        n = [int(r["streams"]) for r in rr]
        ax[0].plot(n, [float(r["total_p99_ms"]) for r in rr], "-o", ms=3, label=name)
        ax[1].plot(n, [float(r["qwait_p99_ms"]) for r in rr], "-o", ms=3, label=name)
        c, w = ceiling(rr)
        summary.append(("T8", name, f"RT ceiling {c} streams (window {w:.0f}s)"))
    for a, t in zip(ax, ("VAD total latency p99 (due -> applied)", "VAD queue wait p99")):
        a.axhline(50, color="r", ls="--", lw=1); a.set(yscale="log", xlabel="active VAD streams", ylabel="ms", title=t)
    ax[0].legend(fontsize=6); save(fig, "g5_vad_p99_all.png")

    # best config detail
    best = "sharded-w11-b8-i1" if "sharded-w11-b8-i1" in cfgs else max(cfgs, key=lambda k: ceiling(cfgs[k])[0])
    rr = cfgs[best]
    n = [int(r["streams"]) for r in rr]
    fig, ax = plt.subplots(1, 3, figsize=(14, 4))
    for k in ("total_p50_ms", "total_p95_ms", "total_p99_ms", "qwait_p99_ms", "infer_p99_ms", "feeder_lag_p99_ms", "submitlag_p99_ms"):
        ax[0].plot(n, [float(r[k]) for r in rr], "-o", ms=3, label=k)
    ax[0].axhline(50, color="r", ls="--", lw=1); ax[0].set(yscale="log", xlabel="active streams", ylabel="ms", title=f"T7 latency decomposition: {best}")
    ax[0].legend(fontsize=7)
    ax[1].plot(n, [float(r["sess_p95_max_ms"]) for r in rr], "-o", label="worst session p95")
    ax[1].plot(n, [float(r["sess_p95_med_ms"]) for r in rr], "-o", label="median session p95")
    ax[1].plot(n, [float(r["sess_p99_max_ms"]) for r in rr], "-o", label="worst session p99")
    ax[1].axhline(100, color="r", ls="--", lw=1); ax[1].set(yscale="log", xlabel="active streams", ylabel="ms", title="per-session fairness"); ax[1].legend(fontsize=7)
    ax[2].plot(n, [float(r["cpu_pct"]) for r in rr], "-o", label="host CPU %")
    ax[2].plot(n, [float(r["worker_util_pct"]) for r in rr], "-o", label="VAD worker busy %")
    ax2 = ax[2].twinx(); ax2.plot(n, [float(r["drop_pct"]) for r in rr], "-rx", label="drop %"); ax2.set_ylabel("drop %")
    ax[2].set(xlabel="active streams", title="saturation"); ax[2].legend(fontsize=7, loc="upper left")
    save(fig, "g6_vad_best_detail.png")

    fig, ax = plt.subplots(figsize=(8, 4))
    names = [k for k in cfgs if "duty" not in k and "fakes" not in k]
    ax.barh(names, [ceiling(cfgs[k])[0] for k in names])
    py = rows("python_vad.csv")
    if py:
        ok = [int(r["total_streams"]) for r in py if int(r["procs"]) == 12 and r["realtime_ok"].lower() in ("true", "1")]
        if ok:
            ax.axvline(max(ok), color="k", ls="--", label=f"Python 12 procs ({max(ok)})"); ax.legend(fontsize=8)
    ax.set(xlabel="real-time ceiling (active VAD streams meeting SLO)", title="T8 scheduler comparison"); save(fig, "g7_scheduler.png")

# --- T9 duty cycle ---
t9 = {os.path.basename(f): rows(os.path.basename(f)) for f in sorted(glob.glob(P("t9_*.csv"))) if "artifact" not in f and "confirm" not in f}
if t9:
    fig, ax = plt.subplots(figsize=(6, 3.5))
    xs, ys = [], []
    for name, rr in t9.items():
        c, w = ceiling(rr)
        d = float(rr[0]["duty"]); g = float(rr[0]["gate"])
        xs.append(d if g > 0 else 1.0); ys.append(c)
        summary.append(("T9", name, f"duty={d} gate={g}: connected-session RT ceiling {c} (window {w:.0f}s)"))
    order = sorted(zip(xs, ys)); ax.plot([a for a, _ in order], [b for _, b in order], "-o")
    ax.set(xlabel="speech duty cycle (1.0 = gate off, every chunk inferred)", ylabel="connected sessions at SLO", title="T9 capacity vs duty cycle"); save(fig, "g8_duty.png")

# --- T11 admission ---
fig, ax = plt.subplots(1, 2, figsize=(11, 3.5)); any11 = False
for name in ("off", "on"):
    rr = rows(f"t11_admission_{name}.csv")
    if not rr: continue
    any11 = True
    t = [int(r["t_s"]) for r in rr]
    ax[0].plot(t, [float(r["p99_ms"]) for r in rr], label=f"admission {name}: VAD p99")
    ax[1].plot(t, [int(r["active"]) for r in rr], label=f"admission {name}: active")
    ax[1].plot(t, [int(r["offered"]) for r in rr], ":", label=f"admission {name}: offered")
    for s in rows(f"t11_summary_{name}.csv"):
        summary.append(("T11", f"admission {name}", f"active={s['active']} rejected={s['rejected']} hold p99={s['hold_p99_ms']}ms drop={s['hold_drop_pct']}% slo_met={s['slo_met']}"))
if any11:
    ax[0].axhline(50, color="r", ls="--", lw=1); ax[0].set(yscale="log", xlabel="s", ylabel="ms", title="T11 VAD p99 (1 s windows)")
    ax[0].legend(fontsize=7); ax[1].legend(fontsize=7); ax[1].set(xlabel="s", title="sessions"); save(fig, "g9_admission.png")
else:
    plt.close(fig)

# --- T5/T6 containment ---
t56 = rows("t56_containment.csv")
if t56:
    faults = [r["fault"] for r in t56 if r["phase"] == "fault"]
    ctl = {r["fault"]: float(r["healthy_p99_ms"]) for r in t56 if r["phase"] == "control"}
    flt = {r["fault"]: float(r["healthy_p99_ms"]) for r in t56 if r["phase"] == "fault"}
    fig, ax = plt.subplots(figsize=(9, 3.5))
    x = range(len(faults))
    ax.bar([i - .2 for i in x], [ctl[f] for f in faults], .4, label="control (no fault)")
    ax.bar([i + .2 for i in x], [flt[f] for f in faults], .4, label="fault injected (healthy sessions)")
    ax.set_xticks(list(x)); ax.set_xticklabels(faults, rotation=30, fontsize=8)
    ax.set(ylabel="healthy VAD p99 ms", title="T5/T6 containment: healthy-session p99, control vs fault"); ax.legend(fontsize=8)
    save(fig, "g10_containment.png")

# --- Q1 ---
q1 = rows("q1_pipeline.csv")
if q1:
    fig, ax = plt.subplots(figsize=(5.5, 3.5))
    n = [int(r["processors"]) for r in q1]
    ax.plot(n, [float(r["sync_ns_per_frame"]) for r in q1], "-o", label="sync run loop (this POC)")
    ax.plot(n, [float(r["chan_ns_per_frame"]) for r in q1], "-o", label="goroutine+channel per processor")
    ax.set(xlabel="processors in pipeline", ylabel="ns per frame", title="Q1 per-frame pipeline cost"); ax.legend(fontsize=8)
    save(fig, "g11_pipeline.png")

with open(P("summary.txt"), "w") as f:
    for s in summary:
        f.write(" | ".join(s) + "\n")
print("\n".join(" | ".join(s) for s in summary))
