package elevenlabs

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core/netpool"
	"govoice/core/resilience"
)

func warmFixture(t *testing.T, d time.Duration, br *resilience.Breaker) (*TTSProcessor, *atomic.Int32) {
	t.Helper()
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) { <-ctx.Done() })
	netpool.Configure(netpool.Settings{Shared: true, Predial: true, SpareTTL: 5 * time.Second})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
	var dials atomic.Int32
	pool := netpool.Get(netpool.Options{Provider: "elevenlabs", Stage: "warmtest", Variant: t.Name(), HTTP1Only: true,
		Control: func(string, string, syscall.RawConn) error { dials.Add(1); time.Sleep(d); return nil }})
	t.Cleanup(pool.Close)
	cfg := f.cfg()
	cfg.Pool, cfg.CircuitBreaker = pool, br
	p, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, &dials
}

func firstDial(t *testing.T, p *TTSProcessor) time.Duration {
	t.Helper()
	start := time.Now()
	c, err := (&unitRun{p: p}).dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	c.CloseNow()
	return d
}

func TestWarmPredialCutsFirstUnitConnect(t *testing.T) {
	const delay = 80 * time.Millisecond
	p, dials := warmFixture(t, delay, nil)
	cold := firstDial(t, p)
	dials.Store(0)
	if err := p.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	warm := firstDial(t, p)
	t.Logf("first unit connect: cold=%v warm=%v", cold, warm)
	if dials.Load() != 1 {
		t.Fatalf("warm run must dial exactly once, got %d", dials.Load())
	}
	if cold < delay || warm > cold-delay/2 {
		t.Fatalf("pre-dial not measurably faster: cold=%v warm=%v", cold, warm)
	}
}

func TestWarmSkippedWhenBreakerOpen(t *testing.T) {
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	p, dials := warmFixture(t, 0, br)
	if err := p.Warm(context.Background()); err != nil || dials.Load() != 0 {
		t.Fatalf("err=%v dials=%d", err, dials.Load())
	}
}
