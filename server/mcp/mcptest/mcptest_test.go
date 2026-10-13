package mcptest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"govoice/server/mcp"
	"govoice/server/mcp/mcptest"
)

func start(t *testing.T) (*mcptest.Server, string, *mcp.SDKClient) {
	t.Helper()
	srv := mcptest.New(mcptest.Options{Token: "tok-123"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts.URL, mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
}

func names(tools []mcp.RemoteTool) string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return strings.Join(out, ",")
}

func lastCall(srv *mcptest.Server, tool string) mcptest.Record {
	recs := srv.Records()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Tool == tool {
			return recs[i]
		}
	}
	return mcptest.Record{}
}

func TestOKNeedsTheToken(t *testing.T) {
	srv, base, c := start(t)
	ctx := context.Background()
	tools, err := c.ListTools(ctx, base+"/ok", "tok-123")
	if err != nil || names(tools) != "echo,lookup_order" && names(tools) != "lookup_order,echo" {
		t.Fatalf("tools=%s err=%v", names(tools), err)
	}
	res, err := c.CallTool(ctx, base+"/ok", "tok-123", "lookup_order", json.RawMessage(`{"order_id":"42"}`))
	if err != nil || string(res.JSON) != `{"items":2,"order_id":"42","status":"shipped"}` {
		t.Fatalf("result=%s err=%v", res.JSON, err)
	}
	if r := lastCall(srv, "lookup_order"); r.Auth != "match" || string(r.Arguments) != `{"order_id":"42"}` {
		t.Fatalf("record=%+v", r)
	}
	if _, err := c.ListTools(ctx, base+"/ok", "wrong"); !errors.Is(err, mcp.ErrAuthenticationFailed) {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := c.ListTools(ctx, base+"/ok", ""); !errors.Is(err, mcp.ErrAuthenticationFailed) {
		t.Fatalf("no token: %v", err)
	}
	var sawNone, sawMismatch bool
	for _, r := range srv.Records() {
		sawNone = sawNone || r.Path == "/ok" && r.Auth == "none"
		sawMismatch = sawMismatch || r.Path == "/ok" && r.Auth == "mismatch"
	}
	if !sawNone || !sawMismatch {
		t.Fatalf("auth not recorded: %+v", srv.Records())
	}
}

func TestOpenNeedsNoToken(t *testing.T) {
	srv, base, c := start(t)
	if _, err := c.CallTool(context.Background(), base+"/open", "", "echo", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if r := lastCall(srv, "echo"); r.Auth != "none" || r.Path != "/open" {
		t.Fatalf("record=%+v", r)
	}
}

func TestFailingTools(t *testing.T) {
	_, base, c := start(t)
	ctx := context.Background()
	for _, tool := range []string{"tool_error", "protocol_error", "drop", "too_big"} {
		if _, err := c.CallTool(ctx, base+"/fail/tools", "", tool, json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s: want an error", tool)
		}
	}
	if _, err := c.CallTool(ctx, base+"/fail/tools", "", "protocol_error", json.RawMessage(`{}`)); !errors.Is(err, mcp.ErrExecutionFailed) {
		t.Errorf("protocol_error: %v", err)
	}
	if _, err := c.ListTools(ctx, base+"/fail/auth", ""); !errors.Is(err, mcp.ErrAuthenticationFailed) {
		t.Errorf("/fail/auth: %v", err)
	}
	if _, err := c.ListTools(ctx, base+"/fail/list", ""); !errors.Is(err, mcp.ErrDiscoveryFailed) {
		t.Errorf("/fail/list: %v", err)
	}
}

func TestSlowHonoursCancellation(t *testing.T) {
	srv, base, c := start(t)
	began := time.Now()
	if _, err := c.CallTool(context.Background(), base+"/slow", "", "slow_lookup", json.RawMessage(`{"delay_ms":200}`)); err != nil || time.Since(began) < 200*time.Millisecond {
		t.Fatalf("slow call: err=%v after %v", err, time.Since(began))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, base+"/slow", "", "slow_lookup", json.RawMessage(`{"delay_ms":1500}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for !lastCall(srv, "slow_lookup").Cancelled {
		if time.Now().After(deadline) {
			t.Fatalf("cancellation not recorded: %+v", srv.Records())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBlackHoleTimesOutAndLeavesNothing(t *testing.T) {
	before := runtime.NumGoroutine()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hole, stop := mcptest.ServeBlackHole(l)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err = c.ListTools(ctx, "http://"+l.Addr().String()+"/mcp", "")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || hole.Accepted() < 1 {
		t.Fatalf("err=%v accepted=%d", err, hole.Accepted())
	}
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, before %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRecordIsBoundedAndHoldsNoToken(t *testing.T) {
	srv, base, c := start(t)
	for i := 0; i < 110; i++ {
		_, _ = c.CallTool(context.Background(), base+"/ok", "tok-123", "echo", json.RawMessage(`{"text":"x"}`))
	}
	recs := srv.Records()
	if len(recs) != 100 {
		t.Fatalf("records=%d, want 100", len(recs))
	}
	raw, _ := json.Marshal(recs)
	if strings.Contains(string(raw), "tok-123") {
		t.Fatal("record holds the token")
	}
}
