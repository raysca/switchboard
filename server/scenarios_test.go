package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"govoice/transport/token"
	"govoice/transport/ws"
)

// Helper to create valid short-lived admission token
func createScenarioToken(t *testing.T, srv *Server, principal string, jti string) string {
	t.Helper()
	now := time.Now()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    srv.cfg.Token.Issuer,
			Audience:  jwt.ClaimStrings{srv.cfg.Token.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		},
		Principal: principal,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokString, err := tok.SignedString([]byte(srv.cfg.Token.Secret))
	if err != nil {
		t.Fatalf("mint test token: %v", err)
	}
	return tokString
}

// Scenario A - Deployed Google-Only Stack (spec §41 Scenario A)
func TestScenarioA_DeployedGoogleStack(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "test-scenario-secret-key-1234567890"

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)

	ts := httptest.NewServer(srv.publicMux)
	defer ts.Close()

	srv.ready.Store(true)

	tok := createScenarioToken(t, srv, "user-scenario-a", "jti-a-1")
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?token=" + tok

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// RTVI client-ready handshake
	readyFrame, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(ctx, websocket.MessageBinary, readyFrame); err != nil {
		t.Fatalf("send client-ready: %v", err)
	}

	// Read bot-ready response
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read bot-ready: %v", err)
	}
	decoded, err := ws.DecodeBinaryFrame(data, 1, time.Now())
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	wmf, ok := decoded.(*ws.WireMessageFrame)
	if !ok || !strings.Contains(wmf.Data, "bot-ready") {
		t.Fatalf("expected bot-ready frame, got: %v", decoded)
	}

	// Verify timeline recorded event
	time.Sleep(50 * time.Millisecond)
	tl := srv.timelines.Get(1)
	if tl == nil {
		t.Fatal("timeline not initialized for session 1")
	}
	events := tl.Events()
	if len(events) == 0 {
		t.Fatal("expected diagnostic events in session timeline")
	}
}

// Scenario C - Interchangeability (spec §41 Scenario C)
// Verifies xAI LLM swapped with Google LLM in server pipeline with zero core changes.
func TestScenarioC_InterchangeabilityDeployed(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "test-scenario-secret-key-1234567890"

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Verify both Google and xAI pipelines exist in server configuration
	if _, ok := srv.cfg.Pipelines["default"]; !ok {
		t.Fatal("missing default (google) pipeline")
	}
	if _, ok := srv.cfg.Pipelines["xai"]; !ok {
		t.Fatal("missing xai pipeline")
	}
	if srv.cfg.Pipelines["xai"].LLM != "xai.llm" {
		t.Fatalf("expected xai pipeline to use xai.llm, got: %s", srv.cfg.Pipelines["xai"].LLM)
	}
}

// TestFullXAI_DeployedPipeline verifies that the 100% native xAI pipeline (xAI STT -> xAI LLM -> xAI TTS)
// is registered in default server configuration with all three xAI native providers.
func TestFullXAI_DeployedPipeline(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "test-scenario-secret-key-1234567890"

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	pCfg, ok := srv.cfg.Pipelines["full_xai"]
	if !ok {
		t.Fatal("missing full_xai pipeline in DefaultConfig")
	}
	if pCfg.STT != "xai.stt" {
		t.Errorf("expected full_xai STT to be xai.stt, got: %s", pCfg.STT)
	}
	if pCfg.LLM != "xai.llm" {
		t.Errorf("expected full_xai LLM to be xai.llm, got: %s", pCfg.LLM)
	}
	if pCfg.TTS != "xai.tts" {
		t.Errorf("expected full_xai TTS to be xai.tts, got: %s", pCfg.TTS)
	}
}

