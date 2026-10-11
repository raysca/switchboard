package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

type testTokens map[string]string

func (t testTokens) Token(name string) (string, bool) { v, ok := t[name]; return v, ok && v != "" }

type allowHTTP struct{}

func (allowHTTP) Validate(_ context.Context, u *url.URL) error {
	if u.Scheme == "https" || u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" {
		return nil
	}
	return errors.New("destination denied")
}

func newTestService(t *testing.T, remote RemoteClient, tokens TokenSource) (*Service, *storage.Store) {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/mcp.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	svc, err := NewService(ServiceConfig{Store: s, Remote: remote, Tokens: tokens, Destinations: allowHTTP{}, Timeout: 100 * time.Millisecond, MaxResultBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	return svc, s
}

func createTestServer(t *testing.T, s *Service, endpoint, authEnv string) storage.MCPServer {
	t.Helper()
	server, err := s.Create(context.Background(), ServerConfig{Name: "test", Endpoint: endpoint, Transport: "streamable-http", AuthEnv: authEnv, Enabled: true})
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
	svc, store := newTestService(t, remote, testTokens{})
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
			svc, store := newTestService(t, remote, testTokens{})
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
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}`)}}}
	svc, _ := newTestService(t, remote, testTokens{"MCP_TOKEN_CRM": credential})
	server := createTestServer(t, svc, "https://mcp.example/rpc", "MCP_TOKEN_CRM")
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
	svc, _ := newTestService(t, remote, testTokens{})
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
	svc, _ := newTestService(t, remote, testTokens{})
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
	svc, _ := newTestService(t, remote, testTokens{})
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

func TestTypedConnectionAndInitializeFailuresPrecedeDiscoveryFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		state string
	}{
		{name: "connection", err: ErrConnectionFailed, state: StateConnectionFailed},
		{name: "initialize", err: ErrInitializeFailed, state: StateConnectionFailed},
		{name: "discovery", err: ErrDiscoveryFailed, state: StateDiscoveryFailed},
		{name: "wrapped connection", err: fmt.Errorf("transport: %w", remoteError(ErrConnectionFailed)), state: StateConnectionFailed},
		{name: "wrapped initialize", err: fmt.Errorf("client: %w", remoteError(ErrInitializeFailed)), state: StateConnectionFailed},
		{name: "canceled", err: context.Canceled, state: StateConnectionFailed},
		{name: "deadline", err: context.DeadlineExceeded, state: StateConnectionFailed},
		{name: "unknown list failure", err: errors.New("remote body"), state: StateDiscoveryFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &testRemote{list: func(context.Context) ([]RemoteTool, error) { return nil, tc.err }}
			svc, _ := newTestService(t, remote, testTokens{})
			server := createTestServer(t, svc, "https://mcp.example/rpc", "")
			if _, err := svc.Refresh(context.Background(), server.ID); err == nil {
				t.Fatal("expected refresh failure")
			}
			updated, err := svc.Get(context.Background(), server.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.ValidationState != tc.state {
				t.Fatalf("state=%q, want %q", updated.ValidationState, tc.state)
			}
		})
	}
}

func TestSchemaPreflightRejectsRecursiveRefsUnsupportedDialectsAndExcessComplexity(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"next":{"$ref":"#"}}}`,
		`{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"object"}`,
	} {
		if _, err := validateSchemaContext(context.Background(), []byte(raw), defaultMaxSchemaBytes); err == nil {
			t.Fatalf("accepted unsafe schema %s", raw)
		}
	}
	deep := `{"type":"object","properties":{"x":` + strings.Repeat(`{"properties":{"x":`, 70) + `{"type":"string"}` + strings.Repeat(`}}`, 70) + `}}`
	if _, err := validateSchemaContext(context.Background(), []byte(deep), defaultMaxSchemaBytes); err == nil {
		t.Fatal("accepted excessive schema depth")
	}
}

