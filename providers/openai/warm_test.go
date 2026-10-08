package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core/netpool"
	"govoice/core/resilience"
)

func connServer(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	var total, open atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	s.Config.ConnState = func(c netConn, st http.ConnState) {
		switch st {
		case http.StateNew:
			total.Add(1)
			open.Add(1)
		case http.StateClosed:
			open.Add(-1)
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	return s, &total, &open
}

func warmCfg(url string) Config {
	return Config{BaseURL: url, AllowPrivate: true, APIKey: "k"}
}

func useSpare(t *testing.T, ttl time.Duration) {
	netpool.Configure(netpool.Settings{Shared: true, Predial: true, SpareTTL: ttl})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
}

func TestSecondSessionReusesPooledConnection(t *testing.T) {
	s, total, _ := connServer(t)
	p1, err := NewProcessor(warmCfg(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer p1.pool.Close()
	p2, _ := NewProcessor(warmCfg(s.URL))
	if p1.pool != p2.pool {
		t.Fatal("sessions must share one pool")
	}
	for _, p := range []*Processor{p1, p2, p1} {
		resp, err := p.cfg.HTTPClient.Get(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if total.Load() != 1 {
		t.Fatalf("server saw %d connections for 3 requests over 2 sessions, want 1", total.Load())
	}
}

func TestWarmPredialsOneAndFirstRequestUsesIt(t *testing.T) {
	s, total, open := connServer(t)
	useSpare(t, 5*time.Second)
	cfg := warmCfg(s.URL)
	p, _ := NewProcessor(cfg)
	before := total.Load()
	for i := 0; i < 2; i++ {
		if err := p.Warm(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // server counts accepts asynchronously
	if d := total.Load() - before; d != 1 {
		t.Fatalf("warm dialed %d connections, want exactly 1", d)
	}
	resp, err := p.cfg.HTTPClient.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d := total.Load() - before; d != 1 {
		t.Fatalf("first request opened a new connection (%d total)", d)
	}
	p.pool.Close()
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if open.Load() != 0 {
		t.Fatalf("%d connections leaked after pool Close", open.Load())
	}
}

func TestWarmKeepsSSRFGuard(t *testing.T) {
	useSpare(t, time.Second)
	p, err := NewProcessor(Config{BaseURL: "https://127.0.0.1:9", APIKey: "k"}) // loopback, private not allowed
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Warm(context.Background()); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("pre-dial must hit the dial guard, got %v", err)
	}
}

func TestWarmSkippedWhenBreakerOpen(t *testing.T) {
	s, total, _ := connServer(t)
	useSpare(t, time.Second)
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	cfg := warmCfg(s.URL)
	cfg.CircuitBreaker = br
	p, _ := NewProcessor(cfg)
	err := p.Warm(context.Background())
	time.Sleep(100 * time.Millisecond)
	if err != nil || total.Load() != 0 {
		t.Fatalf("err=%v conns=%d", err, total.Load())
	}
}

// A spare dialed through the allow_private pool must never serve a strict
// request, and the pool count must not grow with tenant base URLs.
func TestPoolsIsolatedByGuardModeAndBoundedByMode(t *testing.T) {
	s, total, _ := connServer(t)
	useSpare(t, 5*time.Second)
	priv, _ := NewProcessor(warmCfg(s.URL))
	if err := priv.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if total.Load() != 1 {
		t.Fatalf("warm dialed %d, want 1", total.Load())
	}
	strict := guardedPool("openai", false)
	if strict == priv.pool {
		t.Fatal("strict and allow_private must not share a pool")
	}
	// Same host:port the permissive spare is parked under: the strict pool must
	// dial itself (and be refused by the guard), not take that spare.
	if _, err := guardedClient(strict).Get(s.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("strict request to loopback: err=%v, want ErrBlockedAddress", err)
	}
	if _, err := strict.Transport().DialContext(context.Background(), "tcp", s.Listener.Addr().String()); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("strict dial: err=%v", err)
	}
	for _, u := range []string{"https://a.example", "https://b.example:8443/v1", "https://api.openai.com/v1"} {
		p, err := NewProcessor(Config{BaseURL: u, APIKey: "k"})
		if err != nil || p.pool != strict {
			t.Fatalf("%s: base URL must not create a new pool (err=%v)", u, err)
		}
	}
	if p, _ := NewProcessor(Config{BaseURL: "https://c.example", APIKey: "k", AllowPrivate: true}); p.pool != priv.pool {
		t.Fatal("allow_private processors must share one pool")
	}
}
