package core

import (
	"testing"
	"time"
)

func newObserverSession(t *testing.T) *Session {
	t.Helper()
	pipe, _ := NewPipeline(hungProcessor{release: make(chan struct{})})
	s, err := NewSession(1, pipe, teardownBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	return s
}

func dbg(n string) DebugEvent { return NewDebugEvent(n, time.Time{}, "1", "", 0, nil) }

// A blocked observer must not stall Emit; overflow is dropped and counted.
func TestBlockedObserverDoesNotStallAndDropsCounted(t *testing.T) {
	s := newObserverSession(t)
	block, started := make(chan struct{}), make(chan struct{}, 1)
	sub, _, err := s.Subscribe(2, func(DebugEvent) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-block
	})
	if err != nil {
		t.Fatal(err)
	}
	s.EmitDebug(dbg("a"))
	<-started // observer now blocked inside fn; buffer (2) is empty
	finished := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			s.EmitDebug(dbg("x"))
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("EmitDebug blocked on a stuck observer")
	}
	if sub.Dropped() != 8 || s.DebugDropped() != 8 {
		t.Fatalf("dropped = %d/%d, want 8", sub.Dropped(), s.DebugDropped())
	}
	close(block)
	_ = s.Close()
}

// Unsubscribe stops delivery and the task exits; teardown leaves no leaked tasks.
func TestObserverUnsubscribeAndTeardownNoLeak(t *testing.T) {
	s := newObserverSession(t)
	got := make(chan string, 4)
	sub, cancel, err := s.Subscribe(4, func(e DebugEvent) { got <- e.Name })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Subscribe(4, func(DebugEvent) {}); err != nil {
		t.Fatal(err)
	}
	s.EmitDebug(dbg("one"))
	if n := <-got; n != "one" {
		t.Fatalf("got %q", n)
	}
	cancel()
	cancel() // idempotent
	s.EmitDebug(dbg("two"))
	if len(sub.ch) != 0 || len(got) != 0 { // removal is synchronous, so nothing is enqueued
		t.Fatal("delivered after unsubscribe")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.LeakedTasks() != 0 || s.activeTasks.Load() != 0 {
		t.Fatalf("leaked=%d active=%d", s.LeakedTasks(), s.activeTasks.Load())
	}
	if _, _, err := s.Subscribe(1, func(DebugEvent) {}); err != ErrSessionClosed {
		t.Fatalf("subscribe after close = %v", err)
	}
}

// A panicking observer must not fail the session; later events still arrive.
func TestObserverPanicIsolated(t *testing.T) {
	s := newObserverSession(t)
	got := make(chan string, 1)
	if _, _, err := s.Subscribe(4, func(e DebugEvent) {
		if e.Name == "boom" {
			panic("observer bug")
		}
		got <- e.Name
	}); err != nil {
		t.Fatal(err)
	}
	s.EmitDebug(dbg("boom"))
	s.EmitDebug(dbg("ok"))
	select {
	case n := <-got:
		if n != "ok" {
			t.Fatalf("got %q", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observer died after panic")
	}
	if s.Err() != nil || s.State() != StateRunning {
		t.Fatalf("session affected: state=%v err=%v", s.State(), s.Err())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
