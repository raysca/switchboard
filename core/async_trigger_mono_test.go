package core

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBusyMarkIsMonotonicOffset(t *testing.T) {
	var mark atomic.Int64
	set(&mark, true)
	if got := mark.Load(); got <= 0 || got > int64(time.Hour) {
		t.Fatalf("busy mark = %d, want a small monotonic offset, not a Unix timestamp", got)
	}
	tr := NewAsyncTrigger(AsyncTriggerConfig{MaxWait: time.Minute})
	if !tr.on(&mark) {
		t.Fatal("a mark just stored must still be busy")
	}
	mark.Store(int64(time.Since(monoBase)) - int64(2*time.Minute))
	if tr.on(&mark) {
		t.Fatal("a mark older than MaxWait must be off")
	}
	mark.Store(0)
	if tr.on(&mark) {
		t.Fatal("a cleared mark must be off")
	}
}
