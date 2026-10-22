package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent /ready probes share one in-flight store ping.
func TestStoreHealthySharesOnePing(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	var pings atomic.Int32
	r.s.pingDB = func(context.Context) error {
		pings.Add(1)
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !r.s.storeHealthy(t.Context()) {
				t.Error("store reported unhealthy")
			}
		}()
	}
	wg.Wait()
	if n := pings.Load(); n != 1 {
		t.Fatalf("pings = %d, want 1", n)
	}
}

// No lock is held while the ping is in flight.
func TestStoreHealthyHoldsNoLockDuringPing(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	in, release := make(chan struct{}), make(chan struct{})
	r.s.pingDB = func(context.Context) error { close(in); <-release; return nil }
	done := make(chan bool)
	go func() { done <- r.s.storeHealthy(t.Context()) }()
	<-in
	if !r.s.pingMu.TryLock() {
		t.Fatal("pingMu held across the ping")
	}
	r.s.pingMu.Unlock()
	close(release)
	if !<-done {
		t.Fatal("unhealthy")
	}
}
