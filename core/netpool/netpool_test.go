package netpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type srv struct {
	*httptest.Server
	conns, open, reqs atomic.Int64
}

func newSrv(t *testing.T) *srv {
	t.Helper()
	s := &srv{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.reqs.Add(1); w.Write([]byte("ok")) }))
	s.EnableHTTP2 = true
	s.Config.ConnState = func(c netConn, st http.ConnState) {
		switch st {
		case http.StateNew:
			s.conns.Add(1)
			s.open.Add(1)
		case http.StateClosed, http.StateHijacked:
			s.open.Add(-1)
		}
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func testPool(t *testing.T, s *srv, ctl func(string, string, syscall.RawConn) error) *Pool {
	t.Helper()
	Configure(Settings{Shared: true, Predial: true, SpareTTL: time.Second})
	t.Cleanup(func() { Configure(Settings{Shared: true}) })
	p := Get(Options{Provider: "t", Stage: t.Name()[:4], Variant: t.Name(), Control: ctl, TLS: clientTLS(s)})
	t.Cleanup(p.Close)
	return p
}

func get(t *testing.T, p *Pool, url string) {
	t.Helper()
	resp, err := p.Client(5 * time.Second).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestSharedPoolReusesConnectionAcrossSessions(t *testing.T) {
	s := newSrv(t)
	p := testPool(t, s, nil)
	get(t, p, s.URL)
	// "second session": Get returns the very same pool, so no new dial.
	p2 := Get(Options{Provider: "t", Stage: t.Name()[:4], Variant: t.Name(), TLS: clientTLS(s)})
	if p2 != p {
		t.Fatal("Get must return the shared pool")
	}
	get(t, p2, s.URL)
	get(t, p2, s.URL)
	if n := s.conns.Load(); n != 1 {
		t.Fatalf("server saw %d connections, want 1 (HTTP/2 reuse)", n)
	}
}

func TestUnsharedPoolDoesNotReuse(t *testing.T) {
	s := newSrv(t)
	Configure(Settings{Shared: false})
	t.Cleanup(func() { Configure(Settings{Shared: true}) })
	for i := 0; i < 2; i++ {
		p := Get(Options{Provider: "t", Stage: "uns", TLS: clientTLS(s)})
		get(t, p, s.URL)
		p.Close()
	}
	if n := s.conns.Load(); n != 2 {
		t.Fatalf("got %d connections, want 2", n)
	}
}

func TestWarmPredialsExactlyOneAndFirstRequestUsesIt(t *testing.T) {
	s := newSrv(t)
	p := testPool(t, s, nil)
	for i := 0; i < 3; i++ { // repeated/concurrent warms must not dial more
		if err := p.Warm(context.Background(), s.URL); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // server counts accepts asynchronously
	if n := s.conns.Load(); n != 1 {
		t.Fatalf("after warm: %d connections, want exactly 1", n)
	}
	if n := s.reqs.Load(); n != 0 { // TCP+TLS only: no request, no credentials
		t.Fatalf("warm sent %d requests, want 0", n)
	}
	get(t, p, s.URL)
	get(t, p, s.URL)
	if n := s.conns.Load(); n != 1 {
		t.Fatalf("first request must reuse the spare: %d connections", n)
	}
	if p.st.hit.Load() < 1 {
		t.Fatal("expected a warm hit")
	}
	var b bytes.Buffer
	WriteMetrics(&b)
	if !strings.Contains(b.String(), "voice_provider_connect_seconds_count") {
		t.Fatal("metrics missing")
	}
}

func TestWarmOffIsNoop(t *testing.T) {
	s := newSrv(t)
	p := testPool(t, s, nil)
	Configure(Settings{Shared: true, Predial: false})
	if err := p.Warm(context.Background(), s.URL); err != nil || s.conns.Load() != 0 {
		t.Fatalf("err=%v conns=%d", err, s.conns.Load())
	}
}

func TestSpareExpiresAndClosesNoLeak(t *testing.T) {
	s := newSrv(t)
	base := runtime.NumGoroutine()
	Configure(Settings{Shared: true, Predial: true, SpareTTL: 50 * time.Millisecond})
	p := Get(Options{Provider: "t", Stage: "ttl", TLS: clientTLS(s)})
	defer p.Close()
	if err := p.Warm(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.open.Load() == 0 }, "spare closed after TTL")
	// an expired spare must not break the next request
	get(t, p, s.URL)
	p.Close()
	waitFor(t, func() bool { return s.open.Load() == 0 }, "pooled conn closed after Close")
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base+2 }, "goroutines")
}