func TestSchemaDialectAllowlistMatchesValidatorAndAcceptedSchemasValidate(t *testing.T) {
	for _, tc := range []struct {
		uri string
		ok  bool
	}{
		{"https://json-schema.org/draft/2020-12/schema", true},
		{"http://json-schema.org/draft-07/schema#", true},
		{"https://json-schema.org/draft-07/schema#", true},
		{"https://json-schema.org/draft/2020-12/schema#", false},
		{"http://json-schema.org/draft-07/schema", false},
		{"https://json-schema.org/draft-07/schema", false},
		{"https://json-schema.org/draft/2020-12/schema/", false},
		{"https://json-schema.org/draft/2020-12/schema?x=1", false},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			raw := []byte(`{"$schema":` + fmt.Sprintf("%q", tc.uri) + `,"type":"object"}`)
			resolved, err := validateSchemaContext(context.Background(), raw, defaultMaxSchemaBytes)
			if !tc.ok {
				if err == nil {
					t.Fatal("accepted unsupported or noncanonical dialect URI")
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected supported dialect: %v", err)
			}
			if err := resolved.Validate(map[string]any{}); err != nil {
				t.Fatalf("accepted dialect did not execute: %v", err)
			}
			remote := &testRemote{tools: []RemoteTool{{Name: "echo", InputSchema: raw}}}
			svc, _ := newTestService(t, remote, testTokens{})
			server := createTestServer(t, svc, "https://mcp.example/rpc", "")
			if _, err := svc.Refresh(context.Background(), server.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Execute(context.Background(), server.ID, "echo", json.RawMessage(`{}`)); err != nil {
				t.Fatalf("accepted discovery could not execute empty object: %v", err)
			}
		})
	}
}

