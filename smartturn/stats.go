package smartturn

import (
	"fmt"
	"io"
	"sync/atomic"
)

// Fallback reasons: a bounded label set for voice_smartturn_fallbacks_total.
const (
	ReasonTimeout     = "timeout"     // verdict not ready inside the latency budget
	ReasonError       = "error"       // model/worker error or bad output
	ReasonOverload    = "overload"    // every worker busy
	ReasonUnavailable = "unavailable" // no worker alive (crashed / crash loop)
	ReasonNoAudio     = "no_audio"    // nothing buffered for the turn
)

var reasons = [...]string{ReasonTimeout, ReasonError, ReasonOverload, ReasonUnavailable, ReasonNoAudio}

var latencyBoundsMs = [...]float64{5, 10, 20, 35, 50, 100, 250, 500}

// stats are process-wide counters rendered by WriteMetrics.
var stats struct {
	fallbacks [len(reasons)]atomic.Uint64
	crashes   atomic.Uint64
	complete  atomic.Uint64
	incompl   atomic.Uint64
	buckets   [len(latencyBoundsMs) + 1]atomic.Uint64 // last = +Inf
	latSumUs  atomic.Uint64
	latCount  atomic.Uint64
}

func observeLatency(ms float64) {
	i := 0
	for i < len(latencyBoundsMs) && ms > latencyBoundsMs[i] {
		i++
	}
	stats.buckets[i].Add(1)
	stats.latSumUs.Add(uint64(ms * 1000))
	stats.latCount.Add(1)
}

func countFallback(reason string) {
	for i, r := range reasons {
		if r == reason {
			stats.fallbacks[i].Add(1)
			return
		}
	}
}

// Fallbacks returns the fallback count for a reason (tests, diagnostics).
func Fallbacks(reason string) uint64 {
	for i, r := range reasons {
		if r == reason {
			return stats.fallbacks[i].Load()
		}
	}
	return 0
}

// Crashes is the number of worker processes lost (exit, hang, bad frame).
func Crashes() uint64 { return stats.crashes.Load() }

// WriteMetrics renders Prometheus text for the smart-turn model.
func WriteMetrics(w io.Writer) {
	fmt.Fprint(w, "# HELP voice_smartturn_inference_ms Smart-turn inference latency.\n# TYPE voice_smartturn_inference_ms histogram\n")
	var cum uint64
	for i, b := range latencyBoundsMs {
		cum += stats.buckets[i].Load()
		fmt.Fprintf(w, "voice_smartturn_inference_ms_bucket{le=\"%g\"} %d\n", b, cum)
	}
	cum += stats.buckets[len(latencyBoundsMs)].Load()
	fmt.Fprintf(w, "voice_smartturn_inference_ms_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "voice_smartturn_inference_ms_sum %g\nvoice_smartturn_inference_ms_count %d\n", float64(stats.latSumUs.Load())/1000, stats.latCount.Load())
	fmt.Fprint(w, "# HELP voice_smartturn_fallbacks_total Turns that fell back to speech-timeout, by reason.\n# TYPE voice_smartturn_fallbacks_total counter\n")
	for i, r := range reasons {
		fmt.Fprintf(w, "voice_smartturn_fallbacks_total{reason=%q} %d\n", r, stats.fallbacks[i].Load())
	}
	fmt.Fprint(w, "# HELP voice_smartturn_verdicts_total Model verdicts.\n# TYPE voice_smartturn_verdicts_total counter\n")
	fmt.Fprintf(w, "voice_smartturn_verdicts_total{verdict=\"complete\"} %d\nvoice_smartturn_verdicts_total{verdict=\"incomplete\"} %d\n", stats.complete.Load(), stats.incompl.Load())
	fmt.Fprint(w, "# HELP voice_smartturn_worker_crashes_total Smart-turn worker processes lost.\n# TYPE voice_smartturn_worker_crashes_total counter\n")
	fmt.Fprintf(w, "voice_smartturn_worker_crashes_total %d\n", stats.crashes.Load())
}