func TestClosedByServerSpareIsDiscarded(t *testing.T) {
	s := newSrv(t)
	p := testPool(t, s, nil)
	if err := p.Warm(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	s.CloseClientConnections()
	time.Sleep(50 * time.Millisecond)
	get(t, p, s.URL) // must dial fresh, not fail on the dead spare
}

// Deterministic form of the case above that only Linux hit by timing: the
// server finished its handshake and sent its ticket and h2 SETTINGS before a
// clean close, so the spare holds unread bytes followed by EOF.
func TestSpareClosedAfterServerSpokeIsDiscarded(t *testing.T) {
	s := newSrv(t)
	p := testPool(t, s, nil)
	if err := p.Warm(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // loopback handshake + h2 SETTINGS (h2 conns skip ConnState hooks)
	s.CloseClientConnections()
	time.Sleep(50 * time.Millisecond)
	get(t, p, s.URL)
}

func TestWarmErrorIsReturnedAndGuardApplies(t *testing.T) {
	s := newSrv(t)
	blocked := errors.New("blocked")
	p := testPool(t, s, func(string, string, syscall.RawConn) error { return blocked })
	if err := p.Warm(context.Background(), s.URL); err == nil {
		t.Fatal("guard must apply to pre-dial")
	}
	if s.conns.Load() != 0 {
		t.Fatal("blocked pre-dial reached the server")
	}
	if p.st.warmErr.Load() < 1 {
		t.Fatal("warm error not counted")
	}
}

// With a slow dial (injected 80ms in the dial guard, standing in for DNS+TCP+
// TLS RTTs) the first request is faster when a spare was pre-dialed.
func TestPredialCutsFirstRequestLatency(t *testing.T) {
	const delay = 80 * time.Millisecond
	slow := func(string, string, syscall.RawConn) error { time.Sleep(delay); return nil }
	run := func(warm bool) time.Duration {
		s := newSrv(t)
		Configure(Settings{Shared: true, Predial: true, SpareTTL: time.Second})
		p := Get(Options{Provider: "t", Stage: "lat", Variant: t.Name() + time.Now().String(), Control: slow, TLS: clientTLS(s)})
		defer p.Close()
		if warm {
			if err := p.Warm(context.Background(), s.URL); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		get(t, p, s.URL)
		return time.Since(start)
	}
	cold, warm := run(false), run(true)
	t.Logf("first request: cold=%v warm=%v", cold, warm)
	if cold < delay || warm > cold-delay/2 {
		t.Fatalf("warm not measurably faster: cold=%v warm=%v", cold, warm)
	}
}

func waitFor(t *testing.T, f func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

// A guarded pool never shares a registry slot with an unguarded one, even if
// the caller forgot to set Variant.
func TestGuardedPoolNotSharedWithUnguarded(t *testing.T) {
	o := Options{Provider: "t", Stage: "guardkey"}
	plain := Get(o)
	t.Cleanup(plain.Close)
	o.Control = func(string, string, syscall.RawConn) error { return errors.New("blocked") }
	guarded := Get(o)
	t.Cleanup(guarded.Close)
	if plain == guarded {
		t.Fatal("guarded and unguarded options returned the same pool")
	}
	if guarded.Transport().Proxy != nil {
		t.Fatal("guarded pool must not use a proxy")
	}
}

// Every request through a pool client (and CountRequest for gRPC) lands in
// voice_provider_requests_total, retries included: harnesses cap on this number.
func TestRequestsCounted(t *testing.T) {
	Configure(Settings{Shared: true, Predial: true, SpareTTL: time.Second})
	t.Cleanup(func() { Configure(Settings{Shared: true}) })
	s := newSrv(t)
	p := Get(Options{Provider: "t", Stage: "reqs", Variant: t.Name(), TLS: clientTLS(s)})
	if err := p.Warm(context.Background(), s.URL); err != nil { // a pre-dial is a connection, not a request
		t.Fatal(err)
	}
	base := Requests()["t|reqs"] // counters are process-wide: -count=N reruns start above 0
	c := p.Client(0)
	for i := 0; i < 3; i++ {
		resp, err := c.Get(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	CountRequest("t", "reqs")
	if got := Requests()["t|reqs"] - base; got != 4 {
		t.Errorf("requests = %d, want 3 HTTP + 1 counted", got)
	}
	var b bytes.Buffer
	WriteMetrics(&b)
	if !strings.Contains(b.String(), fmt.Sprintf(`voice_provider_requests_total{provider="t",stage="reqs"} %d`, base+4)) {
		t.Errorf("metrics lack the counter:\n%s", b.String())
	}
}
