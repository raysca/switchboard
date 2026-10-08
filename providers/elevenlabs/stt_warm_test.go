package elevenlabs

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"govoice/core/netpool"
	"govoice/core/resilience"
)

func sttWarmPool(t *testing.T, d time.Duration) (*netpool.Pool, *atomic.Int32) {
	t.Helper()
	netpool.Configure(netpool.Settings{Shared: true, Predial: true, SpareTTL: 5 * time.Second})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
	var dials atomic.Int32
	p := netpool.Get(netpool.Options{Provider: "elevenlabs", Stage: "sttwarmtest", Variant: t.Name(), HTTP1Only: true,
		Control: func(string, string, syscall.RawConn) error { dials.Add(1); time.Sleep(d); return nil }})
	t.Cleanup(p.Close)
	return p, &dials
}

func firstSTTStream(t *testing.T, f *sttFake, pool interface{ Close() }, cfg STTConfig, warm bool) time.Duration {
	t.Helper()
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
	return d
}

func TestSTTWarmCutsFirstStreamConnectLatency(t *testing.T) {
	const delay = 80 * time.Millisecond
	f := newSTTFake(t, holdSTT)
	pool, dials := sttWarmPool(t, delay)
	cfg := f.cfg()
	cfg.Pool = pool
	cold := firstSTTStream(t, f, pool, cfg, false)
	dials.Store(0)
	warm := firstSTTStream(t, f, pool, cfg, true)
	if dials.Load() != 1 {
		t.Fatalf("warm run must dial exactly once (the pre-dial), got %d", dials.Load())
	}
	if cold < delay || warm > cold-delay/2 {
		t.Fatalf("pre-dial not measurably faster: cold=%v warm=%v", cold, warm)
	}
}

func TestSTTWarmSkippedWhenBreakerOpen(t *testing.T) {
	f := newSTTFake(t, holdSTT)
	pool, dials := sttWarmPool(t, 0)
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	cfg := f.cfg()
	cfg.Pool, cfg.CircuitBreaker = pool, br
	proc, _ := NewSTTProcessor(cfg)
	if err := proc.Warm(context.Background()); err != nil || dials.Load() != 0 {
		t.Fatalf("err=%v dials=%d", err, dials.Load())
	}
}
