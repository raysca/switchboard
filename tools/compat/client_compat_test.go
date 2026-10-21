package compat_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/transport/token"
	"govoice/transport/ws"
)

const (
	compatSecret   = "v1-compat-secret-32-bytes-minimum!!"
	compatIssuer   = "govoice-v1"
	compatAudience = "govoice-v1-client"
)

func mintCompatToken(t *testing.T, jti string, iat time.Time, exp time.Time) string {
	t.Helper()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    compatIssuer,
			Audience:  jwt.ClaimStrings{compatAudience},
			IssuedAt:  jwt.NewNumericDate(iat),
			ExpiresAt: jwt.NewNumericDate(exp),
			Subject:   "client-js-user",
		},
		Principal: "client-js-user",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "v1"
	s, err := tok.SignedString([]byte(compatSecret))
	if err != nil {
		t.Fatalf("failed to sign compat token: %v", err)
	}
	return s
}

// FakeSTTForCompat converts client audio into user speech frames and transcription
type FakeSTTForCompat struct {
	triggered atomic.Bool
}

func (s *FakeSTTForCompat) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.InputAudioFrame); ok {
		if s.triggered.CompareAndSwap(false, true) {
			_ = e.PushDirection(&core.UserStartedSpeakingFrame{Timestamp: time.Now()}, core.Downstream)
			_ = e.PushDirection(&core.InterimTranscriptionFrame{Text: "Hello from", UserID: "user-js", Timestamp: time.Now(), Turn: "turn-1", Gen: 1}, core.Downstream)
			_ = e.PushDirection(&core.FinalTranscriptionFrame{
				Text:      "Hello from unmodified client-js",
				UserID:    "user-js",
				Timestamp: time.Now(),
				Turn:      "turn-1",
				Gen:       1,
			}, core.Downstream)
			_ = e.PushDirection(&core.UserStoppedSpeakingFrame{Timestamp: time.Now()}, core.Downstream)
			_ = e.PushDirection(&core.ErrorFrame{Err: errors.New("compat_test_error"), Fatal: false}, core.Downstream)
		}
		return nil
	}
	return e.PushDirection(f, dir)
}

func TestCompat_UnmodifiedClientPackage_RegressionGate(t *testing.T) {
	now := time.Now()
	validator, err := token.NewValidator(token.DefaultConfig(compatIssuer, compatAudience, []byte(compatSecret)), now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("NewValidator error: %v", err)
	}

	cfg := ws.ServerConfig{
		Security: ws.SecurityConfig{
			AllowedOrigins:     []string{"*"},
			AllowMissingOrigin: true,
		},
		TokenValidator:             validator,
		Pacer:                      ws.DefaultPacerConfig(),
		HandshakeTimeout:           5 * time.Second,
		RTVIReadyTimeout:           5 * time.Second,
		ReadTimeout:                30 * time.Second,
		PingInterval:               15 * time.Second,
		PingTimeout:                10 * time.Second,
		PingMaxFailures:            2,
		MaxConnectionLifetime:      60 * time.Minute,
		MaxConcurrentSessions:      100,
		MaxConcurrentSessionsPerIP: 10,
		MaxPendingHandshakes:       50,
		HandshakeRateLimit:         20.0,
		FixedPlayerRate:            ws.FixedPlayerSampleRate,
	}
	server := ws.NewServer(cfg)

	var sessionTornDown atomic.Bool
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		ctxModel := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
		userCtx := core.NewUserContextProcessor(ctxModel, core.UserContextConfig{TurnPrefix: "turn"})
		asstCtx := core.NewAssistantContextProcessor(ctxModel, core.DefaultAssistantContextConfig())
		fakeSTT := &FakeSTTForCompat{}
		fakeLLM := coretest.NewLLMProcessor(coretest.LLMConfig{
			ResponseChunks: []string{"Hello ", "client-js!"},
		})
		fakeTTS := coretest.NewTTSProcessor(coretest.TTSConfig{
			FramesPerUnit: 2,
		})

		pipe, err := core.NewPipeline(fakeSTT, userCtx, fakeLLM, asstCtx, fakeTTS, conn)
		if err != nil {
			return nil, err
		}
		sess, err := core.NewSession(500, pipe, core.DefaultResourceBudget())
		if err != nil {
			return nil, err
		}
		go func() {
			<-sess.Done()
			sessionTornDown.Store(true)
		}()
		return sess, nil
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	tok := mintCompatToken(t, "jti-client-js-1", now, now.Add(5*time.Minute))

	// Execute Node client test script with exact released packages
	scriptPath := filepath.Join(".", "test_rtvi_client.js")
	cmd := exec.Command("node", scriptPath, "--url", wsURL, "--token", tok)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Unmodified client-js test failed:\nOutput: %s\nError: %v", string(out), err)
	}
	t.Logf("Node client output:\n%s", string(out))

	// Wait for clean session teardown
	time.Sleep(100 * time.Millisecond)
	if !sessionTornDown.Load() {
		t.Fatalf("Session was not cleanly torn down after client disconnect")
	}

	// Verify consumed token cannot reconnect
	reconnectCmd := exec.Command("node", scriptPath, "--url", wsURL, "--token", tok)
	reconnectOut, reconnectErr := reconnectCmd.CombinedOutput()
	if reconnectErr == nil {
		t.Fatalf("Reconnection with consumed token should have failed, but succeeded:\n%s", string(reconnectOut))
	}
	t.Logf("Consumed token reconnect correctly failed as expected.")
}
