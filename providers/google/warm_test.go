package google

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"govoice/core/netpool"
)

func grpcFixture(t *testing.T, listen bool) (*grpc.ClientConn, *atomic.Int32) {
	t.Helper()
	netpool.Configure(netpool.Settings{Shared: true, Predial: true})
	t.Cleanup(func() { netpool.Configure(netpool.Settings{Shared: true}) })
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	if listen {
		go srv.Serve(lis)
	} else {
		lis.Close()
	}
	t.Cleanup(srv.Stop)
	var dials atomic.Int32
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, a string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, "tcp", a)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, &dials
}

func TestWarmConnBringsChannelToReadyWithoutRPC(t *testing.T) {
	conn, dials := grpcFixture(t, true)
	if dials.Load() != 0 {
		t.Fatal("grpc.NewClient must be lazy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := warmConn(ctx, conn, "stt"); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 || conn.GetState().String() != "READY" {
		t.Fatalf("dials=%d state=%v", dials.Load(), conn.GetState())
	}
	if err := warmConn(ctx, conn, "stt"); err != nil || dials.Load() != 1 {
		t.Fatalf("second warm must be a no-op: err=%v dials=%d", err, dials.Load())
	}
}

func TestWarmConnOffAndFailureAreHarmless(t *testing.T) {
	conn, dials := grpcFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := warmConn(ctx, conn, "tts"); err == nil {
		t.Fatal("unreachable server must return an error (swallowed by the caller)")
	}
	netpool.Configure(netpool.Settings{Shared: true, Predial: false})
	before := dials.Load()
	conn2, dials2 := grpcFixture(t, true)
	netpool.Configure(netpool.Settings{Shared: true, Predial: false})
	if err := warmConn(ctx, conn2, "tts"); err != nil || dials2.Load() != 0 || before == 0 {
		t.Fatalf("predial off must not dial: err=%v dials=%d", err, dials2.Load())
	}
}
