package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"govoice/transport/token"
)

func apiTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func apiRequest(s *Server, method, path, bearer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, r)
	return w
}

func TestAPIAuthRouteSeparationAndEnvelope(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	w := apiRequest(s, http.MethodGet, "/api/v1/server-config", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", w.Code)
	}
	var failure struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Error.Code == "" || failure.Error.RequestID == "" {
		t.Fatalf("bad envelope: %s %v", w.Body.String(), err)
	}
	if w.Header().Get("X-Request-ID") != failure.Error.RequestID {
		t.Fatal("request ID mismatch")
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", "wrong-key-sentinel", ""); strings.Contains(w.Body.String(), "wrong-key-sentinel") {
		t.Fatal("unauthorized response echoed credential")
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", s.cfg.Token.Secret, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("voice secret admitted: %d", w.Code)
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", key, ""); w.Code != http.StatusOK {
		t.Fatalf("valid key status=%d body=%s", w.Code, w.Body.String())
	}
	pub := httptest.NewRecorder()
	s.publicMux.ServeHTTP(pub, httptest.NewRequest("GET", "/api/v1/server-config", nil))
	if pub.Code != http.StatusNotFound {
		t.Fatalf("management route exposed publicly: %d", pub.Code)
	}
	if w := apiRequest(s, http.MethodDelete, "/api/v1/server-config", key, ""); w.Code != http.StatusMethodNotAllowed || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("method error: %d %s", w.Code, w.Body.String())
	}
}

func TestAPITokenExpiryScopeReplay(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", key, `{"principal":"alice","pipeline":"default"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
			Scope     string    `json:"scope"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Scope != "voice:connect" || response.Data.ExpiresAt.After(time.Now().Add(61*time.Second)) {
		t.Fatalf("bad scope/lifetime: %+v", response.Data)
	}
	claims, err := s.validator.ValidateAndConsume(response.Data.Token)
	if err != nil || claims.Principal != "alice" || claims.Scope != "voice:connect" {
		t.Fatalf("invalid issued token: %+v %v", claims, err)
	}
	if !response.Data.ExpiresAt.Equal(claims.ExpiresAt.Time) {
		t.Fatalf("reported expiry %s differs from JWT expiry %s", response.Data.ExpiresAt, claims.ExpiresAt.Time)
	}
	if _, err := s.validator.ValidateAndConsume(response.Data.Token); err != token.ErrTokenReplayed {
		t.Fatalf("replay: %v", err)
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", response.Data.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("voice token admitted to management: %d", w.Code)
	}
	for _, tc := range []struct {
		name, scope string
		expiry      time.Time
	}{
		{"wrong scope", "management:read", time.Now().Add(time.Minute)},
		{"expired", "voice:connect", time.Now().Add(-time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issued := time.Now()
			if tc.name == "expired" {
				issued = time.Now().Add(-2 * time.Minute)
			}
			claims := token.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: s.cfg.Token.Issuer, Audience: jwt.ClaimStrings{s.cfg.Token.Audience}, Subject: "alice", ID: tc.name, IssuedAt: jwt.NewNumericDate(issued), ExpiresAt: jwt.NewNumericDate(tc.expiry)}, Scope: tc.scope}
			jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			jwtToken.Header["kid"] = s.cfg.Token.KeyID
			signed, err := jwtToken.SignedString(s.voiceSignKey)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.validator.ValidateAndConsume(signed)
			if err == nil {
				t.Fatal("accepted invalid voice token")
			}
			if tc.name == "expired" && !errors.Is(err, token.ErrTokenExpired) {
				t.Fatalf("expiry error=%v", err)
			}
			if tc.name == "wrong scope" && !errors.Is(err, token.ErrTokenInvalid) {
				t.Fatalf("scope error=%v", err)
			}
		})
	}
}

func TestAuthManagementUnavailableAndSeparateKey(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-secret-32-characters-long-123456"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(s, "GET", "/api/v1/server-config", "anything", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status=%d", w.Code)
	}
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", cfg.Token.Secret)
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("accepted identical management and voice secrets")
	}
}

func TestAuthDefaultEnvironmentForLoadedConfig(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Management.APIKeyEnv = "" // YAML without a management block
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(s, "GET", "/api/v1/server-config", "management-test-key-32-bytes-long-and-separate", ""); w.Code != http.StatusOK {
		t.Fatalf("default env status=%d", w.Code)
	}
}

func TestAPIConfigSecretRedactionAndJSONErrors(t *testing.T) {
	s := apiTestServer(t)
	s.cfg.Providers.GoogleLLM.SystemPrompt = "secret-prompt-sentinel"
	w := apiRequest(s, "GET", "/api/v1/server-config", "management-test-key-32-bytes-long-and-separate", "")
	for _, secret := range []string{s.cfg.Token.Secret, "management-test-key-32-bytes-long-and-separate", "secret-prompt-sentinel", s.cfg.Providers.GoogleLLM.APIKeyEnv} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("config leaked secret: %s", secret)
		}
	}
	w = apiRequest(s, "POST", "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","unexpected":1}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("invalid JSON: %d %s", w.Code, w.Body.String())
	}
}
