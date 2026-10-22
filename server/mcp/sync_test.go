package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"govoice/server/mcp/mcptest"
	"govoice/server/storage"
)

// syncRig is one mcptest server behind a switch: down answers 503 (a connection failure).
type syncRig struct {
	svc   *Service
	store *storage.Store
	down  atomic.Bool
	id    string
}

func newSyncRig(t *testing.T) *syncRig {
	t.Helper()
	r := &syncRig{}
	srv := mcptest.New(mcptest.Options{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		srv.Handler().ServeHTTP(w, req)
	}))
	t.Cleanup(ts.Close)
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/mcp.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r.store = store
	if r.svc, err = NewService(ServiceConfig{Store: store, Remote: NewSDKClient(AllowLoopbackHTTP()), Destinations: LoopbackHTTPDestinationPolicy{}, Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	r.id = createTestServer(t, r.svc, ts.URL+"/open", "").ID
	return r
}

func (r *syncRig) state(t *testing.T) storage.MCPServer {
	t.Helper()
	m, err := r.svc.Get(context.Background(), r.id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func quietSyncer(s *Service, onResult func(bool)) *Syncer {
	return NewSyncer(s, time.Minute, slog.New(slog.DiscardHandler), onResult)
}

func TestSyncRoundMarksHealthyAndStoresOneDiscovery(t *testing.T) {
	r := newSyncRig(t)
	var oks atomic.Int32
	y := quietSyncer(r.svc, func(ok bool) {
		if ok {
			oks.Add(1)
		}
	})
	y.round(context.Background())
	first := r.state(t)
	if first.ValidationState != StateHealthy || first.LatestDiscoveryID == "" || first.LastConnectedAt.IsZero() {
		t.Fatalf("after round 1: %+v", first)
	}
	y.round(context.Background())
	if second := r.state(t); second.LatestDiscoveryID != first.LatestDiscoveryID {
		t.Fatalf("unchanged tools added a discovery: %s then %s", first.LatestDiscoveryID, second.LatestDiscoveryID)
	}
	if oks.Load() != 2 {
		t.Fatalf("onResult ok calls = %d, want 2", oks.Load())
	}
}

func TestSyncTwoStrikes(t *testing.T) {
	r := newSyncRig(t)
	y := quietSyncer(r.svc, nil)
	ctx := context.Background()
	y.round(ctx)
	r.down.Store(true)
	y.round(ctx)
	if got := r.state(t).ValidationState; got != StateHealthy {
		t.Fatalf("one failure on a healthy server: state %q, want healthy", got)
	}
	y.round(ctx)
	if got := r.state(t).ValidationState; got != StateConnectionFailed {
		t.Fatalf("two failures in a row: state %q, want connection_failed", got)
	}
	// A manual Sync heals it; the next background failure is a fresh first strike.
	r.down.Store(false)
	if _, err := r.svc.Refresh(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	r.down.Store(true)
	y.round(ctx)
	if got := r.state(t).ValidationState; got != StateHealthy {
		t.Fatalf("first background failure after a manual heal: state %q, want healthy", got)
	}
	y.round(ctx)
	if got := r.state(t).ValidationState; got != StateConnectionFailed {
		t.Fatalf("second failure after a manual heal: state %q, want connection_failed", got)
	}
	r.down.Store(false)
	y.round(ctx)
	if got := r.state(t).ValidationState; got != StateHealthy {
		t.Fatalf("first success: state %q, want healthy", got)
	}
	// The success cleared the count: one more failure is a first strike again.
	r.down.Store(true)
	y.round(ctx)
	if got := r.state(t).ValidationState; got != StateHealthy {
		t.Fatalf("a failure after a success: state %q, want healthy", got)
	}
}

func TestSyncAlreadyFailingServerIsUpdatedEveryRound(t *testing.T) {
	r := newSyncRig(t)
	r.down.Store(true)
	if _, err := r.svc.Refresh(context.Background(), r.id); err == nil {
		t.Fatal("refresh of a down server succeeded")
	}
	before := r.state(t)
	if before.ValidationState != StateConnectionFailed {
		t.Fatalf("manual refresh: %q", before.ValidationState)
	}
	y := quietSyncer(r.svc, nil)
	time.Sleep(10 * time.Millisecond) // updated_at must move
	y.round(context.Background())
	after := r.state(t)
	if after.ValidationState != StateConnectionFailed || !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("failing server not written: before %+v after %+v", before, after)
	}
}

func TestSyncAlternatingFailuresStayHealthy(t *testing.T) {
	r := newSyncRig(t)
	y := quietSyncer(r.svc, nil)
	y.round(context.Background())
	for i := range 6 {
		r.down.Store(i%2 == 0)
		y.round(context.Background())
		if got := r.state(t).ValidationState; got != StateHealthy {
			t.Fatalf("round %d (down=%v): state %q, want healthy", i, i%2 == 0, got)
		}
	}
}

func TestSyncSkipsDisabledAndPendingDeletion(t *testing.T) {
	r := newSyncRig(t)
	var calls atomic.Int32
	y := quietSyncer(r.svc, func(bool) { calls.Add(1) })
	if err := r.svc.Disable(context.Background(), r.id); err != nil {
		t.Fatal(err)
	}
	y.round(context.Background())
	if _, err := r.store.DB().Exec("UPDATE mcp_servers SET enabled=1, pending_delete=1 WHERE id=?", r.id); err != nil {
		t.Fatal(err)
	}
	y.round(context.Background())
	if calls.Load() != 0 {
		t.Fatalf("skipped servers were synced %d times", calls.Load())
	}
	if m := r.state(t); m.LatestDiscoveryID != "" || m.ValidationState == StateHealthy {
		t.Fatalf("skipped server was written: %+v", m)
	}
}

// fakeSyncService is a store with one server whose ListTools runs list.
func fakeSyncService(t *testing.T, list func(context.Context) ([]RemoteTool, error)) *Service {
	t.Helper()
	svc, _ := newTestService(t, &testRemote{list: list}, testTokens{})
	createTestServer(t, svc, "https://mcp.example/rpc", "")
	return svc
}

func stopWithin(t *testing.T, y *Syncer, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := y.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestSyncerStartStopLeavesNoGoroutine(t *testing.T) {
	svc := fakeSyncService(t, func(context.Context) ([]RemoteTool, error) { return nil, nil })
	before := goleak.IgnoreCurrent() // the store's own goroutines end at cleanup
	y := NewSyncer(svc, 5*time.Millisecond, slog.New(slog.DiscardHandler), nil)
	y.Start()
	time.Sleep(20 * time.Millisecond)
	stopWithin(t, y, time.Second)
	stopWithin(t, y, time.Second) // idempotent
	if err := goleak.Find(before); err != nil {
		t.Fatal(err)
	}
}

func TestSyncerStopCancelsTheRoundInProgress(t *testing.T) {
	started := make(chan struct{}, 1)
	seen := make(chan error, 1)
	svc := fakeSyncService(t, func(ctx context.Context) ([]RemoteTool, error) {
		started <- struct{}{}
		<-ctx.Done()
		seen <- ctx.Err()
		return nil, ctx.Err()
	})
	svc.timeout = time.Minute
	y := NewSyncer(svc, time.Millisecond, slog.New(slog.DiscardHandler), nil)
	y.Start()
	<-started
	stopWithin(t, y, time.Second)
	if err := <-seen; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight sync ended with %v, want context.Canceled", err)
	}
	if m, _ := svc.store.MCP().ListServers(context.Background()); m[0].ValidationState != StateNotValidated {
		t.Fatalf("a sync cancelled by Stop was written: %q", m[0].ValidationState)
	}
}

func TestSyncerRecoversAPanickingRound(t *testing.T) {
	svc := fakeSyncService(t, func(context.Context) ([]RemoteTool, error) { return nil, nil })
	var rounds atomic.Int32
	y := NewSyncer(svc, time.Millisecond, slog.New(slog.DiscardHandler), func(bool) {
		if rounds.Add(1) == 1 {
			panic("boom")
		}
	})
	y.Start()
	defer stopWithin(t, y, time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for rounds.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("loop stopped after a panic: %d rounds", rounds.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSyncerRoundsNeverOverlap(t *testing.T) {
	const slow = 30 * time.Millisecond
	var active, overlap atomic.Int32
	starts := make(chan time.Time, 8)
	svc := fakeSyncService(t, func(context.Context) ([]RemoteTool, error) {
		if active.Add(1) > 1 {
			overlap.Store(1)
		}
		select {
		case starts <- time.Now():
		default:
		}
		time.Sleep(slow)
		active.Add(-1)
		return nil, nil
	})
	svc.timeout = time.Minute
	y := NewSyncer(svc, time.Millisecond, slog.New(slog.DiscardHandler), nil)
	y.Start()
	first, second, third := <-starts, <-starts, <-starts
	stopWithin(t, y, time.Second)
	if overlap.Load() != 0 {
		t.Fatal("two rounds ran at once")
	}
	if second.Sub(first) < slow || third.Sub(second) < slow {
		t.Fatalf("next round started before the slow one ended: gaps %v, %v", second.Sub(first), third.Sub(second))
	}
}
