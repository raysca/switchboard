package vad

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// directChildren counts this test process's live direct children via pgrep -P.
func directChildren(t *testing.T) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	return len(strings.Fields(string(out)))
}

// A respawn that completes between executeBatch's health read and markDown
// must not be undone: markDown on a healthy worker would count a spurious
// restart, start a second respawnLoop and orphan the first process.
func TestMarkDownIgnoresWorkerRespawnedInTheGap(t *testing.T) {
	ex, err := New(Config{Mode: "process", Workers: 2, Batch: 4, QueueCap: 256, IntraOp: 1, PerSessionLimit: 1,
		ParentDeadline: 500 * time.Millisecond, ParentGrace: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	b := ex.backend.(*workerProcessBackend)
	wp := b.workers[0]
	if err := ex.HardKill(0); err != nil {
		t.Fatal(err)
	}
	wp.mu.Lock()
	wp.gapHook = func() {
		wp.markDown() // the crash noticed first: starts the one respawnLoop
		for end := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
			wp.mu.Lock()
			up := wp.healthy && !wp.respawning
			wp.mu.Unlock()
			if up {
				return
			}
			if time.Now().After(end) {
				t.Error("respawn never completed")
				return
			}
		}
	}
	wp.mu.Unlock()
	done := make(chan struct{})
	st := ex.Register(0)
	defer st.Close()
	if err := st.Submit(&Job{Seq: 1, Done: func(*Job) { close(done) }}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job never finished")
	}
	time.Sleep(100 * time.Millisecond) // a wrongly started second respawn would land here
	if n := ex.Stats().Restarts; n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}
	if a, tot := ex.Capacity(); a != tot {
		t.Fatalf("capacity %d/%d, want full", a, tot)
	}
	if n := directChildren(t); n != 2 {
		t.Fatalf("live worker children = %d, want 2", n)
	}
}
