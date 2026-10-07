package xai

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core/netpool"
	"govoice/core/resilience"
)

func warmServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var total atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	s.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			total.Add(1)
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	return s, &total
}

func get(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestLLMSessionsShareOnePooledConnection(t *testing.T) {
	s, total := warmServer(t)
	a, err := NewProcessor(Config{Endpoint: s.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.pool.Close()
	b, _ := NewProcessor(Config{Endpoint: s.URL, APIKey: "k"})
	if a.pool != b.pool {
		t.Fatal("sessions must share one pool")
	}
	get(t, a.cfg.HTTPClient, s.URL)
	get(t, b.cfg.HTTPClient, s.URL)
	if total.Load() != 1 {
		t.Fatalf("%d connections for two sessions, want 1", total.Load())
	}
}

func TestLLMWarmSkippedWhenBreakerOpen(t *testing.T) {
	s, total := warmServer(t)
	netpool.Configure(netpool.Settings{Shared: true, Predial: true, SpareTTL: 5 * time.Second})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	p, _ := NewProcessor(Config{Endpoint: s.URL, APIKey: "k", CircuitBreaker: br})
	defer p.pool.Close()
	err := p.Warm(context.Background())
	time.Sleep(100 * time.Millisecond) // server counts accepts asynchronously
	if err != nil || total.Load() != 0 {
		t.Fatalf("open breaker must skip warm: err=%v conns=%d", err, total.Load())
	}
}

func TestLLMWarmPredialsOneAndFirstRequestUsesIt(t *testing.T) {
	s, total := warmServer(t)
	netpool.Configure(netpool.Settings{Shared: true, Predial: true, SpareTTL: 5 * time.Second})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
	p, _ := NewProcessor(Config{Endpoint: s.URL, APIKey: "k"})
	defer p.pool.Close()
	err := p.Warm(context.Background())
	time.Sleep(100 * time.Millisecond)
	if err != nil || total.Load() != 1 {
		t.Fatalf("err=%v conns=%d", err, total.Load())
	}
	get(t, p.cfg.HTTPClient, s.URL)
	if total.Load() != 1 {
		t.Fatalf("first request must use the spare, conns=%d", total.Load())
	}
}