func TestRefreshRejectsUnsupportedReferenceKeywordsWithoutReplacingDiscovery(t *testing.T) {
	for _, keyword := range []string{"$dynamicRef", "$recursiveRef", "$dynamicAnchor", "$recursiveAnchor", "$anchor", "$id", "$REF", "$DynamicRef"} {
		t.Run(keyword, func(t *testing.T) {
			remote := &testRemote{tools: []RemoteTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
			svc, store := newTestService(t, remote, testTokens{})
			server := createTestServer(t, svc, "https://mcp.example/rpc", "")
			previous, err := svc.Refresh(context.Background(), server.ID)
			if err != nil {
				t.Fatal(err)
			}
			remote.tools[0].InputSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"next":{%q:""}}}`, keyword))
			if _, err := svc.Refresh(context.Background(), server.ID); !errors.Is(err, ErrInvalidSchema) {
				t.Fatalf("unsupported keyword was accepted: %v", err)
			}
			latest, err := store.MCP().LatestDiscovery(context.Background(), server.ID)
			if err != nil || latest.ID != previous.ID {
				t.Fatalf("invalid schema replaced discovery: latest=%q previous=%q err=%v", latest.ID, previous.ID, err)
			}
			updated, err := svc.Get(context.Background(), server.ID)
			if err != nil || updated.ValidationState != StateSchemaInvalid {
				t.Fatalf("schema failure state=%q err=%v", updated.ValidationState, err)
			}
		})
	}
}

func expandedReferenceSchema(combinator string, levels int) []byte {
	var schema strings.Builder
	schema.WriteString(`{"type":"object","$defs":{`)
	for i := 0; i < levels; i++ {
		if i > 0 {
			schema.WriteByte(',')
		}
		fmt.Fprintf(&schema, `"n%d":{%q:[{"$ref":"#/$defs/n%d"},{"$ref":"#/$defs/n%d"}]}`, i, combinator, i+1, i+1)
	}
	fmt.Fprintf(&schema, `,"n%d":{"type":"object"}},"allOf":[{"$ref":"#/$defs/n0"}]}`, levels)
	return []byte(schema.String())
}

func TestSchemaAcceptsSmallSharedReferenceGraph(t *testing.T) {
	resolved, err := validateSchemaContext(context.Background(), expandedReferenceSchema("allOf", 3), defaultMaxSchemaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(map[string]any{}); err != nil {
		t.Fatalf("bounded shared refs did not execute: %v", err)
	}
}

func TestSchemaAcceptsReferenceToEmptySchema(t *testing.T) {
	// jsonschema-go serializes an empty schema object as the boolean true.
	resolved, err := validateSchemaContext(context.Background(), []byte(`{"type":"object","$defs":{"anything":{}},"properties":{"value":{"$ref":"#/$defs/anything"}}}`), defaultMaxSchemaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(map[string]any{"value": 42}); err != nil {
		t.Fatalf("empty referenced schema rejected value: %v", err)
	}
}

func TestSchemaPreflightAndValidatorUseSameDecodedJSON(t *testing.T) {
	// encoding/json merges repeated objects when decoding into struct maps.
	// Preflight's JSON map uses the last value, which must also reach validation.
	resolved, err := validateSchemaContext(context.Background(), []byte(`{"type":"object","properties":{"blocked":false},"properties":{}}`), defaultMaxSchemaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(map[string]any{"blocked": true}); err != nil {
		t.Fatalf("validator retained a schema discarded by preflight: %v", err)
	}
}

func TestSchemaRejectsNoncanonicalStructuralKeywords(t *testing.T) {
	// The lowercase alias overwrites gate in the validator's struct map, making
	// the ref recursive even though the graph of the raw JSON is acyclic.
	raw := []byte(`{"type":"object","dependentSchemas":{"gate":{"type":"object"}},"dependentschemas":{"gate":{"$ref":"#/dependentSchemas/gate"}}}`)
	if _, err := validateSchemaContext(context.Background(), raw, defaultMaxSchemaBytes); err == nil {
		t.Fatal("accepted structural keyword alias that changes reference targets")
	}
}

func TestSchemaRejectsExcessiveExpandedReferenceDepth(t *testing.T) {
	var schema strings.Builder
	schema.WriteString(`{"type":"object","$defs":{`)
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&schema, `"n%d":{"$ref":"#/$defs/n%d"},`, i, i+1)
	}
	schema.WriteString(`"n80":{"type":"object"}},"$ref":"#/$defs/n0"}`)
	if _, err := validateSchemaContext(context.Background(), []byte(schema.String()), defaultMaxSchemaBytes); err == nil {
		t.Fatal("accepted excessive expanded reference depth")
	}
}

func TestDynamicReferenceCannotCrashProcess(t *testing.T) {
	if os.Getenv("MCP_DYNAMIC_SCHEMA_SUBPROCESS") == "1" {
		_, err := validateSchemaContext(context.Background(), []byte(`{"$dynamicAnchor":"node","type":"object","properties":{"next":{"$dynamicRef":"#node"}}}`), defaultMaxSchemaBytes)
		if err == nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDynamicReferenceCannotCrashProcess$")
	cmd.Env = append(os.Environ(), "MCP_DYNAMIC_SCHEMA_SUBPROCESS=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dynamic ref subprocess crashed or hung: err=%v output=%s", err, output)
	}
}

func TestExpandedReferenceWorkCannotCrashProcess(t *testing.T) {
	if os.Getenv("MCP_EXPANDED_SCHEMA_SUBPROCESS") == "1" {
		for _, combinator := range []string{"allOf", "anyOf", "oneOf"} {
			if _, err := validateSchemaContext(context.Background(), expandedReferenceSchema(combinator, 28), defaultMaxSchemaBytes); err == nil {
				os.Exit(2)
			}
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExpandedReferenceWorkCannotCrashProcess$")
	cmd.Env = append(os.Environ(), "MCP_EXPANDED_SCHEMA_SUBPROCESS=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("expanded ref subprocess crashed or hung: err=%v output=%s", err, output)
	}
}

func TestRecursiveReferenceCannotCrashProcess(t *testing.T) {
	if os.Getenv("MCP_SCHEMA_SUBPROCESS") == "1" {
		_, err := validateSchemaContext(context.Background(), []byte(`{"type":"object","properties":{"next":{"$ref":"#"}}}`), defaultMaxSchemaBytes)
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
	clean, _ := sanitizeResult(raw, credential, defaultMaxResultBytes)
	if !json.Valid(clean) {
		t.Fatalf("sanitized result is invalid JSON: %s", clean)
	}
	for _, needle := range []string{credential, secret, "hidden"} {
		if strings.Contains(string(clean), needle) {
			t.Fatalf("sanitizer leaked %q: %s", needle, clean)
		}
	}
	escaped, _ := sanitizeResult(json.RawMessage(`{"message":"s3cr\u0065t-token","authorization":{"nested":"value"}}`), credential, defaultMaxResultBytes)
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

type asyncDurationTracer struct{ ended chan map[string]any }

func (t *asyncDurationTracer) StartSpan(context.Context, core.TraceEvent) core.Span {
	return &asyncDurationSpan{tracer: t, attrs: make(map[string]any)}
}

type asyncDurationSpan struct {
	tracer *asyncDurationTracer
	mu     sync.Mutex
	attrs  map[string]any
}

func (s *asyncDurationSpan) SetAttribute(key string, value any) {
	s.mu.Lock()
	s.attrs[key] = value
	s.mu.Unlock()
}
func (*asyncDurationSpan) AddEvent(core.TraceEvent) {}
func (s *asyncDurationSpan) End(error) {
	s.mu.Lock()
	attrs := make(map[string]any, len(s.attrs))
	for key, value := range s.attrs {
		attrs[key] = value
	}
	s.mu.Unlock()
	s.tracer.ended <- attrs
}

func TestExecuteAsyncTraceExportsDurationBeforeSpanEnd(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/mcp-async-trace.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	exporter := &asyncDurationTracer{ended: make(chan map[string]any, 1)}
	asyncTracer := core.NewAsyncTracer(exporter, 16)
	defer asyncTracer.Close()
	svc, err := NewService(ServiceConfig{Store: store, Remote: safeConcurrentRemote{}, Destinations: allowHTTP{}, Tracer: asyncTracer})
	if err != nil {
		t.Fatal(err)
	}
	server, err := svc.Create(context.Background(), ServerConfig{Name: "async trace", Endpoint: "https://mcp.example/rpc", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(context.Background(), server.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exporter.ended: // consume the discovery span; the next is execution.
	case <-time.After(2 * time.Second):
		t.Fatal("async exporter did not finish the discovery span")
	}
	for _, name := range []string{"echo", "missing"} {
		_, err := svc.Execute(context.Background(), server.ID, name, json.RawMessage(`{}`))
		if (name == "echo" && err != nil) || (name == "missing" && !errors.Is(err, ErrNotFound)) {
			t.Fatalf("execute %s error=%v", name, err)
		}
		select {
		case attrs := <-exporter.ended:
			if duration, ok := attrs["mcp.duration_ms"].(int64); !ok || duration < 0 {
				t.Fatalf("duration missing from ended async span for %s: %#v", name, attrs)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("async exporter did not finish the execution span")
		}
	}
}

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
	svc, _ := newTestService(t, &testRemote{}, testTokens{})
	for _, cfg := range []ServerConfig{
		{Name: "local", Transport: "stdio", Enabled: true},
		{Name: "plain", Transport: "streamable-http", Endpoint: "http://example.org/mcp", Enabled: true},
	} {
		if _, err := svc.Create(context.Background(), cfg); err == nil {
			t.Fatalf("accepted unsafe transport/config %#v", cfg)
		}
	}
}

// An async (LongCall) execution outlives the service's short call timeout but
// stays bounded by, and cancelled with, the caller's context (session close).
func TestLongCallUsesCallerDeadlineAndCancellation(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	remote := &testRemote{tools: []RemoteTool{{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`)}}, call: func(ctx context.Context, _ string, args json.RawMessage) (RemoteResult, error) {
		select {
		case <-time.After(300 * time.Millisecond): // > the 100ms service timeout
			return RemoteResult{JSON: json.RawMessage(`{"ok":true}`)}, nil
		case <-ctx.Done():
			cancelled <- struct{}{}
			return RemoteResult{}, ctx.Err()
		}
	}}
	svc, _ := newTestService(t, remote, testTokens{})
	server := createTestServer(t, svc, "https://mcp.example/rpc", "")
	discovery, err := svc.Refresh(context.Background(), server.ID)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := svc.CaptureTool(context.Background(), server.ID, "slow", discovery.Tools[0].SchemaHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.ExecuteCall(context.Background(), json.RawMessage(`{}`), "c1", "s", "t", 1); err == nil {
		t.Fatal("a plain call must keep the service timeout")
	}
	<-cancelled
	ctx, cancel := context.WithTimeout(LongCall(context.Background()), 5*time.Second)
	defer cancel()
	if _, err := tool.ExecuteCall(ctx, json.RawMessage(`{}`), "c2", "s", "t", 1); err != nil {
		t.Fatalf("long call: %v", err)
	}
	ctx, cancel = context.WithTimeout(LongCall(context.Background()), 5*time.Second)
	time.AfterFunc(20*time.Millisecond, cancel) // session close
	if _, err := tool.ExecuteCall(ctx, json.RawMessage(`{}`), "c3", "s", "t", 1); err == nil {
		t.Fatal("cancelled long call succeeded")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("remote call not cancelled")
	}
}

