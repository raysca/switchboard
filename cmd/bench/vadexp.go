package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync/atomic"
	"time"

	"govoice/call"
	"govoice/voice"
)

var slo = 50 * time.Millisecond

// Window is one measurement window over a set of calls.
type Window struct {
	Streams                              int
	Wall                                 time.Duration
	QWait, Infer, Total, SubmitLag, Lag  voice.Snap
	Deliver                              voice.Snap
	SessP95Max, SessP95Med, SessP99Max   time.Duration
	Inferred, Gated, Dropped, InboxDrops uint64
	DropPct                              float64
	CPUPct, WorkerUtil                   float64
	QDepthMax                            int
	AvgBatch                             float64
	Onsets, Hits, Misses, False          int
	DelayP50, DelayP99                   time.Duration
	FalsePerMin                          float64
	Failed                               int64
	Pass                                 bool
}

type snapAll struct {
	qw, inf, tot, sub, del, lag voice.Snap
	inferred, gated, dropped    uint64
	inbox                       int64
	cpu                         float64
	busy, batches, jobs         int64
	sess                        []voice.Snap
	t                           time.Time
}

func takeSnap(rt *call.Runtime, calls []*call.Call) snapAll {
	h := rt.Host
	st, es := rt.M.Stats(), rt.Exec.Stats()
	s := snapAll{
		qw: rt.Exec.QueueWait.Snapshot(), inf: rt.Exec.Inference.Snapshot(), tot: h.Total.Snapshot(),
		sub: h.SubmitLag.Snapshot(), del: h.Deliver.Snapshot(), lag: rt.Pool.Lag(),
		inferred: h.Inferred.Count(), gated: h.Gated.Count(), dropped: h.Dropped.Count(),
		inbox: st.AudioDropped, cpu: cpuSeconds(rt.Exec.PIDs()), busy: es.BusyNanos, t: time.Now(),
	}
	for _, c := range calls {
		s.sess = append(s.sess, c.S.VADLatency.Snapshot())
	}
	return s
}

// measure runs one window over calls (all must be streaming) and returns stats.
func measure(rt *call.Runtime, calls []*call.Call, d time.Duration) Window {
	var qmax atomic.Int64
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if q := int64(rt.Exec.QueueDepth()); q > qmax.Load() {
					qmax.Store(q)
				}
			}
		}
	}()
	failed0 := countFailed(calls)
	a := takeSnap(rt, calls)
	time.Sleep(d)
	b := takeSnap(rt, calls)
	close(stop)
	w := Window{Streams: len(calls), Wall: b.t.Sub(a.t), QDepthMax: int(qmax.Load())}
	w.QWait, w.Infer, w.Total = b.qw.Sub(a.qw), b.inf.Sub(a.inf), b.tot.Sub(a.tot)
	w.SubmitLag, w.Deliver, w.Lag = b.sub.Sub(a.sub), b.del.Sub(a.del), b.lag.Sub(a.lag)
	w.Inferred, w.Gated, w.Dropped = b.inferred-a.inferred, b.gated-a.gated, b.dropped-a.dropped
	w.InboxDrops = uint64(b.inbox - a.inbox)
	if n := w.Inferred + w.Dropped + w.InboxDrops; n > 0 {
		w.DropPct = 100 * float64(w.Dropped+w.InboxDrops) / float64(n)
	}
	w.CPUPct = 100 * (b.cpu - a.cpu) / w.Wall.Seconds() / float64(runtime.NumCPU())
	w.WorkerUtil = 100 * float64(b.busy-a.busy) / float64(w.Wall) / float64(rt.Exec.Config().Workers)
	w.AvgBatch = rt.Exec.Stats().AvgBatch
	var p95s []time.Duration
	for i := range calls {
		s := b.sess[i].Sub(a.sess[i])
		if s.N == 0 {
			continue
		}
		p95s = append(p95s, s.Quantile(.95))
		w.SessP99Max = max(w.SessP99Max, s.Quantile(.99))
	}
	sort.Slice(p95s, func(i, j int) bool { return p95s[i] < p95s[j] })
	if len(p95s) > 0 {
		w.SessP95Max, w.SessP95Med = p95s[len(p95s)-1], p95s[len(p95s)/2]
	}
	var delays []time.Duration
	for _, c := range calls {
		sc := c.Score(a.t, b.t)
		w.Onsets += sc.Onsets
		w.Hits += sc.Hits
		w.Misses += sc.Misses
		w.False += sc.False
		delays = append(delays, sc.Delays...)
	}
	sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
	if len(delays) > 0 {
		w.DelayP50, w.DelayP99 = delays[len(delays)/2], delays[len(delays)*99/100]
	}
	w.FalsePerMin = float64(w.False) / float64(len(calls)) / w.Wall.Minutes()
	w.Failed = countFailed(calls) - failed0
	missPct := 0.0
	if w.Onsets > 0 {
		missPct = 100 * float64(w.Misses) / float64(w.Onsets)
	}
	accuracyOK := missPct <= 1 && w.FalsePerMin <= 1
	if rt.Exec.Config().Mode == "stub" { // harness control: no real inference, judge latency only
		accuracyOK = true
	}
	w.Pass = w.Total.Quantile(.99) <= slo && w.DropPct <= 0.1 && w.SessP95Max <= 100*time.Millisecond && accuracyOK
	return w
}

