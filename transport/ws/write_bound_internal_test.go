package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/rtvi"
)

// A client that stalls the socket during an RTVI write ends as a slow client within the run
// loop's watchdog budget, however many messages the frame writes. The stall here is the message
// lock held by an unfinished writer; it covers the wait, not the close code (a real stall inside
// a frame also drops the close frame: the client sees 1006).
func TestProcessFrameStalledRTVIWriteEndsAsSlowClient(t *testing.T) {
	srvConn := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		srvConn <- ws
		<-release
	}))
	defer hs.Close()
	defer close(release)
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	client, _, err := websocket.Dial(cctx, "ws"+hs.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	ws := <-srvConn

	var slow atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Connection{
		conn: ws, ctx: ctx, cancel: cancel, rtvi: rtvi.NewHandler(),
		cfg: ServerConfig{
			Pacer: PacerConfig{WriteTimeout: 2 * time.Second},
			Observe: func(ev string, _ uint64) {
				if ev == "slow_client" {
					slow.Add(1)
				}
			},
		},
	}
	defer ws.CloseNow()
	stuck, err := ws.Writer(ctx, websocket.MessageBinary) // holds the message lock until closed
	if err != nil {
		t.Fatal(err)
	}
	_ = stuck

	start := time.Now()
	f := &core.LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: "hello"}
	if err := c.ProcessFrame(context.Background(), f, core.Downstream, nopEmitter{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 1900*time.Millisecond {
		t.Fatalf("ProcessFrame took %v, want under the run loop's 2s watchdog (1s write limit + 0.5s bounded close)", d)
	}
	if n := slow.Load(); n != 1 {
		t.Fatalf("slow_client observed %d times, want 1", n)
	}
	// Let the server's pending close handshake finish so cleanup stays fast.
	go func() { _, _, _ = client.Read(cctx) }()
	if ctx.Err() == nil {
		t.Fatal("connection context not cancelled")
	}
}
