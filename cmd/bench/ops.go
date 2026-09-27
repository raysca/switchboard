package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"govoice/call"
	"govoice/vad"
	"govoice/voice"
)

var maxDrop = flag.Int("maxdrop", 0, "override Budget.MaxDroppedAudio (debug)")

func streamingCall(rt *call.Runtime, seed int) *call.Call {
	b := voice.DefaultBudget()
	if *maxDrop > 0 {
		b.MaxDroppedAudio = *maxDrop
	}
	c, _, err := rt.NewCall(call.Opts{Fakes: true, FakeLatency: 50 * time.Millisecond, Feed: true, Duty: 0.5, Seed: uint64(seed), Budget: b})
	if err != nil {
		panic(err)
	}
	return c
}

// T5/T6: fault containment near capacity, each fault paired with a no-fault control window.
func expContain() {
	n := flag.Int("n", 2000, "healthy streaming sessions (~80% of RT ceiling)")
	window := flag.Duration("window", 15*time.Second, "")
	faults := flag.String("faults", "panic,childpanic,error,flood,overflow,slowsync,vadabuse,stall,cancel,hang,badteardown", "")
	targets := flag.Int("targets", 5, "")
	flag.CommandLine.Parse(os.Args[2:])
	rt := newRT()
	defer rt.Close()
	w := newCSV("t56_containment.csv", "fault", "phase", "healthy", "healthy_p50_ms", "healthy_p99_ms", "healthy_sess_p95_max_ms",
		"healthy_failed", "healthy_drop_pct", "cpu_pct", "qdepth_max", "target_states", "contain_ms_max",
		"leaked_sessions", "leaked_goroutines", "goroutines_before", "goroutines_after", "heap_before_mb", "heap_after_mb", "pass")
	defer w.Close()
	seed := 0
	calls := make([]*call.Call, 0, *n)
	for len(calls) < *n {
		seed++
		calls = append(calls, streamingCall(rt, seed))
	}
	time.Sleep(5 * time.Second)
	for _, fault := range strings.Split(*faults, ",") {
		ctl := measure(rt, calls, *window) // no forced GC here: it pollutes the control window
		w.Row(fault, "control", len(calls), ctl.Total.Quantile(.5), ctl.Total.Quantile(.99), ctl.SessP95Max, ctl.Failed, ctl.DropPct, ctl.CPUPct, ctl.QDepthMax, "", "", "", "", "", "", "", "", "")
		// pick targets
		rand.Shuffle(len(calls), func(i, j int) { calls[i], calls[j] = calls[j], calls[i] })
		tg, healthy := calls[:*targets], calls[*targets:]
		st0 := rt.M.Stats()
		g0, h0 := runtime.NumGoroutine(), sample(false).HeapMB
		t0 := time.Now()
		var contain atomic.Int64
		for _, c := range tg {
			mode := fault
			if fault == "cancel" {
				mode = "stall"
			}
			if !c.Fault(mode, "stt") {
				panic("bad fault " + fault)
			}
			go func(c *call.Call) {
				switch fault {
				case "stall", "cancel", "hang", "badteardown":
					time.Sleep(time.Second) // work outstanding, then the call is hung up
					c.S.Stop()
				}
				<-c.S.Done()
				if d := int64(time.Since(t0)); d > contain.Load() {
					contain.Store(d)
				}
			}(c)
		}
		res := measure(rt, healthy, *window)
		var states []string
		for _, c := range tg {
			select {
			case <-c.S.Done():
			case <-time.After(5 * time.Second):
			}
			states = append(states, c.S.State().String())
		}
		time.Sleep(time.Second)
		runtime.GC()
		st1 := rt.M.Stats()
		leakedSess := st1.LeakedSessions - st0.LeakedSessions
		// leaked goroutines: managed goroutines still alive for sessions no longer registered + stuck run loops
		leakedG := st1.RunLoops - st1.Sessions + (st1.ManagedGoroutines - managedOfLive(rt))
		g1, h1 := runtime.NumGoroutine(), sample(false).HeapMB
		allTerminal := true
		for _, s := range states {
			if s == "running" || s == "stopping" {
				allTerminal = false
			}
		}
		p99Delta := ms(res.Total.Quantile(.99) - ctl.Total.Quantile(.99))
		pass := allTerminal && res.Failed == 0 && leakedSess == 0 &&
			(p99Delta <= 5 || p99Delta <= 0.2*ms(ctl.Total.Quantile(.99)))
		w.Row(fault, "fault", len(healthy), res.Total.Quantile(.5), res.Total.Quantile(.99), res.SessP95Max, res.Failed, res.DropPct,
			res.CPUPct, res.QDepthMax, strings.Join(states, "|"), f2(float64(contain.Load())/1e6), leakedSess, leakedG, g0, g1, f2(h0), f2(h1), pass)
		logf("fault=%s ctl p99=%v fault p99=%v healthyFailed=%d states=%v contain=%v leakedSess=%d leakedG=%d pass=%v",
			fault, ctl.Total.Quantile(.99), res.Total.Quantile(.99), res.Failed, states, time.Duration(contain.Load()), leakedSess, leakedG, pass)
		// replace targets
		for i := range tg {
			seed++
			calls[i] = streamingCall(rt, seed)
		}
		time.Sleep(2 * time.Second)
	}
	close(voice.HangRelease)
	time.Sleep(time.Second)
	logf("after HangRelease: managed goroutines=%d (expect ~live tasks only)", rt.M.Stats().ManagedGoroutines)
}

