package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

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
