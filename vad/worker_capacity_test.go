package vad

import "testing"

func TestWorkerRespawnFailuresDoNotDrainCapacity(t *testing.T) {
	b := &workerProcessBackend{cfg: Config{WorkerBinary: "/nonexistent/vadworker"}}
	for i := 0; i < 2; i++ {
		b.workers = append(b.workers, &workerProcess{id: i, backend: b, stderr: newBoundedBuffer(64)})
	}
	b.availCount.Store(2)

	for i := 0; i < 3; i++ {
		if b.workers[0].respawnWithBackoff() {
			t.Fatal("respawn of a missing binary succeeded")
		}
	}
	if avail, _ := b.capacity(); avail != 1 {
		t.Fatalf("available = %d after repeated failed respawns of one worker, want 1", avail)
	}
}
