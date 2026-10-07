package main

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	s := stat([]float64{5, 1, 4, 2, 3, 6, 7, 8, 9, 10})
	if s.P50 != 5 || s.P95 != 10 || s.P99 != 10 || s.Max != 10 || s.N != 10 {
		t.Fatalf("%+v", s)
	}
	if percentile(nil, 50) != 0 {
		t.Fatal("empty")
	}
	if s := stat([]float64{7}); s.P50 != 7 || s.P99 != 7 { // nearest-rank: tiny N collapses to max
		t.Fatalf("%+v", s)
	}
}

// Bounds are loose on purpose (-race, loaded CI): this guards wiring, not speed.
func TestOverheadSane(t *testing.T) {
	r, err := run(cfg{turns: 5, sessions: 2, stt: 20 * time.Millisecond, llm: 20 * time.Millisecond, tts: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if r.Overhead.N != 10 || r.Overhead.P50 < -1 || r.Overhead.P95 > 500 {
		t.Fatalf("overhead %+v", r.Overhead)
	}
	if _, ok := r.Stages["startup"]; ok || r.Stages["voice_to_voice"].N != 10 {
		t.Fatalf("stages %+v", r.Stages)
	}
	if r.GoroutinesAfter > r.GoroutinesBefore+2 {
		t.Fatalf("goroutine leak %d -> %d", r.GoroutinesBefore, r.GoroutinesAfter)
	}
}