func managedOfLive(rt *call.Runtime) int64 {
	var n int64
	for _, s := range rt.M.List() {
		n += int64(s.Tasks())
	}
	return n
}

// T11: ramp offered load past the ceiling with admission off/on.
func expAdmission() {
	ceiling := flag.Int("ceiling", 3000, "RT ceiling from T7 (active streams)")
	adm := flag.Bool("admission", true, "")
	rate := flag.Int("rate", 100, "new sessions per second")
	qwMax := flag.Duration("qwait", 15*time.Millisecond, "queue-wait p99 feedback threshold")
	hold := flag.Duration("hold", 20*time.Second, "")
	flag.CommandLine.Parse(os.Args[2:])
	rt := newRT()
	defer rt.Close()
	if *adm {
		var closed atomic.Bool
		go func() { // feedback: VAD queue-wait p99 over the last second
			prev := rt.Exec.QueueWait.Snapshot()
			for range time.Tick(250 * time.Millisecond) {
				cur := rt.Exec.QueueWait.Snapshot()
				win := cur.Sub(prev)
				if win.N > 1000 {
					closed.Store(win.Quantile(.99) > *qwMax)
					prev = cur
				}
			}
		}()
		limit := *ceiling
		f := func() error {
			if rt.M.Len() >= limit || closed.Load() {
				return voice.ErrAdmission
			}
			return nil
		}
		rt.M.Admit.Store(&f)
	}
	name := "off"
	if *adm {
		name = "on"
	}
	tl := newCSV("t11_admission_"+name+".csv", "t_s", "offered", "active", "rejected", "p99_ms", "qwait_p99_ms", "drop_pct", "cpu_pct")
	defer tl.Close()
	var calls []*call.Call
	offered, seed := 0, 0
	offer := func() {
		offered++
		seed++
		c, _, err := rt.NewCall(call.Opts{Fakes: true, FakeLatency: 50 * time.Millisecond, Feed: true, Duty: 0.5, Seed: uint64(seed)})
		if err == nil {
			calls = append(calls, c)
		}
	}
	for offered < *ceiling*7/10 {
		offer()
	}
	target := *ceiling * 3 / 2
	t0 := time.Now()
	prevT, prevQ, prevDrop, prevInf, prevCPU := rt.Host.Total.Snapshot(), rt.Exec.QueueWait.Snapshot(), rt.Host.Dropped.Count(), rt.Host.Inferred.Count(), cpuSeconds(nil)
	holdStart := time.Time{}
	var holdA snapAll
	tick := time.NewTicker(time.Second / time.Duration(*rate))
	lastRow := time.Now()
	for {
		<-tick.C
		if offered < target {
			offer()
		} else if holdStart.IsZero() {
			holdStart = time.Now()
			holdA = takeSnap(rt, nil)
		}
		if time.Since(lastRow) >= time.Second {
			lastRow = time.Now()
			t, q, d, i, cpu := rt.Host.Total.Snapshot(), rt.Exec.QueueWait.Snapshot(), rt.Host.Dropped.Count(), rt.Host.Inferred.Count(), cpuSeconds(nil)
			wt, wq := t.Sub(prevT), q.Sub(prevQ)
			dp := 0.0
			if tot := (d - prevDrop) + (i - prevInf); tot > 0 {
				dp = 100 * float64(d-prevDrop) / float64(tot)
			}
			tl.Row(int(time.Since(t0).Seconds()), offered, rt.M.Len(), rt.M.Stats().Rejected, wt.Quantile(.99), wq.Quantile(.99), dp, 100*(cpu-prevCPU)/float64(runtime.NumCPU()))
			logf("adm=%s offered=%d active=%d rejected=%d p99=%v qwait99=%v drop=%.2f%%", name, offered, rt.M.Len(), rt.M.Stats().Rejected, wt.Quantile(.99), wq.Quantile(.99), dp)
			prevT, prevQ, prevDrop, prevInf, prevCPU = t, q, d, i, cpu
		}
		if !holdStart.IsZero() && time.Since(holdStart) >= *hold {
			break
		}
	}
	holdB := takeSnap(rt, nil)
	tot := holdB.tot.Sub(holdA.tot)
	dp := 0.0
	if n := (holdB.dropped - holdA.dropped) + (holdB.inferred - holdA.inferred); n > 0 {
		dp = 100 * float64(holdB.dropped-holdA.dropped) / float64(n)
	}
	sum := newCSV("t11_summary_"+name+".csv", "admission", "ceiling", "offered", "active", "rejected", "hold_p99_ms", "hold_drop_pct", "slo_met")
	defer sum.Close()
	sum.Row(name, *ceiling, offered, rt.M.Len(), rt.M.Stats().Rejected, tot.Quantile(.99), dp, tot.Quantile(.99) <= slo && dp <= 0.1)
	logf("SUMMARY admission=%s active=%d rejected=%d hold p99=%v drop=%.3f%%", name, rt.M.Len(), rt.M.Stats().Rejected, tot.Quantile(.99), dp)
}

