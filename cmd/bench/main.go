// Command bench runs the POC experiments (T1–T11, Q1) and writes CSVs to results/.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/call"
	"govoice/vad"
	"govoice/voice"
)

var (
	outDir   = flag.String("out", "results", "output dir")
	mode     = flag.String("mode", "global", "vad executor: global|sharded|process|stub")
	workers  = flag.Int("workers", 11, "vad workers/shards/processes")
	batch    = flag.Int("batch", 8, "vad micro-batch")
	intra    = flag.Int("intra", 1, "ORT intra-op threads")
	stubCost = flag.Duration("stubcost", 75*time.Microsecond, "stub inference cost")
	feeders  = flag.Int("feeders", 4, "load-generator feeder goroutines")
	tag      = flag.String("tag", "", "suffix for output files")
)

func vadCfg() vad.Config {
	return vad.Config{Mode: *mode, Workers: *workers, Batch: *batch, IntraOp: *intra, QueueCap: 16384, StubCost: *stubCost}
}

func newRT() *call.Runtime {
	rt, err := call.New(vadCfg(), *feeders)
	if err != nil {
		panic(err)
	}
	return rt
}

func ints(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		v, _ := strconv.Atoi(p)
		out = append(out, v)
	}
	return out
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: bench <startup|burst|memory|churn|pipeline|vad|contain|admission|isolation|cold> [flags]")
		os.Exit(2)
	}
	exp := os.Args[1] // each experiment defines its own flags, then parses
	switch exp {
	case "startup":
		expStartup()
	case "burst":
		expBurst()
	case "memory":
		expMemory()
	case "churn":
		expChurn()
	case "pipeline":
		expPipeline()
	case "vad":
		expVAD()
	case "contain":
		expContain()
	case "admission":
		expAdmission()
	case "isolation":
		expIsolation()
	case "cold":
		expColdHTTP()
	default:
		fmt.Println("unknown experiment", exp)
		os.Exit(2)
	}
}

var idle = call.Opts{Fakes: true, FakeLatency: 50 * time.Millisecond}

// T1: warm session startup vs number of existing (idle, connected) sessions.
func expStartup() {
	plateaus := flag.String("plateaus", "0,1,10,50,100,200,500,1000,2000,5000", "")
	samples := flag.Int("samples", 200, "")
	flag.CommandLine.Parse(os.Args[2:])
	rt := newRT()
	defer rt.Close()
	w := newCSV("t1_startup.csv", "existing", "sample", "allocated_us", "pipeline_us", "registered_us", "ready_us")
	defer w.Close()
	var held []*call.Call
	for _, p := range ints(*plateaus) {
		for len(held) < p {
			c, _, _ := rt.NewCall(idle)
			held = append(held, c)
		}
		runtime.GC()
		var rs []time.Duration
		for i := 0; i < *samples; i++ {
			c, tm, err := rt.NewCall(idle)
			if err != nil {
				panic(err)
			}
			us := func(d time.Duration) string { return f3(float64(d) / 1e3) }
			w.Row(p, i, us(tm.Allocated), us(tm.PipelineCreated), us(tm.Registered), us(tm.Ready))
			rs = append(rs, tm.Ready)
			c.S.Stop()
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
		logf("existing=%d ready p50=%v p99=%v", p, rs[len(rs)/2], rs[len(rs)*99/100])
	}
}

// T2: burst creation while 200 sessions stream audio.
func expBurst() {
	base := flag.Int("base", 200, "streaming sessions")
	bursts := flag.String("bursts", "10,50,100,250,1000", "")
	flag.CommandLine.Parse(os.Args[2:])
	rt := newRT()
	defer rt.Close()
	for i := 0; i < *base; i++ {
		rt.NewCall(call.Opts{Fakes: true, FakeLatency: 50 * time.Millisecond, Feed: true, Duty: 0.5, Seed: uint64(i + 1)})
	}
	time.Sleep(3 * time.Second)
	w := newCSV("t2_burst.csv", "burst", "ready_p50_ms", "ready_p95_ms", "ready_p99_ms", "wall_p50_ms", "wall_p99_ms",
		"running_vad_p99_before_ms", "running_vad_p99_during_ms", "running_failed")
	defer w.Close()
	for _, b := range ints(*bursts) {
		s0 := rt.M.VADLatency.Snapshot()
		time.Sleep(2 * time.Second)
		s1 := rt.M.VADLatency.Snapshot()
		before := s1.Sub(s0)
		var mu sync.Mutex
		var ready, wall []time.Duration
		var made []*call.Call
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := 0; i < b; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				t0 := time.Now()
				c, tm, err := rt.NewCall(idle)
				d := time.Since(t0)
				if err != nil {
					return
				}
				mu.Lock()
				ready, wall, made = append(ready, tm.Ready), append(wall, d), append(made, c)
				mu.Unlock()
			}()
		}
		close(gate)
		wg.Wait()
		time.Sleep(2 * time.Second)
		s2 := rt.M.VADLatency.Snapshot()
		during := s2.Sub(s1)
		for _, c := range made {
			c.S.Stop()
		}
		q := func(v []time.Duration, p float64) time.Duration {
			sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
			return v[min(len(v)-1, int(float64(len(v))*p))]
		}
		w.Row(b, q(ready, .5), q(ready, .95), q(ready, .99), q(wall, .5), q(wall, .99),
			before.Quantile(.99), during.Quantile(.99), rt.M.Stats().Failures)
		logf("burst=%d ready p99=%v wall p99=%v running vad p99 %v -> %v", b, q(ready, .99), q(wall, .99), before.Quantile(.99), during.Quantile(.99))
	}
}

