package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/storage"
)

type testRemote struct {
	tools []RemoteTool
	list  func(context.Context) ([]RemoteTool, error)
	call  func(context.Context, string, json.RawMessage) (RemoteResult, error)
	seen  string
}

func (r *testRemote) ListTools(ctx context.Context, endpoint, bearer string) ([]RemoteTool, error) {
	r.seen = bearer
	if r.list != nil {
		return r.list(ctx)
	}
	return append([]RemoteTool(nil), r.tools...), nil
}
func (r *testRemote) CallTool(ctx context.Context, endpoint, bearer, name string, args json.RawMessage) (RemoteResult, error) {
	r.seen = bearer
	if r.call != nil {
		return r.call(ctx, name, args)
	}
	return RemoteResult{JSON: json.RawMessage(`{"ok":true}`)}, nil
}

type testCredential struct {
	token string
	err   error
}

func (c testCredential) Resolve(context.Context, string) (string, error) { return c.token, c.err }

type allowHTTP struct{}

func (allowHTTP) Validate(_ context.Context, u *url.URL) error {
	if u.Scheme == "https" || u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" {
		return nil
	}
	return errors.New("destination denied")
}

func newTestService(t *testing.T, remote RemoteClient, cred CredentialResolver) (*Service, *storage.Store) {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/mcp.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	svc, err := NewService(ServiceConfig{Store: s, Remote: remote, Credentials: cred, Destinations: allowHTTP{}, Timeout: 100 * time.Millisecond, MaxResultBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	return svc, s
}

func createTestServer(t *testing.T, s *Service, endpoint, credential string) storage.MCPServer {
	t.Helper()
	server, err := s.Create(context.Background(), ServerConfig{Name: "test", Endpoint: endpoint, Transport: "streamable-http", CredentialID: credential, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestRefreshPersistsSortedCanonicalImmutableSnapshots(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{
		{Name: "zeta", Description: "last", InputSchema: json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}}}`)},
		{Name: "alpha", InputSchema: json.RawMessage(`{"properties":{"id":{"type":"integer"}},"type":"object"}`)},
	}}
	svc, store := newTestService(t, remote, testCredential{})
	server := createTestServer(t, svc, "https://mcp.example/rpc", "")
	first, err := svc.Refresh(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tools) != 2 || first.Tools[0].RemoteName != "alpha" || first.Tools[1].RemoteName != "zeta" {
		t.Fatalf("tools not sorted: %#v", first.Tools)
	}
	got, err := store.MCP().Discovery(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DiscoveryHash != first.DiscoveryHash || got.Tools[0].SchemaHash == "" {
		t.Fatalf("bad canonical hashes: %#v", got)
	}
	remote.tools[0].Description = "updated description"
	second, err := svc.Refresh(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.MCP().Discovery(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Tools[1].Description != "last" || second.ID == first.ID {
		t.Fatalf("refresh mutated old snapshot: old=%#v new=%#v", old, second)
	}
}

func TestRefreshRejectsMalformedAndOversizedSchemasWithoutPublishing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema json.RawMessage
	}{
		{"malformed", json.RawMessage(`{"type":`)},
		{"oversized", json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", 70<<10) + `"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &testRemote{tools: []RemoteTool{{Name: "bad", InputSchema: tc.schema}}}
			svc, store := newTestService(t, remote, testCredential{})
			server := createTestServer(t, svc, "https://mcp.example/rpc", "")
			if _, err := svc.Refresh(context.Background(), server.ID); err == nil {
				t.Fatal("accepted invalid schema")
			}
			if _, err := store.MCP().LatestDiscovery(context.Background(), server.ID); err == nil {
				t.Fatal("published failed discovery")
			}
		})
	}
}

func TestExecuteValidatesArgumentsBoundsResultsAndSanitizesErrors(t *testing.T) {
	const credential = "credential-secret-sentinel"
	credentialID := storage.NewID()
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}`)}}}
	svc, store := newTestService(t, remote, testCredential{token: credential})
	if err := store.Credentials().Put(context.Background(), storage.Credential{ID: credentialID, Provider: "mcp", Ciphertext: []byte("encrypted"), KeyID: "test-key"}); err != nil {
		t.Fatal(err)
	}
	server := createTestServer(t, svc, "https://mcp.example/rpc", credentialID)
	if _, err := svc.Refresh(context.Background(), server.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(context.Background(), server.ID, "lookup", json.RawMessage(`{}`)); err == nil {
		t.Fatal("invalid arguments executed")
	}
	remote.call = func(context.Context, string, json.RawMessage) (RemoteResult, error) {
		return RemoteResult{JSON: json.RawMessage(`{"token":"` + credential + `","safe":true}`)}, nil
	}
	result, err := svc.Execute(context.Background(), server.ID, "lookup", json.RawMessage(`{"id":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.JSON), credential) || remote.seen != credential {
		t.Fatalf("credential leak or missing auth: result=%s", result.JSON)
	}
	remote.call = func(context.Context, string, json.RawMessage) (RemoteResult, error) {
		return RemoteResult{JSON: json.RawMessage(`{"data":"` + strings.Repeat("x", 256) + `"}`)}, nil
	}
	if _, err := svc.Execute(context.Background(), server.ID, "lookup", json.RawMessage(`{"id":"1"}`)); err == nil {
		t.Fatal("accepted oversized result")
	}
	remote.call = func(context.Context, string, json.RawMessage) (RemoteResult, error) {
		return RemoteResult{}, errors.New("remote secret " + credential)
	}
	_, err = svc.Execute(context.Background(), server.ID, "lookup", json.RawMessage(`{"id":"1"}`))
	if err == nil || strings.Contains(err.Error(), credential) || strings.Contains(err.Error(), "remote secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestRefreshAndExecuteHonorTimeoutAndCancellation(t *testing.T) {
	remote := &testRemote{list: func(ctx context.Context) ([]RemoteTool, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
			return nil, nil
		}
	}}
	svc, _ := newTestService(t, remote, testCredential{})
	server := createTestServer(t, svc, "https://mcp.example/rpc", "")
	start := time.Now()
	if _, err := svc.Refresh(context.Background(), server.ID); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout not enforced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Refresh(ctx, server.ID); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestCanceledContextStopsBeforeStorageOrRemoteWork(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "unused", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	svc, _ := newTestService(t, remote, testCredential{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Refresh(ctx, storage.NewID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh error=%v", err)
	}
	if remote.seen != "" {
		t.Fatal("canceled operation reached remote client")
	}
}

func TestTypedAuthenticationFailureUpdatesValidationState(t *testing.T) {
	remote := &testRemote{list: func(context.Context) ([]RemoteTool, error) { return nil, ErrAuthenticationFailed }}
	svc, _ := newTestService(t, remote, testCredential{})
	server := createTestServer(t, svc, "https://mcp.example/rpc", "")
	if _, err := svc.Refresh(context.Background(), server.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("refresh error=%v", err)
	}
	updated, err := svc.Get(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ValidationState != StateAuthenticationFailed {
		t.Fatalf("state=%q", updated.ValidationState)
	}
}

func TestSchemaPreflightRejectsRecursiveRefsUnsupportedDialectsAndExcessComplexity(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"next":{"$ref":"#"}}}`,
		`{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"object"}`,
	} {
		if _, err := validateSchema([]byte(raw)); err == nil {
			t.Fatalf("accepted unsafe schema %s", raw)
		}
	}
	deep := `{"type":"object","properties":{"x":` + strings.Repeat(`{"properties":{"x":`, 70) + `{"type":"string"}` + strings.Repeat(`}}`, 70) + `}}`
	if _, err := validateSchema([]byte(deep)); err == nil {
		t.Fatal("accepted excessive schema depth")
	}
}

func TestRecursiveReferenceCannotCrashProcess(t *testing.T) {
	if os.Getenv("MCP_SCHEMA_SUBPROCESS") == "1" {
		_, err := validateSchema([]byte(`{"type":"object","properties":{"next":{"$ref":"#"}}}`))
		if err == nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecursiveReferenceCannotCrashProcess$")
	cmd.Env = append(os.Environ(), "MCP_SCHEMA_SUBPROCESS=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recursive ref subprocess crashed or hung: err=%v output=%s", err, output)
	}
}

func TestResultSanitizationParsesNestedValuesAndRedactsDecodedCredential(t *testing.T) {
	credential := "s3cret-token"
	secret := strings.Repeat("x", 800)
	raw := json.RawMessage(`{"nested":[{"token":{"value":"` + secret + `"},"safe":"prefix s3cret-token suffix"}],"credential":["hidden"]}`)
	clean := redactResult(raw, credential)
	if !json.Valid(clean) {
		t.Fatalf("sanitized result is invalid JSON: %s", clean)
	}
	for _, needle := range []string{credential, secret, "hidden"} {
		if strings.Contains(string(clean), needle) {
			t.Fatalf("sanitizer leaked %q: %s", needle, clean)
		}
	}
	escaped := redactResult(json.RawMessage(`{"message":"s3cr\u0065t-token","authorization":{"nested":"value"}}`), credential)
	if strings.Contains(string(escaped), credential) || strings.Contains(string(escaped), "value") || !json.Valid(escaped) {
		t.Fatalf("escaped value not safely sanitized: %s", escaped)
	}
}

func TestPublicEgressPredicateRejectsPrivateAndSpecialUseAddresses(t *testing.T) {
	for _, raw := range []string{"10.0.0.1", "100.64.0.1", "100.127.255.254", "127.0.0.1", "169.254.1.1", "192.0.2.1", "192.31.196.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "::1", "fe80::1", "fec0::1", "fc00::1", "2001:db8::1", "2002::1", "64:ff9b::a00:1"} {
		if ip := net.ParseIP(raw); ip == nil || isPublicEgressIP(ip) {
			t.Errorf("accepted non-public address %s", raw)
		}
	}
	if !isPublicEgressIP(net.ParseIP("1.1.1.1")) || !isPublicEgressIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("rejected public addresses")
	}
}

func TestPublicEgressDialValidatesEveryResolvedAddressBeforeDial(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("100.64.0.1")}}, nil
	}
	dialed := false
	_, err := dialPublicEgress(context.Background(), "tcp", "mcp.example:443", lookup, func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	})
	if err == nil || dialed {
		t.Fatalf("private DNS answer was not rejected before dialing: err=%v dialed=%v", err, dialed)
	}

	lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	var dialAddress string
	got, err := dialPublicEgress(context.Background(), "tcp", "mcp.example:443", lookup, func(_ context.Context, network, address string) (net.Conn, error) {
		dialAddress = network + " " + address
		return clientConn, nil
	})
	if err != nil || got != clientConn || dialAddress != "tcp 1.1.1.1:443" {
		t.Fatalf("public IP was not pinned for dial: got=%v address=%q err=%v", got, dialAddress, err)
	}
}

type safeConcurrentRemote struct{}

func (safeConcurrentRemote) ListTools(context.Context, string, string) ([]RemoteTool, error) {
	return []RemoteTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}
func (safeConcurrentRemote) CallTool(context.Context, string, string, string, json.RawMessage) (RemoteResult, error) {
	return RemoteResult{JSON: json.RawMessage(`{"ok":true}`)}, nil
}

type recordingTracer struct {
	mu       sync.Mutex
	events   []core.TraceEvent
	parented []bool
	attrs    []map[string]any
}

func (t *recordingTracer) StartSpan(ctx context.Context, e core.TraceEvent) core.Span {
	t.mu.Lock()
	t.events = append(t.events, e)
	t.parented = append(t.parented, core.SpanFromContext(ctx) != nil)
	t.attrs = append(t.attrs, map[string]any{})
	i := len(t.events) - 1
	t.mu.Unlock()
	return recordingSpan{t: t, i: i}
}

type recordingSpan struct {
	t *recordingTracer
	i int
}

func (s recordingSpan) SetAttribute(k string, v any) {
	s.t.mu.Lock()
	s.t.attrs[s.i][k] = v
	s.t.mu.Unlock()
}
func (recordingSpan) AddEvent(core.TraceEvent) {}
func (recordingSpan) End(error)                {}

func TestConcurrentExecutionsHaveDistinctTraceAndResponseIdentityWithParent(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/mcp-concurrency.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tracer := &recordingTracer{}
	svc, err := NewService(ServiceConfig{Store: store, Remote: safeConcurrentRemote{}, Destinations: allowHTTP{}, Tracer: tracer})
	if err != nil {
		t.Fatal(err)
	}
	server, err := svc.Create(context.Background(), ServerConfig{Name: "concurrent", Endpoint: "https://mcp.example/rpc", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(context.Background(), server.ID); err != nil {
		t.Fatal(err)
	}
	parent := recordingSpan{t: tracer, i: 0}
	parentCtx := core.ContextWithSpan(context.Background(), parent)
	const count = 12
	ids := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.Execute(parentCtx, server.ID, "echo", json.RawMessage(`{}`))
			if err != nil {
				errs <- err
				return
			}
			ids <- result.ExecutionID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("missing or duplicate execution ID %q", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("response execution IDs=%d want=%d", len(seen), count)
	}
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	if len(tracer.events) != count+1 {
		t.Fatalf("span count=%d", len(tracer.events))
	}
	spanSeen := map[string]bool{}
	for i, e := range tracer.events[1:] {
		if e.Name != "mcp.tool.execute" || e.ToolCallID == "" || spanSeen[e.ToolCallID] {
			t.Errorf("bad trace identity event=%#v", e)
		}
		spanSeen[e.ToolCallID] = true
		if !seen[e.ToolCallID] {
			t.Errorf("trace execution ID %q did not match any returned ID", e.ToolCallID)
		}
		if !tracer.parented[i+1] {
			t.Errorf("execution span %s lost caller parent", e.ToolCallID)
		}
		if tracer.attrs[i+1]["mcp.server_id"] != server.ID || tracer.attrs[i+1]["mcp.tool_name"] != "echo" {
			t.Errorf("trace lacks safe MCP identity: %#v", tracer.attrs[i+1])
		}
		if strings.Contains(strings.ToLower(fmt.Sprint(e.Attributes(), tracer.attrs[i+1])), "result") || strings.Contains(fmt.Sprint(e.Attributes(), tracer.attrs[i+1]), `"ok"`) {
			t.Errorf("trace contains tool content: event=%#v attrs=%#v", e.Attributes(), tracer.attrs[i+1])
		}
	}
}

func TestCreateRejectsUnsupportedLocalAndUnsafeRemoteDestination(t *testing.T) {
	svc, _ := newTestService(t, &testRemote{}, testCredential{})
	for _, cfg := range []ServerConfig{
		{Name: "local", Transport: "stdio", Enabled: true},
		{Name: "plain", Transport: "streamable-http", Endpoint: "http://example.org/mcp", Enabled: true},
	} {
		if _, err := svc.Create(context.Background(), cfg); err == nil {
			t.Fatalf("accepted unsafe transport/config %#v", cfg)
		}
	}
}
