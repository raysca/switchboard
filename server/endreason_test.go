package server

import (
	"sync"
	"testing"

	"govoice/core"
	"govoice/server/storage"
)

func TestTheEndReasonHolderDefaultsToAHangUpAndTheFirstWriterWins(t *testing.T) {
	var e endReason
	if got := e.get(); got != storage.EndCallerHungUp {
		t.Fatalf("default %q", got)
	}
	e.set(storage.EndSilentCaller)
	e.set(storage.EndTimeLimit)
	if got := e.get(); got != storage.EndSilentCaller {
		t.Fatalf("first writer must win, got %q", got)
	}
	var nilHolder *endReason
	nilHolder.set(storage.EndError) // a call built without a holder (tests) must not panic
	if got := nilHolder.get(); got != storage.EndCallerHungUp {
		t.Fatalf("nil holder: %q", got)
	}
}

func TestTheEndReasonHolderIsRaceFreeWithManyWriters(t *testing.T) {
	var e endReason
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				e.set(storage.EndTimeLimit)
			} else {
				e.set(storage.EndServerShutdown)
			}
			_ = e.get()
		}(i)
	}
	wg.Wait()
	if got := e.get(); got != storage.EndTimeLimit && got != storage.EndServerShutdown {
		t.Fatalf("%q", got)
	}
}

func TestEndCauses(t *testing.T) {
	for in, want := range map[string]string{
		core.IdleReasonUser: storage.EndSilentCaller,
		core.IdleReasonBot:  storage.EndAgentStuck,
		EndReasonFlow:       storage.EndedByFlow,
		"max_call_duration": storage.EndTimeLimit,
		"server_draining":   storage.EndServerShutdown,
		"something_new":     storage.EndCallerHungUp,
		"":                  storage.EndCallerHungUp,
	} {
		if got := reasonFromEnd(in); got != want {
			t.Errorf("reasonFromEnd(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"peer_unresponsive":   storage.EndConnectionLost,
		"connection_lifetime": storage.EndTimeLimit,
		"":                    "",
		"unknown":             "",
	} {
		if got := reasonFromTransport(in); got != want {
			t.Errorf("reasonFromTransport(%q) = %q, want %q", in, got, want)
		}
	}
}