func TestAuthEnvCreateValidationAndViews(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	tokens := testTokens{}
	svc, _ := newTestService(t, remote, tokens)
	_, err := svc.Create(context.Background(), ServerConfig{Name: "bad", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", AuthEnv: "TOKEN_SECRET", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "invalid MCP auth_env") {
		t.Fatalf("err=%v", err)
	}
	server := createTestServer(t, svc, "https://mcp.example/rpc", "MCP_TOKEN_CRM")

	// Unset variable: validation takes the existing failure path.
	if v := view(server, svc.tokens); v.AuthEnv != "MCP_TOKEN_CRM" || v.AuthEnvSet {
		t.Fatalf("view=%+v", v)
	}
	if _, err := svc.Validate(context.Background(), server.ID); err == nil {
		t.Fatal("validated without token")
	}
	got, _ := svc.Get(context.Background(), server.ID)
	if got.ValidationState != StateAuthenticationFailed || remote.seen != "" {
		t.Fatalf("state=%s seen=%q", got.ValidationState, remote.seen)
	}

	tokens["MCP_TOKEN_CRM"] = "bearer-secret-sentinel"
	if v := view(server, svc.tokens); !v.AuthEnvSet {
		t.Fatalf("view=%+v", v)
	}
	if _, err := svc.Validate(context.Background(), server.ID); err != nil {
		t.Fatal(err)
	}
	if remote.seen != "bearer-secret-sentinel" {
		t.Fatalf("bearer did not reach remote: %q", remote.seen)
	}
	raw, _ := json.Marshal(view(server, svc.tokens))
	if strings.Contains(string(raw), "bearer-secret-sentinel") || !strings.Contains(string(raw), `"auth_env_set":true`) {
		t.Fatalf("view json=%s", raw)
	}
}

func TestCreateRejectsLegacyCredentialIDField(t *testing.T) {
	svc, _ := newTestService(t, &testRemote{}, testTokens{})
	request := httptest.NewRequest("POST", "/api/v1/mcp-servers", strings.NewReader(`{"name":"o","endpoint":"https://mcp.example/rpc","credential_id":"x","enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	Handler(svc).ServeHTTP(w, request)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d %s", w.Code, w.Body.String())
	}
}

// A row that bypassed Create (an old or hand-edited database) with a variable
// outside the MCP_TOKEN_ prefix must never send that variable's value out.
func TestStoredUnprefixedAuthEnvRefusedAtUse(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	env := map[string]string{"GOVOICE_MANAGEMENT_API_KEY": "mgmt-secret-sentinel"}
	svc, store := newTestService(t, remote, EnvTokens(func(k string) string { return env[k] }))
	m := storage.MCPServer{ID: storage.NewID(), Name: "legacy", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", AuthEnv: "GOVOICE_MANAGEMENT_API_KEY", Enabled: true}
	if err := store.MCP().CreateServer(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Validate(context.Background(), m.ID); err == nil {
		t.Fatal("validated with an unprefixed auth_env")
	}
	if _, err := svc.Refresh(context.Background(), m.ID); err == nil {
		t.Fatal("refreshed with an unprefixed auth_env")
	}
	if remote.seen != "" {
		t.Fatalf("unprefixed variable reached the remote: %q", remote.seen)
	}
	if v := view(m, svc.tokens); v.AuthEnvSet {
		t.Fatalf("view reports an unprefixed variable as set: %+v", v)
	}
}
