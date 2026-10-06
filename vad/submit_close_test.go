package vad

import (
	"sync"
	"testing"
)

func TestSubmitConcurrentWithCloseDoesNotPanic(t *testing.T) {
	for i := 0; i < 500; i++ {
		b := &inProcessBackend{cfg: Config{PerSessionLimit: 1 << 30}, queues: []chan *Job{make(chan *Job, 1<<20)}}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			st := b.register(uint64(g))
			wg.Add(1)
			go func() {
				defer wg.Done()
				for st.Submit(&Job{}) != ErrWorker {
				}
			}()
		}
		_ = b.close()
		wg.Wait()
	}
}
