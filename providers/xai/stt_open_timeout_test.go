//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core/resilience"
)

// hangListener accepts TCP connections and never answers the websocket handshake.
func hangListener(t *testing.T) (addr string, accepted *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted = new(atomic.Int32)
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		for _, c := range conns {
			c.Close()
		}
	})
	return l.Addr().String(), accepted
}

func TestSTTOpenTimesOutOnHangingServerAndRetriesOnce(t *testing.T) {
	addr, accepted := hangListener(t)
	cfg := DefaultSTTConfig()
	cfg.Endpoint = "ws://" + addr + "/v1/stt"
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 2, AttemptTimeout: 100 * time.Millisecond, InitialBackoff: time.Millisecond}
	start := time.Now()
	_, err := (&RealXAISTTOpener{}).OpenStream(context.Background(), cfg)
	if !errors.Is(err, resilience.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.ClientError() {
		t.Fatalf("a hang must be a provider outage the breaker counts, got %#v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("open took %v", d)
	}
	if n := accepted.Load(); n != 2 {
		t.Fatalf("dial attempts = %d, want 2", n)
	}
	// the breaker sees it as a failure
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1})
	br.Report(err, classify)
	if br.Allow() {
		t.Fatal("breaker did not count the hang")
	}
}