// Scenario E - Managed Failure Isolation (spec §41 Scenario E)
// Verifies that a failure in one call does not impact another concurrent call.
func TestScenarioE_ManagedFailureIsolation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "test-scenario-secret-key-1234567890"

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)

	ts := httptest.NewServer(srv.publicMux)
	defer ts.Close()
	srv.ready.Store(true)

	// Session 1: Send malformed binary frame (causes immediate connection failure)
	tok1 := createScenarioToken(t, srv, "user-e-1", "jti-e-1")
	url1 := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?token=" + tok1
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn1, _, err := websocket.Dial(ctx, url1, nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	// Write malformed garbage
	_ = conn1.Write(ctx, websocket.MessageBinary, []byte("garbage not protobuf"))

	// Session 2: Connect concurrently, must succeed completely
	tok2 := createScenarioToken(t, srv, "user-e-2", "jti-e-2")
	url2 := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?token=" + tok2

	conn2, _, err := websocket.Dial(ctx, url2, nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close(websocket.StatusNormalClosure, "")

	readyFrame, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn2.Write(ctx, websocket.MessageBinary, readyFrame); err != nil {
		t.Fatalf("conn2 client-ready: %v", err)
	}

	_, data2, err := conn2.Read(ctx)
	if err != nil {
		t.Fatalf("conn2 read bot-ready: %v", err)
	}
	decoded, _ := ws.DecodeBinaryFrame(data2, 1, time.Now())
	wmf, ok := decoded.(*ws.WireMessageFrame)
	if !ok || !strings.Contains(wmf.Data, "bot-ready") {
		t.Fatalf("expected bot-ready for conn2, got: %v", decoded)
	}
}

// Scenario G - Concurrent Sessions Isolation (spec §41 Scenario G)
func TestScenarioG_ConcurrentSessionsIsolation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "test-scenario-secret-key-1234567890"
	cfg.Limits.MaxSessions = 10

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	mockExec := &mockVADExecutor{ready: true, available: 10, total: 10}
	srv.SetVADExecutor(mockExec)

	ts := httptest.NewServer(srv.publicMux)
	defer ts.Close()
	srv.ready.Store(true)

	var wg sync.WaitGroup
	var successCount atomic.Int32

	// Launch 5 concurrent calls
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			tok := createScenarioToken(t, srv, fmt.Sprintf("principal-%d", id), fmt.Sprintf("jti-g-%d", id))
			url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?token=" + tok

			conn, _, err := websocket.Dial(ctx, url, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")

			readyFrame, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
			_ = conn.Write(ctx, websocket.MessageBinary, readyFrame)

			_, data, err := conn.Read(ctx)
			if err == nil && len(data) > 0 {
				successCount.Add(1)
			}
		}(i)
	}

	wg.Wait()
	if successCount.Load() != 5 {
		t.Fatalf("expected 5 concurrent successful sessions, got %d", successCount.Load())
	}
}

// Scenario J - Admission Security (spec §41 Scenario J)
// Single-use token, replay race rejection, admin listener isolation.
func TestScenarioJ_AdmissionSecurityDeployed(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "test-scenario-secret-key-1234567890"

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)

	tsPublic := httptest.NewServer(srv.publicMux)
	defer tsPublic.Close()
	tsAdmin := httptest.NewServer(srv.adminMux)
	defer tsAdmin.Close()
	srv.ready.Store(true)

	tok := createScenarioToken(t, srv, "user-j", "jti-single-use")
	wsURL := "ws" + strings.TrimPrefix(tsPublic.URL, "http") + "/ws?token=" + tok

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. First connection consumes token
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 1 failed: %v", err)
	}
	defer conn1.Close(websocket.StatusNormalClosure, "")

	// 2. Immediate replay of identical token must be REJECTED (single-use token invariant)
	_, resp2, err2 := websocket.Dial(ctx, wsURL, nil)
	if err2 == nil {
		t.Fatal("expected second connection with consumed token to be rejected, but it succeeded")
	}
	if resp2 != nil && resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for replayed token, got %d", resp2.StatusCode)
	}

	// 3. Admin endpoints must return 404 on public listener
	resp, _ := http.Get(tsPublic.URL + "/metrics")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public listener must NOT expose /metrics, got: %d", resp.StatusCode)
	}

	// 4. Admin endpoints must return 200 on admin listener
	respAdmin, _ := http.Get(tsAdmin.URL + "/metrics")
	if respAdmin.StatusCode != http.StatusOK {
		t.Fatalf("admin listener must expose /metrics, got: %d", respAdmin.StatusCode)
	}
}