// T3: memory/resources vs sessions (idle connected, then streaming), and return after teardown.
func expMemory() {
	idleSteps := flag.String("idle", "0,100,500,1000,2000,5000,10000", "")
	streamSteps := flag.String("streaming", "0,100,250,500,1000", "")
	flag.CommandLine.Parse(os.Args[2:])
	rt := newRT()
	defer rt.Close()
	w := newCSV("t3_memory.csv", append([]string{"kind", "sessions", "managed_goroutines", "vad_streams"}, resHeader...)...)
	defer w.Close()
	row := func(kind string, n int) {
		runtime.GC()
		runtime.GC()
		time.Sleep(500 * time.Millisecond)
		r := sample(true)
		w.Row(kind, n, rt.M.Stats().ManagedGoroutines, rt.Exec.Stats().Streams, r.cols())
		logf("%s n=%d rss=%.1fMB footprint=%.1fMB heap=%.1fMB goroutines=%d threads=%d", kind, n, r.RSSMB, r.FootprintMB, r.HeapMB, r.Goroutines, r.Threads)
	}
	run := func(kind string, steps []int, o call.Opts, settle time.Duration) {
		var held []*call.Call
		for _, n := range steps {
			for len(held) < n {
				o.Seed = uint64(len(held) + 1)
				c, _, _ := rt.NewCall(o)
				held = append(held, c)
			}
			time.Sleep(settle)
			row(kind, n)
		}
		for _, c := range held {
			c.S.Stop()
		}
		row(kind+"-teardown", 0)
	}
	run("idle", ints(*idleSteps), idle, 0)
	run("streaming", ints(*streamSteps), call.Opts{Fakes: true, FakeLatency: 50 * time.Millisecond, Feed: true, Duty: 0.5}, 5*time.Second)
}

