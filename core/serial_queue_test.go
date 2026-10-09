package core_test

import (
	"context"
	"sync"
	"testing"

	"govoice/core"
)

type countingGo struct {
	n  int
	wg sync.WaitGroup
}

func (g *countingGo) Go(fn func(context.Context)) error {
	g.n++
	g.wg.Add(1)
	go func() { defer g.wg.Done(); fn(context.Background()) }()
	return nil
}

func TestSerialQueueRunsInOrderOnOneTask(t *testing.T) {
	var q core.SerialQueue
	g := &countingGo{}
	gate := make(chan struct{})
	var mu sync.Mutex
	var got []int
	for i := 0; i < 50; i++ {
		_ = q.Go(g, func(context.Context) {
			<-gate
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
	}
	close(gate)
	g.wg.Wait()
	if g.n != 1 {
		t.Fatalf("tasks started = %d, want 1", g.n)
	}
	if len(got) != 50 {
		t.Fatalf("ran %d jobs", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("out of order: %v", got)
		}
	}
}