// Isolation limits: (a) worker-process crash under load (Strategy C), (b) native crash in-process.
func expIsolation() {
	n := flag.Int("n", 1000, "streaming sessions for worker-crash test")
	flag.CommandLine.Parse(os.Args[2:])
	w := newCSV("isolation.csv", "test", "sessions_before", "sessions_after", "sessions_failed", "worker_restarts", "jobs_failed", "p99_before_ms", "p99_during_ms", "host_alive", "note")
	defer w.Close()

	// (a) Strategy C: kill -9 VAD worker processes under load.
	*mode = "process"
	rt := newRT()
	var calls []*call.Call
	for i := 0; i < *n; i++ {
		calls = append(calls, streamingCall(rt, i+1))
	}
	time.Sleep(3 * time.Second)
	before := measure(rt, calls, 5*time.Second)
	done := make(chan Window)
	go func() { done <- measure(rt, calls, 12*time.Second) }()
	for k := 0; k < 3; k++ {
		time.Sleep(3 * time.Second)
		pids := rt.Exec.PIDs()
		syscall.Kill(pids[k%len(pids)], syscall.SIGKILL)
		logf("killed vad worker pid %d", pids[k%len(pids)])
	}
	during := <-done
	es := rt.Exec.Stats()
	w.Row("vadworker-kill9-x3", len(calls), rt.M.Len(), countFailed(calls), es.Restarts, es.WorkerErrors,
		before.Total.Quantile(.99), during.Total.Quantile(.99), true, "Strategy C: worker processes SIGKILLed under load")
	logf("worker kill: sessions %d -> %d failed=%d restarts=%d jobsFailed=%d p99 %v -> %v", len(calls), rt.M.Len(), countFailed(calls), es.Restarts, es.WorkerErrors, before.Total.Quantile(.99), during.Total.Quantile(.99))
	rt.Close()

	// (b) native crash inside the shared process (cgo null deref).
	srv := exec.Command(filepath.Join(vad.Root(), "bin", "server"), "-addr", "127.0.0.1:18099", "-vad-mode", "global", "-workers", "4")
	srv.Stderr = io.Discard
	if err := srv.Start(); err != nil {
		panic(err)
	}
	waitHTTP("http://127.0.0.1:18099/sessions")
	for i := 0; i < 50; i++ {
		http.Post("http://127.0.0.1:18099/sessions?feed=1", "", nil)
	}
	time.Sleep(2 * time.Second)
	http.Post("http://127.0.0.1:18099/sessions/1/fault?mode=native-crash", "", nil)
	err := srv.Wait()
	_, gerr := http.Get("http://127.0.0.1:18099/sessions")
	w.Row("inprocess-native-crash", 50, 0, 50, 0, 0, "", "", gerr == nil, fmt.Sprintf("server exit: %v", err))
	logf("native crash: server exit=%v, reachable after=%v", err, gerr == nil)
}

func waitHTTP(url string) time.Duration {
	t0 := time.Now()
	for {
		if r, err := http.Get(url); err == nil {
			r.Body.Close()
			return time.Since(t0)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// T1 over HTTP: cold process -> first session ready, then warm POST /sessions RTT vs existing sessions.
func expColdHTTP() {
	samples := flag.Int("samples", 100, "")
	flag.CommandLine.Parse(os.Args[2:])
	w := newCSV("t1_http.csv", "kind", "existing", "sample", "ms")
	defer w.Close()
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		srv := exec.Command(filepath.Join(vad.Root(), "bin", "server"), "-addr", "127.0.0.1:18098", "-workers", "11")
		srv.Stderr = io.Discard
		srv.Start()
		for {
			r, err := http.Post("http://127.0.0.1:18098/sessions", "", nil)
			if err == nil && r.StatusCode == 200 {
				r.Body.Close()
				break
			}
			time.Sleep(time.Millisecond)
		}
		w.Row("cold_first_session", 0, i, ms(time.Since(t0)))
		logf("cold exec -> first session ready: %v", time.Since(t0))
		if i < 4 {
			srv.Process.Kill()
			srv.Wait()
			continue
		}
		existing := 1
		for _, p := range []int{1, 100, 1000} {
			for existing < p {
				r, _ := http.Post("http://127.0.0.1:18098/sessions", "", nil)
				r.Body.Close()
				existing++
			}
			for k := 0; k < *samples; k++ {
				t := time.Now()
				r, err := http.Post("http://127.0.0.1:18098/sessions", "", nil)
				if err != nil {
					panic(err)
				}
				r.Body.Close()
				w.Row("warm_http_post", p, k, ms(time.Since(t)))
				existing++
			}
		}
		srv.Process.Kill()
		srv.Wait()
	}
}
