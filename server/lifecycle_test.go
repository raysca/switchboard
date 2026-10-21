package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServer_GracefulShutdownLifecycle(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:0"
	cfg.AdminListenAddr = "127.0.0.1:0"
	cfg.Limits.DrainTimeout = 100 * time.Millisecond

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Verify server is ready
	if !srv.ready.Load() {
		t.Fatal("expected server to be ready after start")
	}

	// Verify ready endpoint on admin listener
	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	// In test without live providers, PipelinesLive might be false or true depending on registry, but readiness endpoint exists
	// Now simulate active call drain
	srv.mu.Lock()
	srv.activeCalls[101] = &activeCall{
		sessionID: 101,
		principal: "test-principal",
	}
	srv.mu.Unlock()

	// In a goroutine, simulate session drain after 30ms
	go func() {
		time.Sleep(30 * time.Millisecond)
		srv.mu.Lock()
		delete(srv.activeCalls, 101)
		srv.mu.Unlock()
	}()

	// Trigger shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Verify unready and shut down
	if srv.ready.Load() {
		t.Fatal("expected server to be unready after shutdown")
	}
	if !srv.shuttingDown.Load() {
		t.Fatal("expected shuttingDown flag to be true")
	}

	// Verify ready endpoint now returns 503 Service Unavailable
	reqAfter := httptest.NewRequest("GET", "/ready", nil)
	wAfter := httptest.NewRecorder()
	srv.adminMux.ServeHTTP(wAfter, reqAfter)
	if wAfter.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on /ready after shutdown, got %d", wAfter.Code)
	}
}

func TestServer_ImmediateCancelShutdown(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:0"
	cfg.AdminListenAddr = "127.0.0.1:0"
	cfg.Limits.ImmediateCancel = true
	cfg.Limits.DrainTimeout = 10 * time.Second // Should be ignored because ImmediateCancel is true

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	start := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("immediate cancel took too long: %v (expected < 500ms)", elapsed)
	}
}

func TestServer_ForcedShutdownTimeoutReturnsError(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:0"
	cfg.AdminListenAddr = "127.0.0.1:0"

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Expired context simulates forced shutdown timeout
	shutdownCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err = srv.Shutdown(shutdownCtx)
	if err == nil {
		t.Fatal("expected non-nil error when shutdown deadline is exceeded, got nil")
	}
}

func TestServer_DrainTimeoutForcesCancellation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:0"
	cfg.AdminListenAddr = "127.0.0.1:0"
	cfg.Limits.DrainTimeout = 50 * time.Millisecond // very short drain timeout

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Register an active call that NEVER drains on its own
	srv.mu.Lock()
	srv.activeCalls[202] = &activeCall{
		sessionID: 202,
		principal: "undrained-principal",
	}
	srv.mu.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = srv.Shutdown(shutdownCtx)
	if err == nil {
		t.Fatal("expected error on forced cancellation when drain times out, got nil")
	}
	if !strings.Contains(err.Error(), "forced cancellation of 1 sessions") {
		t.Fatalf("expected forced cancellation error message, got: %v", err)
	}

	// Server should be shut down and unready
	if srv.ready.Load() {
		t.Fatal("expected server to be unready after forced shutdown")
	}
	if !srv.shuttingDown.Load() {
		t.Fatal("expected shuttingDown flag to be true")
	}
}

func TestServer_ServeTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeSelfSigned(t, dir)

	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:0"
	cfg.AdminListenAddr = "127.0.0.1:0"
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = client.Get("https://" + publicAddr(srv) + "/health")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("https health: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("response was not TLS")
	}
}

func writeSelfSigned(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// publicAddr is the bound public listener address once Start has returned.
func publicAddr(s *Server) string { return s.publicAddr }
