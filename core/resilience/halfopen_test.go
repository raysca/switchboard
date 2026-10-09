package resilience

import (
	"testing"
	"time"
)

// The Open to HalfOpen transition is reported to the observer (M1-10).
func TestHalfOpenTransitionIsObserved(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, Cooldown: time.Millisecond})
	var seen []State
	b.SetObserver(func(s State) { seen = append(seen, s) })
	b.RecordFailure()
	time.Sleep(5 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("cooled-down breaker refused the probe")
	}
	b.RecordSuccess()
	want := []State{Open, HalfOpen, Closed}
	if len(seen) != len(want) {
		t.Fatalf("observed %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("observed %v, want %v", seen, want)
		}
	}
}
