package vad

import (
	"testing"
	"time"
)

func TestWorkerRespawnFailuresDoNotDrainCapacity(t *testing.T) {
	b := &workerProcessBackend{cfg: Config{WorkerBinary: "/nonexistent/vadworker"}}
	for i := 0; i < 2; i++ {
		b.workers = append(b.workers, &workerProcess{id: i, backend: b, stderr: newBoundedBuffer(64), quit: make(chan struct{})})
	}
	b.availCount.Store(2)
	defer func() { b.workers[0].close(); b.workers[0].wg.Wait() }() // ends the respawnLoop
	for i := 0; i < 3; i++ {
		b.workers[0].markDown() // the respawn of the missing binary keeps failing meanwhile
	}
	for end := time.Now().Add(5 * time.Second); b.statCounters.restarts.Load() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the respawn loop never retried")
		}
	}
	if avail, _ := b.capacity(); avail != 1 {
		t.Fatalf("available = %d after repeated failed respawns of one worker, want 1", avail)
	}
}

// A parent descheduled past the deadline (timer and reply ready together, or
// the reply a few ms behind the late timer) must not kill a healthy worker;
// a silent worker is still declared hung after deadline + grace.
func TestAwaitReplyToleratesParentStall(t *testing.T) {
	past := time.Now().Add(-100 * time.Millisecond) // the parent woke up 100 ms late
	ch := make(chan readResult, 1)
	go func() { time.Sleep(10 * time.Millisecond); ch <- readResult{payload: []byte{1}} }()
	if res, ok := awaitReply(ch, past, 50*time.Millisecond); !ok || len(res.payload) != 1 {
		t.Fatal("reply right after a late timer was treated as a worker hang")
	}
	start := time.Now()
	if _, ok := awaitReply(make(chan readResult), time.Now().Add(10*time.Millisecond), 20*time.Millisecond); ok {
		t.Fatal("silent worker not declared hung")
	}
	if d := time.Since(start); d < 30*time.Millisecond {
		t.Fatalf("hang declared after %v, before deadline + grace", d)
	}
}
