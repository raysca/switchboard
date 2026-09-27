// Command server is the POC HTTP control surface.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"govoice/call"
	"govoice/vad"
)

var procStart = time.Now()

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	mode := flag.String("vad-mode", "global", "global|sharded|process")
	workers := flag.Int("workers", 8, "VAD workers/shards/processes")
	batch := flag.Int("batch", 8, "VAD micro-batch")
	flag.Parse()

	rt, err := call.New(vad.Config{Mode: *mode, Workers: *workers, Batch: *batch, QueueCap: 8192}, 4)
	if err != nil {
		log.Fatal(err)
	}
	var mu sync.Mutex
	calls := map[uint64]*call.Call{}

	h := http.NewServeMux()
	h.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		o := call.Opts{Fakes: true, FakeLatency: 50 * time.Millisecond, Duty: 0.5}
		q := r.URL.Query()
		o.Feed = q.Get("feed") == "1"
		if d, err := strconv.ParseFloat(q.Get("duty"), 64); err == nil {
			o.Duty = d
		}
		c, tm, err := rt.NewCall(o)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		calls[c.S.ID] = c
		mu.Unlock()
		go func() { <-c.S.Done(); mu.Lock(); delete(calls, c.S.ID); mu.Unlock() }()
		json.NewEncoder(w).Encode(map[string]any{"id": c.S.ID, "ready_us": tm.Ready.Microseconds(),
			"allocated_us": tm.Allocated.Microseconds(), "pipeline_us": tm.PipelineCreated.Microseconds(),
			"registered_us": tm.Registered.Microseconds(), "since_process_start_ms": time.Since(procStart).Milliseconds()})
	})
	get := func(w http.ResponseWriter, r *http.Request) *call.Call {
		id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
		mu.Lock()
		defer mu.Unlock()
		c := calls[id]
		if c == nil {
			http.NotFound(w, r)
		}
		return c
	}
	h.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if c := get(w, r); c != nil {
			json.NewEncoder(w).Encode(map[string]any{"id": c.S.ID, "state": c.S.Stop().String()})
		}
	})
	h.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, s := range rt.M.List() {
			out = append(out, map[string]any{"id": s.ID, "state": s.State().String(), "tasks": s.Tasks(),
				"vad_p99_ms": float64(s.VADLatency.Quantile(0.99)) / 1e6, "audio_dropped": s.AudioDropped.Load()})
		}
		json.NewEncoder(w).Encode(out)
	})
	h.HandleFunc("POST /sessions/{id}/fault", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if mode == "native-crash" {
			nativeCrash()
		}
		if c := get(w, r); c != nil {
			if !c.Fault(mode, r.URL.Query().Get("processor")) {
				http.Error(w, "unknown fault mode", http.StatusBadRequest)
				return
			}
			fmt.Fprintln(w, "ok")
		}
	})
	h.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		st, es := rt.M.Stats(), rt.Exec.Stats()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		p := func(name string, v any) { fmt.Fprintf(w, "govoice_%s %v\n", name, v) }
		p("sessions", st.Sessions)
		p("sessions_created_total", st.Created)
		p("sessions_failed_total", st.Failures)
		p("sessions_leaked_total", st.LeakedSessions)
		p("admission_rejected_total", st.Rejected)
		p("managed_goroutines", st.ManagedGoroutines)
		p("goroutines", runtime.NumGoroutine())
		p("heap_alloc_bytes", ms.HeapAlloc)
		p("audio_dropped_total", st.AudioDropped)
		p("vad_dropped_total", st.VADDropped)
		p("vad_streams", es.Streams)
		p("vad_queue_depth", es.QueueDepth)
		p("vad_stale_total", es.Stale)
		p("vad_worker_restarts_total", es.Restarts)
		for _, q := range []float64{0.5, 0.95, 0.99} {
			fmt.Fprintf(w, "govoice_vad_latency_seconds{quantile=\"%g\"} %g\n", q, rt.M.VADLatency.Quantile(q).Seconds())
			fmt.Fprintf(w, "govoice_vad_queue_wait_seconds{quantile=\"%g\"} %g\n", q, rt.Exec.QueueWait.Quantile(q).Seconds())
			fmt.Fprintf(w, "govoice_vad_inference_seconds{quantile=\"%g\"} %g\n", q, rt.Exec.Inference.Quantile(q).Seconds())
		}
	})
	log.Printf("listening on %s (vad %s) init %v", *addr, rt.Exec.Config(), time.Since(procStart))
	log.Fatal(http.ListenAndServe(*addr, h))
}
