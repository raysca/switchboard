package token_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"govoice/transport/token"
)

const (
	testSecret   = "test-secret-at-least-32-bytes-long!!"
	testIssuer   = "test-issuer"
	testAudience = "test-audience"
)

func createToken(t *testing.T, jti string, iss string, aud string, iat time.Time, exp time.Time, alg jwt.SigningMethod, secret []byte) string {
	t.Helper()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    iss,
			Audience:  jwt.ClaimStrings{aud},
			IssuedAt:  jwt.NewNumericDate(iat),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Principal: "user-123",
	}

	tok := jwt.NewWithClaims(alg, claims)
	tokenStr, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return tokenStr
}

func TestToken_ValidAndConsume(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	val, err := token.NewValidator(cfg, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("NewValidator failed: %v", err)
	}

	tok := createToken(t, "jti-1", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))

	// First consumption: success
	claims, err := val.ValidateAndConsume(tok)
	if err != nil {
		t.Fatalf("expected successful validation, got error: %v", err)
	}
	if claims.ID != "jti-1" || claims.Principal != "user-123" {
		t.Fatalf("unexpected claims: %+v", claims)
	}

	// Replay attempt: rejected
	_, err = val.ValidateAndConsume(tok)
	if err == nil || err != token.ErrTokenReplayed {
		t.Fatalf("expected ErrTokenReplayed, got: %v", err)
	}
}

func TestToken_ConcurrentReplayRace(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	val, err := token.NewValidator(cfg, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("NewValidator failed: %v", err)
	}

	tok := createToken(t, "jti-race", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))

	concurrency := 20
	var wg sync.WaitGroup
	var successes int
	var failures int
	var mu sync.Mutex

	startBarrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier
			_, err := val.ValidateAndConsume(tok)
			mu.Lock()
			if err == nil {
				successes++
			} else if err == token.ErrTokenReplayed {
				failures++
			}
			mu.Unlock()
		}()
	}

	close(startBarrier)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful consumer in race, got %d", successes)
	}
	if failures != concurrency-1 {
		t.Fatalf("expected %d replay rejections, got %d", concurrency-1, failures)
	}
}

func TestToken_Expired(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	val, _ := token.NewValidator(cfg, now.Add(-time.Hour))

	tok := createToken(t, "jti-exp", testIssuer, testAudience, now.Add(-10*time.Minute), now.Add(-5*time.Minute), jwt.SigningMethodHS256, []byte(testSecret))
	_, err := val.ValidateAndConsume(tok)
	if err != token.ErrTokenExpired {
		t.Fatalf("expected ErrTokenExpired, got: %v", err)
	}
}

func TestToken_WrongIssuerAndAudience(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	val, _ := token.NewValidator(cfg, now.Add(-time.Minute))

	// Wrong issuer
	tok := createToken(t, "jti-iss", "wrong-issuer", testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))
	_, err := val.ValidateAndConsume(tok)
	if err != token.ErrTokenIssuer {
		t.Fatalf("expected ErrTokenIssuer, got: %v", err)
	}

	// Wrong audience
	tok = createToken(t, "jti-aud", testIssuer, "wrong-audience", now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))
	_, err = val.ValidateAndConsume(tok)
	if err != token.ErrTokenAudience {
		t.Fatalf("expected ErrTokenAudience, got: %v", err)
	}
}

func TestToken_DisallowedAlgorithm(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	cfg.AllowedAlgorithms = []string{"HS256"}
	val, _ := token.NewValidator(cfg, now.Add(-time.Minute))

	// Sign with HS384 (not in allowlist)
	tok := createToken(t, "jti-alg", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS384, []byte(testSecret))
	_, err := val.ValidateAndConsume(tok)
	if err != token.ErrTokenAlgorithm {
		t.Fatalf("expected ErrTokenAlgorithm, got: %v", err)
	}
}

func TestToken_PostRestartBoundary(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	cfg.ClockSkew = 2 * time.Second

	// Server started at 'now'
	val, _ := token.NewValidator(cfg, now)

	// Token issued 10 seconds before server start (beyond clock skew)
	oldIat := now.Add(-10 * time.Second)
	tok := createToken(t, "jti-old", testIssuer, testAudience, oldIat, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))

	_, err := val.ValidateAndConsume(tok)
	if err != token.ErrTokenBeforeStart {
		t.Fatalf("expected ErrTokenBeforeStart, got: %v", err)
	}
}

func TestToken_MissingPrincipalRejected(t *testing.T) {
	now := time.Now()
	val, err := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "jti-anon",
			Issuer:    testIssuer,
			Audience:  jwt.ClaimStrings{testAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(30 * time.Second)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := val.ValidateAndConsume(signed); err != token.ErrTokenPrincipal {
		t.Fatalf("expected ErrTokenPrincipal, got %v", err)
	}
}

func TestToken_Redaction(t *testing.T) {
	rawURL := "wss://api.example.com/v1/connect?token=secret-token-value-12345&client=pipecat"
	redactedURL := token.RedactToken(rawURL)
	expectedURL := "wss://api.example.com/v1/connect?token=[REDACTED]&client=pipecat"
	if redactedURL != expectedURL {
		t.Fatalf("expected %q, got %q", expectedURL, redactedURL)
	}

	logLine := fmt.Sprintf("failed handshake from ip=1.2.3.4 url=/ws?token=%s&foo=bar", "very-secret-token")
	redactedLog := token.RedactToken(logLine)
	if redactedLog != "failed handshake from ip=1.2.3.4 url=/ws?token=[REDACTED]&foo=bar" {
		t.Fatalf("unexpected redacted log: %s", redactedLog)
	}
}
