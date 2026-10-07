package deepgram

import (
	"context"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core/netpool"
	"govoice/core/resilience"
)

// slowPool counts TCP dials and delays each by d (stand-in for DNS+TCP+TLS).
func slowPool(t *testing.T, d time.Duration) (*netpool.Pool, *atomic.Int32) {
	t.Helper()
	netpool.Configure(netpool.Settings{Shared: true, Predial: true, SpareTTL: 5 * time.Second})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
	var dials atomic.Int32
	p := netpool.Get(netpool.Options{Provider: "deepgram", Stage: "warmtest", Variant: t.Name(), HTTP1Only: true,
		Control: func(string, string, syscall.RawConn) error { dials.Add(1); time.Sleep(d); return nil }})
	t.Cleanup(p.Close)
	return p, &dials
}

func firstStream(t *testing.T, f *fakeDeepgram, pool *netpool.Pool, warm bool, br *resilience.Breaker) (time.Duration, int32) {
	t.Helper()
	cfg := f.cfg()
	cfg.Pool, cfg.CircuitBreaker = pool, br
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if warm {
		if err := proc.Warm(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := proc.ensureStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	proc.Close()
	return d, f.opens.Load()
}

func holdOpen(ctx context.Context, c *websocket.Conn) { <-ctx.Done() }

func TestWarmCutsFirstStreamConnectLatency(t *testing.T) {
	const delay = 80 * time.Millisecond
	base := runtime.NumGoroutine()
	f := newFake(t, holdOpen)
	pool, dials := slowPool(t, delay)
	cold, _ := firstStream(t, f, pool, false, nil)
	if dials.Load() != 1 {
		t.Fatalf("cold dials=%d", dials.Load())
	}
	dials.Store(0)
	warm, _ := firstStream(t, f, pool, true, nil)
	t.Logf("first stream connect: cold=%v warm=%v", cold, warm)
	if dials.Load() != 1 {
		t.Fatalf("warm run must dial exactly once (the pre-dial), got %d", dials.Load())
	}
	if cold < delay || warm > cold-delay/2 {
		t.Fatalf("pre-dial not measurably faster: cold=%v warm=%v", cold, warm)
	}
	pool.Close()
	f.Close()
	for i := 0; i < 100 && runtime.NumGoroutine() > base+3; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+3 {
		t.Fatalf("goroutines %d -> %d", base, n)
	}
}

func TestWarmSkippedWhenBreakerOpen(t *testing.T) {
	f := newFake(t, holdOpen)
	pool, dials := slowPool(t, 0)
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	proc, _ := NewSTTProcessor(func() STTConfig { c := f.cfg(); c.Pool, c.CircuitBreaker = pool, br; return c }())
	if err := proc.Warm(context.Background()); err != nil || dials.Load() != 0 {
		t.Fatalf("err=%v dials=%d", err, dials.Load())
	}
}

func TestWarmErrorDoesNotAffectFirstStream(t *testing.T) {
	f := newFake(t, holdOpen)
	netpool.Configure(netpool.Settings{Shared: true, Predial: true})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
	var n atomic.Int32
	pool := netpool.Get(netpool.Options{Provider: "deepgram", Stage: "warmerr", Variant: t.Name(), HTTP1Only: true,
		Control: func(string, string, syscall.RawConn) error {
			if n.Add(1) == 1 {
				return context.DeadlineExceeded // the pre-dial fails, real dials succeed
			}
			return nil
		}})
	defer pool.Close()
	cfg := f.cfg()
	cfg.Pool = pool
	proc, _ := NewSTTProcessor(cfg)
	if err := proc.Warm(context.Background()); err == nil {
		t.Fatal("expected warm error")
	}
	if err := proc.ensureStream(context.Background()); err != nil {
		t.Fatalf("first stream must still open: %v", err)
	}
	proc.Close()
}