func countFailed(calls []*call.Call) int64 {
	var n int64
	for _, c := range calls {
		if c.S.State() > voice.StateRunning {
			n++
		}
	}
	return n
}

var winHeader = []string{"streams", "window_s", "pass",
	"total_p50_ms", "total_p95_ms", "total_p99_ms",
	"qwait_p50_ms", "qwait_p95_ms", "qwait_p99_ms",
	"infer_p50_ms", "infer_p95_ms", "infer_p99_ms",
	"submitlag_p99_ms", "deliver_p99_ms", "feeder_lag_p99_ms",
	"sess_p95_max_ms", "sess_p95_med_ms", "sess_p99_max_ms",
	"inferred", "gated", "dropped", "inbox_drops", "drop_pct",
	"cpu_pct", "worker_util_pct", "qdepth_max", "avg_batch",
	"onsets", "hits", "misses", "false", "false_per_min", "onset_delay_p50_ms", "onset_delay_p99_ms", "failed"}

func (w Window) cols() []any {
	return []any{w.Streams, f2(w.Wall.Seconds()), w.Pass,
		w.Total.Quantile(.5), w.Total.Quantile(.95), w.Total.Quantile(.99),
		w.QWait.Quantile(.5), w.QWait.Quantile(.95), w.QWait.Quantile(.99),
		w.Infer.Quantile(.5), w.Infer.Quantile(.95), w.Infer.Quantile(.99),
		w.SubmitLag.Quantile(.99), w.Deliver.Quantile(.99), w.Lag.Quantile(.99),
		w.SessP95Max, w.SessP95Med, w.SessP99Max,
		w.Inferred, w.Gated, w.Dropped, w.InboxDrops, w.DropPct,
		w.CPUPct, w.WorkerUtil, w.QDepthMax, w.AvgBatch,
		w.Onsets, w.Hits, w.Misses, w.False, w.FalsePerMin, w.DelayP50, w.DelayP99, w.Failed}
}

func (w Window) String() string {
	return fmt.Sprintf("n=%d pass=%v p99=%v qwait99=%v infer99=%v lag99=%v sessP95max=%v drop=%.3f%% cpu=%.0f%% util=%.0f%% batch=%.1f onsets %d/%d false=%d",
		w.Streams, w.Pass, w.Total.Quantile(.99), w.QWait.Quantile(.99), w.Infer.Quantile(.99), w.Lag.Quantile(.99),
		w.SessP95Max, w.DropPct, w.CPUPct, w.WorkerUtil, w.AvgBatch, w.Hits, w.Onsets, w.False)
}

// T7/T8/T9/T10: ramp active streams through plateaus until SLO clearly breaks.
func expVAD() {
	streams := flag.String("streams", "100,200,400,800,1200,1600,2000,2500,3000,3500,4000,5000,6000,7000,8000", "")
	window := flag.Duration("window", 10*time.Second, "")
	long := flag.Duration("long", 30*time.Second, "window near the knee")
	duty := flag.Float64("duty", 0.5, "")
	gate := flag.Float64("gate", 0, "energy gate RMS")
	fakes := flag.Bool("fakes", false, "add fake STT/LLM/TTS (T10)")
	name := flag.String("name", "", "output file name")
	flag.CommandLine.Parse(os.Args[2:])
	rt := newRT()
	defer rt.Close()
	fn := *name
	if fn == "" {
		fn = fmt.Sprintf("t7_%s%s.csv", vadCfg(), *tag)
	}
	w := newCSV(fn, append([]string{"config", "duty", "gate", "fakes"}, winHeader...)...)
	defer w.Close()
	var calls []*call.Call
	nearKnee, broken := false, 0
	for _, n := range ints(*streams) {
		for len(calls) < n {
			c, _, err := rt.NewCall(call.Opts{Fakes: *fakes, FakeLatency: 100 * time.Millisecond, Feed: true,
				Duty: *duty, Seed: uint64(len(calls) + 1), Gate: float32(*gate)})
			if err != nil {
				panic(err)
			}
			calls = append(calls, c)
		}
		time.Sleep(3 * time.Second)
		d := *window
		if nearKnee {
			d = *long
		}
		res := measure(rt, live(calls), d)
		w.Row(append([]any{vadCfg().String(), *duty, *gate, *fakes}, res.cols()...)...)
		logf("%s %s", vadCfg(), res)
		if res.Total.Quantile(.99) > slo/3 || res.DropPct > 0.02 || res.WorkerUtil > 80 || res.CPUPct > 80 {
			nearKnee = true
		}
		if !res.Pass {
			broken++
		}
		if broken >= 2 || res.Total.Quantile(.99) > 10*slo || res.DropPct > 10 {
			return
		}
	}
}

// live drops calls that were torn down (should be none in a healthy sweep).
func live(calls []*call.Call) []*call.Call {
	out := calls[:0:0]
	for _, c := range calls {
		if c.S.State() == voice.StateRunning {
			out = append(out, c)
		}
	}
	return out
}