// T4: lifecycle churn at constant concurrency.
func expChurn() {
	target := flag.Int("n", 200, "concurrent sessions")
	cycles := flag.Int("cycles", 20000, "")
	flag.CommandLine.Parse(os.Args[2:])
	rt := newRT()
	defer rt.Close()
	w := newCSV("t4_churn.csv", append([]string{"t_s", "cycles", "sessions", "vad_streams", "managed_goroutines", "run_loops", "teardown_p99_ms", "failed", "leaked"}, resHeader...)...)
	defer w.Close()
	type live struct {
		c   *call.Call
		end time.Time
	}
	var mu sync.Mutex
	var calls []live
	var teardown voice.Hist
	var done, seed int64
	spawn := func() {
		seed++
		c, _, err := rt.NewCall(call.Opts{Fakes: true, FakeLatency: 20 * time.Millisecond, Feed: true, Duty: 0.5, Seed: uint64(seed)})
		if err != nil {
			panic(err)
		}
		calls = append(calls, live{c, time.Now().Add(time.Second + time.Duration(rand.Int64N(int64(2*time.Second))))})
	}
	for i := 0; i < *target; i++ {
		spawn()
	}
	t0 := time.Now()
	lastSample := t0
	prev := teardown.Snapshot()
	for {
		now := time.Now()
		mu.Lock()
		keep := calls[:0]
		for _, l := range calls {
			if now.After(l.end) {
				go func(c *call.Call) {
					t := time.Now()
					c.S.Stop()
					teardown.Record(time.Since(t))
					mu.Lock()
					done++
					mu.Unlock()
				}(l.c)
				continue
			}
			keep = append(keep, l)
		}
		calls = keep
		for len(calls) < *target {
			spawn()
		}
		d := done
		mu.Unlock()
		if now.Sub(lastSample) >= 2*time.Second || d >= int64(*cycles) {
			lastSample = now
			cur := teardown.Snapshot()
			win := cur.Sub(prev)
			prev = cur
			st := rt.M.Stats()
			r := sample(true)
			w.Row(int(now.Sub(t0).Seconds()), d, st.Sessions, rt.Exec.Stats().Streams, st.ManagedGoroutines, st.RunLoops, win.Quantile(.99), st.Failures, st.LeakedSessions, r.cols())
			logf("t=%v cycles=%d heap=%.1fMB rss=%.1fMB goroutines=%d", now.Sub(t0).Round(time.Second), d, r.HeapMB, r.RSSMB, r.Goroutines)
		}
		if d >= int64(*cycles) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Q1: per-frame cost of the synchronous pipeline vs goroutine+channel per processor.
type pass struct{}

func (pass) ProcessFrame(_ context.Context, f voice.Frame, d voice.Direction, e voice.Emitter) error {
	return e.PushDirection(f, d)
}

func expPipeline() {
	flag.CommandLine.Parse(os.Args[2:])
	w := newCSV("q1_pipeline.csv", "processors", "sync_ns_per_frame", "chan_ns_per_frame", "ratio", "sync_allocs", "chan_allocs")
	defer w.Close()
	m := voice.NewManager()
	for _, n := range []int{1, 4, 8, 16} {
		sync := testing.Benchmark(func(b *testing.B) {
			done := make(chan struct{}, 1024)
			s, _, _ := m.Create(voice.Spec{Build: func(*voice.Session) ([]voice.Processor, error) {
				ps := make([]voice.Processor, n)
				for i := range ps {
					ps[i] = pass{}
				}
				return ps, nil
			}})
			s.OnOutput = func(voice.Frame) { done <- struct{}{} }
			f := &voice.TextFrame{}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for s.Post(f, voice.Downstream, 0) != nil {
					runtime.Gosched()
				}
				<-done
			}
			b.StopTimer()
			s.Stop()
		})
		ch := testing.Benchmark(func(b *testing.B) {
			chans := make([]chan voice.Frame, n+1)
			for i := range chans {
				chans[i] = make(chan voice.Frame, 64)
			}
			ctx, cancel := context.WithCancel(context.Background())
			for i := 0; i < n; i++ {
				go func(in, out chan voice.Frame) {
					for {
						select {
						case f := <-in:
							out <- f
						case <-ctx.Done():
							return
						}
					}
				}(chans[i], chans[i+1])
			}
			f := &voice.TextFrame{}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				chans[0] <- f
				<-chans[n]
			}
			b.StopTimer()
			cancel()
		})
		w.Row(n, sync.NsPerOp(), ch.NsPerOp(), float64(ch.NsPerOp())/float64(sync.NsPerOp()), sync.AllocsPerOp(), ch.AllocsPerOp())
		logf("procs=%d sync=%dns chan=%dns", n, sync.NsPerOp(), ch.NsPerOp())
	}
}
