package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/server/flow"
	"govoice/server/mcp/mcptest"
	"govoice/server/storage"
)

func ptr(s string) *string { return &s }

// mcptestURL serves a fresh mcptest server and returns it with its base URL.
func mcptestURL(t *testing.T) (*mcptest.Server, string) {
	t.Helper()
	srv := mcptest.New(mcptest.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts.URL
}

func TestReasonCode(t *testing.T) {
	cases := map[error]string{
		ErrInvalidEndpoint:      "invalid_endpoint",
		ErrDestinationDenied:    "destination_denied",
		ErrInvalidAuthEnv:       "invalid_auth_env",
		ErrInvalidMetadata:      "invalid_metadata",
		ErrTransportUnsupported: "transport_unsupported",
		ErrLocalDisabled:        "transport_unsupported",
		ErrNotFound:             "",
	}
	for err, want := range cases {
		if got := ReasonCode(err); got != want {
			t.Errorf("ReasonCode(%v)=%q, want %q", err, got, want)
		}
	}
	if got := ReasonCode(errors.Join(errors.New("x"), ErrInvalidEndpoint)); got != "invalid_endpoint" {
		t.Errorf("wrapped: %q", got)
	}
}

func TestUpdateNameOnlyKeepsStateAndDoesNotSync(t *testing.T) {
	r := newSyncRig(t)
	ctx := context.Background()
	if _, err := r.svc.Refresh(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	before := r.state(t)
	got, err := r.svc.Update(ctx, r.id, ServerUpdate{Name: ptr("renamed"), Description: ptr("d")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.Description != "d" || got.ValidationState != StateHealthy || got.LatestDiscoveryID != before.LatestDiscoveryID {
		t.Fatalf("after rename: %+v", got)
	}
	if !got.LastConnectedAt.Equal(before.LastConnectedAt) {
		t.Fatal("a rename must not sync")
	}
}

func TestUpdateEndpointSyncsAndReportsFailureInState(t *testing.T) {
	r := newSyncRig(t)
	ctx := context.Background()
	_, newURL := mcptestURL(t)
	got, err := r.svc.Update(ctx, r.id, ServerUpdate{Endpoint: ptr(newURL + "/open")})
	if err != nil {
		t.Fatal(err)
	}
	if got.ValidationState != StateHealthy || got.LatestDiscoveryID == "" || !strings.HasSuffix(got.Endpoint, "/open") {
		t.Fatalf("reachable edit: %+v", got)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	got, err = r.svc.Update(ctx, r.id, ServerUpdate{Endpoint: ptr(deadURL + "/open")})
	if err != nil {
		t.Fatalf("unreachable edit must not error: %v", err)
	}
	if got.ValidationState != StateConnectionFailed {
		t.Fatalf("unreachable edit state %q", got.ValidationState)
	}
}

func TestUpdateRejectsWithReasonCodes(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/mcp.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := NewService(ServiceConfig{Store: store, Remote: &testRemote{}, Destinations: HTTPSDestinationPolicy{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := svc.Create(ctx, ServerConfig{Name: "n", Endpoint: "https://8.8.8.8/mcp", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		u    ServerUpdate
		want string
	}{
		{"endpoint", ServerUpdate{Endpoint: ptr("not a url")}, "invalid_endpoint"},
		{"query", ServerUpdate{Endpoint: ptr("https://8.8.8.8/mcp?token=x")}, "invalid_endpoint"},
		{"private", ServerUpdate{Endpoint: ptr("https://10.0.0.1/mcp")}, "destination_denied"},
		{"auth", ServerUpdate{AuthEnv: ptr("TOKEN_SECRET")}, "invalid_auth_env"},
		{"long name", ServerUpdate{Name: ptr(strings.Repeat("x", 129))}, "invalid_metadata"},
		{"blank name", ServerUpdate{Name: ptr("  ")}, "invalid_metadata"},
	}
	for _, c := range cases {
		_, err := svc.Update(ctx, m.ID, c.u)
		if got := ReasonCode(err); got != c.want {
			t.Errorf("%s: reason %q (err %v), want %q", c.name, got, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "10.0.0.1") {
			t.Errorf("%s: error reveals the address: %v", c.name, err)
		}
	}
	if _, err = svc.Update(ctx, "missing", ServerUpdate{Name: ptr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err = svc.store.MCP().DisableServer(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Update(ctx, m.ID, ServerUpdate{Endpoint: ptr("https://8.8.4.4/mcp")}); err != nil || got.ValidationState != StateDisabled {
		t.Fatalf("disabled server edit: %+v err=%v", got, err)
	}
}

func TestUpdateAuthEnvClears(t *testing.T) {
	svc, _ := newTestService(t, &testRemote{}, testTokens{"MCP_TOKEN_A": "v"})
	m := createTestServer(t, svc, "https://mcp.example/rpc", "MCP_TOKEN_A")
	got, err := svc.Update(context.Background(), m.ID, ServerUpdate{AuthEnv: ptr("")})
	if err != nil || got.AuthEnv != "" {
		t.Fatalf("clear: %+v err=%v", got, err)
	}
}

func TestUpdatePendingDeletion(t *testing.T) {
	r := newSyncRig(t)
	ctx := context.Background()
	if _, err := r.svc.Refresh(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	flows := flow.NewService(r.store, allAvailable{})
	f, err := flows.Create(ctx, flow.CreateRequest{Name: "f", Config: json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := r.state(t)
	d, err := r.store.MCP().Discovery(ctx, m.LatestDiscoveryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = flows.SetMCPTools(ctx, f.ID, []flow.ToolSelection{{ServerID: m.ID, RemoteName: d.Tools[0].RemoteName, SchemaHash: d.Tools[0].SchemaHash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	if err = r.svc.DetachAndDelete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if cur, gerr := r.svc.Get(ctx, m.ID); gerr == nil && cur.PendingDelete {
		if _, err = r.svc.Update(ctx, m.ID, ServerUpdate{Name: ptr("x")}); !errors.Is(err, ErrPendingDeletion) {
			t.Fatalf("pending deletion: %v", err)
		}
	}
}

func TestUpdateKeepsFlowReadyAndCapturedSessionOnOldEndpoint(t *testing.T) {
	r := newSyncRig(t)
	ctx := context.Background()
	d, err := r.svc.Refresh(ctx, r.id)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	for _, tool := range d.Tools {
		if tool.RemoteName == "echo" {
			hash = tool.SchemaHash
		}
	}
	flows := flow.NewService(r.store, allAvailable{})
	f, err := flows.Create(ctx, flow.CreateRequest{Name: "f", Config: json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = flows.SetMCPTools(ctx, f.ID, []flow.ToolSelection{{ServerID: r.id, RemoteName: "echo", SchemaHash: hash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	captured, err := r.svc.CaptureTool(ctx, r.id, "echo", hash)
	if err != nil {
		t.Fatal(err)
	}
	newSrv, newURL := mcptestURL(t)
	if _, err = r.svc.Update(ctx, r.id, ServerUpdate{Endpoint: ptr(newURL + "/open")}); err != nil {
		t.Fatal(err)
	}
	got, err := flows.Get(ctx, f.ID)
	if err != nil || got.Readiness.State != "ready" {
		t.Fatalf("flow after edit: readiness=%+v err=%v", got.Readiness, err)
	}
	if _, err = captured.Execute(ctx, json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	for _, rec := range newSrv.Records() {
		if rec.Tool == "echo" {
			t.Fatal("a session captured before the edit called the new endpoint")
		}
	}
}

func TestPatchRouteStatuses(t *testing.T) {
	r := newSyncRig(t)
	h := Handler(r.svc)
	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := do("/api/v1/mcp-servers/"+r.id, `{"name":"new"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"new"`) {
		t.Fatalf("patch=%d %s", w.Code, w.Body.String())
	}
	if w := do("/api/v1/mcp-servers/"+r.id, `{"endpoint":"not a url"}`); w.Code != 400 || !strings.Contains(w.Body.String(), `"reason":"invalid_endpoint"`) || !strings.Contains(w.Body.String(), `"invalid_mcp_server"`) {
		t.Fatalf("bad endpoint=%d %s", w.Code, w.Body.String())
	}
	if w := do("/api/v1/mcp-servers/"+r.id, `{"bogus":1}`); w.Code != 400 {
		t.Fatalf("unknown field=%d", w.Code)
	}
	if w := do("/api/v1/mcp-servers/nope", `{"name":"x"}`); w.Code != 404 {
		t.Fatalf("missing=%d", w.Code)
	}
	create := httptest.NewRequest("POST", "/api/v1/mcp-servers", strings.NewReader(`{"name":"x","endpoint":"http://127.0.0.1:9/mcp","auth_env":"BAD","enabled":true}`))
	create.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, create)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"reason":"invalid_auth_env"`) {
		t.Fatalf("create reason=%d %s", w.Code, w.Body.String())
	}
}
