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
			Subject:   "user-123",
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

func TestToken_ScopedRestartRejects(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	first, err := token.NewValidator(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	claims := token.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: testIssuer, Audience: jwt.ClaimStrings{testAudience}, Subject: "user-123", ID: "scoped-jti", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Scope: "voice:connect", IssuerEpoch: first.ScopeEpoch()}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ValidateAndConsume(signed); err != nil {
		t.Fatalf("current validator rejected token: %v", err)
	}
	second, err := token.NewValidator(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.ValidateAndConsume(signed); err != token.ErrTokenEpoch {
		t.Fatalf("replacement validator accepted/reclassified token: %v", err)
	}
}

func TestToken_LegacyUnscopedRestartCompatibility(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	old, err := token.NewValidator(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	signed := createToken(t, "legacy-jti", testIssuer, testAudience, now, now.Add(time.Minute), jwt.SigningMethodHS256, []byte(testSecret))
	if _, err := old.ValidateAndConsume(signed); err != nil {
		t.Fatal(err)
	}
	replacement, err := token.NewValidator(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.ValidateAndConsume(signed); err != nil {
		t.Fatalf("legacy compatibility changed: %v", err)
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

	claims.Principal = "principal-only"
	tok = jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err = tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := val.ValidateAndConsume(signed); err != token.ErrTokenPrincipal {
		t.Fatalf("principal without sub must be rejected, got %v", err)
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

func TestToken_ReplayCacheFull_NeverEvictsUnexpired(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	cfg.ReplayCacheSize = 3 // small cache for test
	val, err := token.NewValidator(cfg, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	tok1 := createToken(t, "jti-1", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))
	tok2 := createToken(t, "jti-2", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))
	tok3 := createToken(t, "jti-3", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))
	tok4 := createToken(t, "jti-4", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))

	if _, err := val.ValidateAndConsume(tok1); err != nil {
		t.Fatalf("tok1 consume: %v", err)
	}
	if _, err := val.ValidateAndConsume(tok2); err != nil {
		t.Fatalf("tok2 consume: %v", err)
	}
	if _, err := val.ValidateAndConsume(tok3); err != nil {
		t.Fatalf("tok3 consume: %v", err)
	}

	// Cache is full of unexpired tokens. tok4 must be rejected with ErrReplayCacheFull!
	if _, err := val.ValidateAndConsume(tok4); err != token.ErrReplayCacheFull {
		t.Fatalf("expected ErrReplayCacheFull for tok4, got: %v", err)
	}

	// tok1 MUST NOT have been evicted; replaying tok1 must still yield ErrTokenReplayed!
	if _, err := val.ValidateAndConsume(tok1); err != token.ErrTokenReplayed {
		t.Fatalf("expected tok1 to still be rejected with ErrTokenReplayed, got: %v", err)
	}
}

func TestToken_RequireKeyID(t *testing.T) {
	now := time.Now()
	cfg := token.DefaultConfig(testIssuer, testAudience, []byte(testSecret))
	cfg.RequireKeyID = true
	cfg.KeyID = "v1"
	val, err := token.NewValidator(cfg, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// Missing kid
	tokNoKid := createToken(t, "jti-nokid", testIssuer, testAudience, now, now.Add(30*time.Second), jwt.SigningMethodHS256, []byte(testSecret))
	if _, err := val.ValidateAndConsume(tokNoKid); err == nil {
		t.Fatal("expected error for token with missing kid")
	}

	// Mismatched kid
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "jti-wrongkid",
			Issuer:    testIssuer,
			Audience:  jwt.ClaimStrings{testAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(30 * time.Second)),
		},
		Principal: "user-1",
	}
	claims.Subject = "user-1"
	tokWrong := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokWrong.Header["kid"] = "v2"
	signedWrong, err := tokWrong.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := val.ValidateAndConsume(signedWrong); err == nil {
		t.Fatal("expected error for token with mismatched kid")
	}

	// Valid kid
	tokValid := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokValid.Header["kid"] = "v1"
	signedValid, err := tokValid.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	c, err := val.ValidateAndConsume(signedValid)
	if err != nil {
		t.Fatalf("valid kid failed: %v", err)
	}
	if c.ID != "jti-wrongkid" {
		t.Fatalf("unexpected claims: %+v", c)
	}
}
