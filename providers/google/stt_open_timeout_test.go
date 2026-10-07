package google

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"govoice/core/resilience"
)

// A server that accepts TCP and never speaks HTTP/2: the stream open hangs.
func TestSTTOpenStreamTimesOutOnHangingServerAndRetriesOnce(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
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
	client, err := speech.NewClient(context.Background(), option.WithEndpoint(l.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	cfg := DefaultSTTConfig()
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 2, AttemptTimeout: 150 * time.Millisecond, InitialBackoff: time.Millisecond}
	start := time.Now()
	_, err = (&RealGoogleSTTOpener{client: client}).OpenStream(context.Background(), cfg)
	if !errors.Is(err, resilience.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("open took %v", d)
	}
	pe := MapError("stt", err)
	if !pe.Retryable || isClientError(pe) {
		t.Fatalf("a hang must count against the breaker: %+v", pe)
	}
}

func TestDefaultSTTOpenPolicyIsBounded(t *testing.T) {
	if p := DefaultSTTConfig().RetryPolicy; p.MaxAttempts != 2 || p.AttemptTimeout != 5*time.Second {
		t.Fatalf("%+v", p)
	}
}
