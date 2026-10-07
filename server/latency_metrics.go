package server

import (
	"fmt"
	"io"
	"sync"
	"time"

	"govoice/core/observe"
)

var latencyBuckets = [...]float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10}

// latencyHistograms is a Prometheus histogram family labelled only by the
// bounded observe.Stage set; unknown stages are ignored.
type latencyHistograms struct {
	mu sync.Mutex
	h  map[observe.Stage]*hist
}

type hist struct {
	counts [len(latencyBuckets) + 1]uint64 // last is +Inf
	sum    float64
	n      uint64
}

// ObserveLatency records one stage latency.
func (m *Metrics) ObserveLatency(stage observe.Stage, d time.Duration) { m.latency.observe(stage, d) }

func (l *latencyHistograms) observe(stage observe.Stage, d time.Duration) {
	known := false
	for _, s := range observe.Stages {
		known = known || s == stage
	}
	if !known {
		return
	}
	v := d.Seconds()
	i := 0
	for i < len(latencyBuckets) && v > latencyBuckets[i] {
		i++
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.h == nil {
		l.h = make(map[observe.Stage]*hist)
	}
	h := l.h[stage]
	if h == nil {
		h = &hist{}
		l.h[stage] = h
	}
	h.counts[i]++
	h.sum += v
	h.n++
}

func (l *latencyHistograms) write(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(w, "# HELP voice_latency_seconds Per-turn pipeline latency by stage.\n# TYPE voice_latency_seconds histogram\n")
	for _, s := range observe.Stages {
		h := l.h[s]
		if h == nil {
			h = &hist{}
		}
		var cum uint64
		for i, le := range latencyBuckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "voice_latency_seconds_bucket{stage=%q,le=\"%g\"} %d\n", s, le, cum)
		}
		fmt.Fprintf(w, "voice_latency_seconds_bucket{stage=%q,le=\"+Inf\"} %d\n", s, h.n)
		fmt.Fprintf(w, "voice_latency_seconds_sum{stage=%q} %g\nvoice_latency_seconds_count{stage=%q} %d\n", s, h.sum, s, h.n)
	}
}
